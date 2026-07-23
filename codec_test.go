package agenthooks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("agenthookstest", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var testNow = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

func TestDecodeClaudePreToolUse(t *testing.T) {
	payload := fixture(t, "claude/pre_tool_use.json")
	typed, err := decodeClaude(VariantUnknown, DetectionConfig, testNow, payload)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := typed.(*ToolPreEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPreEvent", typed)
	}
	if ev.Kind != KindToolPre || ev.NativeName != "PreToolUse" || ev.Provider != ProviderClaudeCode {
		t.Errorf("envelope wrong: %+v", ev.Event)
	}
	if ev.Session.ID != "sess-claude-1" || ev.Session.CWD != "/work/repo" || ev.Session.PermissionMode != "default" {
		t.Errorf("session wrong: %+v", ev.Session)
	}
	if ev.Tool.Name != "Bash" || ev.Tool.Canonical != ToolShell || ev.Tool.ID != "toolu_01ABC" || ev.Tool.Synthesized {
		t.Errorf("tool wrong: %+v", ev.Tool)
	}
	if string(ev.Raw) != string(payload) {
		t.Error("Raw must be byte-identical to the payload")
	}
}

func TestDecodeClaudeMCP(t *testing.T) {
	typed, err := decodeClaude(VariantUnknown, DetectionConfig, testNow, fixture(t, "claude/pre_tool_use_mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	if ev.Tool.Canonical != ToolMCP || ev.Tool.MCP == nil || ev.Tool.MCP.Server != "github" || ev.Tool.MCP.Tool != "create_issue" {
		t.Errorf("MCP decode wrong: %+v", ev.Tool)
	}
}

func TestDecodeClaudeUnmappedEvent(t *testing.T) {
	typed, err := decodeClaude(VariantUnknown, DetectionConfig, testNow, fixture(t, "claude/setup.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := typed.(*Event)
	if !ok || ev.Kind != KindOther || ev.NativeName != "Setup" {
		t.Fatalf("unmapped event must be KindOther with the native name: %T %+v", typed, typed)
	}
	if string(ev.RawField("worktree_path")) != `"/work/repo/.worktrees/wt1"` {
		t.Error("unmapped payload fields must stay reachable via RawField")
	}
}

func TestDecodeCodex(t *testing.T) {
	typed, err := decodeCodex(VariantUnknown, DetectionConfig, testNow, fixture(t, "codex/pre_tool_use.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	if ev.Provider != ProviderCodex || ev.Session.TurnID != "turn-42" || ev.Tool.Canonical != ToolShell {
		t.Errorf("codex decode wrong: %+v", ev)
	}
}

func TestDecodeCursorShell(t *testing.T) {
	typed, err := decodeCursor(VariantUnknown, DetectionConfig, testNow, fixture(t, "cursor/before_shell_execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	if ev.Session.ID != "conv-cursor-1" || ev.Session.TurnID != "gen-5" || ev.Session.UserEmail != "dev@example.com" {
		t.Errorf("session wrong: %+v", ev.Session)
	}
	if len(ev.Session.WorkspaceRoots) != 2 {
		t.Errorf("workspace roots wrong: %v", ev.Session.WorkspaceRoots)
	}
	if ev.Tool.Canonical != ToolShell || string(ev.Tool.Input) != `{"command":"git push origin main"}` {
		t.Errorf("shell tool wrong: %+v", ev.Tool)
	}
	if !ev.Tool.Synthesized {
		t.Error("cursor shell events carry no tool_use_id; id must be synthesized")
	}
}

func TestDecodeCursorMCPStringInput(t *testing.T) {
	typed, err := decodeCursor(VariantUnknown, DetectionConfig, testNow, fixture(t, "cursor/before_mcp_execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	// quirk #5: stringified tool_input un-stringified into an object...
	var input map[string]any
	if err := json.Unmarshal(ev.Tool.Input, &input); err != nil || input["title"] != "crash on save" {
		t.Errorf("stringified input not normalized: %s", ev.Tool.Input)
	}
	// ...while RawInput keeps the original string form.
	if ev.Tool.RawInput[0] != '"' {
		t.Errorf("RawInput must keep the provider's string form, got %s", ev.Tool.RawInput)
	}
	// quirk #3: MCP: prefix stripped, URL attached, id synthesized.
	if ev.Tool.MCP == nil || ev.Tool.MCP.Tool != "create_issue" || ev.Tool.MCP.URL != "https://mcp.example.com/sse" {
		t.Errorf("MCP call wrong: %+v", ev.Tool.MCP)
	}
	if !ev.Tool.Synthesized {
		t.Error("MCP events omit tool_use_id; id must be synthesized")
	}
}

func TestDecodeCursorStopLoopCount(t *testing.T) {
	typed, err := decodeCursor(VariantUnknown, DetectionConfig, testNow, fixture(t, "cursor/stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*StopEvent)
	if !ev.PreviouslyContinued || ev.LoopCount != 2 {
		t.Errorf("loop guard wrong: %+v", ev)
	}
}

func TestDecodeGeminiTimestampAndError(t *testing.T) {
	typed, err := decodeGemini(VariantUnknown, DetectionConfig, testNow, fixture(t, "gemini/before_tool.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	if !ev.Time.Equal(time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("gemini supplies its own timestamp; got %v", ev.Time)
	}
	if ev.Tool.Canonical != ToolShell {
		t.Errorf("run_shell_command should classify as shell: %+v", ev.Tool)
	}

	typed, err = decodeGemini(VariantUnknown, DetectionConfig, testNow, fixture(t, "gemini/after_tool_error.json"))
	if err != nil {
		t.Fatal(err)
	}
	post := typed.(*ToolPostEvent)
	if !post.Failed || post.Error != "exit status 1" {
		t.Errorf("gemini tool_response.error must surface as failure: %+v", post)
	}
}

func TestDecodeOpenCodeFrames(t *testing.T) {
	typed, err := decodeOpenCodeLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "opencode/tool_execute_before.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPreEvent)
	if ev.Session.ID != "oc-sess-1" || ev.Tool.Name != "bash" || ev.Tool.ID != "call-7" || ev.Tool.Canonical != ToolShell {
		t.Errorf("opencode tool.pre wrong: %+v", ev.Tool)
	}
	var input map[string]any
	if err := json.Unmarshal(ev.Tool.Input, &input); err != nil || input["command"] != "make test" {
		t.Errorf("args not projected into Input: %s", ev.Tool.Input)
	}

	typed, err = decodeOpenCodeLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "opencode/chat_message.json"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := typed.(*PromptEvent)
	if prompt.Prompt != "deploy to staging" {
		t.Errorf("prompt text wrong: %q", prompt.Prompt)
	}

	typed, err = decodeOpenCodeLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "opencode/session_idle.json"))
	if err != nil {
		t.Fatal(err)
	}
	stop, ok := typed.(*StopEvent)
	if !ok {
		t.Fatalf("session.idle should map to agent.stop, got %T", typed)
	}
	if stop.FinalMessage != "Deployed to staging." {
		t.Errorf("shim-spliced finalMessage wrong: %q", stop.FinalMessage)
	}

	typed, err = decodeOpenCodeLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "opencode/message_part_updated_tool_error.json"))
	if err != nil {
		t.Fatal(err)
	}
	failed, ok := typed.(*ToolPostEvent)
	if !ok {
		t.Fatalf("tool-error part update should map to a failed tool.post, got %T", typed)
	}
	if !failed.Failed || failed.Error != "File not found: /tmp/missing.txt" {
		t.Errorf("part error must surface as failure: %+v", failed)
	}
	if failed.Session.ID != "oc-sess-1" || failed.Tool.Name != "read" || failed.Tool.ID != "call-9" {
		t.Errorf("opencode tool-error identity wrong: %+v", failed.Tool)
	}
}

func TestEncodeClaudeGoldens(t *testing.T) {
	pre := &Event{Provider: ProviderClaudeCode, Kind: KindToolPre, NativeName: "PreToolUse"}
	cases := []struct {
		name string
		d    decisionCore
		want string
	}{
		{"no decision", NoDecision().core, `{}`},
		{"deny", Deny("nope").core,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"nope"}}`},
		{"ask", AskUser("sure?").core,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"sure?"}}`},
		{"allow with update", Allow().WithUpdatedInput(map[string]string{"command": "ls"}).core,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"ls"}}}`},
		{"deny with system message", Deny("no").WithSystemMessage("blocked").core,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"},"systemMessage":"blocked"}`},
	}
	for _, c := range cases {
		wire, err := encodeClaude(pre, c.d)
		if err != nil {
			t.Fatal(err)
		}
		if string(wire.Stdout) != c.want || wire.ExitCode != 0 {
			t.Errorf("%s:\n got %s (exit %d)\nwant %s", c.name, wire.Stdout, wire.ExitCode, c.want)
		}
	}

	prompt := &Event{Provider: ProviderClaudeCode, Kind: KindPromptSubmitted, NativeName: "UserPromptSubmit"}
	wire, _ := encodeClaude(prompt, AcceptPrompt().WithContext("today is 2026-07-02").core)
	want := `{"hookSpecificOutput":{"additionalContext":"today is 2026-07-02","hookEventName":"UserPromptSubmit"}}`
	if string(wire.Stdout) != want {
		t.Errorf("prompt context: got %s want %s", wire.Stdout, want)
	}
	wire, _ = encodeClaude(prompt, BlockPrompt("policy").core)
	if string(wire.Stdout) != `{"decision":"block","reason":"policy"}` {
		t.Errorf("prompt block: got %s", wire.Stdout)
	}

	stop := &Event{Provider: ProviderClaudeCode, Kind: KindStop, NativeName: "Stop"}
	wire, _ = encodeClaude(stop, ContinueWith("run the tests").core)
	if string(wire.Stdout) != `{"decision":"block","reason":"run the tests"}` {
		t.Errorf("stop continue: got %s", wire.Stdout)
	}
	wire, _ = encodeClaude(stop, Finish().core)
	if string(wire.Stdout) != `{}` {
		t.Errorf("stop finish: got %s", wire.Stdout)
	}
}

func TestEncodeCodexEmptyStdout(t *testing.T) {
	pre := &Event{Provider: ProviderCodex, Kind: KindToolPre, NativeName: "PreToolUse"}
	wire, err := encodeCodex(pre, NoDecision().core)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Stdout) != 0 {
		t.Errorf("codex no-opinion must be empty stdout (quirk #8), got %q", wire.Stdout)
	}
	wire, _ = encodeCodex(pre, Deny("no").core)
	if string(wire.Stdout) != `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}` {
		t.Errorf("codex deny: got %s", wire.Stdout)
	}
	if _, err := encodeCodex(pre, AskUser("?").core); err == nil {
		t.Error("ask must not reach the codex codec; expected error")
	}
}

func TestEncodeCursorGoldens(t *testing.T) {
	shell := &Event{Provider: ProviderCursor, Kind: KindToolPre, NativeName: "beforeShellExecution"}
	wire, _ := encodeCursor(shell, Deny("risky").core)
	if string(wire.Stdout) != `{"agent_message":"risky","continue":true,"permission":"deny"}` {
		t.Errorf("cursor deny: got %s", wire.Stdout)
	}
	wire, _ = encodeCursor(shell, NoDecision().core)
	if string(wire.Stdout) != `{}` {
		t.Errorf("cursor no-decision must be {}: got %s", wire.Stdout)
	}

	prompt := &Event{Provider: ProviderCursor, Kind: KindPromptSubmitted, NativeName: "beforeSubmitPrompt"}
	wire, _ = encodeCursor(prompt, BlockPrompt("not now").core)
	if string(wire.Stdout) != `{"continue":false,"user_message":"not now"}` {
		t.Errorf("cursor prompt block: got %s", wire.Stdout)
	}

	stop := &Event{Provider: ProviderCursor, Kind: KindStop, NativeName: "stop"}
	wire, _ = encodeCursor(stop, ContinueWith("also update docs").core)
	if string(wire.Stdout) != `{"followup_message":"also update docs"}` {
		t.Errorf("cursor continue: got %s", wire.Stdout)
	}

	// updated_input rides preToolUse only.
	pre := &Event{Provider: ProviderCursor, Kind: KindToolPre, NativeName: "preToolUse"}
	wire, _ = encodeCursor(pre, Allow().WithUpdatedInput(map[string]string{"path": "/safe"}).core)
	if string(wire.Stdout) != `{"continue":true,"permission":"allow","updated_input":{"path":"/safe"}}` {
		t.Errorf("cursor updated input: got %s", wire.Stdout)
	}
}

func TestEncodeGeminiGoldens(t *testing.T) {
	pre := &ToolPreEvent{
		Event: Event{Provider: ProviderGemini, Kind: KindToolPre, NativeName: "BeforeTool"},
		Tool:  ToolCall{Input: json.RawMessage(`{"command":"ls","description":"x"}`)},
	}
	wire, err := encodeGemini(pre, &pre.Event, Deny("blocked").core)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire.Stdout) != `{"decision":"block","reason":"blocked"}` {
		t.Errorf("gemini deny: got %s", wire.Stdout)
	}

	wire, _ = encodeGemini(pre, &pre.Event, NoDecision().core)
	if string(wire.Stdout) != `{}` {
		t.Errorf("gemini no-decision must be explicit {} (quirk #11): got %s", wire.Stdout)
	}

	// Lossy update: dropping a key is inexpressible under shallow merge.
	_, err = encodeGemini(pre, &pre.Event, NoDecision().WithUpdatedInput(map[string]string{"command": "ls -la"}).core)
	if !errors.Is(err, ErrLossyUpdate) {
		t.Errorf("expected ErrLossyUpdate, got %v", err)
	}

	// Full-key update is fine and rides hookSpecificOutput.tool_input.
	wire, err = encodeGemini(pre, &pre.Event, NoDecision().WithUpdatedInput(map[string]string{"command": "ls -la", "description": "x"}).core)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"hookSpecificOutput":{"hookEventName":"BeforeTool","tool_input":{"command":"ls -la","description":"x"}}}`
	if string(wire.Stdout) != want {
		t.Errorf("gemini update: got %s want %s", wire.Stdout, want)
	}
}

func TestDetectFromShape(t *testing.T) {
	cases := []struct {
		payload string
		want    Provider
	}{
		{`{"conversation_id":"c1","hook_event_name":"beforeShellExecution"}`, ProviderCursor},
		{`{"hook_event_name":"beforeSubmitPrompt"}`, ProviderCursor},
		{`{"session_id":"s","hook_event_name":"BeforeTool","timestamp":"2026-07-02T10:00:00Z"}`, ProviderGemini},
		{`{"session_id":"s","turn_id":"t","hook_event_name":"PreToolUse"}`, ProviderCodex},
		{`{"session_id":"s","hook_event_name":"PreToolUse"}`, ProviderClaudeCode},
		{`{"seq":1,"hook":"tool.execute.before","input":{}}`, ProviderOpenCode},
	}
	for _, c := range cases {
		got, ok := detectFromShape([]byte(c.payload))
		if !ok || got != c.want {
			t.Errorf("detectFromShape(%s) = %q ok=%v, want %q", c.payload, got, ok, c.want)
		}
	}
}
