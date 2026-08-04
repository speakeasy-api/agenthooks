package agenthooks

import (
	"encoding/json"
	"fmt"
	"time"
)

// The wire codec serializes typed events for transport off the machine the
// hook fired on (client -> server relays, queues, storage). The format is
// schema-versioned, kind-tagged JSON:
//
//	{"v":"agenthooks.event.v1","kind":"tool.pre","event":{...},"payload":{...}}
//
// "event" is the core envelope (provider, variant, native name, time, the
// core session subset, agent, raw payload); "payload" carries the
// kind-specific fields of the typed event. The schema is decoupled from Go
// field naming via internal DTOs with explicit json tags, so renaming a Go
// field can never silently change the wire.
//
// Deliberately NOT serialized:
//   - Event.Ext: embedder-local extension data; each side stamps its own.
//   - Everything on ClientEvent (detection confidence, backfill flag,
//     LocalSession): client-local by definition.
//
// Schema stability rules (v1): append-only. New optional fields may be
// added; existing fields are never renamed, retyped, or removed. Anything
// non-additive requires a new version string. DecodeWire rejects unknown
// versions, tolerates unknown fields (forward compatibility), and
// zero-values missing optional fields.

// WireSchemaV1 is the wire schema version this package encodes.
const WireSchemaV1 = "agenthooks.event.v1"

type wireEnvelope struct {
	V       string          `json:"v"`
	Kind    string          `json:"kind"`
	Event   wireEvent       `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type wireEvent struct {
	Provider   string          `json:"provider,omitempty"`
	Variant    string          `json:"variant,omitempty"`
	NativeName string          `json:"native_name,omitempty"`
	Time       time.Time       `json:"time,omitzero"`
	Session    wireSession     `json:"session,omitzero"`
	Agent      *wireAgent      `json:"agent,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

type wireSession struct {
	ID        string `json:"id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Model     string `json:"model,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
}

type wireAgent struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type,omitempty"`
}

type wireTool struct {
	ID          string          `json:"id,omitempty"`
	Synthesized bool            `json:"synthesized,omitempty"`
	Name        string          `json:"name,omitempty"`
	Canonical   string          `json:"canonical,omitempty"`
	MCP         *wireMCP        `json:"mcp,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	RawInput    json.RawMessage `json:"raw_input,omitempty"`
}

type wireMCP struct {
	Server     string `json:"server,omitempty"`
	Tool       string `json:"tool,omitempty"`
	URL        string `json:"url,omitempty"`
	Command    string `json:"command,omitempty"`
	FromConfig bool   `json:"from_config,omitempty"`
}

type wireUsage struct {
	InputTokens      *int     `json:"input_tokens,omitempty"`
	OutputTokens     *int     `json:"output_tokens,omitempty"`
	CacheReadTokens  *int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int     `json:"cache_write_tokens,omitempty"`
	Cost             *float64 `json:"cost,omitempty"`
	LoopCount        *int     `json:"loop_count,omitempty"`
	Status           string   `json:"status,omitempty"`
}

// Kind-specific payload DTOs. SubagentStartEvent and ModelEvent carry no
// payload fields; their wire form has no "payload" member.

type wireSessionStartPayload struct {
	Source string `json:"source,omitempty"`
}

type wireSessionEndPayload struct {
	Reason string `json:"reason,omitempty"`
}

type wirePromptPayload struct {
	Prompt string `json:"prompt,omitempty"`
}

type wireToolPayload struct {
	Tool wireTool `json:"tool"`
}

type wireToolPostPayload struct {
	Tool       wireTool        `json:"tool"`
	Output     json.RawMessage `json:"output,omitempty"`
	Failed     bool            `json:"failed,omitempty"`
	Error      string          `json:"error,omitempty"`
	DurationMS *float64        `json:"duration_ms,omitempty"`
}

type wireStopPayload struct {
	PreviouslyContinued bool       `json:"previously_continued,omitempty"`
	LoopCount           int        `json:"loop_count,omitempty"`
	FinalMessage        string     `json:"final_message,omitempty"`
	Usage               *wireUsage `json:"usage,omitempty"`
}

type wireCompactPayload struct {
	Trigger      string `json:"trigger,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type wireNotificationPayload struct {
	Message string `json:"message,omitempty"`
}

type wireFileEditedPayload struct {
	Path string `json:"path,omitempty"`
}

// wireKinds pins which kind tags each concrete typed event may legally
// carry, in both directions: EncodeWire validates the event's Kind against
// it, DecodeWire selects the concrete type from the tag. Kinds absent from
// the map (KindOther, any future kind) ride the bare *Event envelope.
var wireKinds = map[EventKind]string{
	KindSessionStart:    "session-start",
	KindSessionEnd:      "session-end",
	KindPromptSubmitted: "prompt",
	KindToolPre:         "tool-pre",
	KindPermission:      "permission",
	KindToolPost:        "tool-post",
	KindToolError:       "tool-post",
	KindStop:            "stop",
	KindSubagentStop:    "stop",
	KindSubagentStart:   "subagent-start",
	KindCompactPre:      "compact",
	KindCompactPost:     "compact",
	KindNotification:    "notification",
	KindFileEdited:      "file-edited",
	KindModelRequest:    "model",
	KindModelResponse:   "model",
}

