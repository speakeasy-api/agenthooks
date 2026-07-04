package agenthooks

import (
	"encoding/json"
	"time"
)

// Claude Code dialect: snake_case JSON on stdin, camelCase JSON out,
// hookSpecificOutput envelope, exit 0 with an explicit body (the library
// never relies on exit-2 semantics because every blocking intent is
// expressible in the body).

var claudeKinds = map[string]EventKind{
	"SessionStart":       KindSessionStart,
	"SessionEnd":         KindSessionEnd,
	"UserPromptSubmit":   KindPromptSubmitted,
	"PreToolUse":         KindToolPre,
	"PostToolUse":        KindToolPost,
	"PostToolUseFailure": KindToolError,
	"PermissionRequest":  KindPermission,
	"Stop":               KindStop,
	"SubagentStart":      KindSubagentStart,
	"SubagentStop":       KindSubagentStop,
	"PreCompact":         KindCompactPre,
	"PostCompact":        KindCompactPost,
	"Notification":       KindNotification,
	"FileChanged":        KindFileEdited,
	"MessageDisplay":     KindModelResponse, // approximate mapping (§3.4)
}

type claudeIn struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	CWD                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	PermissionMode       string          `json:"permission_mode"`
	Model                string          `json:"model"`
	PromptID             string          `json:"prompt_id"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolError            string          `json:"tool_error"`
	Prompt               string          `json:"prompt"`
	Message              string          `json:"message"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	DurationMS           *float64        `json:"duration_ms"`
	Source               string          `json:"source"`
	Reason               string          `json:"reason"`
	StopHookActive       bool            `json:"stop_hook_active"`
	Trigger              string          `json:"trigger"`
	CustomInstructions   string          `json:"custom_instructions"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	FilePath             string          `json:"file_path"`
}

func decodeClaude(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in claudeIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	kind, ok := claudeKinds[in.HookEventName]
	if !ok {
		kind = KindOther
	}
	base := Event{
		Provider:            ProviderClaudeCode,
		Variant:             v,
		NativeName:          in.HookEventName,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.SessionID,
			TurnID:         in.PromptID,
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
	return buildClaudeShaped(base, &in), nil
}

// buildClaudeShaped constructs typed events from the Claude-shaped wire form.
// Codex deliberately ships the same shapes, so its decoder reuses this.
func buildClaudeShaped(base Event, in *claudeIn) any {
	switch base.Kind {
	case KindToolPre:
		return &ToolPreEvent{Event: base, Tool: makeToolCall(base.Session, in.ToolName, in.ToolUseID, in.ToolInput, in.ToolInput)}
	case KindPermission:
		return &PermissionEvent{Event: base, Tool: makeToolCall(base.Session, in.ToolName, in.ToolUseID, in.ToolInput, in.ToolInput)}
	case KindToolPost, KindToolError:
		return &ToolPostEvent{
			Event:      base,
			Tool:       makeToolCall(base.Session, in.ToolName, in.ToolUseID, in.ToolInput, in.ToolInput),
			Output:     in.ToolResponse,
			Failed:     base.Kind == KindToolError,
			Error:      in.ToolError,
			DurationMS: in.DurationMS,
		}
	case KindPromptSubmitted:
		return &PromptEvent{Event: base, Prompt: in.Prompt}
	case KindStop, KindSubagentStop:
		lc := 0
		if in.StopHookActive {
			lc = 1
		}
		return &StopEvent{Event: base, PreviouslyContinued: in.StopHookActive, LoopCount: lc, FinalMessage: in.LastAssistantMessage, Usage: nil}
	case KindSubagentStart:
		return &SubagentStartEvent{Event: base}
	case KindSessionStart:
		return &SessionStartEvent{Event: base, Source: in.Source}
	case KindSessionEnd:
		return &SessionEndEvent{Event: base, Reason: in.Reason}
	case KindNotification:
		return &NotificationEvent{Event: base, Message: in.Message}
	case KindCompactPre, KindCompactPost:
		return &CompactEvent{Event: base, Trigger: in.Trigger, Instructions: in.CustomInstructions}
	case KindFileEdited:
		return &FileEditedEvent{Event: base, Path: in.FilePath}
	case KindModelRequest, KindModelResponse:
		return &ModelEvent{Event: base}
	}
	ev := base
	return &ev
}

func rootsFor(cwd string) []string {
	if cwd == "" {
		return nil
	}
	return []string{cwd}
}

func encodeClaude(base *Event, d decisionCore) (wireResponse, error) {
	out := map[string]any{}
	hso := map[string]any{}
	ctx := joinContext(d.context)

	switch base.Kind {
	case KindToolPre, KindPermission:
		switch d.kind {
		case decAllow:
			hso["permissionDecision"] = "allow"
			if d.reason != "" {
				hso["permissionDecisionReason"] = d.reason
			}
		case decDeny:
			hso["permissionDecision"] = "deny"
			hso["permissionDecisionReason"] = d.reason
		case decAsk:
			hso["permissionDecision"] = "ask"
			hso["permissionDecisionReason"] = d.reason
		}
		if d.hasUpdatedInput {
			hso["updatedInput"] = d.updatedInput
		}
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
	case KindPromptSubmitted:
		if d.kind == decBlockPrompt {
			out["decision"] = "block"
			out["reason"] = d.reason
		}
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
	case KindSessionStart:
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
	case KindStop, KindSubagentStop:
		if d.kind == decContinue {
			out["decision"] = "block"
			out["reason"] = d.instruction
		}
	case KindToolPost, KindToolError:
		if d.kind == decFlagOutput {
			out["decision"] = "block"
			out["reason"] = d.reason
		}
		if d.hasReplacedOutput {
			hso["updatedToolOutput"] = d.replacedOutput
		}
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
	}

	if d.systemMessage != "" {
		out["systemMessage"] = d.systemMessage
	}
	if d.stopAgent {
		out["continue"] = false
		if d.stopReason != "" {
			out["stopReason"] = d.stopReason
		}
	}
	if len(hso) > 0 {
		hso["hookEventName"] = base.NativeName
		out["hookSpecificOutput"] = hso
	}
	b, err := json.Marshal(out)
	if err != nil {
		return wireResponse{}, err
	}
	return wireResponse{Stdout: b}, nil
}
