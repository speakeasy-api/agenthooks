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
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	// An explicit helper role wins over the binary's name: the stand-in for a
	// running Claude session is the same on-PATH copy the CLI harness uses, so
	// that CLAUDE_PID resolves to a claude executable the way a real session's
	// does. Dispatching on the name alone would turn that launcher into a
	// one-shot CLI run.
	if os.Getenv(testHelperEnv) == "claude-launch" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if strings.EqualFold(name, "claude") {
		fakeClaudeMain()
		os.Exit(0)
	}
	if strings.EqualFold(name, "codex") {
		fakeCodexMain()
		os.Exit(0)
	}
	switch os.Getenv(testHelperEnv) {
	case "fake-claude":
		fakeClaudeMain()
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

func fakeCodexMain() {
	if path := os.Getenv("AGENTHOOKS_FAKE_CODEX_COUNT"); path != "" {
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if f != nil {
			_, _ = f.WriteString("x\n")
			_ = f.Close()
		}
	}
	if path := os.Getenv("AGENTHOOKS_FAKE_CODEX_CALLS"); path != "" {
		cwd, _ := os.Getwd()
		data, _ := json.Marshal(fakeClaudeCall{Args: os.Args[1:], Dir: cwd})
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if f != nil {
			_, _ = f.Write(append(data, '\n'))
			_ = f.Close()
		}
	}
	if gate := os.Getenv("AGENTHOOKS_FAKE_CODEX_GATE"); gate != "" {
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if data, err := os.ReadFile(os.Getenv("AGENTHOOKS_FAKE_CODEX_OUTPUT")); err == nil {
		_, _ = os.Stdout.Write(data)
	}
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
	t.Setenv("COPILOT_HOME", filepath.Join(home, ".copilot"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PATH", filepath.Join(home, "bin-empty"))
	// Claude launch context is recovered from CLAUDE_PID, so a suite run from
	// inside a live Claude Code session would otherwise inherit that session's
	// pid and probe the developer's real CLI instead of the fake. Tests that
	// want a launch context call startClaudeLaunch, which sets its own.
	t.Setenv("CLAUDE_PID", "")
	return home
}

// mcpTestRunner isolates the on-disk state dir so launch-context mcp-list
// caches can't leak across tests or into the real temp dir.
func mcpTestRunner(t *testing.T, opts ...Option) *Runner {
	t.Helper()
	return syncMCPRefresh(quietRunner(append([]Option{WithDedupDir(t.TempDir())}, opts...)...))
}

// syncMCPRefresh runs provider CLI refreshes in-process, so the hook that
// starts one also sees its snapshot. Main runs them in a detached worker,
// which the end-to-end tests below exercise.
func syncMCPRefresh(r *Runner) *Runner {
	r.mcpWarmStart = r.warmClaudeMCP
	r.codexMCPWarmStart = r.warmCodexMCP
	return r
}

// mcpListCachePath is where a runner rooted at stateDir keeps key's snapshot.
func mcpListCachePath(t *testing.T, stateDir, key string) string {
	t.Helper()
	dir := filepath.Join(stateDir, "agenthooks-mcplist")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, key+".json")
}

// waitForMCPListSnapshot waits for a background refresh to land a snapshot
// under stateDir. Hook processes hash their own environment into the cache
// key, so the test finds the snapshot by content rather than by key.
func waitForMCPListSnapshot(t *testing.T, stateDir string) {
	t.Helper()
	dir := filepath.Join(stateDir, "agenthooks-mcplist")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, path := range paths {
			if readMCPListCache(path).HasSnapshot {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no MCP list snapshot under %s", dir)
}

type fakeClaude struct {
	countFile  string
	outputFile string
	callsFile  string
	exitFile   string
	gateFile   string
}

type fakeCodex struct {
	countFile  string
	outputFile string
	callsFile  string
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

func installFakeCodex(t *testing.T, output string) fakeCodex {
	t.Helper()
	binDir := t.TempDir()
	name := "codex"
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
	h := fakeCodex{
		countFile:  filepath.Join(binDir, "count"),
		outputFile: filepath.Join(binDir, "output"),
		callsFile:  filepath.Join(binDir, "calls"),
		gateFile:   filepath.Join(binDir, "gate"),
	}
	writeConfig(t, h.outputFile, output)
	t.Setenv("PATH", binDir)
	t.Setenv("AGENTHOOKS_FAKE_CODEX_COUNT", h.countFile)
	t.Setenv("AGENTHOOKS_FAKE_CODEX_OUTPUT", h.outputFile)
	t.Setenv("AGENTHOOKS_FAKE_CODEX_CALLS", h.callsFile)
	return h
}

func (h fakeCodex) calls(t *testing.T) []fakeClaudeCall {
	t.Helper()
	return readFakeCalls(t, h.callsFile)
}

func agenthooksMainCommand(t *testing.T, stateDir, resultFile, readyFile string, payload []byte) *exec.Cmd {
	t.Helper()
	return agenthooksMainCommandForProvider(t, ProviderClaudeCode, nil, stateDir, resultFile, readyFile, payload)
}

func agenthooksMainCommandForProvider(t *testing.T, provider Provider, extraArgs []string, stateDir, resultFile, readyFile string, payload []byte) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"agenthooks", "run", "--provider=" + string(provider)}
	args = append(args, extraArgs...)
	cmd := exec.Command(exe, args...)
	cmd.Env = append(withoutEnv(os.Environ(), testHelperEnv),
		testHelperEnv+"=agenthooks-main",
		"AGENTHOOKS_TEST_STATE_DIR="+stateDir,
		"AGENTHOOKS_TEST_MCP_RESULT="+resultFile,
		"AGENTHOOKS_TEST_MAIN_READY="+readyFile,
	)
	cmd.Stdin = strings.NewReader(string(payload))
	return cmd
}

func codexHookPayload(t *testing.T, launch codexLaunchContext, payload []byte) []byte {
	t.Helper()
	data, err := json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	return append(append(data, '\n'), payload...)
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
	return readFakeCalls(t, h.callsFile)
}

func readFakeCalls(t *testing.T, path string) []fakeClaudeCall {
	t.Helper()
	f, err := os.Open(path)
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
	// Launch the copy installed on PATH as `claude`, not the raw test binary:
	// resolution recovers the CLI from CLAUDE_PID's executable, and a real
	// session's CLAUDE_PID is always the claude binary itself. Both are copies
	// of this test binary, so the helper behaves identically either way.
	exe, err := exec.LookPath("claude")
	if err != nil {
		exe, err = os.Executable()
	}
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

func TestParseCodexLaunchArgs(t *testing.T) {
	ctx := parseCodexLaunchArgs([]string{
		"codex", "-c", "one=1", "-ctwo=2", "--config=three=3", "-p", "old",
		"exec", "--profile=new", "--enable", "plugin_mcp", "--disable=legacy", "--ignore-user-config", "--", "--config=ignored",
	}, "/work")
	want := []string{"--config", "one=1", "--config", "two=2", "--config", "three=3", "--enable", "plugin_mcp", "--disable", "legacy"}
	if ctx.CWD != "/work" || ctx.Executable != "codex" || ctx.Profile != "new" || !ctx.Unreplayable || strings.Join(ctx.Overrides, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Codex launch context = %+v", ctx)
	}
	wantReplay := append([]string{"--profile", "new"}, want...)
	if strings.Join(ctx.replayArgs(), "\x00") != strings.Join(wantReplay, "\x00") {
		t.Fatalf("Codex replay args = %#v", ctx.replayArgs())
	}
}

func TestParseCodexMCPList(t *testing.T) {
	entries := parseCodexMCPList([]byte(`[
		{"name":"local","enabled":true,"transport":{"type":"stdio","command":"npx","args":["-y","srv"],"env":{"TOKEN":"secret"}}},
		{"name":"remote","enabled":true,"transport":{"type":"streamable_http","url":"https://example.com/mcp","http_headers":{"Authorization":"secret"}}},
		{"name":"off","enabled":false,"transport":{"type":"streamable_http","url":"https://off.example.com"}},
		{"name":"unknown","enabled":true,"transport":{"type":"tcp","url":"tcp://localhost"}}
	]`))
	if len(entries) != 2 || entries[0].Name != "local" || entries[0].Command != "npx -y srv" ||
		entries[1].Name != "remote" || entries[1].URL != "https://example.com/mcp" {
		t.Fatalf("Codex MCP list = %+v", entries)
	}
	if _, ok := decodeCodexMCPList([]byte(`not-json`)); ok {
		t.Fatal("malformed Codex inventory reported success")
	}
}

func TestResolveMCPCodexLaunchInventoryWins(t *testing.T) {
	home := isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.shared]
url = "https://disk.example.com/mcp"
`)
	fake := installFakeCodex(t, `[{"name":"shared","enabled":true,"transport":{"type":"streamable_http","url":"https://launch.example.com/mcp"}}]`)
	launch := parseCodexLaunchArgs([]string{"codex", "-c", `mcp_servers.shared.url="https://launch.example.com/mcp"`}, project)
	launch.Executable = "" // Missing argv recovery falls back to PATH's codex.
	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch
	ev := mcpToolPre(ProviderCodex, project, "mcp__shared__run")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://launch.example.com/mcp" || !ev.Tool.MCP.FromConfig {
		t.Fatalf("Codex launch inventory = %+v", ev.Tool.MCP)
	}
	calls := fake.calls(t)
	want := append(launch.replayArgs(), "mcp", "list", "--json")
	if len(calls) != 1 || strings.Join(calls[0].Args, "\x00") != strings.Join(want, "\x00") || calls[0].Dir != project {
		t.Fatalf("Codex inventory call = %+v, want args %#v dir %q", calls, want, project)
	}
}

func TestResolveMCPCodexFailedProbeFallsBackToDirectConfig(t *testing.T) {
	home := isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.direct]
url = "https://direct.example.com/mcp"
`)
	launch := parseCodexLaunchArgs([]string{"codex"}, project)
	launch.Executable = filepath.Join(project, "missing-codex")
	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch
	ev := mcpToolPre(ProviderCodex, project, "mcp__direct__run")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://direct.example.com/mcp" || !ev.Tool.MCP.FromConfig {
		t.Fatalf("Codex direct fallback = %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPCodexSuccessfulEmptyInventoryStaysUnknown(t *testing.T) {
	home := isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.direct]
url = "https://direct.example.com/mcp"
`)
	installFakeCodex(t, `[]`)
	launch := parseCodexLaunchArgs([]string{"codex"}, project)
	launch.Executable = ""
	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch
	ev := mcpToolPre(ProviderCodex, project, "mcp__direct__run")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || ev.Tool.MCP.Command != "" || ev.Tool.MCP.FromConfig {
		t.Fatalf("successful empty Codex inventory fell back to disk: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPCodexUnreplayableLaunchStaysUnknown(t *testing.T) {
	home := isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.shared]
url = "https://disk.example.com/mcp"
`)
	launch := parseCodexLaunchArgs([]string{"codex", "--ignore-user-config"}, project)
	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch
	ev := mcpToolPre(ProviderCodex, project, "mcp__shared__run")
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "" || ev.Tool.MCP.Command != "" || ev.Tool.MCP.FromConfig {
		t.Fatalf("unreplayable Codex launch resolved from disk: %+v", ev.Tool.MCP)
	}
}

func TestCodexSessionStartWarmsInventoryInBackground(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	stateDir := t.TempDir()
	resultFile := filepath.Join(t.TempDir(), "result")
	readyFile := filepath.Join(t.TempDir(), "ready")
	fake := installFakeCodex(t, `[{"name":"background","enabled":true,"transport":{"type":"streamable_http","url":"https://background.example.com/mcp"}}]`)
	t.Setenv("AGENTHOOKS_FAKE_CODEX_GATE", fake.gateFile)
	t.Cleanup(func() { _ = os.WriteFile(fake.gateFile, []byte("release"), 0o600) })
	launch := parseCodexLaunchArgs([]string{"codex", "-c", `mcp_servers.background.url="https://background.example.com/mcp"`}, project)
	extra := []string{codexLaunchContextFlag}

	sessionPayload := []byte(fmt.Sprintf(
		`{"hook_event_name":"SessionStart","session_id":"s-background","cwd":%q,"source":"startup"}`,
		project,
	))
	runAgenthooksMain(t, agenthooksMainCommandForProvider(t, ProviderCodex, extra, stateDir, resultFile, readyFile,
		codexHookPayload(t, launch, sessionPayload)), "Codex SessionStart")

	waitForTestFile(t, fake.countFile)
	prePayload := []byte(fmt.Sprintf(
		`{"hook_event_name":"PreToolUse","session_id":"s-background","cwd":%q,"tool_name":"mcp__background__run","tool_input":{}}`,
		project,
	))
	runAgenthooksMain(t, agenthooksMainCommandForProvider(t, ProviderCodex, extra, stateDir, resultFile, readyFile,
		codexHookPayload(t, launch, prePayload)), "Codex MCP hook during refresh")
	if data, err := os.ReadFile(resultFile); err != nil || len(data) != 0 {
		t.Fatalf("Codex MCP hook during refresh resolved %q (err %v), want no transport yet", data, err)
	}

	writeConfig(t, fake.gateFile, "release")
	waitForMCPListSnapshot(t, stateDir)
	runAgenthooksMain(t, agenthooksMainCommandForProvider(t, ProviderCodex, extra, stateDir, resultFile, readyFile,
		codexHookPayload(t, launch, prePayload)), "Codex MCP hook after refresh")
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "https://background.example.com/mcp" || cliRuns(t, fake.countFile) != 1 {
		t.Fatalf("Codex background inventory result=%q probes=%d", data, cliRuns(t, fake.countFile))
	}
}

func TestCodexAsyncHookPreservesLaunchContext(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	stateDir := t.TempDir()
	resultFile := filepath.Join(t.TempDir(), "result")
	readyFile := filepath.Join(t.TempDir(), "ready")
	fake := installFakeCodex(t, `[{"name":"async","enabled":true,"transport":{"type":"streamable_http","url":"https://async.example.com/mcp"}}]`)
	launch := parseCodexLaunchArgs([]string{"codex", "-c", `mcp_servers.async.url="https://async.example.com/mcp"`}, project)
	payload := []byte(fmt.Sprintf(
		`{"hook_event_name":"PreToolUse","session_id":"s-async","cwd":%q,"tool_name":"mcp__async__run","tool_input":{}}`,
		project,
	))
	runAsync := func() string {
		t.Helper()
		_ = os.Remove(resultFile)
		cmd := agenthooksMainCommandForProvider(t, ProviderCodex,
			[]string{codexLaunchContextFlag, "--async"}, stateDir, resultFile, readyFile,
			codexHookPayload(t, launch, payload))
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		waitForTestFile(t, resultFile)
		data, err := os.ReadFile(resultFile)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	// The first hook finds no snapshot and starts a refresh, which must
	// replay the launch overrides the detached hook received.
	_ = runAsync()
	waitForMCPListSnapshot(t, stateDir)
	calls := fake.calls(t)
	want := append(launch.replayArgs(), "mcp", "list", "--json")
	if len(calls) != 1 || strings.Join(calls[0].Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("async Codex refresh call = %+v, want args %#v", calls, want)
	}
	if got := runAsync(); got != "https://async.example.com/mcp" {
		t.Fatalf("async Codex transport=%q", got)
	}
}

func TestCodexInvalidLaunchContextPreservesHookPayload(t *testing.T) {
	home := isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.direct]
url = "https://direct.example.com/mcp"
`)
	resultFile := filepath.Join(t.TempDir(), "result")
	payload := []byte(fmt.Sprintf(
		`{"hook_event_name":"PreToolUse","session_id":"s-invalid","cwd":%q,"tool_name":"mcp__direct__run","tool_input":{}}`,
		project,
	))
	input := append([]byte("not-json\n"), payload...)
	cmd := agenthooksMainCommandForProvider(t, ProviderCodex,
		[]string{codexLaunchContextFlag}, t.TempDir(), resultFile, "", input)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil || string(data) != "https://direct.example.com/mcp" {
		t.Fatalf("transport after invalid context=%q err=%v", data, err)
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
	ev.Tool.MCP.Server = "shortcut"
	r.resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://mcp.shortcut.com/sse" || ev.Tool.MCP.Server != "shortcut" || !ev.Tool.MCP.FromConfig {
		t.Errorf("payload server identity was not matched: %+v", ev.Tool.MCP)
	}

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

func TestResolveMCPGeminiPayloadContextWins(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".gemini", "settings.json"),
		`{"mcpServers":{"my_srv":{"httpUrl":"https://wrong.example.com/mcp"}}}`)
	raw := []byte(fmt.Sprintf(`{
		"session_id":"s","cwd":%q,"hook_event_name":"BeforeTool","tool_name":"mcp_my_srv_do","tool_input":{},
		"mcp_context":{"server_name":"my_srv","tool_name":"do","tcp":"localhost:9000"}
	}`, cwd))
	typed, err := decodeGemini(VariantUnknown, DetectionConfig, time.Now(), raw)
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.Server != "my_srv" || ev.Tool.MCP.Tool != "do" ||
		ev.Tool.MCP.URL != "" || ev.Tool.MCP.Command != "" || ev.Tool.MCP.FromConfig {
		t.Fatalf("payload MCP context was overwritten by config: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPGeminiNullContextFallsBackToConfig(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".gemini", "settings.json"),
		`{"mcpServers":{"my_srv":{"httpUrl":"https://g.example.com/mcp"}}}`)
	raw := []byte(fmt.Sprintf(`{
		"session_id":"s","cwd":%q,"hook_event_name":"BeforeTool","tool_name":"mcp_my_srv_do","tool_input":{},
		"mcp_context":null
	}`, cwd))
	typed, err := decodeGemini(VariantUnknown, DetectionConfig, time.Now(), raw)
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://g.example.com/mcp" || !ev.Tool.MCP.FromConfig {
		t.Fatalf("null Gemini MCP context did not fall back to config: %+v", ev.Tool.MCP)
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

func TestResolveMCPOpenCodeCodeMode(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, "opencode.json"), `{
		"mcp": {"weather": {"type": "local", "command": ["node", "weather.mjs"]}}
	}`)
	r := mcpTestRunner(t)

	outer := mcpToolPre(ProviderOpenCode, cwd, "execute")
	r.resolveMCP(outer)
	if outer.Tool.MCP != nil || outer.Tool.Canonical != ToolOther {
		t.Errorf("code-mode execute must stay a native other tool: %+v", outer.Tool)
	}
	nested := mcpToolPre(ProviderOpenCode, cwd, "weather_get_forecast")
	r.resolveMCP(nested)
	if nested.Tool.MCP == nil || nested.Tool.MCP.Server != "weather" || nested.Tool.MCP.Tool != "get_forecast" {
		t.Errorf("nested code-mode MCP call not resolved: %+v", nested.Tool.MCP)
	}
}

func TestResolveMCPOpenCodeV2ServersShape(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, "opencode.json"), `{
		"mcp": {
			"servers": {
				"weather": {"type": "local", "command": ["node", "weather.mjs"]},
				"tracker": {"type": "remote", "url": "https://tracker.example.com/mcp"},
				"off":     {"type": "remote", "url": "https://off.example.com", "disabled": true}
			}
		}
	}`)
	r := mcpTestRunner(t)

	ev := mcpToolPre(ProviderOpenCode, cwd, "weather_get_forecast")
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.Server != "weather" || ev.Tool.MCP.Command != "node weather.mjs" {
		t.Errorf("opencode 2 local server not resolved: %+v", ev.Tool.MCP)
	}
	ev = mcpToolPre(ProviderOpenCode, cwd, "tracker_list_issues")
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://tracker.example.com/mcp" {
		t.Errorf("opencode 2 remote server not resolved: %+v", ev.Tool.MCP)
	}
	ev = mcpToolPre(ProviderOpenCode, cwd, "off_anything")
	r.resolveMCP(ev)
	if ev.Tool.MCP != nil {
		t.Errorf("disabled opencode 2 server must not detect: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPOpenCodeV1ServerNamedServers(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, "opencode.json"), `{
		"mcp": {
			"servers": {"type": "remote", "url": "https://servers.example.com/mcp"},
			"other":   {"type": "remote", "url": "https://other.example.com/mcp"}
		}
	}`)
	r := mcpTestRunner(t)

	for tool, url := range map[string]string{
		"servers_list": "https://servers.example.com/mcp",
		"other_list":   "https://other.example.com/mcp",
	} {
		ev := mcpToolPre(ProviderOpenCode, cwd, tool)
		r.resolveMCP(ev)
		if ev.Tool.MCP == nil || ev.Tool.MCP.URL != url {
			t.Errorf("%s: v1 server named servers mis-read: %+v", tool, ev.Tool.MCP)
		}
	}
}

func TestResolveMCPCopilotDetection(t *testing.T) {
	home := isolateHome(t)
	// Copilot names MCP tools <server>-<tool> verbatim with no reserved
	// prefix (GitHub Copilot CLI 1.0.80), so — as with OpenCode — the config
	// match performs detection as well as transport attach.
	writeConfig(t, filepath.Join(home, ".copilot", "mcp-config.json"), `{
		"mcpServers": {
			"github":  {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"]},
			"tracker": {"url": "https://tracker.example.com/mcp"}
		}
	}`)
	r := mcpTestRunner(t)

	ev := mcpToolPre(ProviderCopilotCLI, "", "github-create_issue")
	if ev.Tool.MCP != nil || ev.Tool.Canonical == ToolMCP {
		t.Fatalf("precondition: codec must not classify copilot MCP names: %+v", ev.Tool)
	}
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.Canonical != ToolMCP {
		t.Fatalf("copilot MCP call not detected: %+v", ev.Tool)
	}
	if ev.Tool.MCP.Server != "github" || ev.Tool.MCP.Tool != "create_issue" ||
		ev.Tool.MCP.Command != "npx -y @modelcontextprotocol/server-github" || !ev.Tool.MCP.FromConfig {
		t.Errorf("copilot identity/transport wrong: %+v", ev.Tool.MCP)
	}

	ev = mcpToolPre(ProviderCopilotCLI, "", "tracker-list_issues")
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://tracker.example.com/mcp" || ev.Tool.MCP.Tool != "list_issues" {
		t.Errorf("copilot remote server wrong: %+v", ev.Tool.MCP)
	}

	// Hyphenated native tool with no matching server: stays native. This is
	// the boundary that keeps the "-" separator usable at all.
	ev = mcpToolPre(ProviderCopilotCLI, "", "read-file")
	r.resolveMCP(ev)
	if ev.Tool.MCP != nil {
		t.Errorf("unconfigured hyphenated native tool must not detect: %+v", ev.Tool.MCP)
	}

	ev = mcpToolPre(ProviderCopilotCLI, "", "shell")
	r.resolveMCP(ev)
	if ev.Tool.MCP != nil || ev.Tool.Canonical != ToolShell {
		t.Errorf("native copilot tool must stay native: %+v", ev.Tool)
	}
}

// TestResolveMCPCopilotHyphenCollision pins the known false positive: with a
// server named "read" configured, a native "read-file" is indistinguishable
// from an MCP call by name alone and resolves as MCP. Copilot supplies no
// marker to break the tie, so this is accepted rather than fixed — the test
// exists so the behaviour changes deliberately, not by accident.
func TestResolveMCPCopilotHyphenCollision(t *testing.T) {
	home := isolateHome(t)
	writeConfig(t, filepath.Join(home, ".copilot", "mcp-config.json"), `{
		"mcpServers": {"read": {"url": "https://read.example.com/mcp"}}
	}`)
	ev := mcpToolPre(ProviderCopilotCLI, "", "read-file")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.Server != "read" || ev.Tool.MCP.Tool != "file" {
		t.Errorf("expected the documented hyphen collision, got: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPCopilotLongestPrefixWins(t *testing.T) {
	home := isolateHome(t)
	// Both the server name and the tool name may contain "-", so the split is
	// ambiguous; configured names match longest-first.
	writeConfig(t, filepath.Join(home, ".copilot", "mcp-config.json"), `{
		"mcpServers": {
			"a":   {"url": "https://short.example.com"},
			"a-b": {"url": "https://long.example.com"}
		}
	}`)
	ev := mcpToolPre(ProviderCopilotCLI, "", "a-b-do")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.Server != "a-b" || ev.Tool.MCP.Tool != "do" ||
		ev.Tool.MCP.URL != "https://long.example.com" {
		t.Errorf("longest-prefix match wrong: %+v", ev.Tool.MCP)
	}
}

// TestResolveMCPCopilotProjectScope pins that Copilot's workspace configs are
// read and outrank the user-scope file, the way Copilot itself resolves them.
func TestResolveMCPCopilotProjectScope(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(home, ".copilot", "mcp-config.json"), `{
		"mcpServers": {
			"github": {"url": "https://user.example.com/mcp"},
			"notes":  {"url": "https://notes.example.com/mcp"}
		}
	}`)
	writeConfig(t, filepath.Join(cwd, ".github", "mcp.json"), `{
		"mcpServers": {
			"github": {"url": "https://workspace.example.com/mcp"},
			"shared": {"url": "https://github-file.example.com/mcp"}
		}
	}`)
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{
		"mcpServers": {
			"tracker": {"url": "https://tracker.example.com/mcp"},
			"shared":  {"url": "https://root-file.example.com/mcp"}
		}
	}`)
	r := mcpTestRunner(t)

	// Same name in both scopes: the workspace transport wins.
	ev := mcpToolPre(ProviderCopilotCLI, cwd, "github-create_issue")
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://workspace.example.com/mcp" {
		t.Errorf("workspace scope must outrank user scope: %+v", ev.Tool.MCP)
	}

	// .mcp.json is read too.
	ev = mcpToolPre(ProviderCopilotCLI, cwd, "tracker-list_issues")
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://tracker.example.com/mcp" {
		t.Errorf(".mcp.json server not resolved: %+v", ev.Tool.MCP)
	}

	// At workspace scope, .mcp.json takes precedence over .github/mcp.json.
	ev = mcpToolPre(ProviderCopilotCLI, cwd, "shared-search")
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://root-file.example.com/mcp" {
		t.Errorf(".mcp.json must outrank .github/mcp.json: %+v", ev.Tool.MCP)
	}

	// User scope still resolves for names the workspace does not define.
	ev = mcpToolPre(ProviderCopilotCLI, cwd, "notes-search")
	r.resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://notes.example.com/mcp" {
		t.Errorf("user scope must still resolve: %+v", ev.Tool.MCP)
	}
}

