package e2e

import (
	"strings"
	"testing"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
)

func runGemini(t *testing.T, proj, prompt string) {
	t.Helper()
	bin := requireE2E(t, "gemini")
	out, err := runAgent(t, proj, nil, bin, "--yolo", "-p", prompt)
	if strings.Contains(out, "Please set an Auth method") {
		t.Skip("gemini installed but not authenticated: run `gemini` once to log in, or export GEMINI_API_KEY")
	}
	if err != nil {
		t.Fatalf("gemini -p failed: %v", err)
	}
}

// TestGeminiEvents installs project-scope hooks and verifies a real Gemini
// CLI run. Skips when gemini is not installed locally.
func TestGeminiEvents(t *testing.T) {
	t.Parallel()
	requireE2E(t, "gemini")
	rec, _ := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderGemini, install.ScopeProject, proj)
		runGemini(t, proj, shellMarkerPrompt("gemini-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs,
		agenthooks.KindPromptSubmitted,
		agenthooks.KindToolPre,
		agenthooks.KindToolPost,
		agenthooks.KindStop,
	)
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) {
			return
		}
	}
	t.Errorf("no shell tool.pre normalized from gemini; got:\n%s", summarize(evs))
}

// TestGeminiDeny verifies a Deny decision blocks the tool in a real run.
func TestGeminiDeny(t *testing.T) {
	t.Parallel()
	requireE2E(t, "gemini")
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, string(agenthooks.ToolShell))
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderGemini, install.ScopeProject, proj)
		runGemini(t, proj, shellMarkerPrompt("denied-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre)
	if markerExists(proj, "denied-marker.txt") {
		t.Error("marker exists: deny decision did not block the shell command on gemini")
	}
}
