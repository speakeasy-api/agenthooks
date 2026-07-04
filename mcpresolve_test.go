package agenthooks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolateHome points HOME, the per-provider config-dir overrides, and PATH
// away from the developer's real config files and `claude` binary so
// resolution only sees what the test set up.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("KIMI_CODE_HOME", filepath.Join(home, ".kimi-code"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PATH", filepath.Join(home, "bin-empty"))
	return home
}

// mcpTestRunner isolates the on-disk state dir so per-session mcp-list
// caches can't leak across tests or into the real temp dir.
func mcpTestRunner(t *testing.T, opts ...Option) *Runner {
	t.Helper()
	return quietRunner(append([]Option{WithDedupDir(t.TempDir())}, opts...)...)
}

// fakeClaudeCLI installs a `claude` shim on PATH that appends to a counter
// file and prints the given `claude mcp list` output.
func fakeClaudeCLI(t *testing.T, output string) (countFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude CLI shim requires a POSIX shell")
	}
	binDir := t.TempDir()
	countFile = filepath.Join(binDir, "count")
	// The test PATH holds only the shim, so restore a real one for `cat`.
	script := "#!/bin/sh\nPATH=/bin:/usr/bin\necho x >> " + countFile +
		"\ncat <<'EOF'\n" + strings.TrimRight(output, "\n") + "\nEOF\n"
	writeConfig(t, filepath.Join(binDir, "claude"), script)
	if err := os.Chmod(filepath.Join(binDir, "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	return countFile
}

func cliRuns(t *testing.T, countFile string) int {
	t.Helper()
	data, err := os.ReadFile(countFile)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "x")
}

func mcpToolPre(p Provider, cwd, name string) *ToolPreEvent {
	s := SessionInfo{ID: "sess-mcp", CWD: cwd}
	return &ToolPreEvent{
		Event: Event{Provider: p, Kind: KindToolPre, Session: s},
		Tool:  makeToolCall(s, name, "tid-1", nil, nil),
	}
}

