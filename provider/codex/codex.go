// Package codex provides typed views over native Codex hook payloads.
//
// Codex ships a deliberate Claude dialect and publishes machine-readable
// JSON schemas in-repo; these structs mirror those schemas (DESIGN.md §3.2)
// and capture unknown fields in Extra so schema drift never drops data.
package codex

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// PreToolUseInput is the native PreToolUse payload.
type PreToolUseInput struct {
	SessionID      string                     `json:"session_id"`
	TurnID         string                     `json:"turn_id"`
	CWD            string                     `json:"cwd"`
	HookEventName  string                     `json:"hook_event_name"`
	PermissionMode string                     `json:"permission_mode"`
	ToolName       string                     `json:"tool_name"`
	ToolInput      json.RawMessage            `json:"tool_input"`
	ToolUseID      string                     `json:"tool_use_id"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// PostToolUseInput is the native PostToolUse payload. Codex folds tool
// failures into PostToolUse (§4.1) — inspect ToolResponse/Extra for error
// state.
type PostToolUseInput struct {
	SessionID     string                     `json:"session_id"`
	TurnID        string                     `json:"turn_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	ToolName      string                     `json:"tool_name"`
	ToolInput     json.RawMessage            `json:"tool_input"`
	ToolUseID     string                     `json:"tool_use_id"`
	ToolResponse  json.RawMessage            `json:"tool_response"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// PermissionRequestInput is the native PermissionRequest payload.
type PermissionRequestInput struct {
	SessionID     string                     `json:"session_id"`
	TurnID        string                     `json:"turn_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	ToolName      string                     `json:"tool_name"`
	ToolInput     json.RawMessage            `json:"tool_input"`
	ToolUseID     string                     `json:"tool_use_id"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// UserPromptSubmitInput is the native UserPromptSubmit payload.
type UserPromptSubmitInput struct {
	SessionID     string                     `json:"session_id"`
	TurnID        string                     `json:"turn_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Prompt        string                     `json:"prompt"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// StopInput is the native Stop / SubagentStop payload.
type StopInput struct {
	SessionID      string                     `json:"session_id"`
	TurnID         string                     `json:"turn_id"`
	CWD            string                     `json:"cwd"`
	HookEventName  string                     `json:"hook_event_name"`
	StopHookActive bool                       `json:"stop_hook_active"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// SessionStartInput is the native SessionStart payload.
type SessionStartInput struct {
	SessionID     string                     `json:"session_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Source        string                     `json:"source"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// SessionEndInput is the native SessionEnd payload.
type SessionEndInput struct {
	SessionID      string                     `json:"session_id"`
	TranscriptPath *string                    `json:"transcript_path"`
	CWD            string                     `json:"cwd"`
	HookEventName  string                     `json:"hook_event_name"`
	Reason         string                     `json:"reason"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// CompactInput is the native PreCompact / PostCompact payload.
type CompactInput struct {
	SessionID     string                     `json:"session_id"`
	TurnID        string                     `json:"turn_id"`
	CWD           string                     `json:"cwd"`
	HookEventName string                     `json:"hook_event_name"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// NotifyPayload is the legacy `codex notify` argv transport (kebab-case).
type NotifyPayload struct {
	Type          string                     `json:"type"`
	TurnID        string                     `json:"turn-id"`
	ThreadID      string                     `json:"thread-id"`
	CWD           string                     `json:"cwd"`
	LastAssistant string                     `json:"last-assistant-message"`
	Extra         map[string]json.RawMessage `json:"-"`
}

func view[T any](e *agenthooks.Event, native string) (*T, bool) {
	if e == nil || e.Provider != agenthooks.ProviderCodex || e.NativeName != native {
		return nil, false
	}
	var v T
	if err := jsonx.Unmarshal(e.Raw, &v); err != nil {
		return nil, false
	}
	return &v, true
}

func PreToolUse(e *agenthooks.Event) (*PreToolUseInput, bool) {
	return view[PreToolUseInput](e, "PreToolUse")
}

func PostToolUse(e *agenthooks.Event) (*PostToolUseInput, bool) {
	return view[PostToolUseInput](e, "PostToolUse")
}

func PermissionRequest(e *agenthooks.Event) (*PermissionRequestInput, bool) {
	return view[PermissionRequestInput](e, "PermissionRequest")
}

func UserPromptSubmit(e *agenthooks.Event) (*UserPromptSubmitInput, bool) {
	return view[UserPromptSubmitInput](e, "UserPromptSubmit")
}

func Stop(e *agenthooks.Event) (*StopInput, bool) {
	return view[StopInput](e, "Stop")
}

func SubagentStop(e *agenthooks.Event) (*StopInput, bool) {
	return view[StopInput](e, "SubagentStop")
}

func SessionStart(e *agenthooks.Event) (*SessionStartInput, bool) {
	return view[SessionStartInput](e, "SessionStart")
}

func SessionEnd(e *agenthooks.Event) (*SessionEndInput, bool) {
	return view[SessionEndInput](e, "SessionEnd")
}

func PreCompact(e *agenthooks.Event) (*CompactInput, bool) {
	return view[CompactInput](e, "PreCompact")
}

func PostCompact(e *agenthooks.Event) (*CompactInput, bool) {
	return view[CompactInput](e, "PostCompact")
}
