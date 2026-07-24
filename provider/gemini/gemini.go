// Package gemini provides typed views over native Gemini CLI hook payloads.
// Gemini is Claude-inspired with renamed events (PreToolUse→BeforeTool,
// Stop→AfterAgent) plus a timestamp on every payload and model-level hooks
// Claude lacks. Unknown fields land in Extra.
package gemini

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// MCPToolContext is the exact MCP identity and non-sensitive transport
// metadata Gemini includes on BeforeTool and AfterTool payloads.
type MCPToolContext struct {
	ServerName string   `json:"server_name"`
	ToolName   string   `json:"tool_name"`
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	CWD        string   `json:"cwd"`
	URL        string   `json:"url"`
	TCP        string   `json:"tcp"`
}

// BeforeToolInput is the native BeforeTool payload.
type BeforeToolInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	ToolName      string                     `json:"tool_name"`
	ToolInput     json.RawMessage            `json:"tool_input"`
	ToolCallID    string                     `json:"tool_call_id"`
	MCPContext    *MCPToolContext            `json:"mcp_context"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// AfterToolInput is the native AfterTool payload. Failures ride
// tool_response.error rather than a dedicated event (§4.1).
type AfterToolInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	ToolName      string                     `json:"tool_name"`
	ToolInput     json.RawMessage            `json:"tool_input"`
	ToolCallID    string                     `json:"tool_call_id"`
	ToolResponse  json.RawMessage            `json:"tool_response"`
	MCPContext    *MCPToolContext            `json:"mcp_context"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// BeforeAgentInput is the native BeforeAgent payload.
type BeforeAgentInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	Prompt        string                     `json:"prompt"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// AfterAgentInput is the native AfterAgent payload.
type AfterAgentInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// SessionStartInput is the native SessionStart payload. Note the open
// upstream regression where SessionStart may not fire (§1).
type SessionStartInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	Source        string                     `json:"source"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// SessionEndInput is the native SessionEnd payload.
type SessionEndInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	Reason        string                     `json:"reason"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// PreCompressInput is the native PreCompress payload.
type PreCompressInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	Trigger       string                     `json:"trigger"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// NotificationInput is the native Notification payload.
type NotificationInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	Message       string                     `json:"message"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// BeforeModelInput is the native BeforeModel / BeforeToolSelection payload
// (model-level hooks Claude lacks; hot path, experimental in the unified
// layer, §12.3).
type BeforeModelInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	Request       json.RawMessage            `json:"request"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// AfterModelInput is the native AfterModel payload. Fires per streamed chunk.
type AfterModelInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Timestamp     string                     `json:"timestamp"`
	Response      json.RawMessage            `json:"response"`
	Extra         map[string]json.RawMessage `json:"-"`
}

func view[T any](e *agenthooks.Event, native string) (*T, bool) {
	if e == nil || e.Provider != agenthooks.ProviderGemini || e.NativeName != native {
		return nil, false
	}
	var v T
	if err := jsonx.Unmarshal(e.Raw, &v); err != nil {
		return nil, false
	}
	return &v, true
}

func BeforeTool(e *agenthooks.Event) (*BeforeToolInput, bool) {
	return view[BeforeToolInput](e, "BeforeTool")
}

func AfterTool(e *agenthooks.Event) (*AfterToolInput, bool) {
	return view[AfterToolInput](e, "AfterTool")
}

func BeforeAgent(e *agenthooks.Event) (*BeforeAgentInput, bool) {
	return view[BeforeAgentInput](e, "BeforeAgent")
}

func AfterAgent(e *agenthooks.Event) (*AfterAgentInput, bool) {
	return view[AfterAgentInput](e, "AfterAgent")
}

func SessionStart(e *agenthooks.Event) (*SessionStartInput, bool) {
	return view[SessionStartInput](e, "SessionStart")
}

func SessionEnd(e *agenthooks.Event) (*SessionEndInput, bool) {
	return view[SessionEndInput](e, "SessionEnd")
}

func PreCompress(e *agenthooks.Event) (*PreCompressInput, bool) {
	return view[PreCompressInput](e, "PreCompress")
}

func Notification(e *agenthooks.Event) (*NotificationInput, bool) {
	return view[NotificationInput](e, "Notification")
}

func BeforeModel(e *agenthooks.Event) (*BeforeModelInput, bool) {
	return view[BeforeModelInput](e, "BeforeModel")
}

func BeforeToolSelection(e *agenthooks.Event) (*BeforeModelInput, bool) {
	return view[BeforeModelInput](e, "BeforeToolSelection")
}

func AfterModel(e *agenthooks.Event) (*AfterModelInput, bool) {
	return view[AfterModelInput](e, "AfterModel")
}
