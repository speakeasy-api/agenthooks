package agenthooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPInventoryCompletesBeforeFirstTool(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"remote":{"url":"https://mcp.example.com"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	reported := false
	r := New(WithDedupDir(t.TempDir()), WithoutMCPListFallback())
	r.OnMCPInventory(func(_ context.Context, event *MCPInventoryEvent) error {
		if len(event.Servers) != 1 || event.Servers[0].Name != "remote" {
			return fmt.Errorf("unexpected inventory: %#v", event.Servers)
		}
		if !event.Complete {
			return errors.New("config-only inventory must be complete when CLI fallback is disabled")
		}
		reported = true
		return nil
	})
	r.OnToolPre(func(_ context.Context, _ *ToolPreEvent) (ToolPreDecision, error) {
		if !reported {
			return NoDecision(), errors.New("inventory handler must complete before the tool handler starts")
		}
		return NoDecision(), nil
	})

	payload := strings.NewReader(fmt.Sprintf(`{"session_id":"session-1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"mcp__remote__call","tool_input":{}}`, cwd))
	var stdout, stderr bytes.Buffer
	if code := r.Run(t.Context(), []string{"run", "--provider=claude-code", "--variant=cli"}, payload, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d: %s", code, stderr.String())
	}
	if !reported {
		t.Fatal("inventory was not reported")
	}
}

func TestMCPInventoryRetriesAfterHandlerFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := New(WithDedupDir(t.TempDir()), WithoutMCPListFallback())
	attempts := 0
	r.OnMCPInventory(func(_ context.Context, _ *MCPInventoryEvent) error {
		attempts++
		if attempts == 1 {
			return errors.New("temporary failure")
		}
		return nil
	})
	r.OnToolPre(func(_ context.Context, _ *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision(), nil
	})

	payload := []byte(`{"session_id":"session-retry","hook_event_name":"PreToolUse","tool_name":"mcp__remote__call","tool_input":{}}`)
	runWith(t, r, claudeArgs(), payload)
	runWith(t, r, claudeArgs(), payload)
	if attempts != 2 {
		t.Fatalf("inventory attempts = %d, want 2", attempts)
	}
}

func TestMCPInventoryNeverWaitsForProbe(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"configured":{"url":"https://configured.example.com/mcp"}}}`)
	stateDir := t.TempDir()
	r := New(WithDedupDir(stateDir))
	r.mcpWarmStart = func(string) { t.Error("refresh started while another worker held the lock") }
	inventories := make(chan *MCPInventoryEvent, 3)
	r.OnMCPInventory(func(_ context.Context, event *MCPInventoryEvent) error {
		inventories <- event
		return nil
	})
	r.OnToolPre(func(_ context.Context, _ *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision(), nil
	})

	// A detached worker is mid-probe: it holds the context's lock.
	cachePath := mcpListCachePath(t, stateDir, currentClaudeLaunchContext(cwd).cacheKey())
	release, locked, err := tryMCPListLock(cachePath + ".lock")
	if err != nil || !locked {
		t.Fatalf("holding probe lock: locked=%v err=%v", locked, err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	runPromptly := func(name string, payload []byte) {
		t.Helper()
		start := time.Now()
		runWith(t, r, claudeArgs(), payload)
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("%s waited %v for the in-flight MCP probe", name, elapsed)
		}
	}
	runPromptly("SessionStart", []byte(fmt.Sprintf(
		`{"session_id":"session-warm","cwd":%q,"hook_event_name":"SessionStart","source":"startup"}`,
		cwd,
	)))
	first := <-inventories
	if first.Complete || len(first.Servers) != 1 || first.Servers[0].Name != "configured" {
		t.Fatalf("SessionStart inventory = %+v", first)
	}

	toolPayload := []byte(fmt.Sprintf(
		`{"session_id":"session-warm","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"mcp__plugin__run","tool_input":{}}`,
		cwd,
	))
	runPromptly("first MCP hook", toolPayload)
	select {
	case again := <-inventories:
		t.Fatalf("unchanged partial inventory was reported twice: %+v", again)
	default:
	}

	// The worker lands its snapshot; the next MCP hook reports it.
	now := time.Now().Unix()
	writeMCPListCache(cachePath, mcpListCache{
		CheckedAt:   now,
		SnapshotAt:  now,
		Entries:     []mcpConfigEntry{{Name: "plugin", URL: "https://plugin.example.com/mcp"}},
		HasSnapshot: true,
	})
	release()
	released = true
	runPromptly("next MCP hook", toolPayload)
	var second *MCPInventoryEvent
	select {
	case second = <-inventories:
	default:
		t.Fatal("next MCP hook did not emit the completed inventory")
	}
	if !second.Complete || len(second.Servers) != 2 {
		t.Fatalf("next MCP hook inventory = %+v", second)
	}
}

