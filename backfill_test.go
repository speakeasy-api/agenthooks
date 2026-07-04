package agenthooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func kimiPre(session string) []byte {
	b, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      session,
		"cwd":             "/work",
		"tool_name":       "Bash",
		"tool_input":      map[string]string{"command": "ls"},
		"tool_call_id":    "c1",
	})
	return b
}

func kimiPrompt(session string) []byte {
	b, _ := json.Marshal(map[string]any{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      session,
		"cwd":             "/work",
		"prompt":          "hello",
	})
	return b
}

// TestBackfillPromptSubmitted: an event that implies a prompt, for a session
// that never delivered one, synthesizes a reporting-only prompt.submitted
// before the triggering event — once per session, with capabilities masked
// and the handler's decision discarded (quirks #30, #31).
func TestBackfillPromptSubmitted(t *testing.T) {
	var seq []string
	var backfilled *PromptEvent
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnPromptSubmitted(func(_ context.Context, e *PromptEvent) (PromptDecision, error) {
		seq = append(seq, "prompt")
		backfilled = e
		// Reporting-only: this block must be discarded, not encoded.
		return BlockPrompt("must be ignored"), nil
	})
	r.OnToolPre(func(_ context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		seq = append(seq, "tool.pre")
		return NoDecision(), nil
	})

	args := []string{"agenthooks", "run", "--provider=kimi-code"}
	out, code := runWith(t, r, args, kimiPre("sess-bf-1"))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(seq) != 2 || seq[0] != "prompt" || seq[1] != "tool.pre" {
		t.Fatalf("backfill must precede the triggering event, got %v", seq)
	}
	if backfilled == nil || !backfilled.Backfilled || backfilled.Raw != nil || backfilled.Prompt != "" {
		t.Errorf("backfilled event malformed: %+v", backfilled)
	}
	if backfilled.Session.ID != "sess-bf-1" || backfilled.Provider != ProviderKimi {
		t.Errorf("backfilled event must inherit session identity: %+v", backfilled.Event)
	}
	if backfilled.Can(CapDeny) || backfilled.Can(CapAddContext) {
		t.Error("backfilled events must report no capabilities")
	}
	// The BlockPrompt from the backfill handler must not leak into the wire
	// response of the triggering tool.pre (kimi no-op = empty stdout, exit 0).
	if out != "" {
		t.Errorf("triggering event's response polluted by backfill decision: %q", out)
	}

	// Second implied event in the same session: no repeat backfill.
	seq = nil
	if _, code := runWith(t, r, args, kimiPre("sess-bf-1")); code != 0 {
		t.Fatal("second run failed")
	}
	if len(seq) != 1 || seq[0] != "tool.pre" {
		t.Errorf("backfill must fire once per session, got %v", seq)
	}
}

// TestBackfillSkippedWhenPromptDelivered: a real prompt.submitted marks the
// session; later events must not trigger a synthetic one.
func TestBackfillSkippedWhenPromptDelivered(t *testing.T) {
	var prompts []bool // Backfilled flag per delivery
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnPromptSubmitted(func(_ context.Context, e *PromptEvent) (PromptDecision, error) {
		prompts = append(prompts, e.Backfilled)
		return AcceptPrompt(), nil
	})

	args := []string{"agenthooks", "run", "--provider=kimi-code"}
	if _, code := runWith(t, r, args, kimiPrompt("sess-bf-2")); code != 0 {
		t.Fatal("prompt run failed")
	}
	if _, code := runWith(t, r, args, kimiPre("sess-bf-2")); code != 0 {
		t.Fatal("tool run failed")
	}
	if len(prompts) != 1 || prompts[0] {
		t.Errorf("expected exactly one real prompt delivery, got backfill flags %v", prompts)
	}
}

