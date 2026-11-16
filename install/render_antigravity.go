package install

import (
	"io/fs"

	"github.com/speakeasy-api/agenthooks"
)

// Antigravity's hooks.json is keyed by handler name, each carrying one list
// per event (quirk #51). Tool events wrap commands in matcher groups; the
// others take a flat command list. Payloads carry no event name, so every
// command passes --event=<native>.
var kindToAntigravity = map[agenthooks.EventKind]string{
	agenthooks.KindSessionStart:  "SessionStart",
	agenthooks.KindToolPre:       "PreToolUse",
	agenthooks.KindToolPost:      "PostToolUse",
	agenthooks.KindStop:          "Stop",
	agenthooks.KindModelRequest:  "PreInvocation",
	agenthooks.KindModelResponse: "PostInvocation",
}

type antigravityHookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds
}

type antigravityMatcherEntry struct {
	Matcher string               `json:"matcher"`
	Hooks   []antigravityHookCmd `json:"hooks"`
}

func renderAntigravity(m Manifest, t Target) (fs.FS, error) {
	name := m.Identity.Name
	if name == "" {
		name = "agenthooks"
	}
	handler := map[string]any{"enabled": true}
	for _, spec := range m.Hooks {
		event, ok := kindToAntigravity[spec.Kind]
		if !ok {
			continue
		}
		cmd := antigravityHookCmd{
			Type:    "command",
			Command: hookCommand(m, agenthooks.ProviderAntigravity, spec) + " --event=" + event,
			Timeout: timeoutSeconds(spec),
		}
		if spec.Kind == agenthooks.KindToolPre || spec.Kind == agenthooks.KindToolPost {
			// The matcher regex can't express canonical classes or MCP globs
			// (every MCP call is call_mcp_tool), so match all tools and let
			// hookCommand's --filter narrow in-process.
			entries, _ := handler[event].([]antigravityMatcherEntry)
			handler[event] = append(entries, antigravityMatcherEntry{Matcher: "*", Hooks: []antigravityHookCmd{cmd}})
			continue
		}
		cmds, _ := handler[event].([]antigravityHookCmd)
		handler[event] = append(cmds, cmd)
	}
	content, err := jsonFile(map[string]any{name: handler})
	if err != nil {
		return nil, err
	}
	path := "hooks.json" // user scope: Target.Dir is ~/.gemini/config; plugin scope: the plugin dir
	if t.Scope == ScopeProject {
		path = ".agents/hooks.json"
	}
	return memFS(map[string][]byte{path: content}), nil
}
