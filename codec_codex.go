package agenthooks

import (
	"encoding/json"
	"time"
)

// Codex dialect: a deliberate Claude dialect (same event names, same
// hookSpecificOutput/permissionDecision shapes) with three deltas the codec
// owns: empty stdout means "no opinion", unknown JSON on stdout is rejected,
// and ask/approve fail the hook run (quirk #8) so Ask must be degraded before
// encoding.

var codexKinds = map[string]EventKind{
	"SessionStart":      KindSessionStart,
	"SessionEnd":        KindSessionEnd,
	"UserPromptSubmit":  KindPromptSubmitted,
	"PreToolUse":        KindToolPre,
	"PostToolUse":       KindToolPost,
	"PermissionRequest": KindPermission,
	"Stop":              KindStop,
	"SubagentStart":     KindSubagentStart,
	"SubagentStop":      KindSubagentStop,
	"PreCompact":        KindCompactPre,
	"PostCompact":       KindCompactPost,
}

type codexIn struct {
	claudeIn
	TurnID string `json:"turn_id"`
}

func decodeCodex(v Variant, now time.Time, payload []byte) (any, LocalSession, error) {
	var in codexIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, LocalSession{}, err
	}
	kind, ok := codexKinds[in.HookEventName]
	if !ok {
		kind = KindOther
	}
	base := Event{
		Provider:   ProviderCodex,
		Variant:    v,
		NativeName: in.HookEventName,
		Kind:       kind,
		Time:       now,
		Session: SessionInfo{
			ID:     in.SessionID,
			TurnID: in.TurnID,
			CWD:    in.CWD,
			Model:  in.Model,
		},
		Raw: json.RawMessage(payload),
	}
	if in.AgentID != "" || in.AgentType != "" {
		base.Agent = &AgentInfo{ID: in.AgentID, Type: in.AgentType}
	}
	local := LocalSession{
		TranscriptPath: in.TranscriptPath,
		WorkspaceRoots: rootsFor(in.CWD),
		PermissionMode: in.PermissionMode,
	}
	return buildClaudeShaped(base, &in.claudeIn), local, nil
}

// decodeCodexNotify handles the legacy `codex notify` transport: kebab-case
// JSON passed in argv rather than on stdin. Mapped to KindNotification.
func decodeCodexNotify(v Variant, now time.Time, payload []byte) (any, LocalSession, error) {
	var in struct {
		Type           string `json:"type"`
		TurnID         string `json:"turn-id"`
		ThreadID       string `json:"thread-id"`
		CWD            string `json:"cwd"`
		LastAssistant  string `json:"last-assistant-message"`
		InputMessages  any    `json:"input-messages"`
		AssistantReply string `json:"assistant-reply"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, LocalSession{}, err
	}
	base := Event{
		Provider:   ProviderCodex,
		Variant:    v,
		NativeName: "notify:" + in.Type,
		Kind:       KindNotification,
		Time:       now,
		Session: SessionInfo{
			ID:     in.ThreadID,
			TurnID: in.TurnID,
			CWD:    in.CWD,
		},
		Raw: json.RawMessage(payload),
	}
	msg := in.LastAssistant
	if msg == "" {
		msg = in.AssistantReply
	}
	return &NotificationEvent{Event: base, Message: msg}, LocalSession{WorkspaceRoots: rootsFor(in.CWD)}, nil
}

func encodeCodex(base *Event, d decisionCore) (wireResponse, error) {
	if d.kind == DecisionAsk {
		// Must have been degraded by policy before reaching the codec.
		return wireResponse{}, ErrUnsupportedDecision
	}
	// UpdateInput is allow-only on Codex; the policy layer drops or errors
	// before encode, so any surviving update rides an allow.
	wire, err := encodeClaude(base, d)
	if err != nil {
		return wire, err
	}
	// Empty object means "nothing to say": Codex wants empty stdout for
	// that (it rejects unknown/unexpected JSON, quirk #8).
	if string(wire.Stdout) == "{}" {
		wire.Stdout = nil
	}
	return wire, nil
}
