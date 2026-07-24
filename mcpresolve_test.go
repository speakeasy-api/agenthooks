package agenthooks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testHelperEnv = "AGENTHOOKS_TEST_HELPER"

func TestMain(m *testing.M) {
	if strings.EqualFold(strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0])), "claude") {
		fakeClaudeMain()
		os.Exit(0)
	}
	switch os.Getenv(testHelperEnv) {
	case "fake-claude":
		fakeClaudeMain()
		os.Exit(0)
	case "claude-launch":
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "agenthooks-main":
		if path := os.Getenv("AGENTHOOKS_TEST_MAIN_READY"); path != "" {
			_ = os.WriteFile(path, []byte("ready"), 0o600)
		}
		r := quietRunner(WithDedupDir(os.Getenv("AGENTHOOKS_TEST_STATE_DIR")))
		if path := os.Getenv("AGENTHOOKS_TEST_MCP_RESULT"); path != "" {
			r.OnToolPre(func(_ context.Context, ev *ToolPreEvent) (ToolPreDecision, error) {
				if ev.Tool.MCP != nil {
					_ = os.WriteFile(path, []byte(ev.Tool.MCP.URL), 0o600)
				}
				return NoDecision(), nil
			})
		}
		Main(r)
	}
	os.Exit(m.Run())
}

func fakeClaudeMain() {
	if path := os.Getenv("AGENTHOOKS_FAKE_CLAUDE_COUNT"); path != "" {
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if f != nil {
			_, _ = f.WriteString("x\n")
			_ = f.Close()
		}
	}
	if path := os.Getenv("AGENTHOOKS_FAKE_CLAUDE_CALLS"); path != "" {
		cwd, _ := os.Getwd()
		data, _ := json.Marshal(fakeClaudeCall{Args: os.Args[1:], Dir: cwd})
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if f != nil {
			_, _ = f.Write(append(data, '\n'))
			_ = f.Close()
		}
	}
	if gate := os.Getenv("AGENTHOOKS_FAKE_CLAUDE_GATE"); gate != "" {
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if data, err := os.ReadFile(os.Getenv("AGENTHOOKS_FAKE_CLAUDE_OUTPUT")); err == nil {
		_, _ = os.Stdout.Write(data)
	}
	if data, err := os.ReadFile(os.Getenv("AGENTHOOKS_FAKE_CLAUDE_EXIT_FILE")); err == nil {
		if code, _ := strconv.Atoi(strings.TrimSpace(string(data))); code != 0 {
			os.Exit(code)
		}
	}
}

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
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this on windows
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("KIMI_CODE_HOME", filepath.Join(home, ".kimi-code"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PATH", filepath.Join(home, "bin-empty"))
	return home
}

// mcpTestRunner isolates the on-disk state dir so launch-context mcp-list
// caches can't leak across tests or into the real temp dir.
func mcpTestRunner(t *testing.T, opts ...Option) *Runner {
	t.Helper()
	return quietRunner(append([]Option{WithDedupDir(t.TempDir())}, opts...)...)
}

type fakeClaude struct {
	countFile  string
	outputFile string
	callsFile  string
	exitFile   string
	gateFile   string
}

type fakeClaudeCall struct {
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
}

// installFakeClaude copies the current test binary onto PATH as `claude`.
// TestMain turns that copy into a cross-platform deterministic CLI harness.
func installFakeClaude(t *testing.T, output string) fakeClaude {
	t.Helper()
	binDir := t.TempDir()
	name := "claude"
	if strings.EqualFold(filepath.Ext(os.Args[0]), ".exe") {
		name += ".exe"
	}
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(binDir, name)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
	h := fakeClaude{
		countFile:  filepath.Join(binDir, "count"),
		outputFile: filepath.Join(binDir, "output"),
		callsFile:  filepath.Join(binDir, "calls"),
		exitFile:   filepath.Join(binDir, "exit"),
		gateFile:   filepath.Join(binDir, "gate"),
	}
	writeConfig(t, h.outputFile, output)
	t.Setenv("PATH", binDir)
	t.Setenv(testHelperEnv, "fake-claude")
	t.Setenv("AGENTHOOKS_FAKE_CLAUDE_COUNT", h.countFile)
	t.Setenv("AGENTHOOKS_FAKE_CLAUDE_OUTPUT", h.outputFile)
	t.Setenv("AGENTHOOKS_FAKE_CLAUDE_CALLS", h.callsFile)
	t.Setenv("AGENTHOOKS_FAKE_CLAUDE_EXIT_FILE", h.exitFile)
	return h
}

func agenthooksMainCommand(t *testing.T, stateDir, resultFile, readyFile string, payload []byte) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "agenthooks", "run", "--provider=claude-code")
	cmd.Env = append(withoutEnv(os.Environ(), testHelperEnv),
		testHelperEnv+"=agenthooks-main",
		"AGENTHOOKS_TEST_STATE_DIR="+stateDir,
		"AGENTHOOKS_TEST_MCP_RESULT="+resultFile,
		"AGENTHOOKS_TEST_MAIN_READY="+readyFile,
	)
	cmd.Stdin = strings.NewReader(string(payload))
	return cmd
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func fakeClaudeCLI(t *testing.T, output string) string {
	t.Helper()
	return installFakeClaude(t, output).countFile
}

func cliRuns(t *testing.T, countFile string) int {
	t.Helper()
	data, err := os.ReadFile(countFile)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "x")
}