// TestKimiPromptRecovery: backfill recovers the prompt text from Kimi's
// session store (session_index.jsonl -> wire.jsonl, latest turn.prompt).
func TestKimiPromptRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	sessDir := filepath.Join(home, "sessions", "wd_x", "session_rec-1")
	if err := os.MkdirAll(filepath.Join(sessDir, "agents", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := `{"sessionId":"other","sessionDir":"/nope"}
{"sessionId":"session_rec-1","sessionDir":` + strconv.Quote(sessDir) + `}
`
	wire := `{"type":"metadata","protocol_version":"1.4"}
{"type":"turn.prompt","input":[{"type":"text","text":"first prompt"}],"origin":{"kind":"user"}}
{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"first prompt"}]}}
{"type":"turn.prompt","input":[{"type":"text","text":"touch the marker"}],"origin":{"kind":"user"}}
`
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "agents", "main", "wire.jsonl"), []byte(wire), 0o644); err != nil {
		t.Fatal(err)
	}

	var got string
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnPromptSubmitted(func(_ context.Context, e *PromptEvent) (PromptDecision, error) {
		got = e.Prompt
		return AcceptPrompt(), nil
	})
	if _, code := runWith(t, r, []string{"agenthooks", "run", "--provider=kimi-code"}, kimiPre("session_rec-1")); code != 0 {
		t.Fatal("run failed")
	}
	if got != "touch the marker" {
		t.Errorf("recovered prompt = %q, want the latest turn.prompt", got)
	}
}

// TestBackfillResumedSession: headless resume (`-p --resume S "prompt B"`)
// drops the prompt event again for a session that already has a backfill
// marker. Once-per-session tracking would lose prompt B forever; the marker
// must therefore track the recovered prompt's fingerprint — a turn whose
// recovered text differs from what was already reported backfills again,
// while repeats of the same turn (same text, or recovery failing mid-turn)
// stay suppressed.
func TestBackfillResumedSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	sessDir := filepath.Join(home, "sessions", "wd_x", "session_resume-1")
	wirePath := filepath.Join(sessDir, "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wirePath), 0o755); err != nil {
		t.Fatal(err)
	}
	index := `{"sessionId":"session_resume-1","sessionDir":` + strconv.Quote(sessDir) + "}\n"
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	turn := func(prompt string) string {
		return `{"type":"turn.prompt","input":[{"type":"text","text":` + strconv.Quote(prompt) + `}]}` + "\n"
	}
	if err := os.WriteFile(wirePath, []byte(turn("prompt A")), 0o644); err != nil {
		t.Fatal(err)
	}

	var prompts []string
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnPromptSubmitted(func(_ context.Context, e *PromptEvent) (PromptDecision, error) {
		prompts = append(prompts, e.Prompt)
		return AcceptPrompt(), nil
	})
	args := []string{"agenthooks", "run", "--provider=kimi-code"}
	run := func(label string) {
		t.Helper()
		if _, code := runWith(t, r, args, kimiPre("session_resume-1")); code != 0 {
			t.Fatalf("%s failed", label)
		}
	}

	run("turn 1, first event") // backfills "prompt A"
	run("turn 1, later event") // same recovered text: no duplicate

	// Resumed turn: the session store gains a new turn.prompt, the marker
	// already exists, and the prompt event is dropped again (quirks #30/#31).
	f, err := os.OpenFile(wirePath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(turn("prompt B")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	run("turn 2, first event") // must backfill "prompt B"
	run("turn 2, later event") // and again only once

	want := []string{"prompt A", "prompt B"}
	if len(prompts) != len(want) || prompts[0] != want[0] || prompts[1] != want[1] {
		t.Errorf("backfilled prompts = %q, want %q (resumed turn must backfill exactly once)", prompts, want)
	}
}

// TestBackfillConsumerGating pins the supported pattern for gating on a
// recovered prompt: decisions returned from the backfilled prompt.submitted
// itself are discarded (the prompt already reached the model), but the
// backfill dispatches in the same process immediately before the triggering
// event, so handler state set there is visible to the triggering event's
// handler — which CAN decide. Denying the tool there is the gate.
func TestBackfillConsumerGating(t *testing.T) {
	violation := false
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnPromptSubmitted(func(_ context.Context, e *PromptEvent) (PromptDecision, error) {
		violation = true // pretend the recovered prompt failed policy
		return BlockPrompt("discarded"), nil
	})
	r.OnToolPre(func(_ context.Context, _ *ToolPreEvent) (ToolPreDecision, error) {
		if violation {
			return Deny("prompt failed policy"), nil
		}
		return NoDecision(), nil
	})

	out, code := runWith(t, r, []string{"agenthooks", "run", "--provider=kimi-code"}, kimiPre("sess-bf-gate"))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, `"permissionDecision":"deny"`) && !strings.Contains(out, `"permissionDecision": "deny"`) {
		t.Errorf("deny from the triggering event's handler must reach the wire, got %q", out)
	}
}