func TestMCPInventoryServesStaleSnapshotAsPartial(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"configured":{"url":"https://configured.example.com/mcp"}}}`)
	stateDir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	r := mcpTestRunner(t, WithDedupDir(stateDir))
	r.now = func() time.Time { return now }
	starts := 0
	r.mcpWarmStart = func(string) { starts++ }
	cachePath := mcpListCachePath(t, stateDir, currentClaudeLaunchContext(cwd).cacheKey())
	event := &Event{Provider: ProviderClaudeCode, Session: SessionInfo{CWD: cwd}}

	old := now.Add(-13 * 24 * time.Hour).Unix()
	writeMCPListCache(cachePath, mcpListCache{
		CheckedAt:   old,
		SnapshotAt:  old,
		Entries:     []mcpConfigEntry{{Name: "plugin", URL: "https://plugin.example.com/mcp"}},
		HasSnapshot: true,
	})
	entries, complete := r.effectiveMCPInventory(event)
	if complete || len(entries) != 2 {
		t.Fatalf("stale inventory = %+v complete=%v, want config plus cached plugin as partial", entries, complete)
	}
	_, _ = r.effectiveMCPInventory(event)
	if starts != 1 {
		t.Fatalf("stale snapshot started %d refreshes, want 1", starts)
	}

	expired := now.Add(-mcpListCacheRetention - time.Hour).Unix()
	writeMCPListCache(cachePath, mcpListCache{
		CheckedAt:   now.Unix(),
		SnapshotAt:  expired,
		Entries:     []mcpConfigEntry{{Name: "plugin", URL: "https://plugin.example.com/mcp"}},
		HasSnapshot: true,
	})
	entries, complete = r.effectiveMCPInventory(event)
	if complete || len(entries) != 1 || entries[0].Name != "configured" {
		t.Fatalf("expired snapshot served: %+v complete=%v", entries, complete)
	}
}

// Cowork connectors come from the claude.ai account, not the VM, so `claude
// mcp list` there always prints no servers. It must never become a complete,
// empty inventory.
func TestMCPInventoryCoworkIsNeverComplete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entrypoint string
		opts       []Option
		complete   bool
		probes     int
	}{
		{name: "cowork", entrypoint: claudeCoworkEntrypoint, probes: 0},
		{name: "cowork without CLI fallback", entrypoint: claudeCoworkEntrypoint, opts: []Option{WithoutMCPListFallback()}, probes: 0},
		{name: "cli", entrypoint: "cli", complete: true, probes: 1},
		{name: "cli without CLI fallback", entrypoint: "cli", opts: []Option{WithoutMCPListFallback()}, complete: true, probes: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			t.Setenv("CLAUDE_CODE_ENTRYPOINT", tc.entrypoint)
			cwd := t.TempDir()
			writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"configured":{"url":"https://configured.example.com/mcp"}}}`)
			fake := installFakeClaude(t, "No MCP servers configured. Use `claude mcp add` to add a server.\n")
			r := mcpTestRunner(t, tc.opts...)
			entries, complete := r.effectiveMCPInventory(&Event{
				Provider: ProviderClaudeCode,
				Session:  SessionInfo{CWD: cwd},
			})
			if complete != tc.complete || len(entries) != 1 || entries[0].Name != "configured" {
				t.Fatalf("inventory = %+v complete=%v, want configured only, complete=%v", entries, complete, tc.complete)
			}
			if got := cliRuns(t, fake.countFile); got != tc.probes {
				t.Fatalf("claude mcp list ran %d times, want %d", got, tc.probes)
			}
		})
	}
}

func TestClaudeCoworkSkipsProbeButResolvesConfig(t *testing.T) {
	isolateHome(t)
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", claudeCoworkEntrypoint)
	project := t.TempDir()
	writeConfig(t, filepath.Join(project, ".mcp.json"), `{"mcpServers":{"configured":{"url":"https://configured.example.com/mcp"}}}`)
	fake := installFakeClaude(t, "connector: https://wrong.example.com/mcp - connected\n")
	startClaudeLaunch(t, project, "-p", "prompt", "--settings", "extra.json")
	r := mcpTestRunner(t)
	r.mcpWarmStart = func(string) { t.Fatal("Cowork session scheduled an MCP list refresh") }

	configured := mcpToolPre(ProviderClaudeCode, project, "mcp__configured__run")
	r.resolveMCP(configured)
	connector := mcpToolPre(ProviderClaudeCode, project, "mcp__connector__run")
	r.resolveMCP(connector)
	if configured.Tool.MCP.URL != "https://configured.example.com/mcp" || connector.Tool.MCP.URL != "" {
		t.Fatalf("Cowork resolution: configured=%+v connector=%+v", configured.Tool.MCP, connector.Tool.MCP)
	}
	payload := []byte(fmt.Sprintf(
		`{"hook_event_name":"SessionStart","session_id":"s-cowork","cwd":%q,"source":"startup"}`,
		project,
	))
	if _, code := runWith(t, r, claudeArgs(), payload); code != 0 {
		t.Fatalf("Cowork SessionStart exit = %d", code)
	}
	if got := cliRuns(t, fake.countFile); got != 0 {
		t.Fatalf("Cowork session ran claude mcp list %d times", got)
	}
}

func TestMCPListCacheView(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	entries := []mcpConfigEntry{{Name: "plugin", URL: "https://plugin.example.com/mcp"}}
	at := func(age time.Duration) int64 { return now.Add(-age).Unix() }
	for _, tc := range []struct {
		name       string
		cached     mcpListCache
		served     bool
		complete   bool
		refreshDue bool
	}{
		{name: "missing", refreshDue: true},
		{name: "fresh", cached: mcpListCache{CheckedAt: at(time.Minute), SnapshotAt: at(time.Minute), Entries: entries, HasSnapshot: true}, served: true, complete: true},
		{name: "stale", cached: mcpListCache{CheckedAt: at(time.Hour), SnapshotAt: at(time.Hour), Entries: entries, HasSnapshot: true}, served: true, refreshDue: true},
		{name: "recent failed attempt keeps stale snapshot", cached: mcpListCache{CheckedAt: at(time.Minute), SnapshotAt: at(time.Hour), Entries: entries, HasSnapshot: true}, served: true},
		{name: "recent failed attempt without snapshot", cached: mcpListCache{CheckedAt: at(time.Minute)}},
		{name: "expired", cached: mcpListCache{CheckedAt: at(time.Minute), SnapshotAt: at(mcpListCacheRetention + time.Hour), Entries: entries, HasSnapshot: true}},
		{name: "legacy snapshot uses checked_at", cached: mcpListCache{CheckedAt: at(time.Minute), Entries: entries, HasSnapshot: true}, served: true, complete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := viewMCPListCache(tc.cached, now)
			if (len(view.Entries) > 0) != tc.served || view.Complete != tc.complete || view.RefreshDue != tc.refreshDue {
				t.Fatalf("view = %+v, want served=%v complete=%v refreshDue=%v", view, tc.served, tc.complete, tc.refreshDue)
			}
		})
	}
}

func TestMCPInventoryPreservesClaudeConfigAfterProbeFailure(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"configured":{"url":"https://configured.example.com/mcp"}}}`)
	fake := installFakeClaude(t, "")
	writeConfig(t, fake.exitFile, "1")
	r := mcpTestRunner(t)
	entries, complete := r.effectiveMCPInventory(&Event{
		Provider: ProviderClaudeCode,
		Session:  SessionInfo{CWD: cwd},
	})
	if complete || len(entries) != 1 || entries[0].Name != "configured" {
		t.Fatalf("Claude failed-probe inventory = %+v complete=%v", entries, complete)
	}
}