func (h fakeClaude) calls(t *testing.T) []fakeClaudeCall {
	t.Helper()
	f, err := os.Open(h.callsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var calls []fakeClaudeCall
	s := bufio.NewScanner(f)
	for s.Scan() {
		var call fakeClaudeCall
		if err := json.Unmarshal(s.Bytes(), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return calls
}

func startClaudeLaunch(t *testing.T, projectDir string, args ...string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(withoutEnv(os.Environ(), testHelperEnv), testHelperEnv+"=claude-launch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	t.Setenv("CLAUDE_PID", strconv.Itoa(cmd.Process.Pid))
	t.Setenv("CLAUDE_PROJECT_DIR", projectDir)
}

func withoutEnv(env []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
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

	// Same launch context: the second resolve must hit the cache, not the CLI.
	if got := cliRuns(t, count); got != 1 {
		t.Errorf("CLI must run once per launch context, ran %d times", got)
	}

	// Unknown server: cached inventory consulted, still no re-run.
	ev = mcpToolPre(ProviderClaudeCode, "", "mcp__unknown__x")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || cliRuns(t, count) != 1 {
		t.Errorf("unknown server must not re-probe: %+v (runs=%d)", ev.Tool.MCP, cliRuns(t, count))
	}
}

func TestResolveMCPClaudeListSharedByContext(t *testing.T) {
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

	// A different session with the same launch context shares the snapshot.
	other := mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__x")
	other.Session.ID = "sess-other"
	r.resolveMCP(other)
	if got := cliRuns(t, count); got != 1 {
		t.Errorf("same launch context must share the cache, ran %d times", got)
	}

	// Cache identity is launch context rather than session id.
	anon := mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__x")
	anon.Session.ID = ""
	r.resolveMCP(anon)
	if got := cliRuns(t, count); got != 1 {
		t.Errorf("session-less events must share the context cache, ran %d times", got)
	}
}

func TestParseClaudeLaunchArgs(t *testing.T) {
	c := parseClaudeLaunchArgs([]string{
		"claude", "--model", "haiku", "prompt",
		"--settings=extra.json", "--setting-sources", "user,project",
		"--plugin-dir", "./one", "--plugin-dir=./two", "--plugin-url", "https://example.com/plugin.zip",
		"--strict-mcp-config", "--mcp-config", "one.json", `{"mcpServers":{"inline":{"url":"https://inline.example.com"}}}`,
	}, "/work")
	if !c.StrictMCP || c.Bare || c.SafeMode {
		t.Fatalf("boolean flags parsed incorrectly: %+v", c)
	}
	if len(c.MCPConfigs) != 2 || len(c.PluginDirs) != 2 {
		t.Fatalf("variadic/repeated flags parsed incorrectly: %+v", c)
	}
	wantReplay := []string{
		"--settings", "extra.json", "--setting-sources", "user,project",
		"--plugin-dir", "./one", "--plugin-dir", "./two",
	}
	if strings.Join(c.ReplayArgs, "\x00") != strings.Join(wantReplay, "\x00") {
		t.Fatalf("replay args = %#v, want %#v", c.ReplayArgs, wantReplay)
	}
}

func TestClaudeSessionStartSchedulesInventoryWarm(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	payload := []byte(fmt.Sprintf(
		`{"hook_event_name":"SessionStart","session_id":"s-warm","cwd":%q,"source":"startup"}`,
		project,
	))

	r := quietRunner()
	var warmed string
	r.mcpWarmStart = func(cwd string) { warmed = cwd }
	if _, code := runWith(t, r, claudeArgs(), payload); code != 0 || warmed != project {
		t.Fatalf("SessionStart warm = %q, exit %d", warmed, code)
	}

	disabled := quietRunner(WithoutMCPListFallback())
	disabled.mcpWarmStart = func(string) { t.Fatal("disabled fallback scheduled an inventory warm") }
	if _, code := runWith(t, disabled, claudeArgs(), payload); code != 0 {
		t.Fatalf("disabled SessionStart exit = %d", code)
	}
}

func TestClaudeSessionStartWarmsInventoryInBackground(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	stateDir := t.TempDir()
	resultFile := filepath.Join(t.TempDir(), "result")
	readyFile := filepath.Join(t.TempDir(), "ready")
	fake := installFakeClaude(t, "background: https://background.example.com/mcp - connected\n")
	t.Setenv("AGENTHOOKS_FAKE_CLAUDE_GATE", fake.gateFile)
	t.Cleanup(func() { _ = os.WriteFile(fake.gateFile, []byte("release"), 0o600) })
	startClaudeLaunch(t, project, "-p", "prompt")

	sessionPayload := []byte(fmt.Sprintf(
		`{"hook_event_name":"SessionStart","session_id":"s-background","cwd":%q,"source":"startup"}`,
		project,
	))
	session := agenthooksMainCommand(t, stateDir, resultFile, readyFile, sessionPayload)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Process.Kill() })
	sessionDone := make(chan error, 1)
	go func() { sessionDone <- session.Wait() }()
	select {
	case err := <-sessionDone:
		if err != nil {
			t.Fatalf("SessionStart process failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = session.Process.Kill()
		t.Fatal("SessionStart waited for blocked inventory discovery")
	}

	// The fake writes its count only after the detached worker owns the
	// per-context lock, then waits on the gate before returning inventory.
	waitForTestFile(t, fake.countFile)
	_ = os.Remove(readyFile)
	prePayload := []byte(fmt.Sprintf(
		`{"hook_event_name":"PreToolUse","session_id":"s-background","cwd":%q,"tool_name":"mcp__background__run","tool_input":{}}`,
		project,
	))
	pre := agenthooksMainCommand(t, stateDir, resultFile, readyFile, prePayload)
	if err := pre.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pre.Process.Kill() })
	preDone := make(chan error, 1)
	go func() { preDone <- pre.Wait() }()
	waitForTestFile(t, readyFile)
	select {
	case err := <-preDone:
		t.Fatalf("MCP hook did not wait for in-flight inventory: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	writeConfig(t, fake.gateFile, "release")
	select {
	case err := <-preDone:
		if err != nil {
			t.Fatalf("MCP hook failed after inventory arrived: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = pre.Process.Kill()
		t.Fatal("MCP hook did not consume background inventory")
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "https://background.example.com/mcp" {
		t.Fatalf("resolved transport = %q", data)
	}
	if got := cliRuns(t, fake.countFile); got != 1 {
		t.Fatalf("background warm and first MCP must share one probe, ran %d times", got)
	}
}

func TestClaudeMCPWaitsForInflightReplacementSnapshot(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	startClaudeLaunch(t, project, "-p", "prompt")
	stateDir := t.TempDir()
	r := quietRunner(WithDedupDir(stateDir))
	now := time.Now().Truncate(time.Second)
	r.now = func() time.Time { return now }
	launch := currentClaudeLaunchContext(project)
	dir := filepath.Join(stateDir, "agenthooks-mcplist")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, launch.cacheKey()+".json")
	writeMCPListCache(path, mcpListCache{
		CheckedAt: now.Add(-mcpListRefreshInterval - time.Second).Unix(),
		Entries:   []mcpConfigEntry{{Name: "shared", URL: "https://stale.example.com/mcp"}},
	})
	release, locked, err := tryMCPListLock(path + ".lock")
	if err != nil || !locked {
		t.Fatalf("holding refresh lock: locked=%v err=%v", locked, err)
	}

	result := make(chan []mcpConfigEntry, 1)
	go func() { result <- r.claudeMCPListEntries(launch) }()
	select {
	case entries := <-result:
		release()
		t.Fatalf("returned stale snapshot while refresh was in flight: %+v", entries)
	case <-time.After(100 * time.Millisecond):
	}
	writeMCPListCache(path, mcpListCache{
		CheckedAt: now.Unix(),
		Entries:   []mcpConfigEntry{{Name: "shared", URL: "https://fresh.example.com/mcp"}},
	})
	release()
	select {
	case entries := <-result:
		if len(entries) != 1 || entries[0].URL != "https://fresh.example.com/mcp" {
			t.Fatalf("replacement snapshot = %+v", entries)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not consume replacement snapshot")
	}
}

func TestClaudeMCPLaunchHarnessDefaultContext(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	fake := installFakeClaude(t, "fallback: https://fallback.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "--model", "haiku", "-p", "prompt")

	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__fallback__run")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://fallback.example.com/mcp" {
		t.Fatalf("default inventory not resolved: %+v", ev.Tool.MCP)
	}
	calls := fake.calls(t)
	if len(calls) != 1 || strings.Join(calls[0].Args, " ") != "mcp list" || calls[0].Dir != project {
		t.Fatalf("contextual CLI call = %+v", calls)
	}
}

func TestClaudeMCPLaunchHarnessStrictConfig(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(project, ".mcp.json"),
		`{"mcpServers":{"shared":{"url":"https://disk.example.com/mcp"}}}`)
	writeConfig(t, filepath.Join(project, "launch.json"),
		`{"mcpServers":{"shared":{"type":"http","url":"https://explicit.example.com/mcp"}}}`)
	fake := installFakeClaude(t, "shared: https://wrong.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt", "--strict-mcp-config", "--mcp-config", "launch.json")

	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__shared__run")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://explicit.example.com/mcp" {
		t.Fatalf("strict config did not override disk config: %+v", ev.Tool.MCP)
	}
	if got := cliRuns(t, fake.countFile); got != 0 {
		t.Fatalf("strict config must not invoke mcp list, ran %d times", got)
	}
}

func TestClaudeMCPLaunchHarnessInlineConfigsUseLastDefinition(t *testing.T) {
	isolateHome(t)
	t.Setenv("MCP_ORIGIN", "https://inline.example.com")
	project := t.TempDir()
	writeConfig(t, filepath.Join(project, "first.json"),
		`{"mcpServers":{"shared":{"type":"http","url":"https://first.example.com/mcp"}}}`)
	inline := `{"mcpServers":{
		"shared":{"type":"http","url":"${MCP_ORIGIN}/mcp"},
		"invalid":{"url":"https://invalid.example.com/mcp"}
	}}`
	fake := installFakeClaude(t, "shared: https://wrong.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt", "--strict-mcp-config", "--mcp-config", "first.json", inline)

	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__shared__run")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://inline.example.com/mcp" {
		t.Fatalf("last inline config did not win: %+v", ev.Tool.MCP)
	}
	invalid := mcpToolPre(ProviderClaudeCode, project, "mcp__invalid__run")
	mcpTestRunner(t).resolveMCP(invalid)
	if invalid.Tool.MCP.URL != "" {
		t.Fatalf("Claude-invalid URL config must stay unresolved: %+v", invalid.Tool.MCP)
	}
	if got := cliRuns(t, fake.countFile); got != 0 {
		t.Fatalf("strict inline config must not invoke mcp list, ran %d times", got)
	}
}

func TestClaudeMCPLaunchHarnessConfigMergedWithInventory(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(project, "launch.json"),
		`{"mcpServers":{"explicit":{"type":"http","url":"https://explicit.example.com/mcp"}}}`)
	fake := installFakeClaude(t, "configured: https://configured.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt", "--mcp-config", "launch.json")
	r := mcpTestRunner(t)

	explicit := mcpToolPre(ProviderClaudeCode, project, "mcp__explicit__run")
	r.resolveMCP(explicit)
	configured := mcpToolPre(ProviderClaudeCode, project, "mcp__configured__run")
	r.resolveMCP(configured)
	if explicit.Tool.MCP.URL != "https://explicit.example.com/mcp" || configured.Tool.MCP.URL != "https://configured.example.com/mcp" {
		t.Fatalf("merged launch inventory wrong: explicit=%+v configured=%+v", explicit.Tool.MCP, configured.Tool.MCP)
	}
	if got := cliRuns(t, fake.countFile); got != 1 {
		t.Fatalf("normal inventory should be probed once, ran %d times", got)
	}
}

func TestClaudeMCPLaunchHarnessReplaysContextFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "settings", args: []string{"--settings", "extra.json"}, want: []string{"--settings", "extra.json", "mcp", "list"}},
		{name: "setting sources", args: []string{"--setting-sources", "user"}, want: []string{"--setting-sources", "user", "mcp", "list"}},
		{name: "plugin directory", args: []string{"--plugin-dir", "./plugin"}, want: []string{"--plugin-dir", "./plugin", "mcp", "list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			project := t.TempDir()
			fake := installFakeClaude(t, "launch-only: https://launch.example.com/mcp - connected\n")
			startClaudeLaunch(t, project, append([]string{"-p", "prompt"}, tc.args...)...)
			ev := mcpToolPre(ProviderClaudeCode, project, "mcp__launch-only__run")
			mcpTestRunner(t).resolveMCP(ev)
			if ev.Tool.MCP.URL != "https://launch.example.com/mcp" {
				t.Fatalf("replayed inventory not resolved: %+v", ev.Tool.MCP)
			}
			calls := fake.calls(t)
			if len(calls) != 1 || strings.Join(calls[0].Args, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("replayed args = %+v, want %#v", calls, tc.want)
			}
		})
	}
}

func TestClaudeMCPLaunchHarnessBareMode(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	pluginDir := filepath.Join(project, "launch-plugin")
	if err := os.Mkdir(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, filepath.Join(project, "launch.json"),
		`{"mcpServers":{"explicit":{"type":"http","url":"https://explicit.example.com/mcp"}}}`)
	writeConfig(t, filepath.Join(project, ".mcp.json"),
		`{"mcpServers":{"excluded":{"url":"https://disk.example.com/mcp"}}}`)
	fake := installFakeClaude(t, strings.Join([]string{
		"excluded: https://disk.example.com/mcp - connected",
		"plugin:launch-plugin:tools: https://plugin.example.com/mcp - connected",
	}, "\n"))
	startClaudeLaunch(t, project, "-p", "prompt", "--bare", "--plugin-dir", pluginDir, "--mcp-config", "launch.json")
	r := mcpTestRunner(t)

	explicit := mcpToolPre(ProviderClaudeCode, project, "mcp__explicit__run")
	r.resolveMCP(explicit)
	plugin := mcpToolPre(ProviderClaudeCode, project, "mcp__plugin_launch-plugin_tools__run")
	r.resolveMCP(plugin)
	excluded := mcpToolPre(ProviderClaudeCode, project, "mcp__excluded__run")
	r.resolveMCP(excluded)
	if explicit.Tool.MCP.URL != "https://explicit.example.com/mcp" || plugin.Tool.MCP.URL != "https://plugin.example.com/mcp" {
		t.Fatalf("bare explicit/plugin inventory wrong: explicit=%+v plugin=%+v", explicit.Tool.MCP, plugin.Tool.MCP)
	}
	if excluded.Tool.MCP.URL != "" {
		t.Fatalf("bare mode must exclude ordinary config: %+v", excluded.Tool.MCP)
	}
	calls := fake.calls(t)
	if len(calls) != 1 || len(calls[0].Args) == 0 || calls[0].Args[0] != "--bare" {
		t.Fatalf("bare plugin discovery call wrong: %+v", calls)
	}
}

func TestClaudeMCPLaunchHarnessBarePluginURLFailsUnknown(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	fake := installFakeClaude(t, "plugin:remote:tools: https://remote.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt", "--bare", "--plugin-url", "https://example.com/plugin.zip")
	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__plugin_remote_tools__run")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || cliRuns(t, fake.countFile) != 0 {
		t.Fatalf("bare remote plugin without a stable local manifest must stay unknown: %+v", ev.Tool.MCP)
	}
}

func TestClaudeMCPLaunchHarnessPluginURLIsNotRefetched(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	fake := installFakeClaude(t, "ordinary: https://ordinary.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt", "--plugin-url", "https://example.com/plugin.zip")
	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__plugin_remote_tools__run")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "" {
		t.Fatalf("remote launch plugin must stay unknown rather than be refetched: %+v", ev.Tool.MCP)
	}
	calls := fake.calls(t)
	if len(calls) != 1 || strings.Join(calls[0].Args, " ") != "mcp list" {
		t.Fatalf("plugin URL leaked into reconstructed command: %+v", calls)
	}
}

func TestClaudeMCPLaunchHarnessSafeMode(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(project, ".mcp.json"),
		`{"mcpServers":{"excluded":{"url":"https://disk.example.com/mcp"}}}`)
	writeConfig(t, filepath.Join(project, "launch.json"),
		`{"mcpServers":{"explicit":{"type":"http","url":"https://explicit.example.com/mcp"}}}`)
	fake := installFakeClaude(t, "excluded: https://wrong.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt", "--safe-mode", "--mcp-config", "launch.json")
	r := mcpTestRunner(t)
	for _, name := range []string{"mcp__excluded__run", "mcp__explicit__run"} {
		ev := mcpToolPre(ProviderClaudeCode, project, name)
		r.resolveMCP(ev)
		if ev.Tool.MCP.URL != "" {
			t.Fatalf("safe mode must expose no MCP inventory: %+v", ev.Tool.MCP)
		}
	}
	if got := cliRuns(t, fake.countFile); got != 0 {
		t.Fatalf("safe mode must not invoke mcp list, ran %d times", got)
	}
}

