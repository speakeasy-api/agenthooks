package agenthooks

import (
	"encoding/json"
	"testing"
)

func TestToolMatcherRoundTrip(t *testing.T) {
	m := ToolMatcher{
		Names:     []string{"Bash", "Write"},
		Canonical: []CanonicalTool{ToolShell},
		MCP:       []string{"github/*"},
	}
	parsed, err := ParseToolMatcher(m.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Encode() != m.Encode() {
		t.Errorf("round trip mismatch: %q vs %q", parsed.Encode(), m.Encode())
	}
}

func TestToolMatcherMatches(t *testing.T) {
	m := ToolMatcher{Canonical: []CanonicalTool{ToolShell}, MCP: []string{"github/*"}}
	shell := ToolCall{Name: "Bash", Canonical: ToolShell}
	read := ToolCall{Name: "Read", Canonical: ToolFileRead}
	gh := ToolCall{Name: "mcp__github__create_issue", Canonical: ToolMCP, MCP: &MCPCall{Server: "github", Tool: "create_issue"}}
	other := ToolCall{Name: "mcp__linear__search", Canonical: ToolMCP, MCP: &MCPCall{Server: "linear", Tool: "search"}}

	if !m.Matches(shell) || !m.Matches(gh) {
		t.Error("expected shell and github MCP to match")
	}
	if m.Matches(read) || m.Matches(other) {
		t.Error("expected read and linear MCP not to match")
	}
	if !(ToolMatcher{}).Matches(read) {
		t.Error("empty matcher must match everything")
	}
}

func TestCompileMatcher(t *testing.T) {
	m := ToolMatcher{Names: []string{"Bash"}, MCP: []string{"github/*"}}
	claude, ok := CompileMatcher(ProviderClaudeCode, m)
	if !ok || claude != "Bash|mcp__github__.*" {
		t.Errorf("claude matcher = %q ok=%v", claude, ok)
	}
	gemini, ok := CompileMatcher(ProviderGemini, m)
	if !ok || gemini != "Bash|mcp_github_.*" {
		t.Errorf("gemini matcher = %q ok=%v", gemini, ok)
	}
	if _, ok := CompileMatcher(ProviderCursor, m); ok {
		t.Error("cursor has no matcher dialect; expected ok=false")
	}
	if expr, ok := CompileMatcher(ProviderCursor, ToolMatcher{}); !ok || expr != "" {
		t.Error("empty matcher is expressible everywhere (match all)")
	}
}

func TestCapabilityDivergences(t *testing.T) {
	// Selected rows from §4.1.
	if !Capabilities(ProviderClaudeCode, "", KindToolPre).Has(CapAsk) {
		t.Error("claude tool.pre must support ask")
	}
	if Capabilities(ProviderCodex, "", KindToolPre).Has(CapAsk) {
		t.Error("codex tool.pre must not support ask (fails the hook run)")
	}
	if Capabilities(ProviderCursor, "", KindStop).Has(CapStopAgent) {
		t.Error("cursor has no stop-agent")
	}
	if !Capabilities(ProviderGemini, "", KindToolPre).Has(CapAsk) {
		t.Error("gemini tool.pre supports ask (undocumented)")
	}
	if Capabilities(ProviderOpenCode, "", KindStop).Has(CapContinueAgent) {
		t.Error("opencode session.idle cannot continue the agent")
	}
	e := &Event{Provider: ProviderCursor, Kind: KindToolPre}
	if !e.Can(CapDeny) {
		t.Error("Event.Can should reflect the matrix")
	}
	if cursorAskSupported("preToolUse") || !cursorAskSupported("beforeShellExecution") {
		t.Error("cursor ask refinement wrong")
	}
}

func TestQuirkRegistry(t *testing.T) {
	qs := Quirks()
	if len(qs) != 31 {
		t.Fatalf("expected the 31 seeded quirks, got %d", len(qs))
	}
	seen := map[int]bool{}
	for _, q := range qs {
		if q.ID == 0 || q.Behavior == "" || q.Mitigation == "" {
			t.Errorf("quirk %+v incomplete", q)
		}
		if seen[q.ID] {
			t.Errorf("duplicate quirk id %d", q.ID)
		}
		seen[q.ID] = true
	}
	if got := len(QuirksFor(ProviderCursor)); got == 0 {
		t.Error("cursor quirks missing")
	}
}

func TestLossyUpdate(t *testing.T) {
	orig := json.RawMessage(`{"command":"ls","description":"x"}`)
	if lossy, _ := lossyUpdate(orig, map[string]string{"command": "ls -la"}); !lossy {
		t.Error("dropping description must be lossy")
	}
	if lossy, _ := lossyUpdate(orig, map[string]string{"command": "ls -la", "description": "y"}); lossy {
		t.Error("keeping all keys is not lossy")
	}
}