func TestMCPInventoryPreservesCodexConfigAfterProbeFailure(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.configured]
url = "https://configured.example.com/mcp"
`)
	installFakeCodex(t, "not-json")
	launch := parseCodexLaunchArgs([]string{"codex"}, cwd)
	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch
	entries, complete := r.effectiveMCPInventory(&Event{
		Provider: ProviderCodex,
		Session:  SessionInfo{CWD: cwd},
	})
	if complete || len(entries) != 1 || entries[0].Name != "configured" {
		t.Fatalf("Codex failed-probe inventory = %+v complete=%v", entries, complete)
	}
}

func TestMCPListRefreshExitsWhenAnotherWorkerHoldsLock(t *testing.T) {
	r := quietRunner(WithDedupDir(t.TempDir()))
	path := filepath.Join(r.mcpListCacheDir(), "busy.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	release, locked, err := tryMCPListLock(path + ".lock")
	if err != nil || !locked {
		t.Fatalf("holding probe lock: locked=%v err=%v", locked, err)
	}
	defer release()

	start := time.Now()
	r.refreshMCPList(t.Context(), "busy", func(context.Context) ([]mcpConfigEntry, bool) {
		t.Fatal("worker must not probe while another worker holds the lock")
		return nil, false
	})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("worker waited %v for the lock", elapsed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worker wrote a cache snapshot without the lock: %v", err)
	}
}

func TestMCPListRefreshRecordsFailedAttempt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.now = func() time.Time { return now }
	path := filepath.Join(r.mcpListCacheDir(), "failing.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := now.Add(-time.Hour).Unix()
	writeMCPListCache(path, mcpListCache{
		CheckedAt:   stale,
		SnapshotAt:  stale,
		Entries:     []mcpConfigEntry{{Name: "kept", URL: "https://kept.example.com/mcp"}},
		HasSnapshot: true,
	})
	probes := 0
	failing := func(context.Context) ([]mcpConfigEntry, bool) {
		probes++
		return nil, false
	}
	r.refreshMCPList(t.Context(), "failing", failing)
	view := viewMCPListCache(readMCPListCache(path), now)
	if len(view.Entries) != 1 || view.Complete || view.RefreshDue {
		t.Fatalf("after failed probe view = %+v, want stale snapshot, partial, backing off", view)
	}
	r.refreshMCPList(t.Context(), "failing", failing)
	if probes != 1 {
		t.Fatalf("failed probe retried inside the backoff window: %d probes", probes)
	}
}

func TestMCPGateDoesNotHitTimeoutPolicyDuringProbe(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	stateDir := t.TempDir()
	r := quietRunner(
		WithDedupDir(stateDir),
		WithPolicy(Policy{Fail: FailClosed, Timeout: 200 * time.Millisecond}),
	)
	r.OnMCPInventory(func(context.Context, *MCPInventoryEvent) error { return nil })
	r.OnToolPre(func(context.Context, *ToolPreEvent) (ToolPreDecision, error) {
		return Allow(), nil
	})

	cachePath := mcpListCachePath(t, stateDir, currentClaudeLaunchContext(cwd).cacheKey())
	release, locked, err := tryMCPListLock(cachePath + ".lock")
	if err != nil || !locked {
		t.Fatalf("holding probe lock: locked=%v err=%v", locked, err)
	}
	defer release()

	payload := []byte(fmt.Sprintf(
		`{"session_id":"session-timeout","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"mcp__remote__call","tool_input":{}}`,
		cwd,
	))
	out, _ := runWith(t, r, claudeArgs(), payload)
	if !strings.Contains(out, `"permissionDecision":"allow"`) {
		t.Fatalf("an in-flight probe must not push the gate into its timeout fallback: %q", out)
	}
}

