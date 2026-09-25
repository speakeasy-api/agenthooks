package agenthooks

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeMoltisToolEvents(t *testing.T) {
	typed, err := decodeMoltis(VariantUnknown, DetectionConfig, testNow, fixture(t, "moltis/before_tool_call.json"))
	if err != nil {
		t.Fatal(err)
	}
	pre, ok := typed.(*ToolPreEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPreEvent", typed)
	}
	if pre.Provider != ProviderMoltis || pre.Kind != KindToolPre || pre.Session.ID != "chat:main" {
		t.Errorf("envelope wrong: %+v", pre.Event)
	}
	if pre.Tool.Name != "exec" || pre.Tool.Canonical != ToolShell || pre.Tool.ID == "" || !pre.Tool.Synthesized {
		t.Errorf("tool wrong: %+v", pre.Tool)
	}
	if pre.Session.TurnID != "" {
		t.Errorf("turn id = %q", pre.Session.TurnID)
	}
	if string(pre.Tool.Input) != "{\n    \"command\": \"git status --short\"\n  }" {
		t.Errorf("input not preserved: %s", pre.Tool.Input)
	}

	typed, err = decodeMoltis(VariantUnknown, DetectionConfig, testNow, fixture(t, "moltis/after_tool_call.json"))
	if err != nil {
		t.Fatal(err)
	}
	post, ok := typed.(*ToolPostEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPostEvent", typed)
	}
	if post.Kind != KindToolPost || post.Failed || !bytes.Contains(post.Output, []byte(`"exit_code": 0`)) {
		t.Errorf("successful post wrong: %+v output=%s", post, post.Output)
	}
	if post.Tool.ID == "" || !post.Tool.Synthesized {
		t.Errorf("legacy Moltis post identity must be synthesized: %+v", post.Tool)
	}
	if post.Tool.RawInput != nil || string(post.Tool.Input) != "{}" {
		t.Errorf("legacy Moltis post must preserve missing arguments; raw=%s input=%s", post.Tool.RawInput, post.Tool.Input)
	}

	typed, err = decodeMoltis(VariantUnknown, DetectionConfig, testNow, fixture(t, "moltis/after_tool_call_failure.json"))
	if err != nil {
		t.Fatal(err)
	}
	failed, ok := typed.(*ToolPostEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPostEvent", typed)
	}
	if failed.Kind != KindToolError || !failed.Failed || failed.Error != "permission denied" {
		t.Errorf("failed post wrong: %+v", failed)
	}
}

