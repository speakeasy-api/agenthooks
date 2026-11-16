package agenthooks

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Antigravity CLI (agy) dialect: camelCase input keyed on conversationId.
// hookEventName arrived in 1.5.9, so generated configs also pass
// --event=<native> (quirk #51). PreToolUse decisions are a top-level
// "decision"; {} denies, so the no-op is "allow", which defers to the default
// policy; consumer allows map to "auto_approve" (quirk #52). Every MCP call arrives as the call_mcp_tool step
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
	HookEventName      string               `json:"hookEventName"`
	Cwd                string               `json:"cwd"`
	ContinuationActive bool                 `json:"continuationActive"`
	ConversationID     string               `json:"conversationId"`
	WorkspacePaths     []string             `json:"workspacePaths"`
	TranscriptPath     string               `json:"transcriptPath"`
	ModelName          string               `json:"modelName"`
	StepIdx            *int                 `json:"stepIdx"`
	ToolCall           *antigravityToolCall `json:"toolCall"`
	Error              string               `json:"error"`
	TerminationReason  string               `json:"terminationReason"`
}

type antigravityToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func decodeAntigravity(native string, v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in antigravityIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	if native == "" {
		native = in.HookEventName
	}
	if native == "" {
		native = inferAntigravityEvent(&in)
	}
	kind, ok := antigravityKinds[native]
	if !ok {
		kind = KindOther
	}
	cwd := in.Cwd
	if cwd == "" && len(in.WorkspacePaths) > 0 {
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
		lc := 0
		if in.ContinuationActive {
			lc = 1
		}
		return &StopEvent{Event: base, PreviouslyContinued: in.ContinuationActive, LoopCount: lc}, nil
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
	input := in.ToolCall.Args
	isMCP := in.ToolCall.Name == antigravityMCPTool
	if isMCP {
		if inner := firstRawField(in.ToolCall.Args, "Arguments", "arguments", "Args", "args"); inner != nil {
			input = inner
		}
	}
	tc := makeToolCall(session, in.ToolCall.Name, "", input, in.ToolCall.Args)
	// Hash the full args so MCP calls to different servers or tools stay
	// distinct; repeated identical calls differ only by step.
	turn := session.TurnID
	if in.StepIdx != nil {
		turn = "step-" + strconv.Itoa(*in.StepIdx)
	}
	if in.ToolCall.ID != "" {
		tc.ID = in.ToolCall.ID
		tc.Synthesized = false
	} else {
		tc.ID = SynthesizeToolID(session.ID, turn, in.ToolCall.Name, normalizeInput(in.ToolCall.Args))
	}
	if !isMCP {
		return tc
	}
	tc.Canonical = ToolMCP
	tc.MCP = &MCPCall{
		Server: firstStringField(in.ToolCall.Args, "ServerName", "serverName", "server_name", "Server", "server"),
		Tool:   firstStringField(in.ToolCall.Args, "ToolName", "toolName", "tool_name", "Name", "name"),
	}
	return tc
}

func firstRawField(raw json.RawMessage, keys ...string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	for _, k := range keys {
		if v, ok := obj[k]; ok && jsonPresent(v) {
			return v
		}
	}
	return nil
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

func encodeAntigravity(typed any, base *Event, d decisionCore) (wireResponse, error) {
	out := map[string]any{}
	switch base.Kind {
	case KindStop:
		// agy has no native cap, so allow one continuation per stop chain:
		// continuationActive marks a turn a Stop hook already continued.
		if st, ok := typed.(*StopEvent); ok && d.kind == DecisionContinue && !st.PreviouslyContinued {
			out["decision"] = "continue"
			out["reason"] = d.instruction
		}
	case KindToolPre:
		switch d.kind {
		case DecisionAllow:
			out["decision"] = "auto_approve"
		case DecisionDeny:
			out["decision"] = "deny"
		case DecisionAsk:
			out["decision"] = "ask"
		}
		if len(out) > 0 && d.reason != "" {
			out["reason"] = d.reason
		}
	}
	if len(out) == 0 {
		return passthroughResponse(ProviderAntigravity, base.NativeName), nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return wireResponse{}, err
	}
	return wireResponse{Stdout: b}, nil
}
