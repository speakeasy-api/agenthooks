package agenthooks

import (
	"encoding/json"
	"os"
	"time"
)

// Cursor dialect: camelCase event names, snake_case fields, per-event output
// schemas (permission/user_message/agent_message, followup_message). The
// codec emits snake_case output plus a harmless legacy `continue` field
// (quirk #4) and un-stringifies MCP tool_input (quirk #5).

var cursorKinds = map[string]EventKind{
	"sessionStart":         KindSessionStart,
	"sessionEnd":           KindSessionEnd,
	"beforeSubmitPrompt":   KindPromptSubmitted,
	"preToolUse":           KindToolPre,
	"beforeShellExecution": KindToolPre,
	"beforeMCPExecution":   KindToolPre,
	"beforeReadFile":       KindToolPre,
	"postToolUse":          KindToolPost,
	"afterShellExecution":  KindToolPost,
	"afterMCPExecution":    KindToolPost,
	"postToolUseFailure":   KindToolError,
	"stop":                 KindStop,
	"subagentStart":        KindSubagentStart,
	"subagentStop":         KindSubagentStop,
	"preCompact":           KindCompactPre,
	"afterFileEdit":        KindFileEdited,
	"afterTabFileEdit":     KindFileEdited,
	"afterAgentResponse":   KindModelResponse,
	"afterAgentThought":    KindModelResponse,
}

type cursorIn struct {
	ConversationID   string          `json:"conversation_id"`
	GenerationID     string          `json:"generation_id"`
	HookEventName    string          `json:"hook_event_name"`
	WorkspaceRoots   []string        `json:"workspace_roots"`
	UserEmail        string          `json:"user_email"`
	Model            string          `json:"model"`
	CWD              string          `json:"cwd"`
	TranscriptPath   string          `json:"transcript_path"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	ToolUseID        string          `json:"tool_use_id"`
	Command          string          `json:"command"`
	URL              string          `json:"url"`
	Prompt           string          `json:"prompt"`
	FilePath         string          `json:"file_path"`
	Output           json.RawMessage `json:"output"`
	ToolResponse     json.RawMessage `json:"tool_response"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	LoopCount        int             `json:"loop_count"`
	SubagentType     string          `json:"subagent_type"`
	SubagentID       string          `json:"subagent_id"`
	Error            string          `json:"error"`
	Duration         *float64        `json:"duration"`
	DurationMS       *float64        `json:"duration_ms"`
	Text             string          `json:"text"`
	InputTokens      *int            `json:"input_tokens"`
	OutputTokens     *int            `json:"output_tokens"`
	CacheReadTokens  *int            `json:"cache_read_tokens"`
	CacheWriteTokens *int            `json:"cache_write_tokens"`
	Cost             *float64        `json:"cost"`
}

func decodeCursor(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in cursorIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	kind, ok := cursorKinds[in.HookEventName]
	if !ok {
		kind = KindOther
	}
	cwd := in.CWD
	if cwd == "" && len(in.WorkspaceRoots) > 0 {
		cwd = in.WorkspaceRoots[0]
	}
	// Cursor ships the transcript location as an env var on hook processes;
	// only some payloads repeat it inline.
	if in.TranscriptPath == "" {
		in.TranscriptPath = os.Getenv("CURSOR_TRANSCRIPT_PATH")
	}
	base := Event{
		Provider:            ProviderCursor,
		Variant:             v,
		NativeName:          in.HookEventName,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.ConversationID,
			TurnID:         in.GenerationID,
			CWD:            cwd,
			WorkspaceRoots: in.WorkspaceRoots,
			TranscriptPath: in.TranscriptPath,
			Model:          in.Model,
			UserEmail:      in.UserEmail,
		},
		Raw: json.RawMessage(payload),
	}
	if in.SubagentID != "" || in.SubagentType != "" {
		base.Agent = &AgentInfo{ID: in.SubagentID, Type: in.SubagentType}
	}

	switch kind {
	case KindToolPre:
		return &ToolPreEvent{Event: base, Tool: cursorToolCall(base.Session, &in)}, nil
	case KindToolPost, KindToolError:
		out := in.Output
		if len(out) == 0 {
			out = in.ToolResponse
		}
		failed := kind == KindToolError || (in.ExitCode != nil && *in.ExitCode != 0)
		duration := in.DurationMS
		if duration == nil {
			duration = in.Duration
		}
		return &ToolPostEvent{
			Event:      base,
			Tool:       cursorToolCall(base.Session, &in),
			Output:     out,
			Failed:     failed,
			Error:      in.Error,
			DurationMS: duration,
		}, nil
	case KindPromptSubmitted:
		return &PromptEvent{Event: base, Prompt: in.Prompt}, nil
	case KindStop, KindSubagentStop:
		return &StopEvent{
			Event:               base,
			PreviouslyContinued: in.LoopCount > 0,
			LoopCount:           in.LoopCount,
			FinalMessage:        in.Text,
			Usage:               cursorUsage(&in),
		}, nil
	case KindSubagentStart:
		return &SubagentStartEvent{Event: base}, nil
	case KindSessionStart:
		return &SessionStartEvent{Event: base}, nil
	case KindSessionEnd:
		return &SessionEndEvent{Event: base, Reason: in.Status}, nil
	case KindCompactPre:
		return &CompactEvent{Event: base}, nil
	case KindFileEdited:
		return &FileEditedEvent{Event: base, Path: in.FilePath}, nil
	case KindModelRequest, KindModelResponse:
		return &ModelEvent{Event: base}, nil
	}
	ev := base
	return &ev, nil
}

