package agenthooks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

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
		"GEMINI_CWD", "GEMINI_CLI", "OPENCODE_SERVER", "OPENCODE", "CLAUDE_PROJECT_DIR", "CLAUDE_PLUGIN_ROOT",
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
