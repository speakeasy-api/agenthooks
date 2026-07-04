package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
	"github.com/speakeasy-api/agenthooks/provider/kimicode"
)

const kimiFallbackPath = "~/.kimi-code/bin/kimi"

// kimiHome builds a sandbox KIMI_CODE_HOME (hooks live only in the user-level
// config.toml there) seeded with the real credentials and base config, so
// runs are authenticated and use the user's model settings without touching
// the real ~/.kimi-code.
func kimiHome(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	realHome := filepath.Join(home, ".kimi-code")
	dir := t.TempDir()
	copied := 0
	for _, f := range []string{"credentials", "device_id", "config.toml"} {
		data, err := os.ReadFile(filepath.Join(realHome, f))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if copied == 0 {
		t.Skip("kimi is not set up locally (~/.kimi-code has no credentials/config)")
	}
	return dir
}

func runKimi(t *testing.T, proj, home, prompt string) {
	t.Helper()
	bin := requireE2E(t, "kimi", kimiFallbackPath)
	if _, err := runAgent(t, proj, []string{"KIMI_CODE_HOME=" + home}, bin, "-p", prompt); err != nil {
		// Kimi occasionally crashes outright (exit 1 with a log pointer);
		// don't fail here — the turn then has no tool.pre and runToolTurn
		// retries once with a fresh sandbox.
		t.Logf("kimi -p failed (runToolTurn retries if no events landed): %v", err)
	}
}

// TestKimiEventFields verifies the per-event stdin payloads a real Kimi
// binary sends against the typed structs in provider/kimicode: the fields we
// type must be populated, and Extra (unknown-field capture) must be empty —
// a non-empty Extra means real Kimi sends fields the structs don't know.
func TestKimiEventFields(t *testing.T) {
	t.Parallel()
	requireE2E(t, "kimi", kimiFallbackPath)
	rec, _ := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		home := kimiHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderKimi, install.ScopeUser, home)
		runKimi(t, proj, home, shellMarkerPrompt("kimi-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	// Kimi's -p print mode never fires UserPromptSubmit (quirk #30): the
	// runner must backfill a reporting-only prompt.submitted instead.
	requireKinds(t, evs,
		agenthooks.KindSessionStart,
		agenthooks.KindToolPre,
		agenthooks.KindToolPost,
		agenthooks.KindStop,
	)
	requireBackfilledPrompt(t, evs, "kimi-marker.txt")

	for _, e := range ofKind(evs, agenthooks.KindToolPre) {
		if e.Backfilled {
			continue
		}
		ev := &agenthooks.Event{Provider: agenthooks.ProviderKimi, NativeName: e.Native, Raw: e.Raw}
		in, ok := kimicode.PreToolUse(ev)
		if !ok {
			t.Fatalf("PreToolUse view rejected native %q", e.Native)
		}
		if in.SessionID == "" || in.CWD == "" || in.HookEventName != "PreToolUse" {
			t.Errorf("PreToolUse base fields incomplete: %+v (raw: %s)", in.Base, e.Raw)
		}
		if in.ToolName == "" || len(in.ToolInput) == 0 {
			t.Errorf("PreToolUse tool fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		if in.ToolCallID == "" {
			t.Errorf("PreToolUse tool_call_id empty — field rename not matching real Kimi? (raw: %s)", e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("PreToolUse has unknown fields %v — structs incomplete (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindToolPost) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderKimi, NativeName: e.Native, Raw: e.Raw}
		in, ok := kimicode.PostToolUse(ev)
		if !ok {
			t.Fatalf("PostToolUse view rejected native %q", e.Native)
		}
		if in.ToolName == "" || len(in.ToolOutput) == 0 || in.ToolCallID == "" {
			t.Errorf("PostToolUse fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("PostToolUse has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindSessionStart) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderKimi, NativeName: e.Native, Raw: e.Raw}
		in, ok := kimicode.SessionStart(ev)
		if !ok {
			t.Fatalf("SessionStart view rejected native %q", e.Native)
		}
		if in.SessionID == "" {
			t.Errorf("SessionStart fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("SessionStart has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindStop) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderKimi, NativeName: e.Native, Raw: e.Raw}
		in, ok := kimicode.Stop(ev)
		if !ok {
			t.Fatalf("Stop view rejected native %q", e.Native)
		}
		if len(in.Extra) > 0 {
			t.Errorf("Stop has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	// Normalization: the shell call must classify as canonical shell.
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) {
			return
		}
	}
	t.Errorf("no shell tool.pre normalized from kimi; got:\n%s", summarize(evs))
}

// TestKimiDeny verifies the deny dialect (hookSpecificOutput.permissionDecision
// on PreToolUse) actually blocks the tool in a real Kimi run, and uses the
// resulting blocked call to verify the PostToolUseFailure payload — Kimi
// fires it for blocked tools; a command merely exiting non-zero is still a
// successful tool execution and fires plain PostToolUse.
func TestKimiDeny(t *testing.T) {
	t.Parallel()
	requireE2E(t, "kimi", kimiFallbackPath)
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, string(agenthooks.ToolShell))
		home := kimiHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderKimi, install.ScopeUser, home)
		runKimi(t, proj, home, shellMarkerPrompt("denied-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre)
	if markerExists(proj, "denied-marker.txt") {
		t.Error("marker exists: deny decision did not block the shell command on kimi")
	}

	for _, e := range ofKind(evs, agenthooks.KindToolError) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderKimi, NativeName: e.Native, Raw: e.Raw}
		in, ok := kimicode.PostToolUseFailure(ev)
		if !ok {
			t.Fatalf("PostToolUseFailure view rejected native %q", e.Native)
		}
		if in.ToolName == "" || in.Error == "" {
			t.Errorf("PostToolUseFailure fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("PostToolUseFailure has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