func TestClaudeMCPLaunchHarnessContextsStayIsolated(t *testing.T) {
	isolateHome(t)
	fake := installFakeClaude(t, "shared: https://one.example.com/mcp - connected\n")
	stateDir := t.TempDir()
	r := quietRunner(WithDedupDir(stateDir))

	projectOne := t.TempDir()
	startClaudeLaunch(t, projectOne, "-p", "prompt")
	one := mcpToolPre(ProviderClaudeCode, projectOne, "mcp__shared__run")
	r.resolveMCP(one)

	writeConfig(t, fake.outputFile, "shared: https://two.example.com/mcp - connected\n")
	projectTwo := t.TempDir()
	startClaudeLaunch(t, projectTwo, "-p", "prompt")
	two := mcpToolPre(ProviderClaudeCode, projectTwo, "mcp__shared__run")
	r.resolveMCP(two)

	if one.Tool.MCP.URL != "https://one.example.com/mcp" || two.Tool.MCP.URL != "https://two.example.com/mcp" {
		t.Fatalf("launch contexts contaminated each other: one=%+v two=%+v", one.Tool.MCP, two.Tool.MCP)
	}
	if got := cliRuns(t, fake.countFile); got != 2 {
		t.Fatalf("different contexts must probe independently, ran %d times", got)
	}
}

