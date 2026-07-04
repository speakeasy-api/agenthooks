package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
)

// runCursor pipes the prompt via stdin rather than argv on purpose: it keeps
// the prompt out of the process command line, so requireBackfilledPrompt can
// only pass if the transcript-based recovery works (the argv fallback has
// nothing to find).
func runCursor(t *testing.T, proj, prompt string) {
	t.Helper()
	bin := requireE2E(t, "cursor-agent")
	if _, err := runAgentIn(t, proj, nil, prompt, bin,
		"-p", "--force", "--trust", "--output-format", "text"); err != nil {
		t.Fatalf("cursor-agent -p failed: %v", err)
	}
}

// TestCursorEvents installs project-scope hooks and verifies a real
// cursor-agent run: events decode, the double-fired tool.pre siblings
// (preToolUse + beforeShellExecution, quirk #2) dedupe to one delivery per
// call, and shell calls normalize.
func TestCursorEvents(t *testing.T) {
	t.Parallel()
	requireE2E(t, "cursor-agent")
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderCursor, install.ScopeProject, proj)
		runCursor(t, proj, shellMarkerPrompt("cursor-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre)
	if !markerExists(proj, "cursor-marker.txt") {
		t.Error("marker missing: shell command did not run")
	}
	// cursor-agent -p never fires beforeSubmitPrompt (quirk #31): the runner
	// must backfill a reporting-only prompt.submitted instead.
	requireBackfilledPrompt(t, evs, "cursor-marker.txt")

	var shellPres []event
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) {
			shellPres = append(shellPres, e)
		}
	}
	if len(shellPres) == 0 {
		t.Errorf("no shell tool.pre normalized from cursor; got:\n%s", summarize(evs))
	}
	// quirk #2: cursor fires preToolUse AND beforeShellExecution for the same
	// call; the runner must dedupe them into a single tool.pre delivery. The
	// model may legitimately run several distinct commands (e.g. verifying
	// its work), and the markers carry a 30s TTL past which siblings
	// double-deliver by design (a decision is never dropped) — so only
	// same-command deliveries inside the TTL window are dedup failures.
	const dedupTTLMS = 30_000
	byCommand := map[string][]event{}
	for _, e := range shellPres {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(e.ToolInput, &in)
		byCommand[in.Command] = append(byCommand[in.Command], e)
	}
	for cmd, dupes := range byCommand {
		for i := 1; i < len(dupes); i++ {
			gap := dupes[i].TimeMS - dupes[i-1].TimeMS
			if gap < dedupTTLMS-5_000 {
				t.Errorf("cursor double-fire not deduped: %q delivered twice %dms apart; got:\n%s", cmd, gap, summarize(evs))
			} else {
				t.Logf("double delivery of %q %dms apart: designed TTL degradation, not a dedup failure", cmd, gap)
			}
		}
	}
}

// TestCursorResumeBackfill: a resumed print-mode turn (`--resume <chatId>`)
// drops beforeSubmitPrompt again, and the session already carries a backfill
// marker from turn 1 — the fingerprint-keyed markers must still report turn
// 2's prompt (once). Prompts go via stdin, so recovery is transcript-only.
func TestCursorResumeBackfill(t *testing.T) {
	t.Parallel()
	bin := requireE2E(t, "cursor-agent")
	rec := newRecorder(t, "")
	proj := t.TempDir()
	installHooks(t, rec, agenthooks.ProviderCursor, install.ScopeProject, proj)

	runCursor(t, proj, shellMarkerPrompt("resume-marker-1.txt"))
	evs := rec.events(t)
	requireBackfilledPrompt(t, evs, "resume-marker-1.txt")
	session := ""
	for _, e := range evs {
		if e.Session != "" {
			session = e.Session
			break
		}
	}
	if session == "" {
		t.Fatalf("no session id recorded in turn 1; got:\n%s", summarize(evs))
	}

	if _, err := runAgentIn(t, proj, nil, shellMarkerPrompt("resume-marker-2.txt"), bin,
		"-p", "--force", "--trust", "--output-format", "text", "--resume", session); err != nil {
		t.Fatalf("cursor-agent -p --resume failed: %v", err)
	}
	evs = rec.events(t)
	var backfilledPrompts []string
	sameSession := false
	for _, e := range evs {
		if e.Typed && e.Kind == string(agenthooks.KindPromptSubmitted) && e.Backfilled {
			backfilledPrompts = append(backfilledPrompts, e.Prompt)
		}
		if e.TimeMS > 0 && e.Session == session {
			sameSession = true
		}
	}
	// The gap only exists when the resumed turn keeps the conversation id; if
	// cursor ever mints a fresh id on --resume, the quirk notes need updating.
	if !sameSession {
		t.Errorf("resumed turn changed the session id — revisit the resume-backfill quirk notes; got:\n%s", summarize(evs))
	}
	if len(backfilledPrompts) != 2 {
		t.Errorf("want exactly 2 backfilled prompts (one per turn), got %d: %q", len(backfilledPrompts), backfilledPrompts)
	}
	found := false
	for _, p := range backfilledPrompts {
		found = found || strings.Contains(p, "resume-marker-2.txt")
	}
	if !found {
		t.Errorf("resumed turn's prompt not backfilled: %q", backfilledPrompts)
	}
}

// TestCursorDeny verifies a Deny decision blocks the tool in a real run.
func TestCursorDeny(t *testing.T) {
	t.Parallel()
	requireE2E(t, "cursor-agent")
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, string(agenthooks.ToolShell))
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderCursor, install.ScopeProject, proj)
		runCursor(t, proj, shellMarkerPrompt("denied-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre)
	if markerExists(proj, "denied-marker.txt") {
		t.Error("marker exists: deny decision did not block the shell command on cursor")
	}
}
