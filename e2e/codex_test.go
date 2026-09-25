package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
)

// codexHome builds a sandbox CODEX_HOME with the user's real auth copied in,
// so runs are authenticated but no real config is touched. Cleanup is
// retrying and best-effort rather than t.TempDir: codex leaves a background
// plugins-clone process writing into the home briefly after exiting, and a
// racing RemoveAll would fail the test over nothing.
func codexHome(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	auth, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Skip("codex is not authenticated (~/.codex/auth.json missing)")
	}
	dir, err := os.MkdirTemp("", "agenthooks-codex-home-*") //nolint:usetesting // t.TempDir cleanup races codex's background teardown and fails the test
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 5 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runCodex(t *testing.T, proj, home, prompt string) string {
	t.Helper()
	args := []string{
		"exec", "--skip-git-repo-check", "-s", "workspace-write", "--ephemeral",
	}
	args = append(args, prompt)
	out, err := runAgent(t, proj, []string{"CODEX_HOME=" + home}, "codex", args...)
	if err != nil {
		t.Fatalf("codex exec failed: %v", err)
	}
	return out
}

// TestCodexPortableHandler drives the same provider-free recorder, context,
// and natural prompt as TestMoltisEventsAndDecisions. This is the incumbent
// half of the cross-provider canary: context must reach the final answer and
// the real shell lifecycle must normalize through the shared handler.
func TestCodexPortableHandler(t *testing.T) {
	requireE2E(t, "codex")

	home := codexHome(t)
	rec := newRecorderWithConfig(t, recorderConfig{
		PromptContext: "Portable context marker: AGENTHOOKS_SHARED_CONTEXT_OK",
	})
	installHooks(t, rec, agenthooks.ProviderCodex, install.ScopeUser, home)
	proj := t.TempDir()
	marker := filepath.Join(proj, "shared-marker.txt")
	out := runCodex(t, proj, home, portableContextShellPrompt(marker))

	if !fileExists(marker) {
		t.Fatal("Codex did not execute the shared handler's allowed shell call")
	}
	if !strings.Contains(out, "AGENTHOOKS_SHARED_CONTEXT_OK") {
		t.Fatalf("Codex final response omitted the shared prompt context marker:\n%s", out)
	}
	evs := rec.events(t)
	requireKinds(t, evs,
		agenthooks.KindPromptSubmitted,
		agenthooks.KindToolPre,
		agenthooks.KindToolPost,
		agenthooks.KindStop,
	)
	if !hasCanonicalTool(evs, agenthooks.ToolShell) {
		t.Fatalf("Codex tool did not normalize as shell:\n%s", summarize(evs))
	}
}

// TestCodexTrustPreseeding is the DefinitionHash verification, run as a
// differential so a pass can't be vacuous:
//
//  1. with a deliberately tampered trusted_hash, Codex must NOT run the hooks
//     (proves the trust gate is enforced in this codex build), and
//  2. with the install-package trust seeding as rendered, Codex MUST run them
//     (proves our DefinitionHash and state-key reimplementation match what
//     Codex computes).
func TestCodexTrustPreseeding(t *testing.T) {
	t.Parallel()
	requireE2E(t, "codex")

	t.Run("tampered hash is not trusted", func(t *testing.T) {
		home := codexHome(t)
		rec := newRecorder(t, "")
		installHooks(t, rec, agenthooks.ProviderCodex, install.ScopeUser, home)
		tamperTrust(t, filepath.Join(home, "config.toml"))

		runCodex(t, t.TempDir(), home, "Reply with exactly the word: pong")
		if evs := rec.events(t); len(evs) != 0 {
			t.Errorf("hooks ran despite tampered trusted_hash — trust gate not effective:\n%s", summarize(evs))
		}
	})

	t.Run("pre-seeded hash is trusted", func(t *testing.T) {
		rec, proj := runToolTurn(t, func() (recorder, string) {
			home := codexHome(t)
			rec := newRecorder(t, "")
			installHooks(t, rec, agenthooks.ProviderCodex, install.ScopeUser, home)
			proj := t.TempDir()
			runCodex(t, proj, home, shellMarkerPrompt("e2e-marker.txt"))
			return rec, proj
		})
		evs := rec.events(t)
		requireKinds(t, evs,
			agenthooks.KindSessionStart,
			agenthooks.KindPromptSubmitted,
			agenthooks.KindToolPre,
			agenthooks.KindStop,
		)
		if !markerExists(proj, "e2e-marker.txt") {
			t.Log("note: agent did not create the marker (tool may have been blocked or skipped)")
		}
		for _, e := range typedToolPres(evs) {
			if e.Canonical == string(agenthooks.ToolShell) {
				return
			}
		}
		t.Errorf("no shell tool.pre normalized from codex; got:\n%s", summarize(evs))
	})
}

// TestCodexDeny verifies the encode path: a Deny decision from the handler
// must block the tool call in the real agent.
func TestCodexDeny(t *testing.T) {
	t.Parallel()
	requireE2E(t, "codex")
	rec, proj := runToolTurn(t, func() (recorder, string) {
		home := codexHome(t)
		rec := newRecorder(t, string(agenthooks.ToolShell))
		installHooks(t, rec, agenthooks.ProviderCodex, install.ScopeUser, home)
		proj := t.TempDir()
		runCodex(t, proj, home, shellMarkerPrompt("denied-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre)
	if markerExists(proj, "denied-marker.txt") {
		t.Error("marker exists: deny decision did not block the shell command")
	}
}

// tamperTrust flips a character in every trusted_hash value in config.toml.
func tamperTrust(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.ReplaceAll(string(data), `trusted_hash = "sha256:`, `trusted_hash = "sha256:0`)
	if tampered == string(data) {
		t.Fatalf("no trusted_hash entries found to tamper in %s:\n%s", path, data)
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
}