func quietRunner(opts ...Option) *Runner {
	opts = append([]Option{WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)
	return New(opts...)
}

func runWith(t *testing.T, r *Runner, args []string, payload []byte) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := r.Run(context.Background(), args, bytes.NewReader(payload), &out, &errb)
	return out.String(), code
}

func claudeArgs() []string { return []string{"agenthooks", "run", "--provider=claude-code"} }

func TestRunClaudeDeny(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		if e.Tool.Canonical != ToolShell {
			t.Errorf("expected shell tool, got %+v", e.Tool)
		}
		return Deny("blocked"), nil
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked"}}`
	if out != want || code != 0 {
		t.Errorf("got %q (exit %d), want %q (exit 0)", out, code, want)
	}
}

func TestRunFailModes(t *testing.T) {
	boom := func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision(), errors.New("boom")
	}

	closed := quietRunner(WithPolicy(Policy{Fail: FailClosed}))
	closed.OnToolPre(boom)
	out, code := runWith(t, closed, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("fail-closed should deny: %q (exit %d)", out, code)
	}

	open := quietRunner(WithPolicy(Policy{Fail: FailOpen}))
	open.OnToolPre(boom)
	out, code = runWith(t, open, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || out != "{}" {
		t.Errorf("fail-open should be a no-op: %q (exit %d)", out, code)
	}
}

