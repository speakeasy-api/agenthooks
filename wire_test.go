package agenthooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var wireNow = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

func wireBase(kind EventKind) Event {
	return Event{
		Provider:   ProviderClaudeCode,
		Variant:    VariantCLI,
		NativeName: "Native",
		Kind:       kind,
		Time:       wireNow,
		Session: SessionInfo{
			ID:        "sess-1",
			TurnID:    "turn-1",
			CWD:       "/work/repo",
			Model:     "opus",
			UserEmail: "dev@example.com",
		},
		Agent: &AgentInfo{ID: "agent-1", Type: "researcher"},
		Raw:   json.RawMessage(`{"hook_event_name":"Native"}`),
	}
}

func wireToolCall() ToolCall {
	return ToolCall{
		ID:        "toolu_01",
		Name:      "mcp__github__create_issue",
		Canonical: ToolMCP,
		MCP: &MCPCall{
			Server:     "github",
			Tool:       "create_issue",
			URL:        "https://mcp.example.com/sse",
			Command:    "npx -y @acme/mcp",
			FromConfig: true,
		},
		Input:    json.RawMessage(`{"title":"crash on save"}`),
		RawInput: json.RawMessage(`{"title":"crash on save"}`),
	}
}

// TestWireRoundTrip: encode -> decode must reproduce every typed event kind
// exactly (core envelope plus payload).
func TestWireRoundTrip(t *testing.T) {
	d := 123.5
	in, out := 1200, 340
	cost := 0.0123
	otherEvent := wireBase(KindOther)

	cases := []struct {
		name  string
		typed any
	}{
		{"session.start", &SessionStartEvent{Event: wireBase(KindSessionStart), Source: "startup"}},
		{"session.end", &SessionEndEvent{Event: wireBase(KindSessionEnd), Reason: "other"}},
		{"prompt.submitted", &PromptEvent{Event: wireBase(KindPromptSubmitted), Prompt: "deploy to staging"}},
		{"tool.pre", &ToolPreEvent{Event: wireBase(KindToolPre), Tool: wireToolCall()}},
		{"permission.request", &PermissionEvent{Event: wireBase(KindPermission), Tool: wireToolCall()}},
		{"tool.post", &ToolPostEvent{
			Event: wireBase(KindToolPost), Tool: wireToolCall(),
			Output: json.RawMessage(`{"ok":true}`), DurationMS: &d,
		}},
		{"tool.error", &ToolPostEvent{
			Event: wireBase(KindToolError), Tool: wireToolCall(),
			Failed: true, Error: "exit status 1",
		}},
		{"agent.stop", &StopEvent{
			Event: wireBase(KindStop), PreviouslyContinued: true, LoopCount: 2,
			FinalMessage: "done",
			Usage:        &Usage{InputTokens: &in, OutputTokens: &out, Cost: &cost, Status: "completed"},
		}},
		{"subagent.stop", &StopEvent{Event: wireBase(KindSubagentStop), LoopCount: 1}},
		{"subagent.start", &SubagentStartEvent{Event: wireBase(KindSubagentStart)}},
		{"compact.pre", &CompactEvent{Event: wireBase(KindCompactPre), Trigger: "auto", Instructions: "keep decisions"}},
		{"compact.post", &CompactEvent{Event: wireBase(KindCompactPost)}},
		{"notification", &NotificationEvent{Event: wireBase(KindNotification), Message: "Task done"}},
		{"file.edited", &FileEditedEvent{Event: wireBase(KindFileEdited), Path: "/work/repo/main.go"}},
		{"model.request", &ModelEvent{Event: wireBase(KindModelRequest)}},
		{"model.response", &ModelEvent{Event: wireBase(KindModelResponse)}},
		{"other", &otherEvent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := EncodeWire(c.typed)
			if err != nil {
				t.Fatalf("EncodeWire: %v", err)
			}
			got, err := DecodeWire(data)
			if err != nil {
				t.Fatalf("DecodeWire: %v", err)
			}
			if !reflect.DeepEqual(got, c.typed) {
				t.Errorf("round trip diverged:\n got %#v\nwant %#v", got, c.typed)
			}
		})
	}
}

// TestWireGoldens pins the wire JSON for representative events so schema
// drift (renamed fields, changed tags, version bumps) fails loudly. Golden
// files also round-trip through DecodeWire back to the expected event.
func TestWireGoldens(t *testing.T) {
	d := 123.5
	cases := []struct {
		golden string
		typed  any
	}{
		{"tool_pre.json", &ToolPreEvent{Event: wireBase(KindToolPre), Tool: wireToolCall()}},
		{"prompt.json", &PromptEvent{Event: wireBase(KindPromptSubmitted), Prompt: "deploy to staging"}},
		{"tool_post.json", &ToolPostEvent{
			Event: wireBase(KindToolPost), Tool: wireToolCall(),
			Output: json.RawMessage(`{"ok":true}`), DurationMS: &d,
		}},
		{"stop_minimal.json", &StopEvent{Event: Event{
			Provider: ProviderKimi,
			Kind:     KindStop,
			Session:  SessionInfo{ID: "sess-min"},
		}}},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			path := filepath.Join("testdata", "wire", c.golden)
			data, err := EncodeWire(c.typed)
			if err != nil {
				t.Fatalf("EncodeWire: %v", err)
			}
			if os.Getenv("UPDATE_GOLDEN") != "" {
				if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != strings.TrimSpace(string(want)) {
				t.Errorf("wire JSON drifted from golden %s:\n got %s\nwant %s", c.golden, data, want)
			}
			got, err := DecodeWire(want)
			if err != nil {
				t.Fatalf("DecodeWire(golden): %v", err)
			}
			if !reflect.DeepEqual(got, c.typed) {
				t.Errorf("golden decode diverged:\n got %#v\nwant %#v", got, c.typed)
			}
		})
	}
}

