package agenthooks

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// invocation is the parsed argv contract baked into generated configs:
//
//	mybinary agenthooks run    --provider=claude-code            # stdin JSON
//	mybinary agenthooks run    --provider=cursor --argv-payload  # legacy cursor CLI
//	mybinary agenthooks notify --provider=codex                  # legacy codex notify (argv JSON)
//	mybinary agenthooks serve  --provider=opencode               # NDJSON daemon for the shim
type invocation struct {
	mode        string // "run", "notify", "serve"
	provider    Provider
	variant     Variant
	confidence  DetectionConfidence
	argvPayload bool
	payload     string
	timeout     time.Duration
	filter      *ToolMatcher
}

var validProviders = map[Provider]bool{
	ProviderClaudeCode: true,
	ProviderCursor:     true,
	ProviderCodex:      true,
	ProviderGemini:     true,
	ProviderOpenCode:   true,
	ProviderKimi:       true,
}

func parseArgs(args []string) (*invocation, error) {
	inv := &invocation{mode: "run"}
	rest := args
	// Generated configs put consumer-binary flags before the sentinel
	// ("mybinary --config=x agenthooks serve --provider=opencode"), so the
	// sentinel and mode are located anywhere in argv, not just at the front.
	// Everything before the sentinel belongs to the consumer and is dropped
	// from agenthooks parsing.
	for i, a := range rest {
		if a == "agenthooks" {
			rest = rest[i+1:]
			break
		}
	}
	if len(rest) > 0 {
		switch rest[0] {
		case "run", "notify", "serve":
			inv.mode = rest[0]
			rest = rest[1:]
		}
	}
	var positional []string
	for _, a := range rest {
		switch {
		case a == "--argv-payload":
			inv.argvPayload = true
		case strings.HasPrefix(a, "--provider="):
			p := Provider(strings.TrimPrefix(a, "--provider="))
			if p == "kimi" {
				p = ProviderKimi
			}
			if !validProviders[p] {
				return nil, fmt.Errorf("agenthooks: unknown provider %q", p)
			}
			inv.provider = p
			inv.confidence = DetectionConfig
		case strings.HasPrefix(a, "--variant="):
			inv.variant = Variant(strings.TrimPrefix(a, "--variant="))
		case strings.HasPrefix(a, "--timeout="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--timeout="))
			if err != nil {
				return nil, fmt.Errorf("agenthooks: bad --timeout: %w", err)
			}
			inv.timeout = d
		case strings.HasPrefix(a, "--filter="):
			m, err := ParseToolMatcher(strings.TrimPrefix(a, "--filter="))
			if err != nil {
				return nil, err
			}
			inv.filter = &m
		case strings.HasPrefix(a, "--"):
			// Unknown flags are tolerated for forward compatibility with
			// newer generated configs driving older library versions.
		default:
			positional = append(positional, a)
		}
	}
	inv.payload = strings.Join(positional, " ")
	return inv, nil
}

// detectProvider resolves the invoking provider. Flag-first is a hard rule:
// Codex and Cursor deliberately export CLAUDE_* compat vars (quirk #20), so
// env sniffing alone is insufficient. Shape sniffing is the last resort.
func detectProvider(inv *invocation, payload []byte) (Provider, DetectionConfidence) {
	if inv.provider != "" {
		return inv.provider, DetectionConfig
	}
	if p, ok := detectFromEnv(); ok {
		return p, DetectionEnv
	}
	if p, ok := detectFromShape(payload); ok {
		return p, DetectionShape
	}
	return "", ""
}

func detectFromEnv() (Provider, bool) {
	// Provider-unique vars first; CLAUDE_* last because it is cross-set.
	if os.Getenv("CURSOR_VERSION") != "" || os.Getenv("CURSOR_TRACE_ID") != "" || os.Getenv("CURSOR_AGENT") != "" {
		return ProviderCursor, true
	}
	if os.Getenv("CODEX_HOME") != "" || os.Getenv("CODEX_SANDBOX") != "" {
		return ProviderCodex, true
	}
	if os.Getenv("GEMINI_CWD") != "" || os.Getenv("GEMINI_CLI") != "" {
		return ProviderGemini, true
	}
	if os.Getenv("OPENCODE_SERVER") != "" || os.Getenv("OPENCODE") != "" {
		return ProviderOpenCode, true
	}
	if os.Getenv("CLAUDE_PROJECT_DIR") != "" || os.Getenv("CLAUDE_PLUGIN_ROOT") != "" {
		return ProviderClaudeCode, true
	}
	return "", false
}

func detectFromShape(payload []byte) (Provider, bool) {
	var probe struct {
		HookEventName  string          `json:"hook_event_name"`
		ConversationID string          `json:"conversation_id"`
		TurnID         string          `json:"turn_id"`
		ToolCallID     string          `json:"tool_call_id"`
		Timestamp      string          `json:"timestamp"`
		SessionID      string          `json:"session_id"`
		Seq            json.RawMessage `json:"seq"`
		Hook           string          `json:"hook"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", false
	}
	switch {
	case probe.Hook != "" && len(probe.Seq) > 0:
		return ProviderOpenCode, true
	case probe.ConversationID != "":
		return ProviderCursor, true
	case probe.HookEventName != "" && isCamel(probe.HookEventName):
		return ProviderCursor, true
	case geminiKinds[probe.HookEventName] != "" && (probe.Timestamp != "" || claudeKinds[probe.HookEventName] == ""):
		return ProviderGemini, true
	case probe.TurnID != "":
		return ProviderCodex, true
	// Kimi is Claude-shaped; the reliable discriminators are its tool_call_id
	// key (Claude uses tool_use_id) and its Kimi-only event names.
	case probe.HookEventName != "" && probe.ToolCallID != "":
		return ProviderKimi, true
	case kimiOnlyEvents[probe.HookEventName]:
		return ProviderKimi, true
	case probe.HookEventName != "":
		return ProviderClaudeCode, true
	}
	return "", false
}

// kimiOnlyEvents are native event names Kimi fires that no Claude-shaped
// sibling dialect has.
var kimiOnlyEvents = map[string]bool{
	"PermissionResult": true,
	"StopFailure":      true,
	"Interrupt":        true,
}

func isCamel(s string) bool {
	return s != "" && s[0] >= 'a' && s[0] <= 'z' && strings.ContainsFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' })
}

// detectVariant encodes the runtime tricks that distinguish provider
// sub-flavors (§6). Best-effort by design; "" means unknown/default.
func detectVariant(p Provider) Variant {
	switch p {
	case ProviderClaudeCode:
		if os.Getenv("CLAUDE_CODE_REMOTE") != "" {
			return VariantRemote
		}
		// cowork: cmux-managed project dirs are the observable signature.
		if dir := os.Getenv("CLAUDE_PROJECT_DIR"); strings.Contains(dir, "/cmux/") || strings.Contains(dir, "/cowork/") {
			return VariantCowork
		}
	case ProviderCursor:
		if os.Getenv("CURSOR_AGENT") != "" {
			return VariantCLI
		}
	}
	return VariantUnknown
}