func TestRunPanicRecovery(t *testing.T) {
	r := quietRunner(WithPolicy(Policy{Fail: FailClosed}))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		panic("handler bug")
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || !strings.Contains(out, `"deny"`) {
		t.Errorf("panic must not leak garbage; got %q (exit %d)", out, code)
	}
}

func TestRunTimeout(t *testing.T) {
	r := quietRunner(WithPolicy(Policy{Fail: FailClosed, Timeout: 50 * time.Millisecond}))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		time.Sleep(2 * time.Second) // ignores ctx on purpose
		return Allow(), nil
	})
	start := time.Now()
	out, _ := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("deadline not enforced: took %v", elapsed)
	}
	if !strings.Contains(out, `"deny"`) {
		t.Errorf("timeout under fail-closed should deny: %q", out)
	}
}

func TestAskDegradationOnCodex(t *testing.T) {
	ask := func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return AskUser("confirm?"), nil
	}
	args := []string{"agenthooks", "run", "--provider=codex"}
	payload := fixture(t, "codex/pre_tool_use.json")

	toDeny := quietRunner(WithPolicy(Policy{Unsupported: Degrade, AskFallback: FallbackDeny}))
	toDeny.OnToolPre(ask)
	out, _ := runWith(t, toDeny, args, payload)
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("ask should degrade to deny: %q", out)
	}

	toNone := quietRunner(WithPolicy(Policy{Unsupported: Degrade, AskFallback: FallbackNoDecision}))
	toNone.OnToolPre(ask)
	out, _ = runWith(t, toNone, args, payload)
	if out != "" {
		t.Errorf("ask should degrade to codex empty-stdout no-op: %q", out)
	}

	strict := quietRunner(WithPolicy(Policy{Unsupported: Strict, Fail: FailClosed}))
	strict.OnToolPre(ask)
	out, _ = runWith(t, strict, args, payload)
	if !strings.Contains(out, `"deny"`) {
		t.Errorf("strict unsupported + fail-closed should deny: %q", out)
	}
}

