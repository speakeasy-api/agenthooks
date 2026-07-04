package agenthooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseMCPName(t *testing.T) {
	cases := []struct {
		name   string
		server string
		tool   string
		nilOut bool
	}{
		{"mcp__github__create_issue", "github", "create_issue", false},
		{"mcp__srv__ns__tool", "srv", "ns__tool", false},
		{"MCP:create_issue", "", "create_issue", false},
		{"mcp_linear_search", "linear", "search", false},
		{"Bash", "", "", true},
		{"mcpish_tool", "", "", true},
	}
	for _, c := range cases {
		got := ParseMCPName(c.name)
		if c.nilOut {
			if got != nil {
				t.Errorf("ParseMCPName(%q) = %+v, want nil", c.name, got)
			}
			continue
		}
		if got == nil || got.Server != c.server || got.Tool != c.tool {
			t.Errorf("ParseMCPName(%q) = %+v, want server=%q tool=%q", c.name, got, c.server, c.tool)
		}
	}
}

func TestCanonicalToolFor(t *testing.T) {
	cases := map[string]CanonicalTool{
		"Bash":              ToolShell,
		"run_shell_command": ToolShell,
		"Read":              ToolFileRead,
		"Write":             ToolFileWrite,
		"apply_patch":       ToolFileEdit,
		"Grep":              ToolSearch,
		"WebFetch":          ToolFetch,
		"Task":              ToolTask,
		"mcp__gh__issues":   ToolMCP,
		"MCP:issues":        ToolMCP,
		"SomethingCustom":   ToolOther,
	}
	for name, want := range cases {
		if got := CanonicalToolFor(name); got != want {
			t.Errorf("CanonicalToolFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSynthesizeToolID(t *testing.T) {
	a := SynthesizeToolID("s1", "t1", "Bash", []byte(`{"command":"ls"}`))
	b := SynthesizeToolID("s1", "t1", "Bash", []byte(`{"command":"ls"}`))
	c := SynthesizeToolID("s1", "t1", "Bash", []byte(`{"command":"rm"}`))
	if a != b {
		t.Errorf("synthesized ids not stable: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different inputs produced the same id %q", a)
	}
	if !strings.HasPrefix(a, "hook_synth_") || len(a) != len("hook_synth_")+16 {
		t.Errorf("id %q does not match hook_synth_<16 hex>", a)
	}
}

func TestRawField(t *testing.T) {
	e := &Event{Raw: json.RawMessage(`{"a":{"b":[{"c":42}]},"stop_hook_active":true}`)}
	if got := string(e.RawField("a.b.0.c")); got != "42" {
		t.Errorf("RawField(a.b.0.c) = %q, want 42", got)
	}
	if got := string(e.RawField("stop_hook_active")); got != "true" {
		t.Errorf("RawField(stop_hook_active) = %q, want true", got)
	}
	if got := e.RawField("missing.path"); got != nil {
		t.Errorf("RawField(missing.path) = %q, want nil", got)
	}
}

func TestNormalizeInput(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{``, `{}`},
		{`null`, `{}`},
		{`"{\"title\":\"x\"}"`, `{"title":"x"}`},       // cursor stringified form (quirk #5)
		{`"plain string"`, `{"value":"plain string"}`}, // non-object wrapped, not hidden
		{`[1,2]`, `{"value":[1,2]}`},
	}
	for _, c := range cases {
		if got := string(normalizeInput(json.RawMessage(c.in))); got != c.want {
			t.Errorf("normalizeInput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMakeToolCallSynthesizesID(t *testing.T) {
	s := SessionInfo{ID: "sess", TurnID: "turn"}
	tc := makeToolCall(s, "MCP:create_issue", "", json.RawMessage(`{"title":"x"}`), nil)
	if !tc.Synthesized || !strings.HasPrefix(tc.ID, "hook_synth_") {
		t.Errorf("expected synthesized id, got %+v", tc)
	}
	if tc.Canonical != ToolMCP || tc.MCP == nil || tc.MCP.Tool != "create_issue" {
		t.Errorf("MCP decode failed: %+v", tc)
	}

	tc2 := makeToolCall(s, "Bash", "toolu_1", json.RawMessage(`{"command":"ls"}`), nil)
	if tc2.Synthesized || tc2.ID != "toolu_1" {
		t.Errorf("native id should win: %+v", tc2)
	}
}
