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
//	mybinary agenthooks run    --provider=moltis                 # Moltis shell hook
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
	ProviderOpenClaw:   true,
	ProviderMoltis:     true,
	ProviderKimi:       true,
	ProviderCopilotCLI: true,
	// VS Code ships no provider env marker and no field Claude Code doesn't
	// also send, so this one is reachable by the generated --provider flag
	// only: neither detectFromEnv nor detectFromShape can produce it.
	ProviderVSCodeCopilot: true,
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
	// Copilot cross-sets CLAUDE_PLUGIN_ROOT/CLAUDE_PROJECT_DIR into hook
	// processes (observed on CLI 1.0.80), so it must be checked before Claude.
	if copilotCLIEnv() {
		return ProviderCopilotCLI, true
	}
	if os.Getenv("CLAUDECODE") == "1" || os.Getenv("CLAUDE_PROJECT_DIR") != "" || os.Getenv("CLAUDE_PLUGIN_ROOT") != "" {
		return ProviderClaudeCode, true
	}
	return "", false
}

// copilotCLIEnv reports whether the Copilot CLI spawned this process. These
// are the CLI's own variables, not ones agenthooks injects; VS Code Copilot
// Chat sets no marker at all, which is what makes their absence meaningful in
// demoteVSCodeToCLI.
func copilotCLIEnv() bool {
	return os.Getenv("COPILOT_CLI") != "" || os.Getenv("COPILOT_PLUGIN_ROOT") != "" || os.Getenv("COPILOT_PLUGIN_DATA") != ""
}

// demoteVSCodeToCLI resolves which of the two runtimes that read the SAME hook
// file is actually running us, and is the one place a --provider flag is
// overridden rather than obeyed.
//
// VS Code Copilot Chat and the Copilot CLI glob the same two directories
// (~/.copilot/hooks, .github/hooks), so every file install writes is loaded by
// both. PascalCase event keys make the INPUT identical — that is the CLI's
// documented Claude-compat mode — but the CLI reads decisions from a FLAT body
// while VS Code reads them from a nested hookSpecificOutput. The CLI is the
// only one of the pair that marks its hook processes, so its env is the
// discriminator and --provider=vscode-copilot is the default it overrides.
//
// Resolving the runtime here, before decode, means each session gets the
// already-correct capability row and encoder with no branch downstream:
// decodeCopilot's Claude-shaped fallthrough handles the snake_case payload and
// encodeCopilot answers flat.
func demoteVSCodeToCLI(p Provider) Provider {
	if p == ProviderVSCodeCopilot && copilotCLIEnv() {
		return ProviderCopilotCLI
	}
	return p
}

func detectFromShape(payload []byte) (Provider, bool) {
	var probe struct {
		HookEventName  string `json:"hook_event_name"`
		ConversationID string `json:"conversation_id"`
		TurnID         string `json:"turn_id"`
		ToolCallID     string `json:"tool_call_id"`
		// Raw, not string: Copilot ships an epoch-ms NUMBER here and a typed
		// mismatch would fail the whole probe, not just this field.
		Timestamp      json.RawMessage `json:"timestamp"`
		SessionIDCamel string          `json:"sessionId"`
		Seq            json.RawMessage `json:"seq"`
		Hook           string          `json:"hook"`
		Event          json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", false
	}
	switch {
	// Both shim dialects frame as {seq, hook, ...}; OpenClaw carries the
	// payload under "event" (+"ctx"), OpenCode under "input"/"output".
	case probe.Hook != "" && jsonPresent(probe.Seq) && jsonPresent(probe.Event):
		return ProviderOpenClaw, true
	case probe.Hook != "" && jsonPresent(probe.Seq):
		return ProviderOpenCode, true
	// Moltis process-per-event hooks carry one of its typed string event
	// discriminators. OpenClaw's framed event above is an object; GatewayStop
	// carries no session_key, so the event catalog is the reliable shape test.
	case moltisNativeEvents[rawJSONString(probe.Event)]:
		return ProviderMoltis, true
	// Copilot is the only dialect keying the session on camelCase sessionId;
	// its payloads carry no event-name field at all on most events, so this is
	// the discriminator (verified against Copilot CLI 1.0.80).
	case probe.SessionIDCamel != "":
		return ProviderCopilotCLI, true
	case probe.ConversationID != "":
		return ProviderCursor, true
	case probe.HookEventName != "" && isCamel(probe.HookEventName):
		return ProviderCursor, true
	// A JSON null decodes into RawMessage as the 4-byte literal, so presence
	// is len>0 AND not null — otherwise `"timestamp": null` would read as set.
	case geminiKinds[probe.HookEventName] != "" && (jsonPresent(probe.Timestamp) || claudeKinds[probe.HookEventName] == ""):
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

func rawJSONString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// kimiOnlyEvents are native event names Kimi fires that no Claude-shaped
// sibling dialect has.
var kimiOnlyEvents = map[string]bool{
	"PermissionResult": true,
	"StopFailure":      true,
	"Interrupt":        true,
}

// jsonPresent reports whether a probe field was set to something other than
// null. Probe fields are json.RawMessage so a type mismatch cannot fail the
// whole unmarshal, and that also means an explicit null arrives as a non-empty
// 4-byte literal rather than as an absent field.
func jsonPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
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
