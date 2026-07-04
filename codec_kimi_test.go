package agenthooks

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDecodeKimiPreToolUse(t *testing.T) {
	payload := fixture(t, "kimi/pre_tool_use.json")
	typed, err := decodeKimi(VariantUnknown, DetectionConfig, testNow, payload)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := typed.(*ToolPreEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPreEvent", typed)
	}
	if ev.Kind != KindToolPre || ev.NativeName != "PreToolUse" || ev.Provider != ProviderKimi {
		t.Errorf("envelope wrong: %+v", ev.Event)
	}
	if ev.Session.ID != "sess-kimi-1" || ev.Session.CWD != "/work/repo" {
		t.Errorf("session wrong: %+v", ev.Session)
	}
	if ev.Tool.Name != "Bash" || ev.Tool.Canonical != ToolShell || ev.Tool.ID != "call_01K" || ev.Tool.Synthesized {
		t.Errorf("tool wrong: %+v", ev.Tool)
	}
}

func TestDecodeKimiPostToolUseFailure(t *testing.T) {
	payload := fixture(t, "kimi/post_tool_use_failure.json")
	typed, err := decodeKimi(VariantUnknown, DetectionConfig, testNow, payload)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := typed.(*ToolPostEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPostEvent", typed)
	}
	if ev.Kind != KindToolError || !ev.Failed || ev.Error != "exit status 1" {
		t.Errorf("failure fields wrong: kind=%s failed=%v error=%q", ev.Kind, ev.Failed, ev.Error)
	}
	if ev.Tool.ID != "call_03K" {
		t.Errorf("tool_call_id not mapped: %+v", ev.Tool)
	}
}

func TestDecodeKimiStopAndNotification(t *testing.T) {
	typed, err := decodeKimi(VariantUnknown, DetectionConfig, testNow, fixture(t, "kimi/stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := typed.(*StopEvent)
	if !ok || !st.PreviouslyContinued || st.LoopCount != 1 {
		t.Errorf("stop_hook_active not surfaced: %#v", typed)
	}

	typed, err = decodeKimi(VariantUnknown, DetectionConfig, testNow, fixture(t, "kimi/notification.json"))
	if err != nil {
		t.Fatal(err)
	}
	n, ok := typed.(*NotificationEvent)
	if !ok || n.Message != "Task done" {
		t.Errorf("notification body not surfaced: %#v", typed)
	}
}

func TestDecodeKimiInterruptIsOther(t *testing.T) {
	typed, err := decodeKimi(VariantUnknown, DetectionConfig, testNow, fixture(t, "kimi/interrupt.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := eventOf(typed)
	if ev.Kind != KindOther || ev.NativeName != "Interrupt" {
		t.Errorf("Interrupt should be KindOther with native name intact: %+v", ev)
	}
}

func kimiRun(t *testing.T, r *Runner, payload []byte) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = r.Run(context.Background(),
		[]string{"agenthooks", "run", "--provider=kimi-code"},
		bytes.NewReader(payload), &out, &errb)
	return out.String(), errb.String(), code
}

func TestEncodeKimiToolPreDeny(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("dangerous command"), nil
	})
	out, _, code := kimiRun(t, r, fixture(t, "kimi/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"dangerous command"}}`
	if out != want || code != 0 {
		t.Errorf("deny wire = %q (exit %d), want %q (exit 0)", out, code, want)
	}
}

func TestEncodeKimiToolPreAllow(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Allow(), nil
	})
	out, _, code := kimiRun(t, r, fixture(t, "kimi/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`
	if out != want || code != 0 {
		t.Errorf("allow wire = %q (exit %d), want %q (exit 0)", out, code, want)
	}
}

func TestKimiAskDegrades(t *testing.T) {
	// No ask on Kimi (quirk #22): FallbackDeny hardens, FallbackNoDecision
	// yields the empty no-op.
	deny := quietRunner(WithPolicy(Policy{AskFallback: FallbackDeny}))
	deny.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		if e.Can(CapAsk) {
			t.Error("kimi tool.pre must not report CapAsk")
		}
		return AskUser("confirm?"), nil
	})
	out, _, code := kimiRun(t, deny, fixture(t, "kimi/pre_tool_use.json"))
	if !strings.Contains(out, `"permissionDecision":"deny"`) || code != 0 {
		t.Errorf("ask should degrade to deny: %q (exit %d)", out, code)
	}

	noop := quietRunner(WithPolicy(Policy{AskFallback: FallbackNoDecision}))
	noop.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return AskUser("confirm?"), nil
	})
	out, _, code = kimiRun(t, noop, fixture(t, "kimi/pre_tool_use.json"))
	if out != "" || code != 0 {
		t.Errorf("ask should degrade to empty no-op: %q (exit %d)", out, code)
	}
}