func TestResolveMCPClaudeProjectConfig(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{
		"github":         {"url": "https://api.example.com/mcp"},
		"local (stdio)":  {"command": "npx", "args": ["-y", "srv"]}
	}}`)
	r := mcpTestRunner(t)

	ev := mcpToolPre(ProviderClaudeCode, cwd, "mcp__github__create_issue")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://api.example.com/mcp" || !ev.Tool.MCP.FromConfig {
		t.Errorf("url not resolved: %+v", ev.Tool.MCP)
	}
	if ev.Tool.MCP.Server != "github" || ev.Tool.MCP.Tool != "create_issue" {
		t.Errorf("identity clobbered: %+v", ev.Tool.MCP)
	}

	// "local (stdio)" sanitizes to prefix local_stdio (spaces -> _, parens dropped).
	ev = mcpToolPre(ProviderClaudeCode, cwd, "mcp__local_stdio__run")
	r.resolveMCP(ev)
	if ev.Tool.MCP.Command != "npx -y srv" || ev.Tool.MCP.URL != "" {
		t.Errorf("stdio command not resolved: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPClaudeScopePrecedence(t *testing.T) {
	home := isolateHome(t)
	cwd := "/work/proj" // no .mcp.json on disk; only ~/.claude.json scopes apply
	writeConfig(t, filepath.Join(home, ".claude.json"), `{
		"mcpServers": {"linear": {"url": "https://user.example.com/mcp"}},
		"projects": {"/work/proj": {"mcpServers": {"linear": {"url": "https://local.example.com/mcp"}}}}
	}`)
	ev := mcpToolPre(ProviderClaudeCode, cwd, "mcp__linear__list_issues")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://local.example.com/mcp" {
		t.Errorf("local scope must win over user scope: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPClaudeAmbiguousPrefix(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	// Both names sanitize to the prefix "a_b": attribution is ambiguous.
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{
		"a b": {"url": "https://one.example.com"},
		"a_b": {"url": "https://two.example.com"}
	}}`)
	ev := mcpToolPre(ProviderClaudeCode, cwd, "mcp__a_b__tool")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || ev.Tool.MCP.FromConfig {
		t.Errorf("ambiguous match must stay empty: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPCodex(t *testing.T) {
	home := isolateHome(t)
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `
model = "gpt-5" # unrelated top-level key

[mcp_servers.int-linear] # sanitizes to int_linear
command = "npx"
args = [
  "-y",
  "linear-mcp",
]

[mcp_servers."foo--bar"]
url = "https://foo.example.com/mcp"

[mcp_servers.off]
url = "https://off.example.com"
enabled = false

[mcp_servers.int-linear.env]
url = "https://red-herring.example.com"
`)
	r := mcpTestRunner(t)

	ev := mcpToolPre(ProviderCodex, "", "mcp__int_linear__create_issue")
	r.resolveMCP(ev)
	if ev.Tool.MCP.Command != "npx -y linear-mcp" || ev.Tool.MCP.URL != "" || !ev.Tool.MCP.FromConfig {
		t.Errorf("codex stdio not resolved: %+v", ev.Tool.MCP)
	}

	// "foo--bar" sanitizes to "foo__bar": the naive first-"__" split yields
	// Server "foo" / Tool "bar__list"; longest-prefix matching repairs it.
	ev = mcpToolPre(ProviderCodex, "", "mcp__foo__bar__list")
	r.resolveMCP(ev)
	if ev.Tool.MCP.Server != "foo__bar" || ev.Tool.MCP.Tool != "list" {
		t.Errorf("codex split not repaired: %+v", ev.Tool.MCP)
	}
	if ev.Tool.MCP.URL != "https://foo.example.com/mcp" {
		t.Errorf("codex url not resolved: %+v", ev.Tool.MCP)
	}

	ev = mcpToolPre(ProviderCodex, "", "mcp__off__anything")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "" {
		t.Errorf("disabled server must not resolve: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPCursor(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".cursor", "mcp.json"),
		`{"mcpServers":{"shortcut":{"url":"https://mcp.shortcut.com/sse"}}}`)
	r := mcpTestRunner(t)

	// MCP:<tool> has no server identity; a single configured server is the
	// only sound attribution.
	ev := mcpToolPre(ProviderCursor, cwd, "MCP:create_story")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://mcp.shortcut.com/sse" || ev.Tool.MCP.Server != "shortcut" || !ev.Tool.MCP.FromConfig {
		t.Errorf("single-server cursor attribution failed: %+v", ev.Tool.MCP)
	}

	// A second server (user scope) makes attribution ambiguous.
	writeConfig(t, filepath.Join(home, ".cursor", "mcp.json"),
		`{"mcpServers":{"other":{"command":"other-mcp"}}}`)
	ev = mcpToolPre(ProviderCursor, cwd, "MCP:create_story")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || ev.Tool.MCP.FromConfig {
		t.Errorf("multi-server cursor attribution must stay empty: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPPayloadTransportWins(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".cursor", "mcp.json"),
		`{"mcpServers":{"shortcut":{"url":"https://from-config.example.com"}}}`)
	ev := mcpToolPre(ProviderCursor, cwd, "MCP:create_story")
	ev.Tool.MCP.URL = "https://from-payload.example.com" // as beforeMCPExecution ships it
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://from-payload.example.com" || ev.Tool.MCP.FromConfig {
		t.Errorf("payload-borne transport must never be overwritten: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPGeminiSplitRepair(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".gemini", "settings.json"),
		`{"mcpServers":{"my_srv":{"httpUrl":"https://g.example.com/mcp"}}}`)
	// Naive single-underscore split yields Server "my" / Tool "srv_do";
	// matching against the configured name repairs it (quirk #15).
	ev := mcpToolPre(ProviderGemini, cwd, "mcp_my_srv_do")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.Server != "my_srv" || ev.Tool.MCP.Tool != "do" || ev.Tool.MCP.URL != "https://g.example.com/mcp" {
		t.Errorf("gemini split not repaired: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPKimi(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	r := mcpTestRunner(t)

	// Mirrors the live probe against kimi-code 0.22.2: project-scoped
	// .kimi-code/mcp.json, hyphenated server name kept verbatim in
	// mcp__test-echo__echo.
	writeConfig(t, filepath.Join(cwd, ".kimi-code", "mcp.json"),
		`{"mcpServers":{"test-echo":{"command":"npx","args":["-y","@modelcontextprotocol/server-everything"]}}}`)
	ev := mcpToolPre(ProviderKimi, cwd, "mcp__test-echo__echo")
	r.resolveMCP(ev)
	if ev.Tool.MCP.Command != "npx -y @modelcontextprotocol/server-everything" || !ev.Tool.MCP.FromConfig {
		t.Errorf("kimi project-scope transport not resolved: %+v", ev.Tool.MCP)
	}
	if ev.Tool.MCP.Server != "test-echo" || ev.Tool.MCP.Tool != "echo" {
		t.Errorf("kimi hyphenated split wrong: %+v", ev.Tool.MCP)
	}

	// KIMI_CODE_HOME (isolateHome pins it under the temp home).
	writeConfig(t, filepath.Join(home, ".kimi-code", "mcp.json"),
		`{"mcpServers":{"github":{"url":"https://kimi.example.com/mcp"}}}`)
	ev = mcpToolPre(ProviderKimi, "", "mcp__github__create_issue")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://kimi.example.com/mcp" {
		t.Errorf("kimi user-scope transport not resolved: %+v", ev.Tool.MCP)
	}

	// Legacy kimi-cli fallback path.
	writeConfig(t, filepath.Join(home, ".kimi", "mcp.json"),
		`{"mcpServers":{"legacy":{"url":"https://legacy.example.com/mcp"}}}`)
	ev = mcpToolPre(ProviderKimi, "", "mcp__legacy__x")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://legacy.example.com/mcp" {
		t.Errorf("legacy ~/.kimi fallback not read: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPOpenCodeDetection(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	// Mirrors the live probe against opencode 1.17.8: MCP tools are named
	// <server>_<tool> verbatim with no reserved prefix, so the codec cannot
	// classify them — the config match performs detection too (quirk #28).
	writeConfig(t, filepath.Join(cwd, "opencode.json"), `{
		"mcp": {
			"test-echo": {"type": "local", "command": ["npx", "-y", "@modelcontextprotocol/server-everything"]},
			"tracker":   {"type": "remote", "url": "https://tracker.example.com/mcp"},
			"off":       {"type": "remote", "url": "https://off.example.com", "enabled": false}
		}
	}`)
	r := mcpTestRunner(t)

	ev := mcpToolPre(ProviderOpenCode, cwd, "test-echo_echo")
	if ev.Tool.MCP != nil || ev.Tool.Canonical == ToolMCP {
		t.Fatalf("precondition: codec must not classify opencode MCP names: %+v", ev.Tool)
	}
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.Canonical != ToolMCP {
		t.Fatalf("opencode MCP call not detected: %+v", ev.Tool)
	}
	if ev.Tool.MCP.Server != "test-echo" || ev.Tool.MCP.Tool != "echo" ||
		ev.Tool.MCP.Command != "npx -y @modelcontextprotocol/server-everything" || !ev.Tool.MCP.FromConfig {
		t.Errorf("opencode identity/transport wrong: %+v", ev.Tool.MCP)
	}

	ev = mcpToolPre(ProviderOpenCode, cwd, "tracker_list_issues")
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://tracker.example.com/mcp" || ev.Tool.MCP.Tool != "list_issues" {
		t.Errorf("opencode remote server wrong: %+v", ev.Tool.MCP)
	}

	// Disabled server: no detection, stays a plain tool.
	ev = mcpToolPre(ProviderOpenCode, cwd, "off_anything")
	r.resolveMCP(ev)
	if ev.Tool.MCP != nil {
		t.Errorf("disabled opencode server must not detect: %+v", ev.Tool.MCP)
	}

	// Native tool: untouched.
	ev = mcpToolPre(ProviderOpenCode, cwd, "bash")
	r.resolveMCP(ev)
	if ev.Tool.MCP != nil || ev.Tool.Canonical != ToolShell {
		t.Errorf("native opencode tool must stay native: %+v", ev.Tool)
	}
}

func TestResolveMCPOpenCodeJSONCGlobal(t *testing.T) {
	home := isolateHome(t)
	// Real-world shape: the global config is a .jsonc with comments under
	// $XDG_CONFIG_HOME/opencode.
	writeConfig(t, filepath.Join(home, ".config", "opencode", "opencode.jsonc"), `{
		// comment with a "quote" and a url: https://nope.example.com
		"$schema": "https://opencode.ai/config.json",
		/* block
		   comment */
		"mcp": {
			"notes": {"type": "remote", "url": "https://notes.example.com/mcp"}
		}
	}`)
	ev := mcpToolPre(ProviderOpenCode, "", "notes_search")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://notes.example.com/mcp" {
		t.Errorf("jsonc global config not read: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPOpenCodeLongestPrefixWins(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, "opencode.json"), `{
		"mcp": {
			"a":   {"type": "remote", "url": "https://short.example.com"},
			"a_b": {"type": "remote", "url": "https://long.example.com"}
		}
	}`)
	ev := mcpToolPre(ProviderOpenCode, cwd, "a_b_do")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.Server != "a_b" || ev.Tool.MCP.Tool != "do" ||
		ev.Tool.MCP.URL != "https://long.example.com" {
		t.Errorf("longest-prefix match wrong: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPDisabled(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".mcp.json"),
		`{"mcpServers":{"github":{"url":"https://api.example.com/mcp"}}}`)
	ev := mcpToolPre(ProviderClaudeCode, cwd, "mcp__github__create_issue")
	mcpTestRunner(t, WithoutMCPResolution()).resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || ev.Tool.MCP.FromConfig {
		t.Errorf("WithoutMCPResolution must be a no-op: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPNonMCPToolUntouched(t *testing.T) {
	isolateHome(t)
	ev := mcpToolPre(ProviderClaudeCode, t.TempDir(), "Bash")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP != nil {
		t.Errorf("non-MCP tool must stay non-MCP: %+v", ev.Tool)
	}
}

func TestRunClaudeMCPTransportResolved(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".mcp.json"),
		`{"mcpServers":{"github":{"url":"https://api.example.com/mcp"}}}`)
	payload := fmt.Sprintf(
		`{"hook_event_name":"PreToolUse","session_id":"s1","cwd":%q,"tool_name":"mcp__github__create_issue","tool_input":{"title":"x"}}`,
		cwd)

	r := mcpTestRunner(t)
	var got MCPCall
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		got = *e.Tool.MCP
		return NoDecision(), nil
	})
	if _, code := runWith(t, r, claudeArgs(), []byte(payload)); code != 0 {
		t.Fatalf("run failed with exit %d", code)
	}
	if got.URL != "https://api.example.com/mcp" || !got.FromConfig {
		t.Errorf("handler must see resolved transport: %+v", got)
	}
}

func TestParseCodexMCPServers(t *testing.T) {
	entries := parseCodexMCPServers([]byte(`
# leading comment
inline = { note = "unrelated inline table" }

[mcp_servers."with space"] # trailing comment
command = "run" # comment after value
args = ["--flag", "a # not a comment"]

[other_table]
url = "https://not-a-server.example.com"

[mcp_servers.plain]
url = "https://plain.example.com"

[mcp_servers.plain.env]
SECRET = "ignored"
`))
	// Entries come back name-sorted.
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %+v", entries)
	}
	if entries[0].Name != "plain" || entries[0].URL != "https://plain.example.com" {
		t.Errorf("plain entry wrong: %+v", entries[0])
	}
	if entries[1].Name != "with space" || entries[1].Command != `run --flag a # not a comment` {
		t.Errorf("quoted header/values wrong: %+v", entries[1])
	}
}

func TestParseCodexMCPServersInlineTables(t *testing.T) {
	entries := parseCodexMCPServers([]byte(
		`mcp_servers.hosted = { url = "https://hosted.example.com/mcp" }` + "\n"))
	if len(entries) != 1 || entries[0].URL != "https://hosted.example.com/mcp" {
		t.Errorf("inline-table server wrong: %+v", entries)
	}
}

func TestResolveMCPGeminiExtensions(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".gemini", "extensions", "sec-ext", "gemini-extension.json"),
		`{"name":"sec-ext","version":"1.0.0","mcpServers":{"scanner":{"command":"scan-mcp","args":["--fast"]}}}`)
	writeConfig(t, filepath.Join(home, ".gemini", "extensions", "home-ext", "gemini-extension.json"),
		`{"name":"home-ext","version":"1.0.0","mcpServers":{"notes":{"httpUrl":"https://notes.example.com/mcp"}}}`)
	r := mcpTestRunner(t)

	ev := mcpToolPre(ProviderGemini, cwd, "mcp_scanner_run")
	r.resolveMCP(ev)
	if ev.Tool.MCP.Command != "scan-mcp --fast" || !ev.Tool.MCP.FromConfig {
		t.Errorf("workspace extension server not resolved: %+v", ev.Tool.MCP)
	}

	ev = mcpToolPre(ProviderGemini, cwd, "mcp_notes_search")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://notes.example.com/mcp" {
		t.Errorf("home extension server not resolved: %+v", ev.Tool.MCP)
	}

	// settings.json wins a name collision with an extension server.
	writeConfig(t, filepath.Join(cwd, ".gemini", "settings.json"),
		`{"mcpServers":{"scanner":{"url":"https://settings-wins.example.com"}}}`)
	ev = mcpToolPre(ProviderGemini, cwd, "mcp_scanner_run")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://settings-wins.example.com" {
		t.Errorf("settings must win over extension on collision: %+v", ev.Tool.MCP)
	}
}

