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

func decodeCodex(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in codexIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	kind, ok := codexKinds[in.HookEventName]
	if !ok {
		kind = KindOther
	}
	base := Event{
		Provider:            ProviderCodex,
		Variant:             v,
		NativeName:          in.HookEventName,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.SessionID,
			TurnID:         in.TurnID,
			CWD:            in.CWD,
			WorkspaceRoots: rootsFor(in.CWD),
			TranscriptPath: in.TranscriptPath,
			Model:          in.Model,
			PermissionMode: in.PermissionMode,
		},
		Raw: json.RawMessage(payload),
	}
	if in.AgentID != "" || in.AgentType != "" {
		base.Agent = &AgentInfo{ID: in.AgentID, Type: in.AgentType}
	}
	return buildClaudeShaped(base, &in.claudeIn), nil
}

// decodeCodexNotify handles the legacy `codex notify` transport: kebab-case
// JSON passed in argv rather than on stdin. Mapped to KindNotification.
func decodeCodexNotify(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
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
		return nil, err
	}
	base := Event{
		Provider:            ProviderCodex,
		Variant:             v,
		NativeName:          "notify:" + in.Type,
		Kind:                KindNotification,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.ThreadID,
			TurnID:         in.TurnID,
			CWD:            in.CWD,
			WorkspaceRoots: rootsFor(in.CWD),
		},
		Raw: json.RawMessage(payload),
	}
	msg := in.LastAssistant
	if msg == "" {
		msg = in.AssistantReply
	}
	return &NotificationEvent{Event: base, Message: msg}, nil
}

func encodeCodex(base *Event, d decisionCore) (wireResponse, error) {
	if d.kind == decAsk {
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