func TestEncodeKimiPromptBlockAndContext(t *testing.T) {
	block := quietRunner()
	block.OnPromptSubmitted(func(ctx context.Context, e *PromptEvent) (PromptDecision, error) {
		return BlockPrompt("policy violation"), nil
	})
	out, errb, code := kimiRun(t, block, fixture(t, "kimi/user_prompt_submit.json"))
	if code != 2 || errb != "policy violation" || out != "" {
		t.Errorf("prompt block: exit=%d stderr=%q stdout=%q, want exit 2 + stderr reason + empty stdout", code, errb, out)
	}

	withCtx := quietRunner()
	withCtx.OnPromptSubmitted(func(ctx context.Context, e *PromptEvent) (PromptDecision, error) {
		return AcceptPrompt().WithContext("today is a holiday"), nil
	})
	out, errb, code = kimiRun(t, withCtx, fixture(t, "kimi/user_prompt_submit.json"))
	if code != 0 || out != "today is a holiday" || errb != "" {
		t.Errorf("prompt context: exit=%d stdout=%q stderr=%q, want plain-text stdout", code, out, errb)
	}
}

func TestEncodeKimiStopContinue(t *testing.T) {
	r := quietRunner()
	r.OnStop(func(ctx context.Context, e *StopEvent) (StopDecision, error) {
		return ContinueWith("run the tests before finishing"), nil
	})
	// stop.json has stop_hook_active:true → LoopCount 1, below the default
	// cap, so the continuation goes through.
	out, errb, code := kimiRun(t, r, fixture(t, "kimi/stop.json"))
	if code != 2 || errb != "run the tests before finishing" || out != "" {
		t.Errorf("stop continue: exit=%d stderr=%q stdout=%q", code, errb, out)
	}
}

func TestKimiToolPostFlagOutputDegrades(t *testing.T) {
	// PostToolUse is observation-only on Kimi: FlagOutput must degrade to the
	// empty no-op, not fake a block.
	r := quietRunner()
	r.OnToolPost(func(ctx context.Context, e *ToolPostEvent) (ToolPostDecision, error) {
		return FlagOutput("looks wrong"), nil
	})
	out, errb, code := kimiRun(t, r, fixture(t, "kimi/post_tool_use.json"))
	if out != "" || errb != "" || code != 0 {
		t.Errorf("flag-output should degrade to no-op: stdout=%q stderr=%q exit=%d", out, errb, code)
	}
}

func TestKimiFailClosed(t *testing.T) {
	r := quietRunner(WithPolicy(Policy{Fail: FailClosed}))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		panic("boom")
	})
	out, _, code := kimiRun(t, r, fixture(t, "kimi/pre_tool_use.json"))
	if !strings.Contains(out, `"permissionDecision":"deny"`) || code != 0 {
		t.Errorf("fail-closed should deny in-process (quirk #21): %q (exit %d)", out, code)
	}
}

func TestKimiProviderAliasAndShapeDetection(t *testing.T) {
	inv, err := parseArgs([]string{"agenthooks", "run", "--provider=kimi"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.provider != ProviderKimi {
		t.Errorf("alias kimi → %q, want %q", inv.provider, ProviderKimi)
	}

	if p, ok := detectFromShape(fixture(t, "kimi/pre_tool_use.json")); !ok || p != ProviderKimi {
		t.Errorf("tool_call_id shape → %q/%v, want kimi", p, ok)
	}
	if p, ok := detectFromShape(fixture(t, "kimi/interrupt.json")); !ok || p != ProviderKimi {
		t.Errorf("kimi-only event shape → %q/%v, want kimi", p, ok)
	}
	// Claude payloads must still detect as Claude.
	if p, ok := detectFromShape(fixture(t, "claude/pre_tool_use.json")); !ok || p != ProviderClaudeCode {
		t.Errorf("claude shape regressed → %q/%v", p, ok)
	}
}