func TestClaudeMCPLaunchHarnessRefreshReplacesRemovedServers(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	fake := installFakeClaude(t, strings.Join([]string{
		"removed: https://removed.example.com/mcp - connected",
		"kept: https://kept.example.com/mcp - connected",
	}, "\n"))
	startClaudeLaunch(t, project, "-p", "prompt")
	now := time.Now().Truncate(time.Second)
	r := mcpTestRunner(t)
	r.now = func() time.Time { return now }

	first := mcpToolPre(ProviderClaudeCode, project, "mcp__removed__run")
	r.resolveMCP(first)
	if first.Tool.MCP.URL == "" {
		t.Fatalf("precondition: removed server did not resolve: %+v", first.Tool.MCP)
	}
	writeConfig(t, fake.outputFile, "kept: https://kept.example.com/mcp - connected\n")
	now = now.Add(mcpListRefreshInterval + time.Second)
	after := mcpToolPre(ProviderClaudeCode, project, "mcp__removed__run")
	r.resolveMCP(after)
	if after.Tool.MCP.URL != "" {
		t.Fatalf("successful replacement refresh retained removed server: %+v", after.Tool.MCP)
	}
	if got := cliRuns(t, fake.countFile); got != 2 {
		t.Fatalf("stale hit must refresh once, ran %d times", got)
	}
}