func TestParseClaudeMCPList(t *testing.T) {
	entries := parseClaudeMCPList(strings.Join([]string{
		"Checking MCP server health...",
		"",
		"github: https://api.githubcopilot.com/mcp/ (HTTP) - ✓ Connected",
		"claude.ai Linear (Acme): https://mcp.linear.app/sse (SSE) - ✓ Connected",
		"plugin:slack:slack: npx -y slack-mcp - ✗ Failed to connect",
	}, "\n"))
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %+v", entries)
	}
	if entries[0].Prefix != "github" || entries[0].URL != "https://api.githubcopilot.com/mcp/" {
		t.Errorf("local entry wrong: %+v", entries[0])
	}
	if entries[1].Prefix != "claude_ai_Linear_Acme" || entries[1].Name != "Linear (Acme)" {
		t.Errorf("claude.ai entry wrong: %+v", entries[1])
	}
	if entries[2].Prefix != "plugin_slack_slack" || entries[2].Command != "npx -y slack-mcp" {
		t.Errorf("plugin entry wrong: %+v", entries[2])
	}
}

func TestResolveMCPClaudeListFallback(t *testing.T) {
	isolateHome(t) // no config files: the fast path finds nothing
	count := fakeClaudeCLI(t, strings.Join([]string{
		"Checking MCP server health...",
		"claude.ai Linear (Acme): https://mcp.linear.app/sse (SSE) - ✓ Connected",
		"plugin:tools:tools: npx -y tools-mcp - ✓ Connected",
	}, "\n"))
	r := mcpTestRunner(t)

	ev := mcpToolPre(ProviderClaudeCode, "", "mcp__claude_ai_Linear_Acme__create_issue")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://mcp.linear.app/sse" || !ev.Tool.MCP.FromConfig {
		t.Errorf("claude.ai connector not resolved via CLI fallback: %+v", ev.Tool.MCP)
	}
	if ev.Tool.MCP.Server != "claude_ai_Linear_Acme" || ev.Tool.MCP.Tool != "create_issue" {
		t.Errorf("identity wrong: %+v", ev.Tool.MCP)
	}

	ev = mcpToolPre(ProviderClaudeCode, "", "mcp__plugin_tools_tools__do_thing")
	r.resolveMCP(ev)
	if ev.Tool.MCP.Command != "npx -y tools-mcp" {
		t.Errorf("plugin server not resolved via CLI fallback: %+v", ev.Tool.MCP)
	}

	// Same session: the second resolve must hit the cache, not the CLI.
	if got := cliRuns(t, count); got != 1 {
		t.Errorf("CLI must run once per session, ran %d times", got)
	}

	// Unknown server: cached inventory consulted, still no re-run.
	ev = mcpToolPre(ProviderClaudeCode, "", "mcp__unknown__x")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || cliRuns(t, count) != 1 {
		t.Errorf("unknown server must not re-probe: %+v (runs=%d)", ev.Tool.MCP, cliRuns(t, count))
	}
}

