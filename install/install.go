// Package install renders and installs provider hook configurations from a
// single Go Manifest: correct hooks.json / settings.json / config.toml /
// plugin scaffolding per provider, with the per-provider timing/async/
// fail-mode workarounds baked in (DESIGN.md §7).
package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing/fstest"
	"time"

	"github.com/speakeasy-api/agenthooks"
)

// Scope selects where the configuration lands.
type Scope string

const (
	ScopeUser    Scope = "user"    // provider's user-level config dir
	ScopeProject Scope = "project" // repo-relative config
	ScopePlugin  Scope = "plugin"  // plugin-based install (Claude Code)
)

// Target names a provider plus scope; Dir is the filesystem root Install
// writes under (ignored by Render, which returns a relative fs.FS).
type Target struct {
	Provider agenthooks.Provider
	Scope    Scope
	Dir      string
}

// Identity is the plugin name/version/description for plugin-based installs
// and for Gemini's /hooks enable|disable UX.
type Identity struct {
	Name        string
	Version     string
	Description string
}

// ToolMatcher aliases the unified matcher (compiled to provider dialects
// where expressible, enforced in-process via --filter where not).
type ToolMatcher = agenthooks.ToolMatcher

// HookSpec subscribes one unified event kind.
type HookSpec struct {
	Kind     agenthooks.EventKind
	Tools    ToolMatcher
	Blocking bool // decision path vs telemetry path
	Timeout  time.Duration
}

// Manifest is the single source from which every provider config is
// generated.
type Manifest struct {
	Command  []string // how to invoke the consumer binary (abs path or PATH lookup)
	Hooks    []HookSpec
	Identity Identity
	// Fail drives fail-mode workarounds in generated config (e.g. Cursor's
	// failClosed flag, quirk #7). Match the Policy your Runner uses.
	Fail agenthooks.FailMode
}

// Render produces the provider configuration as an in-memory filesystem with
// target-relative paths. Kinds a provider cannot subscribe to (see the
// mapping table, §3.4) are skipped; model.request/model.response are excluded
// from generation by design (§12.3).
func Render(m Manifest, t Target) (fs.FS, error) {
	if len(m.Command) == 0 {
		return nil, errors.New("install: Manifest.Command is required")
	}
	switch t.Provider {
	case agenthooks.ProviderClaudeCode:
		return renderClaude(m, t)
	case agenthooks.ProviderCursor:
		return renderCursor(m, t)
	case agenthooks.ProviderCodex:
		return renderCodex(m, t)
	case agenthooks.ProviderGemini:
		return renderGemini(m, t)
	case agenthooks.ProviderOpenCode:
		return renderOpenCode(m, t)
	case agenthooks.ProviderKimi:
		return renderKimi(m, t)
	}
	return nil, fmt.Errorf("install: unknown provider %q", t.Provider)
}

// ChangeState classifies one file in a Diff.
type ChangeState string

const (
	StateCreate    ChangeState = "create"
	StateUpdate    ChangeState = "update"
	StateUnchanged ChangeState = "unchanged"
)

// Change is one file-level difference between the rendered config and disk.
type Change struct {
	Path  string
	State ChangeState
}

// InstallOption configures Install.
type InstallOption func(*installCfg)

type installCfg struct {
	dryRun bool
}

// WithDryRun plans without writing.
func WithDryRun() InstallOption {
	return func(c *installCfg) { c.dryRun = true }
}

