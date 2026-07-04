package e2e

import (
	"testing"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
)

func runClaude(t *testing.T, proj, prompt string) {
	t.Helper()
	bin := requireE2E(t, "claude")
	if _, err := runAgent(t, proj, nil, bin,
		"-p", prompt, "--allowedTools", "Bash"); err != nil {
		t.Fatalf("claude -p failed: %v", err)
	}
}

// TestClaudeEvents installs project-scope hooks and verifies the standard
// event sequence decodes from a real Claude Code run, including canonical
// shell normalization.
func TestClaudeEvents(t *testing.T) {
	t.Parallel()
	requireE2E(t, "claude")
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderClaudeCode, install.ScopeProject, proj)
		runClaude(t, proj, shellMarkerPrompt("claude-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs,
		agenthooks.KindSessionStart,
		agenthooks.KindPromptSubmitted,
		agenthooks.KindToolPre,
		agenthooks.KindToolPost,
		agenthooks.KindStop,
	)
	if !markerExists(proj, "claude-marker.txt") {
		t.Error("marker missing: allowed shell command did not run")
	}
	// Claude fires the real UserPromptSubmit: nothing may be synthesized.
	requireNoBackfill(t, evs)
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) {
			return
		}
	}
	t.Errorf("no shell tool.pre normalized from claude; got:\n%s", summarize(evs))
}

// TestClaudeDeny verifies a Deny decision blocks the tool in a real run.
func TestClaudeDeny(t *testing.T) {
	t.Parallel()
	requireE2E(t, "claude")
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, string(agenthooks.ToolShell))
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderClaudeCode, install.ScopeProject, proj)
		runClaude(t, proj, shellMarkerPrompt("denied-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre)
	if markerExists(proj, "denied-marker.txt") {
		t.Error("marker exists: deny decision did not block the shell command on claude")
	}
}
