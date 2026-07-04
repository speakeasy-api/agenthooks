package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
)

// recorderBin is built once in TestMain from testdata/recorder.
var recorderBin string

func TestMain(m *testing.M) {
	if os.Getenv("AGENTHOOKS_E2E") != "1" {
		// Tests skip individually; skip the build too.
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "agenthooks-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	recorderBin = filepath.Join(dir, "recorder")
	build := exec.Command("go", "build", "-o", recorderBin, "./testdata/recorder")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building recorder:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// requireE2E gates every test on the explicit opt-in (real agents, real
// tokens) and on the agent binary being installed. Extra lookup paths cover
// agents that install outside PATH.
func requireE2E(t *testing.T, agent string, extraPaths ...string) string {
	t.Helper()
	if os.Getenv("AGENTHOOKS_E2E") != "1" {
		t.Skip("set AGENTHOOKS_E2E=1 to run end-to-end agent tests (spends real tokens)")
	}
	if p, err := exec.LookPath(agent); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range extraPaths {
		p = strings.ReplaceAll(p, "~", home)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skipf("agent %q not installed", agent)
	return ""
}

// recorder is one per-test instance of the recorder hook binary: a hard link
// to the shared build with its own sidecar config and events sink.
type recorder struct {
	Bin    string
	Events string
}

// newRecorder links the recorder binary into a per-test directory and writes
// its sidecar config. deny names a CanonicalTool class to deny ("" = allow).
func newRecorder(t *testing.T, deny string) recorder {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "recorder")
	if err := os.Link(recorderBin, bin); err != nil {
		src, err := os.ReadFile(recorderBin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, src, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rec := recorder{Bin: bin, Events: filepath.Join(dir, "events.jsonl")}
	cfg, err := json.Marshal(map[string]string{"out": rec.Events, "deny": deny})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+".e2e.json", cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	return rec
}

// manifest is the standard hook subscription the suite installs everywhere.
func manifest(bin string) install.Manifest {
	return install.Manifest{
		Command: []string{bin},
		Hooks: []install.HookSpec{
			{Kind: agenthooks.KindSessionStart, Blocking: true, Timeout: 30 * time.Second},
			{Kind: agenthooks.KindPromptSubmitted, Blocking: true, Timeout: 30 * time.Second},
			{Kind: agenthooks.KindToolPre, Blocking: true, Timeout: 30 * time.Second},
			{Kind: agenthooks.KindToolPost, Blocking: true, Timeout: 30 * time.Second},
			{Kind: agenthooks.KindToolError, Blocking: true, Timeout: 30 * time.Second},
			{Kind: agenthooks.KindStop, Blocking: true, Timeout: 30 * time.Second},
		},
		Identity: install.Identity{Name: "agenthooks-e2e", Version: "0.0.1", Description: "agenthooks e2e recorder"},
		Fail:     agenthooks.FailOpen,
	}
}

// installHooks renders and installs the standard manifest for a provider.
func installHooks(t *testing.T, rec recorder, provider agenthooks.Provider, scope install.Scope, dir string) {
	t.Helper()
	err := install.Install(context.Background(), manifest(rec.Bin), install.Target{
		Provider: provider,
		Scope:    scope,
		Dir:      dir,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runAgent runs one headless agent turn with a hard deadline, from dir, with
// extra env appended. Returns combined output; the exit code is reported via
// err (nil on 0).
func runAgent(t *testing.T, dir string, env []string, bin string, args ...string) (string, error) {
	t.Helper()
	return runAgentIn(t, dir, env, "", bin, args...)
}

// runAgentIn is runAgent with the prompt (or any input) piped via stdin.
func runAgentIn(t *testing.T, dir string, env []string, stdin, bin string, args ...string) (string, error) {
	t.Helper()
	const timeout = 5 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// cmd.Dir changes the real cwd but not the inherited $PWD, and some
	// agents (opencode) trust $PWD for project resolution.
	base := os.Environ()[:0:0]
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PWD=") {
			base = append(base, kv)
		}
	}
	cmd.Env = append(append(base, "PWD="+dir), env...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("agent %s timed out after %s\noutput:\n%s", bin, timeout, tail(string(out), 4000))
	}
	t.Logf("agent output (%s):\n%s", filepath.Base(bin), tail(string(out), 4000))
	return string(out), err
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// event mirrors the recorder's JSONL schema.
type event struct {
	Typed      bool            `json:"typed"`
	Backfilled bool            `json:"backfilled"`
	TimeMS     int64           `json:"time_ms"`
	Provider   string          `json:"provider"`
	Variant    string          `json:"variant"`
	Native     string          `json:"native"`
	Kind       string          `json:"kind"`
	Session    string          `json:"session_id"`
	CWD        string          `json:"cwd"`
	Tool       string          `json:"tool"`
	Canonical  string          `json:"canonical"`
	ToolInput  json.RawMessage `json:"tool_input"`
	Prompt     string          `json:"prompt"`
	Denied     bool            `json:"denied"`
	Raw        json.RawMessage `json:"raw"`
}

// events reads the recorder's sink; missing file means no events (yet).
func (r recorder) events(t *testing.T) []event {
	t.Helper()
	f, err := os.Open(r.Events)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []event
	dec := json.NewDecoder(f)
	for {
		var e event
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("corrupt events file: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// ofKind filters raw (OnAny) records by unified kind.
func ofKind(evs []event, kind agenthooks.EventKind) []event {
	var out []event
	for _, e := range evs {
		if !e.Typed && e.Kind == string(kind) {
			out = append(out, e)
		}
	}
	return out
}

// typedToolPres returns the typed tool.pre records (normalized tool fields).
func typedToolPres(evs []event) []event {
	var out []event
	for _, e := range evs {
		if e.Typed && e.Kind == string(agenthooks.KindToolPre) {
			out = append(out, e)
		}
	}
	return out
}

// summarize renders a compact event log for failure messages.
func summarize(evs []event) string {
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "  typed=%-5v kind=%-18s native=%-24s tool=%s\n", e.Typed, e.Kind, e.Native, e.Tool)
	}
	if b.Len() == 0 {
		return "  (no events recorded)"
	}
	return b.String()
}

// requireKinds asserts at least one raw event of each kind arrived.
func requireKinds(t *testing.T, evs []event, kinds ...agenthooks.EventKind) {
	t.Helper()
	for _, k := range kinds {
		if len(ofKind(evs, k)) == 0 {
			t.Errorf("no %s event recorded; got:\n%s", k, summarize(evs))
		}
	}
}

// requireBackfilledPrompt asserts the runner synthesized a reporting-only
// prompt.submitted — the mitigation for providers whose print modes never
// fire the real event (quirks #30, #31) — and that the prompt text was
// recovered from the provider's session store (it must contain
// promptSubstring). A real (non-backfilled) prompt event means the provider
// fixed the gap upstream and the quirk needs revisiting.
func requireBackfilledPrompt(t *testing.T, evs []event, promptSubstring string) {
	t.Helper()
	found := false
	for _, e := range evs {
		if e.Kind != string(agenthooks.KindPromptSubmitted) {
			continue
		}
		if !e.Backfilled {
			t.Errorf("provider now fires a real prompt.submitted — revisit the backfill quirk: %+v", e)
			continue
		}
		found = true
		if e.Typed && !strings.Contains(e.Prompt, promptSubstring) {
			t.Errorf("backfilled prompt not recovered from session store: want substring %q, got %q", promptSubstring, e.Prompt)
		}
	}
	if !found {
		t.Errorf("no backfilled prompt.submitted recorded; got:\n%s", summarize(evs))
	}
}

// requireNoBackfill asserts nothing was synthesized (the provider delivered
// its events for real).
func requireNoBackfill(t *testing.T, evs []event) {
	t.Helper()
	for _, e := range evs {
		if e.Backfilled {
			t.Errorf("unexpected backfilled event (provider fired the real one): kind=%s", e.Kind)
		}
	}
}

// shellMarkerPrompt instructs the agent to run one exact shell command that
// creates markerName in the working directory. Used both for the allow path
// (marker must exist) and the deny path (marker must not exist). The framing
// matters: bare "create an arbitrary file" gets refused as not a real task,
// and "this is an automated test" reads as prompt injection to some models
// (z-ai/glm-5.2 flagged both) — a purposeful fixture request lands better.
func shellMarkerPrompt(markerName string) string {
	return "Please set up a fixture for me: our test suite expects an empty placeholder file named " +
		markerName + " in the current working directory. It must be created by the shell command `touch " +
		markerName + "` specifically. If that command is blocked or fails, report that and stop — do not " +
		"create the file any other way, and do not run anything else."
}

// runToolTurn runs one agent turn via run (which must build a fresh recorder,
// install, and drive the agent), retrying once if the model made no tool call
// at all — real agents occasionally decline even innocuous prompts, and a
// turn with zero tool.pre events proves nothing either way.
func runToolTurn(t *testing.T, run func() (recorder, string)) (recorder, string) {
	t.Helper()
	rec, proj := run()
	if len(ofKind(rec.events(t), agenthooks.KindToolPre)) > 0 {
		return rec, proj
	}
	t.Log("agent made no tool call (likely a refusal); retrying once")
	return run()
}

func markerExists(dir, markerName string) bool {
	_, err := os.Stat(filepath.Join(dir, markerName))
	return err == nil
}
