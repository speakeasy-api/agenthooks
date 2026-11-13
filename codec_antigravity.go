package agenthooks

import (
	"encoding/json"
	"strings"
	"time"
)

// Antigravity CLI (agy) dialect: camelCase input keyed on conversationId with
// no event-name field (quirk #51), so generated configs pass --event=<native>.
// PreToolUse decisions are a top-level "decision"; {} denies, so the no-op is
// empty stdout (quirk #52). Every MCP call arrives as the call_mcp_tool step
// with the server and tool named in its args.

const antigravityMCPTool = "call_mcp_tool"

var antigravityKinds = map[string]EventKind{
	"SessionStart":   KindSessionStart,
	"PreToolUse":     KindToolPre,
	"PostToolUse":    KindToolPost,
	"PreInvocation":  KindModelRequest,
	"PostInvocation": KindModelResponse,
	"Stop":           KindStop,
}

type antigravityIn struct {
	ConversationID    string               `json:"conversationId"`
	WorkspacePaths    []string             `json:"workspacePaths"`
	TranscriptPath    string               `json:"transcriptPath"`
	ModelName         string               `json:"modelName"`
	StepIdx           *int                 `json:"stepIdx"`
	ToolCall          *antigravityToolCall `json:"toolCall"`
	Error             string               `json:"error"`
	TerminationReason string               `json:"terminationReason"`
}

type antigravityToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func decodeAntigravity(native string, v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in antigravityIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	if native == "" {
		native = inferAntigravityEvent(&in)
	}
	kind, ok := antigravityKinds[native]
	if !ok {
		kind = KindOther
	}
	cwd := ""
	if len(in.WorkspacePaths) > 0 {
		cwd = in.WorkspacePaths[0]
	}
	base := Event{
		Provider:            ProviderAntigravity,
		Variant:             v,
		NativeName:          native,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.ConversationID,
			CWD:            cwd,
			WorkspaceRoots: in.WorkspacePaths,
			TranscriptPath: in.TranscriptPath,
			Model:          in.ModelName,
		},
		Raw: json.RawMessage(payload),
	}

	switch kind {
	case KindToolPre:
		if in.ToolCall == nil {
			break
		}
		return &ToolPreEvent{Event: base, Tool: antigravityToolCallOf(base.Session, &in)}, nil
	case KindToolPost:
		// PostToolUse also fires for non-tool steps with a null toolCall, and
		// carries no tool output (quirk #51).
		if in.ToolCall == nil {
			break
		}
		return &ToolPostEvent{
			Event:  base,
			Tool:   antigravityToolCallOf(base.Session, &in),
			Failed: in.Error != "",
			Error:  in.Error,
		}, nil
	case KindStop:
		return &StopEvent{Event: base}, nil
	case KindSessionStart:
		return &SessionStartEvent{Event: base}, nil
	case KindModelRequest, KindModelResponse:
		return &ModelEvent{Event: base}, nil
	}
	ev := base
	return &ev, nil
}

// inferAntigravityEvent is the fallback for invocations without --event. Only
// Stop has a discriminating field; tool steps are ambiguous between Pre and
// Post, so they resolve to PostToolUse, which can never block.
func inferAntigravityEvent(in *antigravityIn) string {
	switch {
	case in.TerminationReason != "":
		return "Stop"
	case in.ToolCall != nil:
		return "PostToolUse"
	}
	return "Antigravity"
}

func antigravityToolCallOf(session SessionInfo, in *antigravityIn) ToolCall {
	tc := makeToolCall(session, in.ToolCall.Name, "", in.ToolCall.Args, in.ToolCall.Args)
	if in.ToolCall.Name != antigravityMCPTool {
		return tc
	}
	tc.Canonical = ToolMCP
	tc.MCP = &MCPCall{
		Server: firstStringField(in.ToolCall.Args, "ServerName", "serverName", "server_name", "Server", "server"),
		Tool:   firstStringField(in.ToolCall.Args, "ToolName", "toolName", "tool_name", "Name", "name"),
	}
	return tc
}

func firstStringField(raw json.RawMessage, keys ...string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	for _, k := range keys {
		var s string
		if json.Unmarshal(obj[k], &s) == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func encodeAntigravity(base *Event, d decisionCore) (wireResponse, error) {
	out := map[string]any{}
	switch base.Kind {
	case KindToolPre:
		switch d.kind {
		case DecisionAllow:
			out["decision"] = "allow"
		case DecisionDeny:
			out["decision"] = "deny"
		case DecisionAsk:
			out["decision"] = "ask"
		}
		if len(out) > 0 && d.reason != "" {
			out["reason"] = d.reason
		}
	case KindStop:
		if d.kind == DecisionContinue {
			out["decision"] = "continue"
			out["reason"] = d.instruction
		}
	}
	if len(out) == 0 {
		return noOpResponse(ProviderAntigravity), nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return wireResponse{}, err
	}
	return wireResponse{Stdout: b}, nil
}
