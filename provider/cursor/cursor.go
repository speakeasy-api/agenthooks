// Package cursor provides typed views over native Cursor hook payloads
// (IDE, cursor-agent CLI, and cloud agents). Unknown fields land in Extra.
//
// Note the input-shape quirks the unified layer normalizes: tool_input is an
// object on preToolUse but a JSON-encoded string on MCP events (quirk #5),
// and MCP events omit tool_use_id (quirk #3). These views hand you the wire
// truth; use the unified ToolCall for the normalized form.
package cursor

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// BeforeShellExecutionInput is the native beforeShellExecution payload.
type BeforeShellExecutionInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	UserEmail      string                     `json:"user_email"`
	Command        string                     `json:"command"`
	CWD            string                     `json:"cwd"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// AfterShellExecutionInput is the native afterShellExecution payload.
type AfterShellExecutionInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	Command        string                     `json:"command"`
	Output         json.RawMessage            `json:"output"`
	ExitCode       *int                       `json:"exit_code"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// BeforeMCPExecutionInput is the native beforeMCPExecution payload. ToolInput
// arrives as a JSON-encoded string (quirk #5) and ToolUseID is typically
// absent (quirk #3).
type BeforeMCPExecutionInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	ToolName       string                     `json:"tool_name"`
	ToolInput      json.RawMessage            `json:"tool_input"`
	MCPServerName  string                     `json:"mcp_server_name"`
	URL            string                     `json:"url"`
	Command        string                     `json:"command"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// AfterMCPExecutionInput is the native afterMCPExecution payload.
type AfterMCPExecutionInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	ToolName       string                     `json:"tool_name"`
	ToolInput      json.RawMessage            `json:"tool_input"`
	ToolResponse   json.RawMessage            `json:"tool_response"`
	MCPServerName  string                     `json:"mcp_server_name"`
	URL            string                     `json:"url"`
	Command        string                     `json:"command"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// PreToolUseInput is the native preToolUse payload (generic form; fires in
// addition to the specific shell/MCP events for the same call, quirk #2).
type PreToolUseInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	ToolName       string                     `json:"tool_name"`
	ToolInput      json.RawMessage            `json:"tool_input"`
	ToolUseID      string                     `json:"tool_use_id"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// PostToolUseInput is the native postToolUse payload.
type PostToolUseInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	ToolName       string                     `json:"tool_name"`
	ToolInput      json.RawMessage            `json:"tool_input"`
	ToolUseID      string                     `json:"tool_use_id"`
	ToolResponse   json.RawMessage            `json:"tool_response"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// BeforeReadFileInput is the native beforeReadFile payload (allow/deny only).
type BeforeReadFileInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	FilePath       string                     `json:"file_path"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// AfterFileEditInput is the native afterFileEdit payload.
type AfterFileEditInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	FilePath       string                     `json:"file_path"`
	Edits          json.RawMessage            `json:"edits"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// BeforeSubmitPromptInput is the native beforeSubmitPrompt payload.
type BeforeSubmitPromptInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	Prompt         string                     `json:"prompt"`
	Attachments    json.RawMessage            `json:"attachments"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// StopInput is the native stop / subagentStop payload.
type StopInput struct {
	ConversationID string                     `json:"conversation_id"`
	GenerationID   string                     `json:"generation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	Status         string                     `json:"status"`
	LoopCount      int                        `json:"loop_count"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// SessionStartInput is the native sessionStart / sessionEnd payload.
type SessionStartInput struct {
	ConversationID string                     `json:"conversation_id"`
	HookEventName  string                     `json:"hook_event_name"`
	WorkspaceRoots []string                   `json:"workspace_roots"`
	UserEmail      string                     `json:"user_email"`
	Model          string                     `json:"model"`
	Extra          map[string]json.RawMessage `json:"-"`
}

func view[T any](e *agenthooks.Event, native string) (*T, bool) {
	if e == nil || e.Provider != agenthooks.ProviderCursor || e.NativeName != native {
		return nil, false
	}
	var v T
	if err := jsonx.Unmarshal(e.Raw, &v); err != nil {
		return nil, false
	}
	return &v, true
}

func BeforeShellExecution(e *agenthooks.Event) (*BeforeShellExecutionInput, bool) {
	return view[BeforeShellExecutionInput](e, "beforeShellExecution")
}

func AfterShellExecution(e *agenthooks.Event) (*AfterShellExecutionInput, bool) {
	return view[AfterShellExecutionInput](e, "afterShellExecution")
}

func BeforeMCPExecution(e *agenthooks.Event) (*BeforeMCPExecutionInput, bool) {
	return view[BeforeMCPExecutionInput](e, "beforeMCPExecution")
}

func AfterMCPExecution(e *agenthooks.Event) (*AfterMCPExecutionInput, bool) {
	return view[AfterMCPExecutionInput](e, "afterMCPExecution")
}

func PreToolUse(e *agenthooks.Event) (*PreToolUseInput, bool) {
	return view[PreToolUseInput](e, "preToolUse")
}

func PostToolUse(e *agenthooks.Event) (*PostToolUseInput, bool) {
	return view[PostToolUseInput](e, "postToolUse")
}

func BeforeReadFile(e *agenthooks.Event) (*BeforeReadFileInput, bool) {
	return view[BeforeReadFileInput](e, "beforeReadFile")
}

func AfterFileEdit(e *agenthooks.Event) (*AfterFileEditInput, bool) {
	return view[AfterFileEditInput](e, "afterFileEdit")
}

func BeforeSubmitPrompt(e *agenthooks.Event) (*BeforeSubmitPromptInput, bool) {
	return view[BeforeSubmitPromptInput](e, "beforeSubmitPrompt")
}

func Stop(e *agenthooks.Event) (*StopInput, bool) {
	return view[StopInput](e, "stop")
}

func SubagentStop(e *agenthooks.Event) (*StopInput, bool) {
	return view[StopInput](e, "subagentStop")
}

func SessionStart(e *agenthooks.Event) (*SessionStartInput, bool) {
	return view[SessionStartInput](e, "sessionStart")
}

func SessionEnd(e *agenthooks.Event) (*SessionStartInput, bool) {
	return view[SessionStartInput](e, "sessionEnd")
}
