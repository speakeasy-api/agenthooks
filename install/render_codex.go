package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/speakeasy-api/agenthooks"
)

var kindToCodex = map[agenthooks.EventKind]string{
	agenthooks.KindSessionStart:    "SessionStart",
	agenthooks.KindSessionEnd:      "SessionEnd",
	agenthooks.KindPromptSubmitted: "UserPromptSubmit",
	agenthooks.KindToolPre:         "PreToolUse",
	agenthooks.KindToolPost:        "PostToolUse",
	agenthooks.KindPermission:      "PermissionRequest",
	agenthooks.KindStop:            "Stop",
	agenthooks.KindSubagentStart:   "SubagentStart",
	agenthooks.KindSubagentStop:    "SubagentStop",
	agenthooks.KindCompactPre:      "PreCompact",
	agenthooks.KindCompactPost:     "PostCompact",
}

func renderCodex(m Manifest, t Target) (fs.FS, error) {
	hooks := map[string][]claudeMatcherEntry{} // Codex uses the Claude hooks.json shape
	type trustEntry struct {
		key  string
		hash string
	}
	var trust []trustEntry
	// Codex keys hook state by "<source>:<event_label>:<group>:<handler>"
	// where source is the absolute path of the hooks.json it discovered. Codex
	// 0.152.1 on Linux retains the lexical CODEX_HOME symlink in that key,
	// whereas other builds/platforms canonicalize it. Seed both identities when
	// they differ so an archived/symlinked CODEX_HOME stays non-interactive.
	dir := t.Dir
	if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	sources := []string{filepath.Join(dir, "hooks.json")}
	if resolved, err := evalSymlinksAllowMissing(dir); err == nil && resolved != dir {
		sources = append(sources, filepath.Join(resolved, "hooks.json"))
	}
	for _, spec := range m.Hooks {
		event, ok := kindToCodex[spec.Kind]
		if !ok {
			continue
		}
		matcher, _ := agenthooks.CompileMatcher(agenthooks.ProviderCodex, spec.Tools)
		command := hookCommand(m, agenthooks.ProviderCodex, spec)
		// Codex parses-but-skips async:true (quirk #10): telemetry hooks get
		// --async, which makes the runner re-exec itself as a detached worker
		// and return immediately — no shell involved.
		if !spec.Blocking {
			command += " --async"
		}
		secs := timeoutSeconds(spec)
		if event == "SessionEnd" && secs > 3 {
			secs = 3
		}
		for _, source := range sources {
			trust = append(trust, trustEntry{
				key:  fmt.Sprintf("%s:%s:%d:0", source, codexEventLabel[event], len(hooks[event])),
				hash: DefinitionHash(event, matcher, command, secs),
			})
		}
		hooks[event] = append(hooks[event], claudeMatcherEntry{
			Matcher: matcher,
			Hooks: []claudeHookCmd{{
				Type:    "command",
				Command: command,
				Timeout: secs,
			}},
		})
	}
	hooksJSON, err := jsonFile(map[string]any{"hooks": hooks})
	if err != nil {
		return nil, err
	}

	// Codex hooks require user trust of the definition hash (quirk #9):
	// installs pre-seed [hooks.state] tables so generated hooks run without
	// an interactive trust prompt. Rendered into config.toml inside the
	// managed marker region (merged like Kimi's TOML configs).
	sort.Slice(trust, func(i, j int) bool { return trust[i].key < trust[j].key })
	var toml strings.Builder
	toml.WriteString(tomlBeginMarker + "\n")
	for _, e := range trust {
		fmt.Fprintf(&toml, "\n[hooks.state.%s]\n", tomlString(e.key))
		fmt.Fprintf(&toml, "trusted_hash = %s\n", tomlString(e.hash))
	}
	toml.WriteString("\n" + tomlEndMarker + "\n")

	return memFS(map[string][]byte{
		"hooks.json":  hooksJSON,
		"config.toml": []byte(toml.String()),
	}), nil
}

// evalSymlinksAllowMissing resolves the longest existing prefix of path and
// then restores any missing suffix. Render must stay side-effect free, but a
// fresh CODEX_HOME can still sit below a symlinked parent.
func evalSymlinksAllowMissing(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// mergeCodexTrustTOML replaces the managed trust region and removes stale
// copies of those exact hook-state tables elsewhere in config.toml. Codex may
// have persisted interactive trust before agenthooks began managing a hook;
// leaving both copies makes the whole TOML document invalid.
func mergeCodexTrustTOML(existing, rendered []byte) []byte {
	targets := make(map[string]struct{})
	for _, line := range strings.Split(string(rendered), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[hooks.state.") && strings.HasSuffix(line, "]") {
			targets[line] = struct{}{}
		}
	}

	withoutManaged := stripManagedTOMLRegion(string(existing))
	var cleaned strings.Builder
	skipping := false
	for _, line := range strings.SplitAfter(withoutManaged, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			_, skipping = targets[trimmed]
		}
		if !skipping {
			cleaned.WriteString(line)
		}
	}
	return mergeManagedTOML([]byte(cleaned.String()), rendered)
}

func stripManagedTOMLRegion(existing string) string {
	begin := strings.Index(existing, tomlBeginMarker)
	if begin < 0 {
		return existing
	}
	end := strings.Index(existing[begin:], tomlEndMarker)
	if end < 0 {
		return existing[:begin]
	}
	tail := existing[begin+end+len(tomlEndMarker):]
	return existing[:begin] + strings.TrimPrefix(tail, "\n")
}

// renderCodexTrustForMergedHooks computes state keys after hooks.json has been
// merged with foreign handlers. Their preceding groups change agenthooks'
// actual group indexes, so hashes rendered from the standalone manifest would
// be trusted under the wrong identities.
func renderCodexTrustForMergedHooks(t Target, hooksJSON []byte) ([]byte, error) {
	var config struct {
		Hooks map[string][]claudeMatcherEntry `json:"hooks"`
	}
	if err := json.Unmarshal(hooksJSON, &config); err != nil {
		return nil, fmt.Errorf("install: decode merged Codex hooks for trust: %w", err)
	}

	dir := t.Dir
	if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	sources := []string{filepath.Join(dir, "hooks.json")}
	if resolved, err := evalSymlinksAllowMissing(dir); err == nil && resolved != dir {
		sources = append(sources, filepath.Join(resolved, "hooks.json"))
	}

	type trustEntry struct {
		key  string
		hash string
	}
	var trust []trustEntry
	for event, groups := range config.Hooks {
		label, ok := codexEventLabel[event]
		if !ok {
			continue
		}
		for groupIndex, group := range groups {
			for handlerIndex, hook := range group.Hooks {
				if !strings.Contains(hook.Command, "agenthooks") {
					continue
				}
				for _, source := range sources {
					trust = append(trust, trustEntry{
						key: fmt.Sprintf(
							"%s:%s:%d:%d", source, label, groupIndex, handlerIndex,
						),
						hash: DefinitionHash(event, group.Matcher, hook.Command, hook.Timeout),
					})
				}
			}
		}
	}
	sort.Slice(trust, func(i, j int) bool { return trust[i].key < trust[j].key })
	var output strings.Builder
	output.WriteString(tomlBeginMarker + "\n")
	for _, entry := range trust {
		fmt.Fprintf(&output, "\n[hooks.state.%s]\n", tomlString(entry.key))
		fmt.Fprintf(&output, "trusted_hash = %s\n", tomlString(entry.hash))
	}
	output.WriteString("\n" + tomlEndMarker + "\n")
	return []byte(output.String()), nil
}
