package install

import (
	"fmt"
	"io/fs"

	"github.com/speakeasy-api/agenthooks"
)

var kindToGemini = map[agenthooks.EventKind]string{
	agenthooks.KindSessionStart:    "SessionStart",
	agenthooks.KindSessionEnd:      "SessionEnd",
	agenthooks.KindPromptSubmitted: "BeforeAgent",
	agenthooks.KindToolPre:         "BeforeTool",
	agenthooks.KindToolPost:        "AfterTool",
	agenthooks.KindStop:            "AfterAgent",
	agenthooks.KindCompactPre:      "PreCompress",
	agenthooks.KindNotification:    "Notification",
}

type geminiHookCmd struct {
	Type        string `json:"type"`
	Command     string `json:"command"`
	Timeout     int64  `json:"timeout,omitempty"` // milliseconds (quirk #14)
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type geminiMatcherEntry struct {
	Matcher string          `json:"matcher,omitempty"`
	Hooks   []geminiHookCmd `json:"hooks"`
}

func renderGemini(m Manifest, t Target) (fs.FS, error) {
	hooks := map[string][]geminiMatcherEntry{}
	for _, spec := range m.Hooks {
		event, ok := kindToGemini[spec.Kind]
		if !ok {
			continue
		}
		matcher, _ := agenthooks.CompileMatcher(agenthooks.ProviderGemini, spec.Tools)
		name := m.Identity.Name
		if name == "" {
			name = "agenthooks"
		}
		hooks[event] = append(hooks[event], geminiMatcherEntry{
			Matcher: matcher,
			Hooks: []geminiHookCmd{{
				Type:    "command",
				Command: hookCommand(m, agenthooks.ProviderGemini, spec),
				// Gemini timeouts are milliseconds where everyone else uses
				// seconds (quirk #14).
				Timeout: int64(timeoutSeconds(spec)) * 1000,
				// name/description enable the /hooks enable|disable UX (§7).
				Name:        fmt.Sprintf("%s:%s", name, spec.Kind),
				Description: m.Identity.Description,
			}},
		})
	}
	content, err := jsonFile(map[string]any{"hooks": hooks})
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	if t.Scope == ScopeProject {
		files[".gemini/settings.json"] = content
	} else {
		files["settings.json"] = content // Target.Dir is ~/.gemini
	}
	return memFS(files), nil
}