// EncodeWire serializes a typed event (or a bare *Event for unmapped kinds)
// into the versioned wire form. Ext and all client-local state are excluded
// by construction.
func EncodeWire(typed any) ([]byte, error) {
	base := eventOf(typed)
	if base == nil {
		return nil, fmt.Errorf("agenthooks: EncodeWire: %T is not an agenthooks event", typed)
	}
	family, ok := wireFamily(typed)
	if !ok {
		return nil, fmt.Errorf("agenthooks: EncodeWire: unsupported event type %T", typed)
	}
	if want, mapped := wireKinds[base.Kind]; mapped != (family != "") || (mapped && want != family) {
		return nil, fmt.Errorf("agenthooks: EncodeWire: kind %q does not belong to %T", base.Kind, typed)
	}

	var payload any
	switch ev := typed.(type) {
	case *Event:
		payload = nil
	case *SessionStartEvent:
		payload = &wireSessionStartPayload{Source: ev.Source}
	case *SessionEndEvent:
		payload = &wireSessionEndPayload{Reason: ev.Reason}
	case *PromptEvent:
		payload = &wirePromptPayload{Prompt: ev.Prompt}
	case *ToolPreEvent:
		payload = &wireToolPayload{Tool: toolToWire(ev.Tool)}
	case *PermissionEvent:
		payload = &wireToolPayload{Tool: toolToWire(ev.Tool)}
	case *ToolPostEvent:
		payload = &wireToolPostPayload{
			Tool:       toolToWire(ev.Tool),
			Output:     ev.Output,
			Failed:     ev.Failed,
			Error:      ev.Error,
			DurationMS: ev.DurationMS,
		}
	case *StopEvent:
		payload = &wireStopPayload{
			PreviouslyContinued: ev.PreviouslyContinued,
			LoopCount:           ev.LoopCount,
			FinalMessage:        ev.FinalMessage,
			Usage:               usageToWire(ev.Usage),
		}
	case *SubagentStartEvent, *ModelEvent:
		payload = nil
	case *CompactEvent:
		payload = &wireCompactPayload{Trigger: ev.Trigger, Instructions: ev.Instructions}
	case *NotificationEvent:
		payload = &wireNotificationPayload{Message: ev.Message}
	case *FileEditedEvent:
		payload = &wireFileEditedPayload{Path: ev.Path}
	}

	env := wireEnvelope{
		V:     WireSchemaV1,
		Kind:  string(base.Kind),
		Event: eventToWire(base),
	}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("agenthooks: EncodeWire: marshaling %s payload: %w", base.Kind, err)
		}
		env.Payload = b
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("agenthooks: EncodeWire: %w", err)
	}
	return out, nil
}

// DecodeWire parses the versioned wire form back into the concrete typed
// event (*ToolPreEvent, *PromptEvent, ...). Unknown schema versions are an
// error; unknown JSON fields are ignored (forward compatibility); missing
// optional fields decode to their zero values. Kind tags with no typed
// mapping — KindOther today, kinds introduced by future versions of this
// package — decode to a bare *Event so the envelope survives even when the
// payload shape is unknown.
func DecodeWire(data []byte) (any, error) {
	var env wireEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("agenthooks: DecodeWire: %w", err)
	}
	if env.V != WireSchemaV1 {
		return nil, fmt.Errorf("agenthooks: DecodeWire: unsupported wire schema version %q (want %q)", env.V, WireSchemaV1)
	}
	base := eventFromWire(&env)

	unmarshalPayload := func(dst any) error {
		if len(env.Payload) == 0 {
			return nil
		}
		if err := json.Unmarshal(env.Payload, dst); err != nil {
			return fmt.Errorf("agenthooks: DecodeWire: parsing %s payload: %w", env.Kind, err)
		}
		return nil
	}

	switch EventKind(env.Kind) {
	case KindSessionStart:
		var p wireSessionStartPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &SessionStartEvent{Event: base, Source: p.Source}, nil
	case KindSessionEnd:
		var p wireSessionEndPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &SessionEndEvent{Event: base, Reason: p.Reason}, nil
	case KindPromptSubmitted:
		var p wirePromptPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &PromptEvent{Event: base, Prompt: p.Prompt}, nil
	case KindToolPre:
		var p wireToolPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &ToolPreEvent{Event: base, Tool: toolFromWire(p.Tool)}, nil
	case KindPermission:
		var p wireToolPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &PermissionEvent{Event: base, Tool: toolFromWire(p.Tool)}, nil
	case KindToolPost, KindToolError:
		var p wireToolPostPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &ToolPostEvent{
			Event:      base,
			Tool:       toolFromWire(p.Tool),
			Output:     p.Output,
			Failed:     p.Failed,
			Error:      p.Error,
			DurationMS: p.DurationMS,
		}, nil
	case KindStop, KindSubagentStop:
		var p wireStopPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &StopEvent{
			Event:               base,
			PreviouslyContinued: p.PreviouslyContinued,
			LoopCount:           p.LoopCount,
			FinalMessage:        p.FinalMessage,
			Usage:               usageFromWire(p.Usage),
		}, nil
	case KindSubagentStart:
		return &SubagentStartEvent{Event: base}, nil
	case KindCompactPre, KindCompactPost:
		var p wireCompactPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &CompactEvent{Event: base, Trigger: p.Trigger, Instructions: p.Instructions}, nil
	case KindNotification:
		var p wireNotificationPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &NotificationEvent{Event: base, Message: p.Message}, nil
	case KindFileEdited:
		var p wireFileEditedPayload
		if err := unmarshalPayload(&p); err != nil {
			return nil, err
		}
		return &FileEditedEvent{Event: base, Path: p.Path}, nil
	case KindModelRequest, KindModelResponse:
		return &ModelEvent{Event: base}, nil
	}
	// KindOther and kinds this build does not know: keep the envelope.
	ev := base
	return &ev, nil
}

