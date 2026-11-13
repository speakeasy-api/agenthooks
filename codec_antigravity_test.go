package agenthooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func runAntigravity(t *testing.T, r *Runner, event string, payload []byte) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := r.Run(t.Context(), []string{"agenthooks", "run", "--provider=antigravity", "--event=" + event}, bytes.NewReader(payload), &stdout, &stderr)
	return strings.TrimSpace(stdout.String()), code
}

func TestDecodeAntigravityPreToolUse(t *testing.T) {
	typed, err := decodeAntigravity("PreToolUse", VariantUnknown, DetectionConfig, testNow, fixture(t, "antigravity/pre_tool_use.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := typed.(*ToolPreEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPreEvent", typed)
	}
	if ev.Provider != ProviderAntigravity || ev.Kind != KindToolPre || ev.NativeName != "PreToolUse" {
		t.Errorf("envelope wrong: %+v", ev.Event)
	}
	if ev.Session.ID != "5e4f131c-7a0b-4c5e-9d1a-2b3c4d5e6f70" || ev.Session.CWD != "/home/dev/project" || ev.Session.Model != "gemini-3-pro" {
		t.Errorf("session wrong: %+v", ev.Session)
	}
	if ev.Tool.Name != "run_command" || ev.Tool.Canonical != ToolShell || ev.Tool.MCP != nil {
		t.Errorf("tool wrong: %+v", ev.Tool)
	}
}

func TestDecodeAntigravityMCPToolCall(t *testing.T) {
	typed, err := decodeAntigravity("PreToolUse", VariantUnknown, DetectionConfig, testNow, fixture(t, "antigravity/pre_tool_use_mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	if ev.Tool.Canonical != ToolMCP || ev.Tool.MCP == nil || ev.Tool.MCP.Server != "linear" || ev.Tool.MCP.Tool != "list_issues" {
		t.Errorf("MCP call not decoded: %+v", ev.Tool.MCP)
	}
}

func TestDecodeAntigravityPostToolUse(t *testing.T) {
	typed, err := decodeAntigravity("PostToolUse", VariantUnknown, DetectionConfig, testNow, fixture(t, "antigravity/post_tool_use.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := typed.(*ToolPostEvent)
	if !ok || !ev.Failed || ev.Error != "exit status 1" {
		t.Fatalf("failure not surfaced: %#v", typed)
	}

	// Non-tool steps fire PostToolUse with a null toolCall.
	typed, err = decodeAntigravity("PostToolUse", VariantUnknown, DetectionConfig, testNow, []byte(`{"conversationId":"c","workspacePaths":["/w"],"toolCall":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := typed.(*Event); !ok {
		t.Errorf("null toolCall decoded as %T, want *Event", typed)
	}
}

func TestDecodeAntigravityInfersEventWithoutFlag(t *testing.T) {
	typed, err := decodeAntigravity("", VariantUnknown, DetectionConfig, testNow, fixture(t, "antigravity/stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := typed.(*StopEvent); !ok {
		t.Errorf("stop decoded as %T", typed)
	}
	// Tool steps are ambiguous without --event and must never resolve to a
	// gate.
	typed, _ = decodeAntigravity("", VariantUnknown, DetectionConfig, testNow, fixture(t, "antigravity/pre_tool_use.json"))
	if _, ok := typed.(*ToolPostEvent); !ok {
		t.Errorf("flagless tool step decoded as %T, want *ToolPostEvent", typed)
	}
}

func TestDetectAntigravityFromShape(t *testing.T) {
	p, ok := detectFromShape(fixture(t, "antigravity/session_start.json"))
	if !ok || p != ProviderAntigravity {
		t.Errorf("detected %q, want antigravity", p)
	}
}

func TestAntigravityDecisions(t *testing.T) {
	isolateHome(t)
	r := quietRunner(WithDedupDir(t.TempDir()))
	var decision ToolPreDecision
	r.OnToolPre(func(context.Context, *ToolPreEvent) (ToolPreDecision, error) { return decision, nil })
	r.OnStop(func(context.Context, *StopEvent) (StopDecision, error) { return ContinueWith("run the tests"), nil })
	payload := fixture(t, "antigravity/pre_tool_use.json")

	for _, tc := range []struct {
		name     string
		decision ToolPreDecision
		want     string
	}{
		// {} denies on Antigravity, so no opinion must be empty stdout.
		{"no-op", NoDecision(), ``},
		{"allow", Allow(), `{"decision":"allow"}`},
		{"deny", Deny("no shell"), `{"decision":"deny","reason":"no shell"}`},
		{"ask", AskUser("confirm"), `{"decision":"ask","reason":"confirm"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision = tc.decision
			got, code := runAntigravity(t, r, "PreToolUse", payload)
			if code != 0 {
				t.Fatalf("exit code %d", code)
			}
			if got != tc.want {
				t.Errorf("stdout = %q, want %q", got, tc.want)
			}
		})
	}

	got, _ := runAntigravity(t, r, "Stop", fixture(t, "antigravity/stop.json"))
	if got != `{"decision":"continue","reason":"run the tests"}` {
		t.Errorf("stop stdout = %q", got)
	}
}

func TestAntigravityMCPInventoryAndResolution(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(home, ".gemini", "config", "mcp_config.json"),
		`{"mcpServers":{"linear":{"serverUrl":"https://mcp.linear.app/mcp","headers":{"Authorization":"Bearer x"}}}}`)
	writeConfig(t, filepath.Join(cwd, ".agents", "mcp_config.json"),
		`{"mcpServers":{"local":{"command":"node","args":["server.js"]}}}`)

	var inventory []MCPServer
	var resolved *MCPCall
	r := mcpTestRunner(t)
	r.OnMCPInventory(func(_ context.Context, ev *MCPInventoryEvent) error {
		inventory = ev.Servers
		return nil
	})
	r.OnToolPre(func(_ context.Context, ev *ToolPreEvent) (ToolPreDecision, error) {
		resolved = ev.Tool.MCP
		return NoDecision(), nil
	})

	start := fmt.Sprintf(`{"conversationId":"conv-1","workspacePaths":[%q]}`, cwd)
	if _, code := runAntigravity(t, r, "SessionStart", []byte(start)); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	got, _ := json.Marshal(inventory)
	want := `[{"Name":"local","URL":"","Command":"node server.js"},{"Name":"linear","URL":"https://mcp.linear.app/mcp","Command":""}]`
	if string(got) != want {
		t.Errorf("inventory = %s\nwant %s", got, want)
	}

	call := fmt.Sprintf(`{"conversationId":"conv-1","workspacePaths":[%q],"toolCall":{"name":"call_mcp_tool","args":{"ServerName":"linear","ToolName":"list_issues"}}}`, cwd)
	runAntigravity(t, r, "PreToolUse", []byte(call))
	if resolved == nil || resolved.URL != "https://mcp.linear.app/mcp" || !resolved.FromConfig {
		t.Errorf("MCP server not resolved from mcp_config.json: %+v", resolved)
	}
}

func TestParseArgsProviderAliases(t *testing.T) {
	for flag, want := range map[string]Provider{"agy": ProviderAntigravity, "copilot": ProviderCopilotCLI, "kimi": ProviderKimi} {
		inv, err := parseArgs([]string{"agenthooks", "run", "--provider=" + flag, "--event=Stop"})
		if err != nil {
			t.Fatal(err)
		}
		if inv.provider != want || inv.event != "Stop" {
			t.Errorf("--provider=%s parsed as %q event %q", flag, inv.provider, inv.event)
		}
	}
}