func TestFilterFlag(t *testing.T) {
	deny := func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("no"), nil
	}
	payload := fixture(t, "claude/pre_tool_use.json") // Bash → shell

	miss := quietRunner()
	miss.OnToolPre(deny)
	out, _ := runWith(t, miss, append(claudeArgs(), "--filter=canonical=file.write"), payload)
	if out != "{}" {
		t.Errorf("filtered-out event must no-op without invoking the handler: %q", out)
	}

	hit := quietRunner()
	hit.OnToolPre(deny)
	out, _ = runWith(t, hit, append(claudeArgs(), "--filter=canonical=shell"), payload)
	if !strings.Contains(out, `"deny"`) {
		t.Errorf("matching filter must dispatch: %q", out)
	}
}

func TestOnAnyAndOnOther(t *testing.T) {
	r := quietRunner()
	var anyNative, otherNative string
	r.OnAny(func(ctx context.Context, e *Event) error {
		anyNative = e.NativeName
		return nil
	})
	r.OnOther("Setup", func(ctx context.Context, e *Event) error {
		otherNative = e.NativeName
		return nil
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/setup.json"))
	if out != "{}" || code != 0 {
		t.Errorf("unmapped event must no-op: %q (exit %d)", out, code)
	}
	if anyNative != "Setup" || otherNative != "Setup" {
		t.Errorf("observers not called: any=%q other=%q", anyNative, otherNative)
	}
}

func TestContinuationCap(t *testing.T) {
	cont := func(ctx context.Context, e *StopEvent) (StopDecision, error) {
		return ContinueWith("keep going"), nil
	}
	payload := fixture(t, "claude/stop.json") // stop_hook_active → LoopCount 1

	capped := quietRunner(WithPolicy(Policy{ContinuationCap: 1}))
	capped.OnStop(cont)
	out, _ := runWith(t, capped, claudeArgs(), payload)
	if out != "{}" {
		t.Errorf("cap reached: ContinueWith must degrade to Finish: %q", out)
	}

	free := quietRunner()
	free.OnStop(cont)
	out, _ = runWith(t, free, claudeArgs(), payload)
	if out != `{"decision":"block","reason":"keep going"}` {
		t.Errorf("under the cap ContinueWith must block: %q", out)
	}
}

func TestCursorDedup(t *testing.T) {
	calls := 0
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		calls++
		if e.NativeName == "beforeMCPExecution" && (e.Tool.MCP == nil || e.Tool.MCP.Server != "srv") {
			t.Errorf("specific MCP server identity = %+v", e.Tool.MCP)
		}
		return Deny("stop"), nil
	})
	args := []string{"agenthooks", "run", "--provider=cursor"}

	first, _ := runWith(t, r, args, fixture(t, "cursor/before_shell_execution.json"))
	sibling := []byte(`{"conversation_id":"conv-cursor-1","generation_id":"gen-5","hook_event_name":"preToolUse","workspace_roots":["/work/repo"],"tool_name":"Shell","tool_input":{"command":"git push origin main"}}`)
	second, _ := runWith(t, r, args, sibling)

	if !strings.Contains(first, `"permission":"deny"`) {
		t.Errorf("first arrival should decide: %q", first)
	}
	if second != "{}" {
		t.Errorf("duplicate sibling should no-op (quirk #2): %q", second)
	}
	if calls != 1 {
		t.Errorf("handler must run once, ran %d times", calls)
	}
}