func TestClaudeMCPLaunchHarnessFailedRefreshKeepsSnapshot(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	fake := installFakeClaude(t, "retained: https://retained.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt")
	now := time.Now().Truncate(time.Second)
	r := mcpTestRunner(t)
	r.now = func() time.Time { return now }

	first := mcpToolPre(ProviderClaudeCode, project, "mcp__retained__run")
	r.resolveMCP(first)
	writeConfig(t, fake.outputFile, "")
	writeConfig(t, fake.exitFile, "1")
	now = now.Add(mcpListRefreshInterval + time.Second)
	after := mcpToolPre(ProviderClaudeCode, project, "mcp__retained__run")
	r.resolveMCP(after)
	if after.Tool.MCP.URL != "https://retained.example.com/mcp" {
		t.Fatalf("failed refresh discarded last successful snapshot: %+v", after.Tool.MCP)
	}
	if got := cliRuns(t, fake.countFile); got != 2 {
		t.Fatalf("stale snapshot should attempt one refresh, ran %d times", got)
	}
}

func TestClaudeMCPLaunchHarnessPreservesQualifiedPluginIdentity(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	installFakeClaude(t, strings.Join([]string{
		"plugin:alpha:tools: https://alpha.example.com/mcp - connected",
		"plugin:beta:tools: https://beta.example.com/mcp - connected",
	}, "\n"))
	startClaudeLaunch(t, project, "-p", "prompt")
	r := mcpTestRunner(t)
	alpha := mcpToolPre(ProviderClaudeCode, project, "mcp__plugin_alpha_tools__run")
	r.resolveMCP(alpha)
	beta := mcpToolPre(ProviderClaudeCode, project, "mcp__plugin_beta_tools__run")
	r.resolveMCP(beta)
	if alpha.Tool.MCP.URL != "https://alpha.example.com/mcp" || beta.Tool.MCP.URL != "https://beta.example.com/mcp" {
		t.Fatalf("source-qualified plugin identities collapsed: alpha=%+v beta=%+v", alpha.Tool.MCP, beta.Tool.MCP)
	}
}