// TestWireVersionCheck: unknown schema versions are rejected outright — the
// version string is the compatibility contract.
func TestWireVersionCheck(t *testing.T) {
	if _, err := DecodeWire([]byte(`{"v":"agenthooks.event.v2","kind":"tool.pre","event":{}}`)); err == nil ||
		!strings.Contains(err.Error(), "unsupported wire schema version") {
		t.Errorf("v2 must be rejected, got %v", err)
	}
	if _, err := DecodeWire([]byte(`{"kind":"tool.pre","event":{}}`)); err == nil {
		t.Error("missing version must be rejected")
	}
}

// TestWireMalformed: garbage and mistyped payloads surface as errors, never
// as silently-zeroed events.
func TestWireMalformed(t *testing.T) {
	if _, err := DecodeWire([]byte(`not-json`)); err == nil {
		t.Error("malformed JSON must error")
	}
	bad := `{"v":"agenthooks.event.v1","kind":"tool.pre","event":{},"payload":[1,2]}`
	if _, err := DecodeWire([]byte(bad)); err == nil {
		t.Error("mistyped payload must error")
	}
}

// TestWireForwardCompat: unknown fields anywhere are ignored, and a missing
// payload decodes to zero-valued payload fields.
func TestWireForwardCompat(t *testing.T) {
	data := `{
		"v": "agenthooks.event.v1",
		"kind": "prompt.submitted",
		"future_top_level": true,
		"event": {"provider": "cursor", "session": {"id": "s1", "future_field": 7}},
		"payload": {"prompt": "hi", "attachments": ["future"]}
	}`
	got, err := DecodeWire([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	pe, ok := got.(*PromptEvent)
	if !ok || pe.Prompt != "hi" || pe.Session.ID != "s1" || pe.Provider != ProviderCursor {
		t.Errorf("unknown fields must be tolerated: %#v", got)
	}

	noPayload := `{"v":"agenthooks.event.v1","kind":"tool.pre","event":{"provider":"codex"}}`
	got, err = DecodeWire([]byte(noPayload))
	if err != nil {
		t.Fatal(err)
	}
	pre, ok := got.(*ToolPreEvent)
	if !ok || pre.Tool.Name != "" {
		t.Errorf("missing payload must zero-value the typed fields: %#v", got)
	}
}

// TestWireUnknownKind: kind tags this build cannot type decode to the bare
// envelope, so relays never drop events across version skew.
func TestWireUnknownKind(t *testing.T) {
	data := `{"v":"agenthooks.event.v1","kind":"hologram.rendered","event":{"provider":"cursor","native_name":"holo"}}`
	got, err := DecodeWire([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := got.(*Event)
	if !ok || ev.Kind != EventKind("hologram.rendered") || ev.NativeName != "holo" {
		t.Errorf("unknown kind must decode to *Event: %#v", got)
	}
}

// TestWireExtNeverSerialized: Ext is embedder-local; it must not cross the
// wire in either direction.
func TestWireExtNeverSerialized(t *testing.T) {
	ev := &PromptEvent{Event: wireBase(KindPromptSubmitted), Prompt: "hi"}
	ev.Ext = map[string]any{"secret_marker_xyzzy": true}
	data, err := EncodeWire(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "xyzzy") || strings.Contains(string(data), "Ext") {
		t.Errorf("Ext leaked into the wire: %s", data)
	}
	got, err := DecodeWire(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.(*PromptEvent).Ext != nil {
		t.Error("decoded events must have nil Ext")
	}
}

// TestWireEncodeValidation: non-events and Kind/type mismatches are encode
// errors, so a malformed producer fails at the boundary instead of shipping
// undecodable frames.
func TestWireEncodeValidation(t *testing.T) {
	if _, err := EncodeWire(42); err == nil {
		t.Error("non-events must be rejected")
	}
	if _, err := EncodeWire(&ToolPreEvent{Event: wireBase(KindToolPost)}); err == nil {
		t.Error("kind/type mismatch must be rejected")
	}
	mistagged := wireBase(KindToolPre)
	if _, err := EncodeWire(&mistagged); err == nil {
		t.Error("bare *Event with a typed kind must be rejected")
	}
}