// TestResolveMCPCopilotHomeOverride verifies COPILOT_HOME relocates the config
// directory, which is also what lets the e2e suite point Copilot and the
// resolver at the same temp config.
func TestResolveMCPCopilotHomeOverride(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	t.Setenv("COPILOT_HOME", dir)
	writeConfig(t, filepath.Join(dir, "mcp-config.json"), `{
		"mcpServers": {"notes": {"url": "https://notes.example.com/mcp"}}
	}`)
	ev := mcpToolPre(ProviderCopilotCLI, "", "notes-search")
	mcpTestRunner(t).resolveMCP(ev)
	if ev.Tool.MCP == nil || ev.Tool.MCP.URL != "https://notes.example.com/mcp" {
		t.Errorf("COPILOT_HOME override not honoured: %+v", ev.Tool.MCP)
	}
}

func TestResolveMCPVSCode(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".vscode", "mcp.json"), `{
		// VS Code uses a servers block and permits JSONC comments and trailing commas.
		"servers": {
			"github": {
				"type": "http",
				"url": "https://example.test/vscode",
			},
		},
	}`)
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{
		"mcpServers": {"My Long Server Name!!!": {"url": "https://example.test/root"}}
	}`)
	r := mcpTestRunner(t)
	for _, tc := range []struct {
		name, server, tool, url string
	}{
		{"mcp_github_list_issues", "github", "list_issues", "https://example.test/vscode"},
		{"mcp_my_long_serve_search", "my_long_serve", "search", "https://example.test/root"},
	} {
		ev := mcpToolPre(ProviderVSCodeCopilot, cwd, tc.name)
		r.resolveMCP(ev)
		if ev.Tool.MCP == nil {
			t.Fatalf("%s: MCP call was not recognized", tc.name)
		}
		if got := ev.Tool.MCP; got.Server != tc.server || got.Tool != tc.tool || got.URL != tc.url || !got.FromConfig {
			t.Errorf("%s: VS Code MCP resolution = %+v", tc.name, got)
		}
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
	r := syncMCPRefresh(quietRunner(WithDedupDir(stateDir)))

	ev := mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__x")
	r.resolveMCP(ev)
	r.resolveMCP(mcpToolPre(ProviderClaudeCode, "", "mcp__ghost__y"))
	if got := cliRuns(t, count); got != 1 {
		t.Errorf("empty inventory must be negative-cached, ran %d times", got)
	}

	// The cache is on disk: a fresh runner (separate hook process in prod)
	// sharing the state dir must not re-probe either.
	r2 := syncMCPRefresh(quietRunner(WithDedupDir(stateDir)))
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
	stateDir := t.TempDir()
	payload := []byte(fmt.Sprintf(
		`{"hook_event_name":"SessionStart","session_id":"s-warm","cwd":%q,"source":"startup"}`,
		project,
	))

	r := quietRunner(WithDedupDir(stateDir))
	var warmed []string
	r.mcpWarmStart = func(cwd string) { warmed = append(warmed, cwd) }
	if _, code := runWith(t, r, claudeArgs(), payload); code != 0 || len(warmed) != 1 || warmed[0] != project {
		t.Fatalf("SessionStart warm = %q, exit %d", warmed, code)
	}

	// A fresh snapshot needs no refresh, even from a new hook process.
	path := mcpListCachePath(t, stateDir, currentClaudeLaunchContext(project).cacheKey())
	now := time.Now().Unix()
	writeMCPListCache(path, mcpListCache{CheckedAt: now, SnapshotAt: now, HasSnapshot: true})
	fresh := quietRunner(WithDedupDir(stateDir))
	fresh.mcpWarmStart = func(string) { t.Fatal("fresh snapshot scheduled an inventory warm") }
	if _, code := runWith(t, fresh, claudeArgs(), payload); code != 0 {
		t.Fatalf("fresh SessionStart exit = %d", code)
	}

	disabled := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPListFallback())
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
	runAgenthooksMain(t, agenthooksMainCommand(t, stateDir, resultFile, readyFile, sessionPayload), "SessionStart")

	// The fake writes its count only after the detached worker owns the
	// per-context lock, then waits on the gate before returning inventory.
	waitForTestFile(t, fake.countFile)
	prePayload := []byte(fmt.Sprintf(
		`{"hook_event_name":"PreToolUse","session_id":"s-background","cwd":%q,"tool_name":"mcp__background__run","tool_input":{}}`,
		project,
	))
	runAgenthooksMain(t, agenthooksMainCommand(t, stateDir, resultFile, readyFile, prePayload), "MCP hook during refresh")
	if data, err := os.ReadFile(resultFile); err != nil || len(data) != 0 {
		t.Fatalf("MCP hook during refresh resolved %q (err %v), want no transport yet", data, err)
	}

	writeConfig(t, fake.gateFile, "release")
	waitForMCPListSnapshot(t, stateDir)
	runAgenthooksMain(t, agenthooksMainCommand(t, stateDir, resultFile, readyFile, prePayload), "MCP hook after refresh")
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "https://background.example.com/mcp" {
		t.Fatalf("resolved transport = %q", data)
	}
	if got := cliRuns(t, fake.countFile); got != 1 {
		t.Fatalf("hooks during an in-flight refresh must not start another probe, ran %d times", got)
	}
}

// runAgenthooksMain runs one hook process and fails if it does not exit
// promptly: no hook may wait for a provider CLI probe.
func runAgenthooksMain(t *testing.T, cmd *exec.Cmd, name string) {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%s waited for a blocked inventory probe", name)
	}
}

func TestClaudeMCPServesStaleSnapshotWhileRefreshInFlight(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	startClaudeLaunch(t, project, "-p", "prompt")
	stateDir := t.TempDir()
	r := quietRunner(WithDedupDir(stateDir))
	now := time.Now().Truncate(time.Second)
	r.now = func() time.Time { return now }
	r.mcpWarmStart = func(string) { t.Error("refresh started while another worker held the lock") }
	path := mcpListCachePath(t, stateDir, currentClaudeLaunchContext(project).cacheKey())
	stale := now.Add(-time.Hour).Unix()
	writeMCPListCache(path, mcpListCache{
		CheckedAt:   stale,
		SnapshotAt:  stale,
		Entries:     []mcpConfigEntry{{Name: "shared", URL: "https://stale.example.com/mcp"}},
		HasSnapshot: true,
	})
	release, locked, err := tryMCPListLock(path + ".lock")
	if err != nil || !locked {
		t.Fatalf("holding refresh lock: locked=%v err=%v", locked, err)
	}
	defer release()

	start := time.Now()
	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__shared__run")
	r.resolveMCP(ev)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("resolution waited %v for the in-flight refresh", elapsed)
	}
	if ev.Tool.MCP.URL != "https://stale.example.com/mcp" {
		t.Fatalf("stale snapshot not served: %+v", ev.Tool.MCP)
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
		"plugin:launch-plugin-extra:leak: https://wrong.example.com/mcp - connected",
	}, "\n"))
	startClaudeLaunch(t, project, "-p", "prompt", "--bare", "--plugin-dir", pluginDir, "--mcp-config", "launch.json")
	r := mcpTestRunner(t)

	explicit := mcpToolPre(ProviderClaudeCode, project, "mcp__explicit__run")
	r.resolveMCP(explicit)
	plugin := mcpToolPre(ProviderClaudeCode, project, "mcp__plugin_launch-plugin_tools__run")
	r.resolveMCP(plugin)
	excluded := mcpToolPre(ProviderClaudeCode, project, "mcp__excluded__run")
	r.resolveMCP(excluded)
	leaked := mcpToolPre(ProviderClaudeCode, project, "mcp__plugin_launch-plugin-extra_leak__run")
	r.resolveMCP(leaked)
	if explicit.Tool.MCP.URL != "https://explicit.example.com/mcp" || plugin.Tool.MCP.URL != "https://plugin.example.com/mcp" {
		t.Fatalf("bare explicit/plugin inventory wrong: explicit=%+v plugin=%+v", explicit.Tool.MCP, plugin.Tool.MCP)
	}
	if excluded.Tool.MCP.URL != "" {
		t.Fatalf("bare mode must exclude ordinary config: %+v", excluded.Tool.MCP)
	}
	if leaked.Tool.MCP.URL != "" {
		t.Fatalf("bare mode matched a similarly named plugin: %+v", leaked.Tool.MCP)
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
	r := syncMCPRefresh(quietRunner(WithDedupDir(stateDir)))

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

	// Every hook that finds the snapshot due may start a worker; only the
	// one holding the context's lock probes.
	const workers = 8
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			quietRunner(WithDedupDir(stateDir)).warmClaudeMCP(project)
		})
	}
	wg.Wait()
	if got := cliRuns(t, fake.countFile); got != 1 {
		t.Fatalf("concurrent workers must share one probe, ran %d times", got)
	}
	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__shared__run")
	syncMCPRefresh(quietRunner(WithDedupDir(stateDir))).resolveMCP(ev)
	if ev.Tool.MCP.URL != "https://shared.example.com/mcp" || cliRuns(t, fake.countFile) != 1 {
		t.Fatalf("worker snapshot not served: %+v (runs=%d)", ev.Tool.MCP, cliRuns(t, fake.countFile))
	}
}

func TestMCPListCacheCleanup(t *testing.T) {
	dir := t.TempDir()
	oldPaths := []string{
		filepath.Join(dir, "old.json"),
		filepath.Join(dir, "old.json.lock"),
		filepath.Join(dir, "inventory-orphan"),
	}
	newPath := filepath.Join(dir, "new.json")
	retainedPath := filepath.Join(dir, "retained.json")
	for _, path := range oldPaths {
		writeConfig(t, path, `{}`)
	}
	writeConfig(t, newPath, `{}`)
	writeConfig(t, retainedPath, `{}`)
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-mcpListCacheRetention - time.Second)
	for _, path := range oldPaths {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(newPath, now, now); err != nil {
		t.Fatal(err)
	}
	retained := now.Add(-mcpListCacheRetention + time.Hour)
	if err := os.Chtimes(retainedPath, retained, retained); err != nil {
		t.Fatal(err)
	}
	cleanupMCPListCache(dir, now, mcpListCacheRetention)
	for _, path := range oldPaths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expired cache artifact %q was not removed: %v", path, err)
		}
	}
	for _, path := range []string{newPath, retainedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cache inside retention %q was removed: %v", path, err)
		}
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

// Hooks inherit the session's environment, and a desktop- or MDM-launched
// Claude frequently passes one without the CLI on PATH. Resolution must fall
// back to the binary that launched the session, or every MCP call from such a
// machine loses its server identity — which downstream reads as shadow MCP.
func TestResolveMCPClaudeListUsesLaunchExecutableWithoutPATH(t *testing.T) {
	home := isolateHome(t)
	count := fakeClaudeCLI(t, "claude.ai Linear (Acme): https://mcp.linear.app/sse (SSE) - ✓ Connected\n")
	project := t.TempDir()
	startClaudeLaunch(t, project, "-p", "prompt")

	// Everything above mirrors a normal session; now take the CLI off PATH.
	empty := filepath.Join(home, "bin-empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", empty)
	if _, err := exec.LookPath("claude"); err == nil {
		t.Fatal("claude must not be resolvable on PATH for this test to mean anything")
	}

	r := mcpTestRunner(t)
	ev := mcpToolPre(ProviderClaudeCode, project, "mcp__claude_ai_Linear_Acme__create_issue")
	r.resolveMCP(ev)

	if ev.Tool.MCP.URL != "https://mcp.linear.app/sse" || !ev.Tool.MCP.FromConfig {
		t.Errorf("launch executable must resolve the connector without PATH: %+v", ev.Tool.MCP)
	}
	if got := cliRuns(t, count); got != 1 {
		t.Errorf("CLI must run once, ran %d times", got)
	}
}