// wireFamily names the payload family of a supported typed event, so encode
// can validate the Kind/type pairing. The empty family is the bare envelope.
func wireFamily(typed any) (string, bool) {
	switch typed.(type) {
	case *Event:
		return "", true
	case *SessionStartEvent:
		return "session-start", true
	case *SessionEndEvent:
		return "session-end", true
	case *PromptEvent:
		return "prompt", true
	case *ToolPreEvent:
		return "tool-pre", true
	case *PermissionEvent:
		return "permission", true
	case *ToolPostEvent:
		return "tool-post", true
	case *StopEvent:
		return "stop", true
	case *SubagentStartEvent:
		return "subagent-start", true
	case *CompactEvent:
		return "compact", true
	case *NotificationEvent:
		return "notification", true
	case *FileEditedEvent:
		return "file-edited", true
	case *ModelEvent:
		return "model", true
	}
	return "", false
}

func eventToWire(base *Event) wireEvent {
	w := wireEvent{
		Provider:   string(base.Provider),
		Variant:    string(base.Variant),
		NativeName: base.NativeName,
		Time:       base.Time,
		Session: wireSession{
			ID:        base.Session.ID,
			TurnID:    base.Session.TurnID,
			CWD:       base.Session.CWD,
			Model:     base.Session.Model,
			UserEmail: base.Session.UserEmail,
		},
		Raw: base.Raw,
	}
	if base.Agent != nil {
		w.Agent = &wireAgent{ID: base.Agent.ID, Type: base.Agent.Type}
	}
	return w
}

func eventFromWire(env *wireEnvelope) Event {
	e := Event{
		Provider:   Provider(env.Event.Provider),
		Variant:    Variant(env.Event.Variant),
		NativeName: env.Event.NativeName,
		Kind:       EventKind(env.Kind),
		Time:       env.Event.Time,
		Session: SessionInfo{
			ID:        env.Event.Session.ID,
			TurnID:    env.Event.Session.TurnID,
			CWD:       env.Event.Session.CWD,
			Model:     env.Event.Session.Model,
			UserEmail: env.Event.Session.UserEmail,
		},
		Raw: env.Event.Raw,
	}
	if env.Event.Agent != nil {
		e.Agent = &AgentInfo{ID: env.Event.Agent.ID, Type: env.Event.Agent.Type}
	}
	return e
}

func toolToWire(t ToolCall) wireTool {
	w := wireTool{
		ID:          t.ID,
		Synthesized: t.Synthesized,
		Name:        t.Name,
		Canonical:   string(t.Canonical),
		Input:       t.Input,
		RawInput:    t.RawInput,
	}
	if t.MCP != nil {
		w.MCP = &wireMCP{
			Server:     t.MCP.Server,
			Tool:       t.MCP.Tool,
			URL:        t.MCP.URL,
			Command:    t.MCP.Command,
			FromConfig: t.MCP.FromConfig,
		}
	}
	return w
}

func toolFromWire(w wireTool) ToolCall {
	t := ToolCall{
		ID:          w.ID,
		Synthesized: w.Synthesized,
		Name:        w.Name,
		Canonical:   CanonicalTool(w.Canonical),
		Input:       w.Input,
		RawInput:    w.RawInput,
	}
	if w.MCP != nil {
		t.MCP = &MCPCall{
			Server:     w.MCP.Server,
			Tool:       w.MCP.Tool,
			URL:        w.MCP.URL,
			Command:    w.MCP.Command,
			FromConfig: w.MCP.FromConfig,
		}
	}
	return t
}

func usageToWire(u *Usage) *wireUsage {
	if u == nil {
		return nil
	}
	return &wireUsage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		Cost:             u.Cost,
		LoopCount:        u.LoopCount,
		Status:           u.Status,
	}
}

func usageFromWire(w *wireUsage) *Usage {
	if w == nil {
		return nil
	}
	return &Usage{
		InputTokens:      w.InputTokens,
		OutputTokens:     w.OutputTokens,
		CacheReadTokens:  w.CacheReadTokens,
		CacheWriteTokens: w.CacheWriteTokens,
		Cost:             w.Cost,
		LoopCount:        w.LoopCount,
		Status:           w.Status,
	}
}
