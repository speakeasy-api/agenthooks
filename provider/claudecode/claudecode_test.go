package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

func TestPreToolUseViewWithExtraCapture(t *testing.T) {
	raw := `{"session_id":"s1","cwd":"/w","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"t1","brand_new_field":"surprise"}`
	e := &agenthooks.Event{
		Provider:   agenthooks.ProviderClaudeCode,
		NativeName: "PreToolUse",
		Kind:       agenthooks.KindToolPre,
		Raw:        json.RawMessage(raw),
	}
	v, ok := PreToolUse(e)
	if !ok {
		t.Fatal("view should decode")
	}
	if v.ToolName != "Bash" || v.SessionID != "s1" {
		t.Errorf("fields wrong: %+v", v)
	}
	if string(v.Extra["brand_new_field"]) != `"surprise"` {
		t.Errorf("unknown field must land in Extra, got %v", v.Extra)
	}

	if _, ok := PostToolUse(e); ok {
		t.Error("wrong native event must not decode")
	}
	e.Provider = agenthooks.ProviderCodex
	if _, ok := PreToolUse(e); ok {
		t.Error("wrong provider must not decode")
	}
}
