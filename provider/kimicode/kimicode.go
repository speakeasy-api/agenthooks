// Package kimicode provides complete typed views over native Kimi Code CLI
// hook payloads. The unified layer in the root package is convenience; this
// typed native layer is the fidelity guarantee: every struct captures
// unrecognized JSON keys in Extra, so provider drift never drops data.
//
// Structs are maintained against the Kimi Code hooks documentation with
// golden fixtures (DESIGN.md §10).
package kimicode

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// Base carries the fields every Kimi hook payload includes.
type Base struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
}

// PreToolUseInput is the native PreToolUse payload.
type PreToolUseInput struct {
	Base
	ToolName   string                     `json:"tool_name"`
	ToolInput  json.RawMessage            `json:"tool_input"`
	ToolCallID string                     `json:"tool_call_id"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// PostToolUseInput is the native PostToolUse payload.
type PostToolUseInput struct {
	Base
	ToolName   string                     `json:"tool_name"`
	ToolInput  json.RawMessage            `json:"tool_input"`
	ToolOutput json.RawMessage            `json:"tool_output"`
	ToolCallID string                     `json:"tool_call_id"` // observed on kimi-code 0.22.2 (undocumented)
	Extra      map[string]json.RawMessage `json:"-"`
}

// PostToolUseFailureInput is the native PostToolUseFailure payload.
type PostToolUseFailureInput struct {
	Base
	ToolName   string                     `json:"tool_name"`
	ToolInput  json.RawMessage            `json:"tool_input"`
	Error      string                     `json:"error"`
	ToolCallID string                     `json:"tool_call_id"` // absent on hook-blocked calls (kimi-code 0.22.2)
	Extra      map[string]json.RawMessage `json:"-"`
}

// PermissionInput is the native PermissionRequest / PermissionResult payload.
// Both are observation-only on Kimi.
type PermissionInput struct {
	Base
	ToolName  string                     `json:"tool_name"`
	ToolInput json.RawMessage            `json:"tool_input"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// UserPromptSubmitInput is the native UserPromptSubmit payload.
type UserPromptSubmitInput struct {
	Base
	Prompt string                     `json:"prompt"`
	Extra  map[string]json.RawMessage `json:"-"`
}

// StopInput is the native Stop payload.
type StopInput struct {
	Base
	StopHookActive bool                       `json:"stop_hook_active"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// StopFailureInput is the native StopFailure payload.
type StopFailureInput struct {
	Base
	ErrorType    string                     `json:"error_type"`
	ErrorMessage string                     `json:"error_message"`
	Extra        map[string]json.RawMessage `json:"-"`
}

// InterruptInput is the native Interrupt payload (user Esc, not timeouts).
type InterruptInput struct {
	Base
	Extra map[string]json.RawMessage `json:"-"`
}

// SessionStartInput is the native SessionStart payload.
type SessionStartInput struct {
	Base
	Source string                     `json:"source"` // "startup" | "resume"
	Extra  map[string]json.RawMessage `json:"-"`
}

// SessionEndInput is the native SessionEnd payload.
type SessionEndInput struct {
	Base
	Reason string                     `json:"reason"`
	Extra  map[string]json.RawMessage `json:"-"`
}

// SubagentStartInput is the native SubagentStart payload.
type SubagentStartInput struct {
	Base
	AgentName string                     `json:"agent_name"`
	Prompt    string                     `json:"prompt"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// SubagentStopInput is the native SubagentStop payload.
type SubagentStopInput struct {
	Base
	AgentName string                     `json:"agent_name"`
	Response  string                     `json:"response"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// PreCompactInput is the native PreCompact payload.
type PreCompactInput struct {
	Base
	Trigger    string                     `json:"trigger"` // "manual" | "auto"
	TokenCount int64                      `json:"token_count"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// PostCompactInput is the native PostCompact payload.
type PostCompactInput struct {
	Base
	Trigger             string                     `json:"trigger"`
	EstimatedTokenCount int64                      `json:"estimated_token_count"`
	Extra               map[string]json.RawMessage `json:"-"`
}

// NotificationInput is the native Notification payload.
type NotificationInput struct {
	Base
	Sink             string                     `json:"sink"`
	NotificationType string                     `json:"notification_type"` // e.g. "task.completed"
	Title            string                     `json:"title"`
	Body             string                     `json:"body"`
	Severity         string                     `json:"severity"`
	Extra            map[string]json.RawMessage `json:"-"`
}

func view[T any](e *agenthooks.Event, native string) (*T, bool) {
	if e == nil || e.Provider != agenthooks.ProviderKimi || e.NativeName != native {
		return nil, false
	}
	var v T
	if err := jsonx.Unmarshal(e.Raw, &v); err != nil {
		return nil, false
	}
	return &v, true
}

// Typed views: nil,false when the event is a different provider/native event.

func PreToolUse(e *agenthooks.Event) (*PreToolUseInput, bool) {
	return view[PreToolUseInput](e, "PreToolUse")
}

func PostToolUse(e *agenthooks.Event) (*PostToolUseInput, bool) {
	return view[PostToolUseInput](e, "PostToolUse")
}

func PostToolUseFailure(e *agenthooks.Event) (*PostToolUseFailureInput, bool) {
	return view[PostToolUseFailureInput](e, "PostToolUseFailure")
}

func PermissionRequest(e *agenthooks.Event) (*PermissionInput, bool) {
	return view[PermissionInput](e, "PermissionRequest")
}

func PermissionResult(e *agenthooks.Event) (*PermissionInput, bool) {
	return view[PermissionInput](e, "PermissionResult")
}

func UserPromptSubmit(e *agenthooks.Event) (*UserPromptSubmitInput, bool) {
	return view[UserPromptSubmitInput](e, "UserPromptSubmit")
}

func Stop(e *agenthooks.Event) (*StopInput, bool) {
	return view[StopInput](e, "Stop")
}

func StopFailure(e *agenthooks.Event) (*StopFailureInput, bool) {
	return view[StopFailureInput](e, "StopFailure")
}

func Interrupt(e *agenthooks.Event) (*InterruptInput, bool) {
	return view[InterruptInput](e, "Interrupt")
}

func SessionStart(e *agenthooks.Event) (*SessionStartInput, bool) {
	return view[SessionStartInput](e, "SessionStart")
}

func SessionEnd(e *agenthooks.Event) (*SessionEndInput, bool) {
	return view[SessionEndInput](e, "SessionEnd")
}

func SubagentStart(e *agenthooks.Event) (*SubagentStartInput, bool) {
	return view[SubagentStartInput](e, "SubagentStart")
}

func SubagentStop(e *agenthooks.Event) (*SubagentStopInput, bool) {
	return view[SubagentStopInput](e, "SubagentStop")
}

func PreCompact(e *agenthooks.Event) (*PreCompactInput, bool) {
	return view[PreCompactInput](e, "PreCompact")
}

func PostCompact(e *agenthooks.Event) (*PostCompactInput, bool) {
	return view[PostCompactInput](e, "PostCompact")
}

func Notification(e *agenthooks.Event) (*NotificationInput, bool) {
	return view[NotificationInput](e, "Notification")
}