// cursorToolCall normalizes the three shapes Cursor uses for the same
// concept: preToolUse (tool_name + object tool_input), shell events (bare
// command field), and MCP events (MCP:-prefixed name + stringified input).
func cursorToolCall(s SessionInfo, in *cursorIn) ToolCall {
	name := in.ToolName
	input := in.ToolInput
	switch in.HookEventName {
	case "beforeShellExecution", "afterShellExecution":
		if name == "" {
			name = "Shell"
		}
		if len(input) == 0 && in.Command != "" {
			b, err := json.Marshal(map[string]string{"command": in.Command})
			if err == nil {
				input = b
			}
		}
	case "beforeReadFile":
		if name == "" {
			name = "ReadFile"
		}
		if len(input) == 0 && in.FilePath != "" {
			b, err := json.Marshal(map[string]string{"file_path": in.FilePath})
			if err == nil {
				input = b
			}
		}
	}
	tc := makeToolCall(s, name, in.ToolUseID, input, in.ToolInput)
	if in.HookEventName == "beforeMCPExecution" || in.HookEventName == "afterMCPExecution" {
		if tc.MCP == nil {
			tc.MCP = &MCPCall{Tool: name}
			tc.Canonical = ToolMCP
		}
		tc.MCP.URL = in.URL
		tc.MCP.Command = in.Command
	}
	return tc
}

func encodeCursor(base *Event, d decisionCore) (wireResponse, error) {
	out := map[string]any{}
	ctx := joinContext(d.context)

	switch base.Kind {
	case KindToolPre, KindPermission:
		switch d.kind {
		case decAllow:
			out["permission"] = "allow"
		case decDeny:
			out["permission"] = "deny"
		case decAsk:
			out["permission"] = "ask"
		}
		if d.kind != decNoDecision {
			// Harmless legacy field for the pre-2.0.64 camelCase era (quirk #4).
			out["continue"] = true
		}
		if d.reason != "" || ctx != "" {
			out["agent_message"] = joinNonEmpty(d.reason, ctx)
		}
		if d.systemMessage != "" {
			out["user_message"] = d.systemMessage
		}
		if d.hasUpdatedInput && base.NativeName == "preToolUse" {
			out["updated_input"] = d.updatedInput
		}
	case KindPromptSubmitted:
		if d.kind == decBlockPrompt {
			out["continue"] = false
			if d.reason != "" {
				out["user_message"] = d.reason
			}
		} else if d.kind != decNoDecision {
			out["continue"] = true
		}
	case KindStop, KindSubagentStop:
		if d.kind == decContinue {
			out["followup_message"] = d.instruction
		}
	case KindToolPost, KindToolError:
		if d.hasReplacedOutput && base.NativeName == "afterMCPExecution" {
			out["updated_mcp_tool_output"] = d.replacedOutput
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return wireResponse{}, err
	}
	return wireResponse{Stdout: b}, nil
}

// cursorUsage collects the end-of-turn token/cost totals Cursor reports on
// stop, returning nil when none are present so telemetry omits an empty block.
func cursorUsage(in *cursorIn) *Usage {
	if in.InputTokens == nil && in.OutputTokens == nil && in.CacheReadTokens == nil &&
		in.CacheWriteTokens == nil && in.Cost == nil && in.LoopCount == 0 && in.Status == "" {
		return nil
	}
	u := &Usage{
		InputTokens:      in.InputTokens,
		OutputTokens:     in.OutputTokens,
		CacheReadTokens:  in.CacheReadTokens,
		CacheWriteTokens: in.CacheWriteTokens,
		Cost:             in.Cost,
		LoopCount:        nil,
		Status:           in.Status,
	}
	if in.LoopCount != 0 {
		lc := in.LoopCount
		u.LoopCount = &lc
	}
	return u
}

func joinNonEmpty(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += p
	}
	return out
}
