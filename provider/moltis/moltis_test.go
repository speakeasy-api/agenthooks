package moltis

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

func rawEvent(t *testing.T, name, native string) *agenthooks.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "agenthookstest", "fixtures", "moltis", name))
	if err != nil {
		t.Fatal(err)
	}
	return &agenthooks.Event{Provider: agenthooks.ProviderMoltis, NativeName: native, Raw: raw}
}

func TestTypedViews(t *testing.T) {
	tests := []struct {
		fixture string
		native  string
		check   func(*agenthooks.Event) bool
	}{
		{"before_agent_start.json", "BeforeAgentStart", func(e *agenthooks.Event) bool {
			v, ok := BeforeAgentStart(e)
			return ok && v.SessionKey == "chat:main" && v.Model == "lmstudio::local-model"
		}},
		{"agent_end.json", "AgentEnd", func(e *agenthooks.Event) bool {
			v, ok := AgentEnd(e)
			return ok && v.TurnID == "turn-shared-1" && v.Text == "finished" && v.Iterations == 3 && v.ToolCalls == 2
		}},
		{"before_llm_call.json", "BeforeLLMCall", func(e *agenthooks.Event) bool {
			v, ok := BeforeLLMCall(e)
			return ok && v.Provider == "lmstudio" && v.ToolCount == 3 && len(v.Messages) > 0
		}},
		{"after_llm_call.json", "AfterLLMCall", func(e *agenthooks.Event) bool {
			v, ok := AfterLLMCall(e)
			return ok && v.Text != nil && *v.Text == "assistant response" && len(v.ToolCalls) == 1
		}},
		{"before_compaction.json", "BeforeCompaction", func(e *agenthooks.Event) bool {
			v, ok := BeforeCompaction(e)
			return ok && v.MessageCount == 42
		}},
		{"after_compaction.json", "AfterCompaction", func(e *agenthooks.Event) bool {
			v, ok := AfterCompaction(e)
			return ok && v.SummaryLen == 512
		}},
		{"message_received.json", "MessageReceived", func(e *agenthooks.Event) bool {
			v, ok := MessageReceived(e)
			return ok && v.TurnID == "turn-shared-1" && v.Content != "" && v.ChannelBinding != nil &&
				v.ChannelBinding.ChannelType == "telegram" && string(v.Extra["future_field"]) == `"preserved"`
		}},
		{"message_sending.json", "MessageSending", func(e *agenthooks.Event) bool {
			v, ok := MessageSending(e)
			return ok && v.Content == "message entering the model"
		}},
		{"message_sent.json", "MessageSent", func(e *agenthooks.Event) bool {
			v, ok := MessageSent(e)
			return ok && v.Content == "message delivered"
		}},
		{"before_tool_call.json", "BeforeToolCall", func(e *agenthooks.Event) bool {
			v, ok := BeforeToolCall(e)
			return ok && v.TurnID == "" && v.ToolCallID == "" &&
				v.ToolName == "exec" && v.Channel != nil && v.Channel.Surface == "web"
		}},
		{"after_tool_call_failure.json", "AfterToolCall", func(e *agenthooks.Event) bool {
			v, ok := AfterToolCall(e)
			return ok && v.TurnID == "turn-shared-1" && v.ToolCallID == "call-shared-1" &&
				!v.Success && len(v.Arguments) > 0 && len(v.Result) > 0
		}},
		{"tool_result_persist.json", "ToolResultPersist", func(e *agenthooks.Event) bool {
			v, ok := ToolResultPersist(e)
			return ok && v.ToolName == "exec" && v.Channel != nil && len(v.Result) > 0
		}},
		{"session_start.json", "SessionStart", func(e *agenthooks.Event) bool {
			v, ok := SessionStart(e)
			return ok && v.SessionKey == "chat:main" && v.Channel != nil
		}},
		{"session_end.json", "SessionEnd", func(e *agenthooks.Event) bool {
			v, ok := SessionEnd(e)
			return ok && v.SessionKey == "chat:main"
		}},
		{"gateway_start.json", "GatewayStart", func(e *agenthooks.Event) bool {
			v, ok := GatewayStart(e)
			return ok && v.Address == "127.0.0.1:13131"
		}},
		{"gateway_stop.json", "GatewayStop", func(e *agenthooks.Event) bool {
			_, ok := GatewayStop(e)
			return ok
		}},
		{"command.json", "Command", func(e *agenthooks.Event) bool {
			v, ok := Command(e)
			return ok && v.Action == "new" && v.SenderID != nil && *v.SenderID == "user-7"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.native, func(t *testing.T) {
			event := rawEvent(t, tc.fixture, tc.native)
			if !tc.check(event) {
				t.Fatalf("%s typed view rejected or decoded the fixture incorrectly", tc.native)
			}
		})
	}

	if _, ok := BeforeToolCall(rawEvent(t, "after_tool_call.json", "AfterToolCall")); ok {
		t.Error("BeforeToolCall must reject another native event")
	}
}

func TestChannelBindingPreservesUnknownNestedFields(t *testing.T) {
	event := &agenthooks.Event{
		Provider:   agenthooks.ProviderMoltis,
		NativeName: "SessionStart",
		Raw:        []byte(`{"event":"SessionStart","session_key":"chat:main","channel":{"surface":"web","future_nested":{"value":7}}}`),
	}
	view, ok := SessionStart(event)
	if !ok || view.Channel == nil {
		t.Fatalf("SessionStart nested channel not decoded: %+v ok=%v", view, ok)
	}
	if string(view.Channel.Extra["future_nested"]) != `{"value":7}` {
		t.Fatalf("unknown nested field not preserved: %+v", view.Channel.Extra)
	}
}

func TestToolLifecycleViewsDecodeForwardCompatibleCallID(t *testing.T) {
	tests := []struct {
		native string
		raw    string
		id     func(*agenthooks.Event) string
	}{
		{
			native: "BeforeToolCall",
			raw:    `{"event":"BeforeToolCall","session_key":"chat:main","tool_call_id":"call-shared","tool_name":"exec","arguments":{"command":"pwd"}}`,
			id: func(event *agenthooks.Event) string {
				view, _ := BeforeToolCall(event)
				return view.ToolCallID
			},
		},
		{
			native: "AfterToolCall",
			raw:    `{"event":"AfterToolCall","session_key":"chat:main","tool_call_id":"call-shared","tool_name":"exec","success":true,"result":{"exit_code":0}}`,
			id: func(event *agenthooks.Event) string {
				view, _ := AfterToolCall(event)
				return view.ToolCallID
			},
		},
		{
			native: "ToolResultPersist",
			raw:    `{"event":"ToolResultPersist","session_key":"chat:main","tool_call_id":"call-shared","tool_name":"exec","result":{"exit_code":0}}`,
			id: func(event *agenthooks.Event) string {
				view, _ := ToolResultPersist(event)
				return view.ToolCallID
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.native, func(t *testing.T) {
			event := &agenthooks.Event{
				Provider:   agenthooks.ProviderMoltis,
				NativeName: tc.native,
				Raw:        []byte(tc.raw),
			}
			if got := tc.id(event); got != "call-shared" {
				t.Fatalf("tool_call_id = %q, want call-shared", got)
			}
		})
	}
}
