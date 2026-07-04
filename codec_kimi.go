package agenthooks

import (
	"encoding/json"
	"time"
)

// Kimi Code dialect: Claude-shaped snake_case JSON on stdin, but a much
// narrower response surface. Only PreToolUse understands JSON output
// (hookSpecificOutput.permissionDecision, deny|allow only — no ask, no
// updatedInput, quirk #22). UserPromptSubmit and Stop block via exit code 2
// with the reason on stderr, and exit-0 stdout is appended to the model
// context (quirk #23), so the no-op form is empty stdout. The runtime itself
// is hard fail-open: crash, timeout, and any non-2 exit all allow (quirk #21).

var kimiKinds = map[string]EventKind{
	"SessionStart":       KindSessionStart,
	"SessionEnd":         KindSessionEnd,
	"UserPromptSubmit":   KindPromptSubmitted,
	"PreToolUse":         KindToolPre,
	"PostToolUse":        KindToolPost,
	"PostToolUseFailure": KindToolError,
	"PermissionRequest":  KindPermission, // observation-only on Kimi (§4.1)
	"Stop":               KindStop,
	"SubagentStart":      KindSubagentStart,
	"SubagentStop":       KindSubagentStop,
	"PreCompact":         KindCompactPre,
	"PostCompact":        KindCompactPost,
	"Notification":       KindNotification,
	// PermissionResult, StopFailure, Interrupt intentionally unmapped:
	// delivered as KindOther with the native name and raw payload intact.
}

// kimiIn is the union of Kimi's per-event stdin fields. Field names diverge
// from Claude where noted (tool_call_id vs tool_use_id, tool_output vs
// tool_response, error vs tool_error).
type kimiIn struct {
	SessionID        string          `json:"session_id"`
	CWD              string          `json:"cwd"`
	HookEventName    string          `json:"hook_event_name"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	ToolCallID       string          `json:"tool_call_id"`
	ToolOutput       json.RawMessage `json:"tool_output"`
	Error            string          `json:"error"`
	Prompt           string          `json:"prompt"`
	StopHookActive   bool            `json:"stop_hook_active"`
	Source           string          `json:"source"`
	Reason           string          `json:"reason"`
	AgentName        string          `json:"agent_name"`
	Trigger          string          `json:"trigger"`
	Sink             string          `json:"sink"`
	NotificationType string          `json:"notification_type"`
	Title            string          `json:"title"`
	Body             string          `json:"body"`
}

func decodeKimi(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in kimiIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	kind, ok := kimiKinds[in.HookEventName]
	if !ok {
		kind = KindOther
	}
	base := Event{
		Provider:            ProviderKimi,
		Variant:             v,
		NativeName:          in.HookEventName,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.SessionID,
			CWD:            in.CWD,
			WorkspaceRoots: rootsFor(in.CWD),
		},
		Raw: json.RawMessage(payload),
	}
	if in.AgentName != "" {
		base.Agent = &AgentInfo{Type: in.AgentName}
	}
	message := in.Body
	if message == "" {
		message = in.Title
	}
	// Kimi ships Claude's shapes under renamed keys: project onto claudeIn
	// and reuse the shared builder.
	shaped := claudeIn{
		SessionID:      in.SessionID,
		CWD:            in.CWD,
		HookEventName:  in.HookEventName,
		ToolName:       in.ToolName,
		ToolInput:      in.ToolInput,
		ToolUseID:      in.ToolCallID,
		ToolResponse:   in.ToolOutput,
		ToolError:      in.Error,
		Prompt:         in.Prompt,
		Message:        message,
		Source:         in.Source,
		Reason:         in.Reason,
		StopHookActive: in.StopHookActive,
		Trigger:        in.Trigger,
	}
	return buildClaudeShaped(base, &shaped), nil
}

func encodeKimi(base *Event, d decisionCore) (wireResponse, error) {
	if d.kind == decAsk {
		// Must have been degraded by policy before reaching the codec.
		return wireResponse{}, ErrUnsupportedDecision
	}
	switch base.Kind {
	case KindToolPre:
		var decision string
		switch d.kind {
		case decAllow:
			decision = "allow"
		case decDeny:
			decision = "deny"
		default:
			return wireResponse{}, nil
		}
		hso := map[string]any{
			"hookEventName":      base.NativeName,
			"permissionDecision": decision,
		}
		if d.reason != "" || d.kind == decDeny {
			hso["permissionDecisionReason"] = d.reason
		}
		b, err := json.Marshal(map[string]any{"hookSpecificOutput": hso})
		if err != nil {
			return wireResponse{}, err
		}
		return wireResponse{Stdout: b}, nil
	case KindPromptSubmitted:
		if d.kind == decBlockPrompt {
			return wireResponse{Stderr: []byte(d.reason), ExitCode: 2}, nil
		}
		// Exit-0 stdout is appended to the model context on UserPromptSubmit
		// — the only context mechanism Kimi has, and it is plain text.
		if ctx := joinContext(d.context); ctx != "" {
			return wireResponse{Stdout: []byte(ctx)}, nil
		}
	case KindStop:
		if d.kind == decContinue {
			return wireResponse{Stderr: []byte(d.instruction), ExitCode: 2}, nil
		}
	}
	// Everything else on Kimi is observation-only; empty stdout keeps the
	// context clean (quirk #23).
	return wireResponse{}, nil
}