func TestClaudeMCPLaunchHarnessConcurrentContextsSingleflight(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	fake := installFakeClaude(t, "shared: https://shared.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt")
	stateDir := t.TempDir()

	const runners = 8
	var wg sync.WaitGroup
	errs := make(chan string, runners)
	for range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := mcpToolPre(ProviderClaudeCode, project, "mcp__shared__run")
			quietRunner(WithDedupDir(stateDir)).resolveMCP(ev)
			if ev.Tool.MCP.URL != "https://shared.example.com/mcp" {
				errs <- fmt.Sprintf("%+v", ev.Tool.MCP)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent resolution failed: %s", err)
	}
	if got := cliRuns(t, fake.countFile); got != 1 {
		t.Fatalf("concurrent contexts must share one probe, ran %d times", got)
	}
}

func TestMCPListCacheCleanup(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.json")
	newPath := filepath.Join(dir, "new.json")
	writeConfig(t, oldPath, `{}`)
	writeConfig(t, newPath, `{}`)
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-mcpListCacheRetention - time.Second)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, now, now); err != nil {
		t.Fatal(err)
	}
	cleanupMCPListCache(dir, now)
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired cache was not removed: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("fresh cache was removed: %v", err)
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

func TestClaudeMCPLaunchHarnessFallbackDisabledKeepsDirectConfig(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(project, ".mcp.json"),
		`{"mcpServers":{"github":{"url":"https://direct.example.com/mcp"}}}`)
	fake := installFakeClaude(t, "github: https://wrong.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt", "--plugin-dir", "./plugin")
	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__github__run")
	mcpTestRunner(t, WithoutMCPListFallback()).resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://direct.example.com/mcp" || cliRuns(t, fake.countFile) != 0 {
		t.Fatalf("disabled fallback must retain direct config resolution: %+v", ev.Tool.MCP)
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
