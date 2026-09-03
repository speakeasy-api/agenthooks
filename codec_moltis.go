package agenthooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Moltis uses one shell process per event. Stdin is a serde internally tagged
// HookPayload ({"event":"BeforeToolCall", ...}); stdout is empty for no
// opinion or {"action":"modify","data":...} for a rewrite. Exit 1 blocks
// and carries the reason on stderr.

var moltisKinds = map[string]EventKind{
	"AgentEnd":         KindStop,
	"BeforeLLMCall":    KindModelRequest,
	"AfterLLMCall":     KindModelResponse,
	"BeforeCompaction": KindCompactPre,
	"AfterCompaction":  KindCompactPost,
	"MessageReceived":  KindPromptSubmitted,
	"BeforeToolCall":   KindToolPre,
	"AfterToolCall":    KindToolPost,
	"SessionStart":     KindSessionStart,
	"SessionEnd":       KindSessionEnd,
}

var moltisNativeEvents = map[string]bool{
	"BeforeAgentStart":  true,
	"AgentEnd":          true,
	"BeforeLLMCall":     true,
	"AfterLLMCall":      true,
	"BeforeCompaction":  true,
	"AfterCompaction":   true,
	"MessageReceived":   true,
	"MessageSending":    true,
	"MessageSent":       true,
	"BeforeToolCall":    true,
	"AfterToolCall":     true,
	"ToolResultPersist": true,
	"SessionStart":      true,
	"SessionEnd":        true,
	"GatewayStart":      true,
	"GatewayStop":       true,
	"Command":           true,
}

type moltisIn struct {
	Event        string          `json:"event"`
	SessionKey   string          `json:"session_key"`
	TurnID       string          `json:"turn_id"`
	Model        string          `json:"model"`
	Content      string          `json:"content"`
	ToolCallID   string          `json:"tool_call_id"`
	ToolName     string          `json:"tool_name"`
	Arguments    json.RawMessage `json:"arguments"`
	Success      *bool           `json:"success"`
	Result       json.RawMessage `json:"result"`
	Text         string          `json:"text"`
	Iterations   int             `json:"iterations"`
	MessageCount int             `json:"message_count"`
	SummaryLen   int             `json:"summary_len"`
}

func decodeMoltis(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in moltisIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	if in.Event == "" {
		return nil, errors.New("agenthooks: Moltis payload has no event discriminator")
	}

	kind, ok := moltisKinds[in.Event]
	if !ok {
		kind = KindOther
	}
	if in.Event == "AfterToolCall" && in.Success != nil && !*in.Success {
		kind = KindToolError
	}
	base := Event{
		Provider:            ProviderMoltis,
		Variant:             v,
		NativeName:          in.Event,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:     in.SessionKey,
			TurnID: in.TurnID,
			Model:  in.Model,
		},
		Raw: json.RawMessage(payload),
	}

	switch in.Event {
	case "SessionStart":
		return &SessionStartEvent{Event: base, Source: "moltis"}, nil
	case "SessionEnd":
		return &SessionEndEvent{Event: base}, nil
	case "MessageReceived":
		return &PromptEvent{Event: base, Prompt: in.Content}, nil
	case "BeforeToolCall":
		rawInput := in.Arguments
		if len(bytes.TrimSpace(rawInput)) == 0 {
			rawInput = json.RawMessage("{}")
		}
		return &ToolPreEvent{
			Event: base,
			Tool:  makeToolCall(base.Session, in.ToolName, in.ToolCallID, rawInput, rawInput),
		}, nil
	case "AfterToolCall":
		output := in.Result
		if len(bytes.TrimSpace(output)) == 0 {
			output = json.RawMessage("null")
		}
		// Patched Moltis payloads carry the effective arguments after any pre-call
		// rewrite. Older releases omit the field; nil RawInput preserves that
		// distinction while makeToolCall keeps normalized Input object-shaped.
		rawInput := in.Arguments
		if len(bytes.TrimSpace(rawInput)) == 0 {
			rawInput = nil
		}
		tool := makeToolCall(base.Session, in.ToolName, in.ToolCallID, rawInput, rawInput)
		failed := in.Success != nil && !*in.Success
		return &ToolPostEvent{
			Event:  base,
			Tool:   tool,
			Output: output,
			Failed: failed,
			Error:  moltisToolError(output, failed),
		}, nil
	case "AgentEnd":
		return &StopEvent{Event: base, FinalMessage: in.Text}, nil
	case "BeforeCompaction", "AfterCompaction":
		return &CompactEvent{Event: base}, nil
	case "BeforeLLMCall", "AfterLLMCall":
		return &ModelEvent{Event: base}, nil
	default:
		return &base, nil
	}
}

func moltisToolError(result json.RawMessage, failed bool) string {
	if !failed {
		return ""
	}
	var obj struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(result, &obj) == nil && obj.Error != "" {
		return obj.Error
	}
	var text string
	if json.Unmarshal(result, &text) == nil && text != "" {
		return text
	}
	return "Moltis tool call failed"
}

func encodeMoltis(typed any, base *Event, d decisionCore) (wireResponse, error) {
	if d.blocks() {
		reason := d.reason
		if reason == "" {
			reason = "blocked by agenthooks policy"
		}
		return wireResponse{Stderr: []byte(reason), ExitCode: 1}, nil
	}

	var data any
	switch base.Kind {
	case KindToolPre:
		if d.hasUpdatedInput {
			data = d.updatedInput
		}
	case KindPromptSubmitted:
		if len(d.context) > 0 {
			prompt, ok := typed.(*PromptEvent)
			if !ok {
				return wireResponse{}, fmt.Errorf("agenthooks: Moltis prompt encoded from %T", typed)
			}
			// The typed projection is authoritative after middleware. Raw remains
			// verbatim for fidelity, but using its stale content here would discard
			// an in-process prompt rewrite before appending additional context.
			content := prompt.Prompt
			context := joinContext(d.context)
			if content == "" {
				content = context
			} else {
				content += "\n\n" + context
			}
			data = map[string]any{"content": content}
		}
	}
	if data == nil {
		return wireResponse{}, nil
	}
	out, err := json.Marshal(map[string]any{"action": "modify", "data": data})
	if err != nil {
		return wireResponse{}, err
	}
	return wireResponse{Stdout: out}, nil
}
