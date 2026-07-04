// Package agenthookstest provides the golden fixture corpus and a
// fake-provider harness so hook binaries built on agenthooks can be
// integration-tested in CI without the actual agents (DESIGN.md §10).
package agenthookstest

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os/exec"
	"path"
	"strings"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

//go:embed fixtures
var FixturesFS embed.FS

// Fixture returns one captured provider payload, e.g. "claude/pre_tool_use.json".
func Fixture(tb testing.TB, name string) []byte {
	tb.Helper()
	b, err := fs.ReadFile(FixturesFS, path.Join("fixtures", name))
	if err != nil {
		tb.Fatalf("agenthookstest: %v", err)
	}
	return b
}

// Fixtures returns every fixture under one provider directory, keyed by
// filename.
func Fixtures(tb testing.TB, provider string) map[string][]byte {
	tb.Helper()
	entries, err := fs.ReadDir(FixturesFS, path.Join("fixtures", provider))
	if err != nil {
		tb.Fatalf("agenthookstest: %v", err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out[e.Name()] = Fixture(tb, path.Join(provider, e.Name()))
	}
	return out
}

// FixtureDir maps a Provider to its fixture directory name.
func FixtureDir(p agenthooks.Provider) string {
	switch p {
	case agenthooks.ProviderClaudeCode:
		return "claude"
	case agenthooks.ProviderCodex:
		return "codex"
	case agenthooks.ProviderCursor:
		return "cursor"
	case agenthooks.ProviderGemini:
		return "gemini"
	case agenthooks.ProviderOpenCode:
		return "opencode"
	case agenthooks.ProviderKimi:
		return "kimi"
	}
	return string(p)
}

// Result captures one hook invocation's wire output.
type Result struct {
	Stdout   []byte
	Stderr   string
	ExitCode int
}

// Invoke runs the Runner in-process exactly as the generated config would:
// stdin payload, --provider flag, streams captured. For Cursor tool events
// construct the Runner with agenthooks.WithoutDedup() (or a per-test
// WithDedupDir) so cross-test markers can't collide. Tests that count
// handler invocations should also pass agenthooks.WithoutBackfill() (or an
// isolated WithDedupDir): prompt-implied events can synthesize a
// reporting-only prompt.submitted, keyed by machine-global session markers.
func Invoke(tb testing.TB, r *agenthooks.Runner, provider agenthooks.Provider, payload []byte, extraArgs ...string) Result {
	tb.Helper()
	args := append([]string{"agenthooks", "run", "--provider=" + string(provider)}, extraArgs...)
	var stdout, stderr bytes.Buffer
	code := r.Run(context.Background(), args, bytes.NewReader(payload), &stdout, &stderr)
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.String(), ExitCode: code}
}

// AssertNoOp fails unless the result is the provider's correct "no opinion"
// form — the round-trip property of DESIGN.md §5.4.
func AssertNoOp(tb testing.TB, provider agenthooks.Provider, res Result) {
	tb.Helper()
	if res.ExitCode != 0 {
		tb.Fatalf("no-op exit code = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
	}
	got := strings.TrimSpace(string(res.Stdout))
	switch provider {
	case agenthooks.ProviderCodex, agenthooks.ProviderKimi:
		// Codex rejects unknown JSON; Kimi appends exit-0 stdout to the model
		// context. Both need a truly empty no-op.
		if got != "" {
			tb.Fatalf("%s no-op stdout = %q, want empty", provider, got)
		}
	case agenthooks.ProviderOpenCode:
		var reply struct {
			Output map[string]any `json:"output"`
			Error  string         `json:"error"`
		}
		if err := json.Unmarshal(res.Stdout, &reply); err != nil {
			tb.Fatalf("opencode no-op stdout = %q: %v", got, err)
		}
		if reply.Error != "" || len(reply.Output) > 0 {
			tb.Fatalf("opencode no-op carries a decision: %q", got)
		}
	default:
		if got != "{}" {
			tb.Fatalf("%s no-op stdout = %q, want {}", provider, got)
		}
	}
}

// FakeAgent spawns a consumer binary exactly like a provider does: payload on
// stdin (or argv), provider env cross-set the way real agents cross-set it,
// and the provider's exit-code interpretation left to the caller.
type FakeAgent struct {
	Provider agenthooks.Provider
	Binary   string
	Args     []string // defaults to the generated-config argv contract
	Env      []string // appended to the inherited environment
}

// Invoke runs the binary against one payload.
func (f FakeAgent) Invoke(ctx context.Context, payload []byte) (Result, error) {
	args := f.Args
	if args == nil {
		args = []string{"agenthooks", "run", "--provider=" + string(f.Provider)}
	}
	cmd := exec.CommandContext(ctx, f.Binary, args...)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if f.Env != nil {
		cmd.Env = append(cmd.Environ(), f.Env...)
	}
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		err = nil
	}
	return res, err
}
