package install

import (
	"errors"
	"io/fs"

	"github.com/speakeasy-api/agenthooks"
)

// kindToClaude maps unified kinds to Claude Code hook event names.
// model.request has no mapping; model.response ≈ MessageDisplay is excluded
// from generation by default (§12.3).
var kindToClaude = map[agenthooks.EventKind]string{
	agenthooks.KindSessionStart:    "SessionStart",
	agenthooks.KindSessionEnd:      "SessionEnd",
	agenthooks.KindPromptSubmitted: "UserPromptSubmit",
	agenthooks.KindToolPre:         "PreToolUse",
	agenthooks.KindToolPost:        "PostToolUse",
	agenthooks.KindToolError:       "PostToolUseFailure",
	agenthooks.KindPermission:      "PermissionRequest",
	agenthooks.KindStop:            "Stop",
	agenthooks.KindSubagentStart:   "SubagentStart",
	agenthooks.KindSubagentStop:    "SubagentStop",
	agenthooks.KindCompactPre:      "PreCompact",
	agenthooks.KindCompactPost:     "PostCompact",
	agenthooks.KindNotification:    "Notification",
	agenthooks.KindFileEdited:      "FileChanged",
}

type claudeHookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}

type claudeMatcherEntry struct {
	Matcher string          `json:"matcher,omitempty"`
	Hooks   []claudeHookCmd `json:"hooks"`
}

func renderClaude(m Manifest, t Target) (fs.FS, error) {
	hooks := map[string][]claudeMatcherEntry{}
	for _, spec := range m.Hooks {
		event, ok := kindToClaude[spec.Kind]
		if !ok {
			continue
		}
		matcher, _ := agenthooks.CompileMatcher(agenthooks.ProviderClaudeCode, spec.Tools)
		async := !spec.Blocking
		// cowork drops async Stop hooks (quirk #1): Stop is forced sync.
		if event == "Stop" {
			async = false
		}
		hooks[event] = append(hooks[event], claudeMatcherEntry{
			Matcher: matcher,
			Hooks: []claudeHookCmd{{
				Type:    "command",
				Command: hookCommand(m, agenthooks.ProviderClaudeCode, spec),
				Timeout: timeoutSeconds(spec),
				Async:   async,
			}},
		})
	}

	files := map[string][]byte{}
	hooksJSON, err := jsonFile(map[string]any{"hooks": hooks})
	if err != nil {
		return nil, err
	}

	switch t.Scope {
	case ScopePlugin:
		if m.Identity.Name == "" {
			return nil, errors.New("install: plugin scope requires Identity.Name")
		}
		pluginJSON, err := jsonFile(map[string]string{
			"name":        m.Identity.Name,
			"version":     m.Identity.Version,
			"description": m.Identity.Description,
		})
		if err != nil {
			return nil, err
		}
		files[".claude-plugin/plugin.json"] = pluginJSON
		// Must be at hooks/hooks.json, not the plugin root (§7).
		files["hooks/hooks.json"] = hooksJSON
	case ScopeProject:
		files[".claude/settings.json"] = hooksJSON
	default: // ScopeUser: Target.Dir is ~/.claude
		files["settings.json"] = hooksJSON
	}
	return memFS(files), nil
}