func TestResolveMCPClaudeListOncePerSession(t *testing.T) {
	isolateHome(t)
	count := fakeClaudeCLI(t, "Checking MCP server health...\n") // empty inventory
	stateDir := t.TempDir()
	r := quietRunner(WithDedupDir(stateDir))

	ev := mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__x")
	r.resolveMCP(ev)
	r.resolveMCP(mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__y"))
	if got := cliRuns(t, count); got != 1 {
		t.Errorf("empty inventory must be negative-cached, ran %d times", got)
	}

	// The cache is on disk: a fresh runner (separate hook process in prod)
	// sharing the state dir must not re-probe either.
	r2 := quietRunner(WithDedupDir(stateDir))
	r2.resolveMCP(mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__z"))
	if got := cliRuns(t, count); got != 1 {
		t.Errorf("cache must be shared across processes, ran %d times", got)
	}

	// A different session re-probes.
	other := mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__x")
	other.Session.ID = "sess-other"
	r.resolveMCP(other)
	if got := cliRuns(t, count); got != 2 {
		t.Errorf("new session must probe again, ran %d times", got)
	}

	// No session id: never probe (nothing to key the cache on).
	anon := mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__x")
	anon.Session.ID = ""
	r.resolveMCP(anon)
	if got := cliRuns(t, count); got != 2 {
		t.Errorf("empty session id must not probe, ran %d times", got)
	}
}

func TestResolveMCPClaudeListFallbackDisabled(t *testing.T) {
	isolateHome(t)
	count := fakeClaudeCLI(t, "github: https://api.example.com/mcp - ✓ Connected\n")
	ev := mcpToolPre(ProviderClaudeCode, "", "mcp__github__create_issue")
	mcpTestRunner(t, WithoutMCPListFallback()).resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || cliRuns(t, count) != 0 {
		t.Errorf("WithoutMCPListFallback must skip the CLI: %+v (runs=%d)", ev.Tool.MCP, cliRuns(t, count))
	}
}

func TestResolveMCPKimiNeverProbesCLI(t *testing.T) {
	isolateHome(t)
	count := fakeClaudeCLI(t, "github: https://api.example.com/mcp - ✓ Connected\n")
	ev := mcpToolPre(ProviderKimi, "", "mcp__github__create_issue")
	mcpTestRunner(t).resolveMCP(ev)
	if cliRuns(t, count) != 0 {
		t.Error("`claude mcp list` is claude-specific; Kimi must not probe it")
	}
}
