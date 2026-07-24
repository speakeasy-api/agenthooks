// Package opencode provides typed views over the NDJSON frames the generated
// agenthooks OpenCode shim plugin proxies from OpenCode's in-process plugin
// hooks (DESIGN.md §8). Event.Raw for OpenCode events is the verbatim frame:
// {seq, hook, input, output}.
package opencode

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// Frame is the shim wire request. Output is the mutable object OpenCode
// passes to plugin hooks; replies merge into it (Object.assign, arrays
// replaced wholesale).
type Frame struct {
	Seq    int64                      `json:"seq"`
	Hook   string                     `json:"hook"`
	Input  json.RawMessage            `json:"input"`
	Output json.RawMessage            `json:"output"`
	Extra  map[string]json.RawMessage `json:"-"`
}

// Reply is the shim wire response. Error re-throws in the plugin to keep
// block-the-tool behavior.
type Reply struct {
	Seq    int64          `json:"seq"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// InitializeInput is the startup frame payload carrying OpenCode server info
// for the optional HTTP client (permission replies via
// POST /session/:id/permissions/:permissionID, context injection via
// session.prompt with noReply).
type InitializeInput struct {
	ServerURL string                     `json:"serverUrl"`
	Directory string                     `json:"directory"`
	Worktree  string                     `json:"worktree"`
	MCP       map[string]MCPServerConfig `json:"mcp"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// MCPServerConfig is the sanitized active MCP inventory sent by the shim.
type MCPServerConfig struct {
	Type    string                     `json:"type"`
	Command []string                   `json:"command"`
	URL     string                     `json:"url"`
	Enabled *bool                      `json:"enabled"`
	Extra   map[string]json.RawMessage `json:"-"`
}

// ToolExecuteBeforeInput is the input half of tool.execute.before.
type ToolExecuteBeforeInput struct {
	SessionID string                     `json:"sessionID"`
	CallID    string                     `json:"callID"`
	Tool      string                     `json:"tool"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// ToolExecuteBeforeOutput is the mutable output half of tool.execute.before.
type ToolExecuteBeforeOutput struct {
	Args  json.RawMessage            `json:"args"`
	Extra map[string]json.RawMessage `json:"-"`
}

// ToolExecuteAfterOutput is the mutable output half of tool.execute.after.
type ToolExecuteAfterOutput struct {
	Title    string                     `json:"title"`
	Output   json.RawMessage            `json:"output"`
	Metadata json.RawMessage            `json:"metadata"`
	Extra    map[string]json.RawMessage `json:"-"`
}

// PermissionAskedInput mirrors the permission.asked event payload. The typed
// plugin hook is dead upstream (quirk #18); replies go over HTTP.
type PermissionAskedInput struct {
	SessionID    string                     `json:"sessionID"`
	PermissionID string                     `json:"permissionID"`
	Type         string                     `json:"type"`
	Title        string                     `json:"title"`
	Extra        map[string]json.RawMessage `json:"-"`
}

// DecodeFrame returns the verbatim frame behind an OpenCode event.
func DecodeFrame(e *agenthooks.Event) (*Frame, bool) {
	if e == nil || e.Provider != agenthooks.ProviderOpenCode {
		return nil, false
	}
	var fr Frame
	if err := jsonx.Unmarshal(e.Raw, &fr); err != nil {
		return nil, false
	}
	return &fr, true
}

// ToolExecuteBefore decodes both halves of a tool.execute.before frame.
func ToolExecuteBefore(e *agenthooks.Event) (*ToolExecuteBeforeInput, *ToolExecuteBeforeOutput, bool) {
	if e == nil || e.Provider != agenthooks.ProviderOpenCode || e.NativeName != "tool.execute.before" {
		return nil, nil, false
	}
	fr, ok := DecodeFrame(e)
	if !ok {
		return nil, nil, false
	}
	var in ToolExecuteBeforeInput
	var out ToolExecuteBeforeOutput
	if err := jsonx.Unmarshal(fr.Input, &in); err != nil {
		return nil, nil, false
	}
	if len(fr.Output) > 0 {
		if err := jsonx.Unmarshal(fr.Output, &out); err != nil {
			return nil, nil, false
		}
	}
	return &in, &out, true
}

// ToolExecuteAfter decodes both halves of a tool.execute.after frame.
func ToolExecuteAfter(e *agenthooks.Event) (*ToolExecuteBeforeInput, *ToolExecuteAfterOutput, bool) {
	if e == nil || e.Provider != agenthooks.ProviderOpenCode || e.NativeName != "tool.execute.after" {
		return nil, nil, false
	}
	fr, ok := DecodeFrame(e)
	if !ok {
		return nil, nil, false
	}
	var in ToolExecuteBeforeInput
	var out ToolExecuteAfterOutput
	if err := jsonx.Unmarshal(fr.Input, &in); err != nil {
		return nil, nil, false
	}
	if len(fr.Output) > 0 {
		if err := jsonx.Unmarshal(fr.Output, &out); err != nil {
			return nil, nil, false
		}
	}
	return &in, &out, true
}

// PermissionAsked decodes a permission.asked frame's input.
func PermissionAsked(e *agenthooks.Event) (*PermissionAskedInput, bool) {
	if e == nil || e.Provider != agenthooks.ProviderOpenCode || e.NativeName != "permission.asked" {
		return nil, false
	}
	fr, ok := DecodeFrame(e)
	if !ok {
		return nil, false
	}
	var in PermissionAskedInput
	if err := jsonx.Unmarshal(fr.Input, &in); err != nil {
		return nil, false
	}
	return &in, true
}
