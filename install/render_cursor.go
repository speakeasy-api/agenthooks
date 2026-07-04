package install

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/speakeasy-api/agenthooks"
)

// kindToCursor expands unified kinds to Cursor hook event lists. tool.pre
// subscribes both the generic and the specific events; the runner dedupes
// the double fire (quirk #2) so decisions stay single-shot.
var kindToCursor = map[agenthooks.EventKind][]string{
	agenthooks.KindSessionStart:    {"sessionStart"},
	agenthooks.KindSessionEnd:      {"sessionEnd"},
	agenthooks.KindPromptSubmitted: {"beforeSubmitPrompt"},
	agenthooks.KindToolPre:         {"beforeShellExecution", "beforeMCPExecution", "beforeReadFile", "preToolUse"},
	agenthooks.KindToolPost:        {"afterShellExecution", "afterMCPExecution", "postToolUse"},
	agenthooks.KindToolError:       {"postToolUseFailure"},
	agenthooks.KindStop:            {"stop"},
	agenthooks.KindSubagentStart:   {"subagentStart"},
	agenthooks.KindSubagentStop:    {"subagentStop"},
	agenthooks.KindCompactPre:      {"preCompact"},
	agenthooks.KindFileEdited:      {"afterFileEdit"},
	// Cursor's assistant-message reports have no unified decision kind; they
	// normalize to model.response and are observation-only. Subscribing the
	// kind opts into both native events.
	agenthooks.KindModelResponse: {"afterAgentResponse", "afterAgentThought"},
}

type cursorHookEntry struct {
	Command    string `json:"command"`
	Timeout    int    `json:"timeout,omitempty"`
	FailClosed bool   `json:"failClosed,omitempty"`
}

func renderCursor(m Manifest, t Target) (fs.FS, error) {
	hooks := map[string][]cursorHookEntry{}
	for _, spec := range m.Hooks {
		events, ok := kindToCursor[spec.Kind]
		if !ok {
			continue
		}
		// A scheme:// URL anywhere in any command makes Cursor silently drop
		// the entire hooks.json (quirk #29). Fail generation instead.
		cmd := hookCommand(m, agenthooks.ProviderCursor, spec)
		if strings.Contains(cmd, "://") {
			return nil, fmt.Errorf("install: cursor drops the whole hooks.json when a command contains a URL (quirk #29); encode endpoints scheme-less or read them from a config file: %q", cmd)
		}
		// Cursor's default is fail-open: a crashed hook allows the action
		// (quirk #7). Decision hooks get failClosed when the manifest policy
		// is FailClosed; telemetry hooks stay fail-open.
		failClosed := m.Fail == agenthooks.FailClosed && spec.Blocking &&
			(spec.Kind == agenthooks.KindToolPre || spec.Kind == agenthooks.KindPromptSubmitted)
		for _, event := range events {
			hooks[event] = append(hooks[event], cursorHookEntry{
				Command:    cmd,
				Timeout:    timeoutSeconds(spec),
				FailClosed: failClosed,
			})
		}
	}
	content, err := jsonFile(map[string]any{
		"version": 1,
		"hooks":   hooks,
	})
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
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
		files[".cursor-plugin/plugin.json"] = pluginJSON
		files["hooks/hooks.json"] = content
	case ScopeProject:
		files[".cursor/hooks.json"] = content
	default:
		files["hooks.json"] = content // Target.Dir is ~/.cursor
	}
	return memFS(files), nil
}