// Install renders the manifest and writes it under t.Dir, idempotently:
// unchanged files are skipped, and JSON settings files are merged so
// agenthooks-managed entries are replaced while foreign entries survive.
func Install(ctx context.Context, m Manifest, t Target, opts ...InstallOption) error {
	var cfg installCfg
	for _, o := range opts {
		o(&cfg)
	}
	if t.Dir == "" {
		return errors.New("install: Target.Dir is required")
	}
	planned, err := plan(m, t)
	if err != nil {
		return err
	}
	for _, p := range planned {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.state == StateUnchanged || cfg.dryRun {
			continue
		}
		dest := filepath.Join(t.Dir, filepath.FromSlash(p.path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("install: %w", err)
		}
		if err := os.WriteFile(dest, p.final, 0o644); err != nil {
			return fmt.Errorf("install: %w", err)
		}
	}
	return nil
}

// Diff reports what Install would do, without writing.
func Diff(m Manifest, t Target) ([]Change, error) {
	planned, err := plan(m, t)
	if err != nil {
		return nil, err
	}
	out := make([]Change, len(planned))
	for i, p := range planned {
		out[i] = Change{Path: p.path, State: p.state}
	}
	return out, nil
}

// Fingerprint hashes the rendered configuration — the idempotence key for
// "re-install only when the manifest changed" flows.
func Fingerprint(m Manifest, t Target) (string, error) {
	rendered, err := Render(m, t)
	if err != nil {
		return "", err
	}
	paths, contents, err := flatten(rendered)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for i, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(contents[i])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type plannedFile struct {
	path  string
	final []byte
	state ChangeState
}

func plan(m Manifest, t Target) ([]plannedFile, error) {
	rendered, err := Render(m, t)
	if err != nil {
		return nil, err
	}
	paths, contents, err := flatten(rendered)
	if err != nil {
		return nil, err
	}
	var out []plannedFile
	for i, p := range paths {
		content := contents[i]
		dest := filepath.Join(t.Dir, filepath.FromSlash(p))
		existing, readErr := os.ReadFile(dest)
		switch {
		case readErr != nil:
			out = append(out, plannedFile{path: p, final: content, state: StateCreate})
		default:
			final := content
			if isMergeableJSON(p) {
				if merged, mergeErr := mergeManagedJSON(existing, content); mergeErr == nil {
					final = merged
				}
			} else if isMergeableTOML(p) {
				final = mergeManagedTOML(existing, content)
			}
			state := StateUpdate
			if bytes.Equal(existing, final) {
				state = StateUnchanged
			}
			out = append(out, plannedFile{path: p, final: final, state: state})
		}
	}
	return out, nil
}

func flatten(fsys fs.FS) (paths []string, contents [][]byte, err error) {
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		b, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}
		paths = append(paths, path)
		contents = append(contents, b)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Sort(&pairSort{paths, contents})
	return paths, contents, nil
}

type pairSort struct {
	paths    []string
	contents [][]byte
}

func (s *pairSort) Len() int           { return len(s.paths) }
func (s *pairSort) Less(i, j int) bool { return s.paths[i] < s.paths[j] }
func (s *pairSort) Swap(i, j int) {
	s.paths[i], s.paths[j] = s.paths[j], s.paths[i]
	s.contents[i], s.contents[j] = s.contents[j], s.contents[i]
}

func isMergeableJSON(path string) bool {
	base := filepath.Base(path)
	return base == "settings.json" || base == "hooks.json"
}

// isMergeableTOML matches Kimi's shared config files, merged via the managed
// marker region (render_kimi.go). Codex's agenthooks-trust.toml is fully
// owned by agenthooks and deliberately excluded.
func isMergeableTOML(path string) bool {
	base := filepath.Base(path)
	return base == "config.toml" || base == "local.toml"
}

// mergeManagedJSON overlays rendered config onto an existing settings file:
// under "hooks", entries recognizably ours (command mentions "agenthooks")
// are replaced by the rendered set while foreign entries survive; every other
// rendered top-level key overwrites.
func mergeManagedJSON(existing, rendered []byte) ([]byte, error) {
	var ex, re map[string]json.RawMessage
	if err := json.Unmarshal(existing, &ex); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rendered, &re); err != nil {
		return nil, err
	}
	for k, v := range re {
		if k != "hooks" {
			ex[k] = v
			continue
		}
		var exHooks, reHooks map[string][]json.RawMessage
		if err := json.Unmarshal(v, &reHooks); err != nil {
			ex[k] = v
			continue
		}
		if raw, ok := ex[k]; ok {
			if err := json.Unmarshal(raw, &exHooks); err != nil {
				exHooks = nil
			}
		}
		if exHooks == nil {
			exHooks = map[string][]json.RawMessage{}
		}
		for event, entries := range reHooks {
			var kept []json.RawMessage
			for _, e := range exHooks[event] {
				if !strings.Contains(string(e), "agenthooks") {
					kept = append(kept, e)
				}
			}
			exHooks[event] = append(kept, entries...)
		}
		merged, err := json.Marshal(exHooks)
		if err != nil {
			return nil, err
		}
		ex[k] = merged
	}
	return json.MarshalIndent(ex, "", "  ")
}

// hookCommand renders the shell command a provider config invokes, including
// the argv contract (--provider, --timeout) and the in-process --filter for
// dialects that can't express the matcher.
func hookCommand(m Manifest, p agenthooks.Provider, spec HookSpec) string {
	parts := make([]string, 0, len(m.Command)+5)
	for _, c := range m.Command {
		parts = append(parts, shellQuote(c))
	}
	parts = append(parts, "agenthooks", "run", "--provider="+string(p))
	if spec.Timeout > 0 {
		parts = append(parts, "--timeout="+spec.Timeout.String())
	}
	if !spec.Tools.IsEmpty() {
		if _, ok := agenthooks.CompileMatcher(p, spec.Tools); !ok {
			parts = append(parts, "--filter="+shellQuoteBody(spec.Tools.Encode()))
		}
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes a command argument for the shell the provider will run
// hook commands with. Configs are rendered on the machine that runs them, so
// the host OS decides the dialect: cmd.exe has no single-quote syntax, POSIX
// shells have no cmd-style escaping.
func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		if s == "" {
			return `""`
		}
		if strings.ContainsAny(s, " \t\"^&|<>()%!") {
			return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
		}
		return s
	}
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, " \t\n\"'\\$&|;<>(){}*?#~`!") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// shellQuoteBody quotes only when needed, for flag values.
func shellQuoteBody(s string) string {
	if runtime.GOOS == "windows" {
		if strings.ContainsAny(s, " \t\"^&|<>()%!") {
			return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
		}
		return s
	}
	if strings.ContainsAny(s, " \t\n\"'\\$&|<>(){}#~`!") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

func timeoutSeconds(spec HookSpec) int {
	if spec.Timeout > 0 {
		secs := int((spec.Timeout + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		return secs
	}
	// SessionStart interactive flows get raised timeouts (§7).
	if spec.Kind == agenthooks.KindSessionStart {
		return 120
	}
	return 60
}

func jsonFile(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func memFS(files map[string][]byte) fs.FS {
	out := fstest.MapFS{}
	for p, b := range files {
		out[p] = &fstest.MapFile{Data: b, Mode: 0o644}
	}
	return out
}
