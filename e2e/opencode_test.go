package e2e

import (
	"sync"
	"testing"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
)

const opencodeFallbackPath = "~/.opencode/bin/opencode"

// opencodeSerial serializes opencode invocations: concurrent `opencode run`
// processes contend on opencode's global session database ("database is
// locked"). The tests stay parallel with other providers.
var opencodeSerial sync.Mutex

func runOpenCode(t *testing.T, proj, prompt string) {
	t.Helper()
	bin := requireE2E(t, "opencode", opencodeFallbackPath)
	opencodeSerial.Lock()
	defer opencodeSerial.Unlock()
	if _, err := runAgent(t, proj, nil, bin, "run", prompt); err != nil {
		t.Fatalf("opencode run failed: %v", err)
	}
}

// TestOpenCodeEvents installs the generated shim plugin and verifies the
// serve-mode bridge end to end: plugin spawns the recorder in serve mode,
// frames map to unified events, shell calls normalize.
func TestOpenCodeEvents(t *testing.T) {
	t.Parallel()
	requireE2E(t, "opencode", opencodeFallbackPath)
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderOpenCode, install.ScopeProject, proj)
		runOpenCode(t, proj, shellMarkerPrompt("opencode-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs,
		agenthooks.KindPromptSubmitted,
		agenthooks.KindToolPre,
		agenthooks.KindToolPost,
	)
	if !markerExists(proj, "opencode-marker.txt") {
		t.Error("marker missing: shell command did not run")
	}
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) {
			return
		}
	}
	t.Errorf("no shell tool.pre normalized from opencode; got:\n%s", summarize(evs))
}

// TestOpenCodeDeny verifies the shim's re-thrown error actually blocks the
// tool in a real OpenCode run.
func TestOpenCodeDeny(t *testing.T) {
	t.Parallel()
	requireE2E(t, "opencode", opencodeFallbackPath)
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, string(agenthooks.ToolShell))
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderOpenCode, install.ScopeProject, proj)
		runOpenCode(t, proj, shellMarkerPrompt("denied-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre)
	if markerExists(proj, "denied-marker.txt") {
		t.Error("marker exists: deny decision did not block the shell command on opencode")
	}
}