func TestDecodeMoltisLegacyPostKeepsMissingArgumentsDistinct(t *testing.T) {
	typed, err := decodeMoltis(VariantUnknown, DetectionConfig, testNow, []byte(
		`{"event":"AfterToolCall","session_key":"chat:main","tool_name":"exec","success":true,"result":{"exit_code":0}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	post := typed.(*ToolPostEvent)
	if post.Tool.RawInput != nil || string(post.Tool.Input) != "{}" {
		t.Errorf("legacy missing arguments must remain distinguishable; raw=%s input=%s", post.Tool.RawInput, post.Tool.Input)
	}
}

func TestDecodeMoltisUsesNativeStableToolCallID(t *testing.T) {
	for _, payload := range []string{
		`{"event":"BeforeToolCall","session_key":"chat:main","tool_call_id":"call-shared","tool_name":"exec","arguments":{"command":"pwd"}}`,
		`{"event":"AfterToolCall","session_key":"chat:main","tool_call_id":"call-shared","tool_name":"exec","success":true,"result":{"exit_code":0}}`,
	} {
		typed, err := decodeMoltis(VariantUnknown, DetectionConfig, testNow, []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		tool := toolOf(typed)
		if tool == nil || tool.ID != "call-shared" || tool.Synthesized {
			t.Fatalf("tool identity = %+v", tool)
		}
	}
}

func TestDecodeMoltisLifecycleAndExtensions(t *testing.T) {
	tests := []struct {
		fixture string
		kind    EventKind
	}{
		{"session_start.json", KindSessionStart},
		{"session_end.json", KindSessionEnd},
		{"message_received.json", KindPromptSubmitted},
		{"before_llm_call.json", KindModelRequest},
		{"before_compaction.json", KindCompactPre},
		{"agent_end.json", KindStop},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			raw := fixture(t, "moltis/"+tc.fixture)
			typed, err := decodeMoltis(VariantUnknown, DetectionConfig, testNow, raw)
			if err != nil {
				t.Fatal(err)
			}
			base := eventOf(typed)
			if base == nil || base.Kind != tc.kind || base.Provider != ProviderMoltis || string(base.Raw) != string(raw) {
				t.Fatalf("decoded envelope wrong: %#v", base)
			}
			if base.Session.ID != "chat:main" && tc.fixture != "message_received.json" {
				t.Errorf("session = %q", base.Session.ID)
			}
		})
	}

	prompt, err := decodeMoltis(VariantUnknown, DetectionConfig, testNow, fixture(t, "moltis/message_received.json"))
	if err != nil {
		t.Fatal(err)
	}
	pe := prompt.(*PromptEvent)
	if pe.Prompt != "please inspect the repository" || string(pe.RawField("future_field")) != `"preserved"` {
		t.Errorf("prompt/raw fidelity wrong: %+v", pe)
	}
	if pe.Session.TurnID != "turn-shared-1" {
		t.Errorf("prompt turn id = %q", pe.Session.TurnID)
	}

	stop, err := decodeMoltis(VariantUnknown, DetectionConfig, testNow, fixture(t, "moltis/agent_end.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := stop.(*StopEvent).FinalMessage; got != "finished" {
		t.Errorf("final message = %q", got)
	}
	if got := stop.(*StopEvent).Session.TurnID; got != "turn-shared-1" {
		t.Errorf("stop turn id = %q", got)
	}
}

func TestMoltisRunEncodesBlockModifyAndContext(t *testing.T) {
	t.Run("block tool", func(t *testing.T) {
		r := quietRunner()
		r.OnToolPre(func(context.Context, *ToolPreEvent) (ToolPreDecision, error) {
			return Deny("blocked by portable policy"), nil
		})
		var stdout, stderr bytes.Buffer
		code := r.Run(context.Background(), []string{"agenthooks", "run", "--provider=moltis"},
			bytes.NewReader(fixture(t, "moltis/before_tool_call.json")), &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || stderr.String() != "blocked by portable policy" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("rewrite tool input", func(t *testing.T) {
		r := quietRunner()
		r.OnToolPre(func(context.Context, *ToolPreEvent) (ToolPreDecision, error) {
			return NoDecision().WithUpdatedInput(map[string]any{"command": "git status --branch"}), nil
		})
		var stdout, stderr bytes.Buffer
		code := r.Run(context.Background(), []string{"agenthooks", "run", "--provider=moltis"},
			bytes.NewReader(fixture(t, "moltis/before_tool_call.json")), &stdout, &stderr)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		var reply struct {
			Action string         `json:"action"`
			Data   map[string]any `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Action != "modify" || reply.Data["command"] != "git status --branch" {
			t.Errorf("reply = %+v", reply)
		}
	})

	t.Run("append prompt context", func(t *testing.T) {
		r := quietRunner()
		r.OnPromptSubmitted(func(context.Context, *PromptEvent) (PromptDecision, error) {
			return AcceptPrompt().WithContext("portable context marker"), nil
		})
		var stdout, stderr bytes.Buffer
		code := r.Run(context.Background(), []string{"agenthooks", "run", "--provider=moltis"},
			bytes.NewReader(fixture(t, "moltis/message_received.json")), &stdout, &stderr)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		var reply struct {
			Action string `json:"action"`
			Data   struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Action != "modify" || !strings.HasSuffix(reply.Data.Content, "\n\nportable context marker") {
			t.Errorf("reply = %+v", reply)
		}
	})

	t.Run("append context after middleware prompt rewrite", func(t *testing.T) {
		r := quietRunner()
		r.Use(func(ctx context.Context, typed any, next Next) (Decision, error) {
			prompt, ok := typed.(*PromptEvent)
			if !ok {
				t.Fatalf("middleware received %T, want *PromptEvent", typed)
			}
			prompt.Prompt = "rewritten by middleware"
			return next(ctx, typed)
		})
		r.OnPromptSubmitted(func(context.Context, *PromptEvent) (PromptDecision, error) {
			return AcceptPrompt().WithContext("portable context marker"), nil
		})
		var stdout, stderr bytes.Buffer
		code := r.Run(context.Background(), []string{"agenthooks", "run", "--provider=moltis"},
			bytes.NewReader(fixture(t, "moltis/message_received.json")), &stdout, &stderr)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		var reply struct {
			Data struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Data.Content != "rewritten by middleware\n\nportable context marker" {
			t.Errorf("middleware prompt rewrite was lost: %q", reply.Data.Content)
		}
	})

	t.Run("neutral output is empty", func(t *testing.T) {
		r := quietRunner()
		var stdout, stderr bytes.Buffer
		code := r.Run(context.Background(), []string{"agenthooks", "run", "--provider=moltis"},
			bytes.NewReader(fixture(t, "moltis/session_start.json")), &stdout, &stderr)
		if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})
}

func TestMoltisCapabilitiesAndShapeDetection(t *testing.T) {
	if !Capabilities(ProviderMoltis, VariantUnknown, KindToolPre).Has(CapDeny) ||
		!Capabilities(ProviderMoltis, VariantUnknown, KindToolPre).Has(CapUpdateInput) ||
		Capabilities(ProviderMoltis, VariantUnknown, KindToolPre).Has(CapAsk) {
		t.Errorf("tool.pre capabilities wrong: %v", Capabilities(ProviderMoltis, VariantUnknown, KindToolPre))
	}
	if !Capabilities(ProviderMoltis, VariantUnknown, KindPromptSubmitted).Has(CapAddContext) {
		t.Error("MessageReceived context projection must be explicit")
	}
	p, ok := detectFromShape(fixture(t, "moltis/before_tool_call.json"))
	if !ok || p != ProviderMoltis {
		t.Errorf("detected %q ok=%v", p, ok)
	}
	p, ok = detectFromShape([]byte(`{"event":"GatewayStop"}`))
	if !ok || p != ProviderMoltis {
		t.Errorf("GatewayStop detected %q ok=%v", p, ok)
	}
}