func TestCursorDedupGenericMCPEchoDoesNotSuppressSpecific(t *testing.T) {
	calls := 0
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		calls++
		return Deny("stop"), nil
	})
	args := []string{"agenthooks", "run", "--provider=cursor"}

	// Cursor fires the generic MCP: echo before beforeMCPExecution. The echo
	// carries no server identity (quirk #3), so it must not claim the dedup
	// marker away from the specific sibling.
	generic := []byte(`{"conversation_id":"conv-mcp-1","generation_id":"gen-9","hook_event_name":"preToolUse","tool_name":"MCP:shadow_lookup","tool_input":{"marker":"x"}}`)
	specific := []byte(`{"conversation_id":"conv-mcp-1","generation_id":"gen-9","hook_event_name":"beforeMCPExecution","tool_name":"shadow_lookup","tool_input":"{\"marker\":\"x\"}","mcp_server_name":"srv","command":"node server.mjs"}`)

	if _, code := runWith(t, r, args, generic); code != 0 {
		t.Fatalf("generic echo run failed")
	}
	second, _ := runWith(t, r, args, specific)
	if !strings.Contains(second, `"permission":"deny"`) {
		t.Errorf("specific sibling must still gate after a generic-first echo: %q", second)
	}
	echoAgain, _ := runWith(t, r, args, generic)
	if echoAgain != "{}" {
		t.Errorf("generic echo after the specific processed should no-op: %q", echoAgain)
	}
	if calls != 2 {
		t.Errorf("handler calls = %d, want 2 (echo passes through, specific gates, trailing echo suppressed)", calls)
	}
}

func TestNotifyMode(t *testing.T) {
	r := quietRunner()
	var msg string
	r.OnNotification(func(ctx context.Context, e *NotificationEvent) error {
		msg = e.Message
		return nil
	})
	payload := `{"type":"agent-turn-complete","turn-id":"t-1","thread-id":"th-1","last-assistant-message":"done"}`
	out, code := runWith(t, r, []string{"agenthooks", "notify", "--provider=codex", payload}, nil)
	if code != 0 || out != "" {
		t.Errorf("notify mode should emit nothing on codex: %q (exit %d)", out, code)
	}
	if msg != "done" {
		t.Errorf("notification not delivered: %q", msg)
	}
}

func TestArgvPayloadMode(t *testing.T) {
	r := quietRunner()
	r.OnPromptSubmitted(func(ctx context.Context, e *PromptEvent) (PromptDecision, error) {
		return BlockPrompt("not now"), nil
	})
	payload := `{"conversation_id":"c1","generation_id":"g1","hook_event_name":"beforeSubmitPrompt","prompt":"hi"}`
	out, code := runWith(t, r, []string{"agenthooks", "run", "--provider=cursor", "--argv-payload", payload}, nil)
	if code != 0 || out != `{"continue":false,"user_message":"not now"}` {
		t.Errorf("argv-payload mode broken: %q (exit %d)", out, code)
	}
}

func TestUndetectableProviderNoOps(t *testing.T) {
	for _, v := range []string{
		"CURSOR_VERSION", "CURSOR_TRACE_ID", "CURSOR_AGENT", "CODEX_HOME", "CODEX_SANDBOX",
		"GEMINI_CWD", "GEMINI_CLI", "OPENCODE_SERVER", "OPENCODE", "CLAUDECODE", "CLAUDE_PROJECT_DIR", "CLAUDE_PLUGIN_ROOT",
	} {
		t.Setenv(v, "")
	}
	r := quietRunner()
	out, code := runWith(t, r, nil, []byte("not json at all"))
	if code != 0 || out != "{}" {
		t.Errorf("undetectable provider must emit a neutral no-op, got %q (exit %d)", out, code)
	}
}

func TestBadFlagsExit64(t *testing.T) {
	r := quietRunner()
	_, code := runWith(t, r, []string{"agenthooks", "run", "--provider=unknown-agent"}, nil)
	if code != 64 {
		t.Errorf("bad provider flag should exit 64, got %d", code)
	}
}