// TestCursorPromptRecovery: backfill recovers the prompt from the session
// transcript the cursor payload names — the last user entry with text; user
// entries that only carry tool results are skipped, and the transcript's
// <user_query> framing is stripped.
func TestCursorPromptRecovery(t *testing.T) {
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	jsonl := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nfirst prompt\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"done"}]}}
{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ntouch the cursor marker\n</user_query>"}]}}
{"role":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}
`
	if err := os.WriteFile(transcriptPath, []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "preToolUse",
		"conversation_id": "sess-cur-tr",
		"cwd":             "/work",
		"transcript_path": transcriptPath,
		"tool_name":       "Shell",
		"tool_input":      map[string]string{"command": "ls"},
	})

	var got string
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnPromptSubmitted(func(_ context.Context, e *PromptEvent) (PromptDecision, error) {
		got = e.Prompt
		return AcceptPrompt(), nil
	})
	if _, code := runWith(t, r, []string{"agenthooks", "run", "--provider=cursor"}, payload); code != 0 {
		t.Fatal("run failed")
	}
	if got != "touch the cursor marker" {
		t.Errorf("recovered prompt = %q, want the last user entry with text, unwrapped", got)
	}
}

// TestCursorPromptFromArgs pins the argv extraction: value flags consumed,
// boolean flags skipped, variadic positionals joined, print-mode required.
func TestCursorPromptFromArgs(t *testing.T) {
	args := []string{
		"/usr/local/bin/cursor-agent", "-p", "--force", "--trust",
		"--output-format", "text", "--model", "gpt-5",
		"Run exactly this shell command:", "touch marker.txt",
	}
	if got, want := cursorPromptFromArgs(args), "Run exactly this shell command: touch marker.txt"; got != want {
		t.Errorf("cursorPromptFromArgs = %q, want %q", got, want)
	}
	if got := cursorPromptFromArgs([]string{"cursor-agent", "-p", "--output-format", "text"}); got != "" {
		t.Errorf("flag values must not be mistaken for prompts, got %q", got)
	}
	if !isCursorAgentArgs(args) {
		t.Error("print-mode cursor-agent argv must match")
	}
	if isCursorAgentArgs([]string{"/usr/local/bin/cursor-agent", "--force", "chat"}) {
		t.Error("interactive cursor-agent argv must not match (no -p)")
	}
	if isCursorAgentArgs([]string{"/bin/sh", "-c", "-p", "x"}) {
		t.Error("non-cursor binaries must not match")
	}
}

// TestFindAncestorArgs walks the real process tree: the test binary's
// ancestry must be readable on this platform (go test is a child of the Go
// tool at minimum).
func TestFindAncestorArgs(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("ancestor argv recovery is linux/darwin only (promptargv_other.go)")
	}
	args, err := procArgs(os.Getpid())
	if err != nil {
		t.Fatalf("procArgs(self): %v", err)
	}
	if len(args) == 0 {
		t.Fatal("procArgs(self) returned no argv")
	}
	if _, err := procPPID(os.Getpid()); err != nil {
		t.Fatalf("procPPID(self): %v", err)
	}
}

// TestBackfillDisabled: WithoutBackfill suppresses synthesis entirely.
func TestBackfillDisabled(t *testing.T) {
	called := false
	r := quietRunner(WithDedupDir(t.TempDir()), WithoutBackfill())
	r.OnPromptSubmitted(func(_ context.Context, _ *PromptEvent) (PromptDecision, error) {
		called = true
		return AcceptPrompt(), nil
	})
	if _, code := runWith(t, r, []string{"agenthooks", "run", "--provider=kimi-code"}, kimiPre("sess-bf-3")); code != 0 {
		t.Fatal("run failed")
	}
	if called {
		t.Error("WithoutBackfill must suppress synthetic prompt.submitted")
	}
}
