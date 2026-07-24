package agenthooks

import (
	"encoding/json"
	"time"
)

// Gemini CLI dialect: Claude-inspired input with renamed events plus a
// timestamp; output uses a top-level decision instead of
// hookSpecificOutput.permissionDecision. The codec always writes explicit
// JSON to stdout because Gemini parses stderr as the decision when stdout is
// empty and blocks on any non-zero exit except 1 (quirk #11). Input rewrites
// are shallow-merged upstream, so key removal surfaces ErrLossyUpdate
// (quirk #12).

var geminiKinds = map[string]EventKind{
	"SessionStart":        KindSessionStart,
	"SessionEnd":          KindSessionEnd,
	"BeforeAgent":         KindPromptSubmitted,
	"BeforeTool":          KindToolPre,
	"AfterTool":           KindToolPost,
	"AfterAgent":          KindStop,
	"PreCompress":         KindCompactPre,
	"Notification":        KindNotification,
	"BeforeModel":         KindModelRequest,
	"BeforeToolSelection": KindModelRequest,
	"AfterModel":          KindModelResponse,
}

type geminiIn struct {
	SessionID      string            `json:"session_id"`
	TranscriptPath string            `json:"transcript_path"`
	CWD            string            `json:"cwd"`
	HookEventName  string            `json:"hook_event_name"`
	Timestamp      string            `json:"timestamp"`
	Model          string            `json:"model"`
	ToolName       string            `json:"tool_name"`
	ToolInput      json.RawMessage   `json:"tool_input"`
	ToolCallID     string            `json:"tool_call_id"`
	ToolResponse   json.RawMessage   `json:"tool_response"`
	Prompt         string            `json:"prompt"`
	Message        string            `json:"message"`
	Source         string            `json:"source"`
	Reason         string            `json:"reason"`
	Trigger        string            `json:"trigger"`
	MCPContext     *geminiMCPContext `json:"mcp_context"`
}

type geminiMCPContext struct {
	ServerName string   `json:"server_name"`
	ToolName   string   `json:"tool_name"`
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	CWD        string   `json:"cwd"`
	URL        string   `json:"url"`
	TCP        string   `json:"tcp"`
}

func decodeGemini(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in geminiIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	kind, ok := geminiKinds[in.HookEventName]
	if !ok {
		kind = KindOther
	}
	t := now
	if in.Timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339, in.Timestamp); err == nil {
			t = parsed
		}
	}
	base := Event{
		Provider:            ProviderGemini,
		Variant:             v,
		NativeName:          in.HookEventName,
		Kind:                kind,
		Time:                t,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.SessionID,
			CWD:            in.CWD,
			WorkspaceRoots: rootsFor(in.CWD),
			TranscriptPath: in.TranscriptPath,
			Model:          in.Model,
		},
		Raw: json.RawMessage(payload),
	}

	switch kind {
	case KindToolPre:
		return &ToolPreEvent{Event: base, Tool: geminiToolCall(base.Session, &in)}, nil
	case KindToolPost:
		// Gemini reports tool failures via tool_response.error rather than a
		// dedicated event (§4.1); surface both on the same typed event.
		errMsg := ""
		if e := rawField(in.ToolResponse, "error"); len(e) > 0 && string(e) != "null" {
			var s string
			if json.Unmarshal(e, &s) == nil {
				errMsg = s
			} else {
				errMsg = string(e)
			}
		}
		return &ToolPostEvent{
			Event:  base,
			Tool:   geminiToolCall(base.Session, &in),
			Output: in.ToolResponse,
			Failed: errMsg != "",
			Error:  errMsg,
		}, nil
	case KindPromptSubmitted:
		return &PromptEvent{Event: base, Prompt: in.Prompt}, nil
	case KindStop:
		return &StopEvent{Event: base}, nil
	case KindSessionStart:
		return &SessionStartEvent{Event: base, Source: in.Source}, nil
	case KindSessionEnd:
		return &SessionEndEvent{Event: base, Reason: in.Reason}, nil
	case KindNotification:
		return &NotificationEvent{Event: base, Message: in.Message}, nil
	case KindCompactPre:
		return &CompactEvent{Event: base, Trigger: in.Trigger}, nil
	case KindModelRequest, KindModelResponse:
		return &ModelEvent{Event: base}, nil
	}
	ev := base
	return &ev, nil
}

func geminiToolCall(session SessionInfo, in *geminiIn) ToolCall {
	tc := makeToolCall(session, in.ToolName, in.ToolCallID, in.ToolInput, in.ToolInput)
	if in.MCPContext == nil {
		return tc
	}
	tc.Canonical = ToolMCP
	tc.MCP = &MCPCall{
		Server:  in.MCPContext.ServerName,
		Tool:    in.MCPContext.ToolName,
		URL:     in.MCPContext.URL,
		Command: joinCommand(in.MCPContext.Command, in.MCPContext.Args),
	}
	return tc
}

func encodeGemini(typed any, base *Event, d decisionCore) (wireResponse, error) {
	out := map[string]any{}
	hso := map[string]any{}
	ctx := joinContext(d.context)

	switch base.Kind {
	case KindToolPre:
		switch d.kind {
		case decAllow:
			out["decision"] = "approve"
			if d.reason != "" {
				out["reason"] = d.reason
			}
		case decDeny:
			out["decision"] = "block"
			out["reason"] = d.reason
		case decAsk:
			// Undocumented upstream but honored (§4.1).
			out["decision"] = "ask"
			out["reason"] = d.reason
		}
		if d.hasUpdatedInput {
			if pre, ok := typed.(*ToolPreEvent); ok {
				lossy, err := lossyUpdate(pre.Tool.Input, d.updatedInput)
				if err != nil {
					return wireResponse{}, err
				}
				if lossy {
					return wireResponse{}, ErrLossyUpdate
				}
			}
			hso["tool_input"] = d.updatedInput
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
	case KindStop:
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
			hso["tool_output"] = d.replacedOutput
		}
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
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
	// Never empty stdout, never bare non-zero exit (quirk #11).
	return wireResponse{Stdout: b}, nil
}
