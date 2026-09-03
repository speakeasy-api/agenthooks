package agenthooks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Provider identifies the coding agent that invoked the hook.
type Provider string

const (
	ProviderClaudeCode Provider = "claude-code" // incl. cowork/desktop/web variants
	ProviderCursor     Provider = "cursor"      // incl. cursor-agent CLI, cloud agents
	ProviderCodex      Provider = "codex"
	ProviderGemini     Provider = "gemini"
	ProviderOpenCode   Provider = "opencode"
	ProviderOpenClaw   Provider = "openclaw"  // OpenClaw Gateway (typed plugin hooks via shim)
	ProviderMoltis     Provider = "moltis"    // Moltis Gateway (process-per-event shell hooks)
	ProviderKimi       Provider = "kimi-code" // Kimi Code CLI ("kimi" accepted as a flag alias)
	// Copilot ships two dialects behind one product name, so a consumer
	// switching on "is this Copilot?" must consider both constants.
	ProviderCopilotCLI    Provider = "copilot-cli"    // GitHub Copilot CLI (camelCase dialect)
	ProviderVSCodeCopilot Provider = "vscode-copilot" // Copilot Chat in VS Code (Claude-shaped dialect)
)

// Variant refines Provider where runtime behavior genuinely differs.
type Variant string

const (
	VariantUnknown Variant = ""
	VariantCLI     Variant = "cli"
	VariantIDE     Variant = "ide"
	VariantCloud   Variant = "cloud"
	VariantCowork  Variant = "cowork"
	VariantRemote  Variant = "remote"
)

// EventKind is the unified, Claude-shaped event taxonomy. The set is open:
// any native event with no mapping is delivered as KindOther with the native
// name and raw payload intact.
type EventKind string

const (
	KindSessionStart    EventKind = "session.start"
	KindSessionEnd      EventKind = "session.end"
	KindMCPInventory    EventKind = "mcp.inventory"
	KindPromptSubmitted EventKind = "prompt.submitted"
	KindToolPre         EventKind = "tool.pre" // gate/rewrite before execution
	KindToolPost        EventKind = "tool.post"
	KindToolError       EventKind = "tool.error"
	KindPermission      EventKind = "permission.request"
	KindStop            EventKind = "agent.stop" // turn finished
	KindSubagentStart   EventKind = "subagent.start"
	KindSubagentStop    EventKind = "subagent.stop"
	KindCompactPre      EventKind = "compact.pre"
	KindCompactPost     EventKind = "compact.post"
	KindNotification    EventKind = "notification"
	KindFileEdited      EventKind = "file.edited"   // post-hoc file-change reports
	KindModelRequest    EventKind = "model.request" // gemini BeforeModel, opencode chat.params
	KindModelResponse   EventKind = "model.response"
	KindOther           EventKind = "other" // any unmapped native event
)

// DetectionConfidence records how the invoking provider was identified.
type DetectionConfidence string

const (
	DetectionConfig DetectionConfidence = "config" // --provider flag from generated config
	DetectionEnv    DetectionConfidence = "env"    // environment variable sniffing
	DetectionShape  DetectionConfidence = "shape"  // payload shape sniffing
)

// Event is the unified envelope. Raw is always the verbatim provider payload;
// normalization is a projection over it, never a replacement.
type Event struct {
	Provider   Provider
	Variant    Variant
	NativeName string    // "PreToolUse", "beforeShellExecution", "BeforeTool", ...
	Kind       EventKind // normalized; KindOther when unmapped
	Time       time.Time // library receive time; Gemini also supplies its own

	Session SessionInfo
	Agent   *AgentInfo // non-nil inside a subagent context

	DetectionConfidence DetectionConfidence

	// Backfilled marks an event the provider never sent: some providers skip
	// events in some modes (quirks #30, #31), and the runner synthesizes the
	// miss on the next delivered event, best-effort. Backfilled events are
	// reporting-only — Raw is nil, Can() reports no capabilities, and any
	// decision returned by the handler is discarded.
	Backfilled bool

	// Raw is the verbatim provider payload. Never normalized, never trimmed.
	Raw json.RawMessage

	// Ext carries embedder-defined extension data keyed by embedder-chosen
	// names. The library never reads, writes, or propagates it: it exists for
	// applications that construct typed events themselves (e.g. a server-side
	// ingest path re-projecting stored payloads) to stamp app-specific
	// context their own handlers read back. Nil on every event the library
	// decodes.
	Ext map[string]any
}

// SessionInfo carries the normalized session identity fields.
type SessionInfo struct {
	ID             string // claude/codex/gemini session_id; cursor conversation_id
	TurnID         string // codex turn_id, cursor generation_id, claude prompt_id ("" if absent)
	CWD            string
	WorkspaceRoots []string // cursor multi-root; others: [CWD] or project dir
	TranscriptPath string   // "" if unavailable; format is provider-specific (see transcript pkg)
	Model          string   // "" if not reported
	PermissionMode string   // claude/codex permission_mode; "" elsewhere
	UserEmail      string   // cursor user_email; "" elsewhere
}

// AgentInfo describes the subagent context, when the event fired inside one.
type AgentInfo struct {
	ID   string
	Type string // provider's subagent type name
}

// RawField resolves a dot-separated path (object keys and array indexes) into
// the raw payload. It returns nil when the path does not resolve.
func (e *Event) RawField(path string) json.RawMessage {
	return rawField(e.Raw, path)
}

func rawField(raw json.RawMessage, path string) json.RawMessage {
	cur := raw
	if len(cur) == 0 {
		return nil
	}
	for _, seg := range strings.Split(path, ".") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cur, &obj); err == nil {
			v, ok := obj[seg]
			if !ok {
				return nil
			}
			cur = v
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(cur, &arr); err == nil {
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(arr) {
				return nil
			}
			cur = arr[i]
			continue
		}
		return nil
	}
	return cur
}

// CanonicalTool classifies native tools into a small cross-provider taxonomy.
type CanonicalTool string

const (
	ToolShell     CanonicalTool = "shell"
	ToolFileRead  CanonicalTool = "file.read"
	ToolFileWrite CanonicalTool = "file.write"
	ToolFileEdit  CanonicalTool = "file.edit"
	ToolSearch    CanonicalTool = "search"
	ToolFetch     CanonicalTool = "fetch"
	ToolTask      CanonicalTool = "task"
	ToolMCP       CanonicalTool = "mcp"
	ToolOther     CanonicalTool = "other"
)

// ToolCall is the normalized view of a tool invocation.
type ToolCall struct {
	ID          string        // native id, or synthesized (see Synthesized)
	Synthesized bool          // true when the provider omitted an id (Cursor MCP cases)
	Name        string        // native tool name, verbatim
	Canonical   CanonicalTool // cross-provider classification
	MCP         *MCPCall      // non-nil when the call targets an MCP tool

	// Input is ALWAYS a JSON object (the library un-stringifies Cursor's
	// string form and wraps non-object inputs as {"value": ...}).
	Input json.RawMessage
	// RawInput is the input exactly as the provider sent it (nil when the
	// provider sent none and Input was constructed from other fields).
	RawInput json.RawMessage
}

// MCPCall carries the decoded MCP tool identity plus the server's transport.
// Cursor and Gemini MCP events ship url/command in the hook payload; elsewhere
// the runner resolves them from provider config (quirk #25) and flags the
// result FromConfig.
type MCPCall struct {
	Server  string // decoded from mcp__s__t / mcp_s_t / MCP:t + context; "" if undecodable
	Tool    string // tool name as the MCP server knows it
	URL     string // remote transport: payload-borne or config-resolved
	Command string // stdio transport: command plus args, space-joined
	// FromConfig marks URL/Command as resolved from the provider's config
	// files rather than carried by the event payload. Config-resolved
	// transport is best-effort: treat it as advisory, not authoritative.
	FromConfig bool
}

var canonicalNames = map[string]CanonicalTool{
	"bash": ToolShell, "shell": ToolShell, "run_shell_command": ToolShell,
	"run_terminal_cmd": ToolShell, "exec": ToolShell, "local_shell": ToolShell,
	"terminal": ToolShell,
	// VS Code Copilot Chat, enumerated from the extension's wire-name table
	// (not its package.json, which registers different names). Everything here
	// executes an arbitrary command or arbitrary code, so a deny-shell policy
	// that missed any of them would have a hole.
	//
	// Deliberately NOT shell, having no execution of their own: the read-only
	// get_terminal_output / get_task_output / terminal_selection /
	// terminal_last_command, the kill_terminal control, and run_vscode_command
	// — which can reach a terminal command indirectly, but classing the whole
	// command palette as shell would make an allow-shell policy far too broad.
	"run_in_terminal": ToolShell, "send_to_terminal": ToolShell,
	"run_task": ToolShell, "create_and_run_task": ToolShell,
	"runtests": ToolShell, "run_notebook_cell": ToolShell,
	"run_playwright_code": ToolShell,

	"read": ToolFileRead, "read_file": ToolFileRead, "readfile": ToolFileRead,
	"view_file": ToolFileRead, "read_many_files": ToolFileRead, "notebookread": ToolFileRead,
	// The Copilot CLI names its file tools with bare verbs no other provider
	// uses: `view` reads a path and `create` writes one ({path, file_text}).
	// Neither matched anything above, so a deny-file-read or deny-file-write
	// policy silently covered nothing on that CLI.
	"view": ToolFileRead,

	"write": ToolFileWrite, "write_file": ToolFileWrite, "writefile": ToolFileWrite,
	"create_file": ToolFileWrite, "save_file": ToolFileWrite,
	"create": ToolFileWrite,

	"edit": ToolFileEdit, "multiedit": ToolFileEdit, "apply_patch": ToolFileEdit,
	"replace": ToolFileEdit, "edit_file": ToolFileEdit, "notebookedit": ToolFileEdit,
	"str_replace": ToolFileEdit, "search_replace": ToolFileEdit, "patch": ToolFileEdit,
	"strreplacefile":         ToolFileEdit,
	"replace_string_in_file": ToolFileEdit, "multi_replace_string_in_file": ToolFileEdit,
	"insert_edit_into_file": ToolFileEdit, "edit_notebook_file": ToolFileEdit,
	"edit_files": ToolFileEdit,

	"grep": ToolSearch, "glob": ToolSearch, "search": ToolSearch,
	"codebase_search": ToolSearch, "search_file_content": ToolSearch,
	"find_files": ToolSearch, "list_directory": ToolSearch, "ls": ToolSearch,
	"glob_file_search": ToolSearch, "grep_search": ToolSearch, "file_search": ToolSearch,
	"list_dir": ToolSearch, "semantic_search": ToolSearch,
	"search_workspace_symbols": ToolSearch, "test_search": ToolSearch,
	"github_repo": ToolSearch, "github_text_search": ToolSearch,
	"read_project_structure": ToolSearch,

	"webfetch": ToolFetch, "web_fetch": ToolFetch, "websearch": ToolFetch,
	"web_search": ToolFetch, "fetch": ToolFetch, "google_web_search": ToolFetch,
	"fetch_webpage": ToolFetch,
	// VS Code's browser tools, split by what they actually do: these four pull
	// external content into the session, which is what a deny-fetch policy is
	// for. The rest of the suite — click_element, type_in_page, hover_element,
	// drag_element, handle_dialog — only manipulates an already-open page and
	// stays ToolOther. (run_playwright_code executes code, so it is shell.)
	"open_browser_page": ToolFetch, "navigate_page": ToolFetch,
	"read_page": ToolFetch, "screenshot_page": ToolFetch,

	"task": ToolTask, "agent": ToolTask, "subagent": ToolTask,
	// VS Code spells its four subagent tools distinctly; none matched the
	// generic names above, so a ToolTask matcher saw no VS Code subagent.
	"runsubagent": ToolTask, "search_subagent": ToolTask,
	"explore_subagent": ToolTask, "execution_subagent": ToolTask,
}

// CanonicalToolFor classifies a native tool name. MCP names (any dialect)
// classify as ToolMCP.
func CanonicalToolFor(name string) CanonicalTool {
	if ParseMCPName(name) != nil {
		return ToolMCP
	}
	if c, ok := canonicalNames[strings.ToLower(name)]; ok {
		return c
	}
	return ToolOther
}

// ParseMCPName decodes the three MCP tool-name dialects:
// mcp__server__tool (Claude/Codex), mcp_server_tool (Gemini/VS Code,
// best-effort since "_" is ambiguous), and MCP:tool (Cursor, server unknown).
// It returns nil when the name is not MCP-shaped.
func ParseMCPName(name string) *MCPCall {
	switch {
	case strings.HasPrefix(name, "mcp__"):
		rest := strings.TrimPrefix(name, "mcp__")
		server, tool, ok := strings.Cut(rest, "__")
		if !ok {
			return &MCPCall{Tool: rest}
		}
		return &MCPCall{Server: server, Tool: tool}
	case strings.HasPrefix(name, "MCP:"):
		return &MCPCall{Tool: strings.TrimPrefix(name, "MCP:")}
	case strings.HasPrefix(name, "mcp_"):
		rest := strings.TrimPrefix(name, "mcp_")
		server, tool, ok := strings.Cut(rest, "_")
		if !ok {
			return &MCPCall{Tool: rest}
		}
		return &MCPCall{Server: server, Tool: tool}
	}
	return nil
}

// SynthesizeToolID derives a stable id for providers that omit one:
// hash(session|turn|tool|input) -> hook_synth_<16 hex>.
func SynthesizeToolID(sessionID, turnID, toolName string, input []byte) string {
	h := sha256.New()
	h.Write([]byte(sessionID))
	h.Write([]byte{'|'})
	h.Write([]byte(turnID))
	h.Write([]byte{'|'})
	h.Write([]byte(toolName))
	h.Write([]byte{'|'})
	h.Write(input)
	return "hook_synth_" + hex.EncodeToString(h.Sum(nil))[:16]
}

// normalizeInput guarantees ToolCall.Input is a JSON object: it un-stringifies
// Cursor's JSON-string form (quirk #5), maps empty/null to {}, and wraps any
// other non-object value as {"value": ...} so nothing is hidden.
func normalizeInput(in json.RawMessage) json.RawMessage {
	trim := bytes.TrimSpace(in)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return json.RawMessage("{}")
	}
	if trim[0] == '"' {
		var s string
		if json.Unmarshal(trim, &s) == nil {
			inner := bytes.TrimSpace([]byte(s))
			if len(inner) > 0 && inner[0] == '{' && json.Valid(inner) {
				return json.RawMessage(inner)
			}
		}
	}
	if trim[0] == '{' && json.Valid(trim) {
		return trim
	}
	wrapped, err := json.Marshal(map[string]json.RawMessage{"value": trim})
	if err != nil {
		return json.RawMessage("{}")
	}
	return wrapped
}

// makeToolCall builds the normalized ToolCall, synthesizing an id when the
// provider omitted one.
func makeToolCall(s SessionInfo, name, id string, input, rawInput json.RawMessage) ToolCall {
	tc := ToolCall{Name: name, RawInput: rawInput}
	tc.Input = normalizeInput(input)
	tc.MCP = ParseMCPName(name)
	if tc.MCP != nil {
		tc.Canonical = ToolMCP
	} else {
		tc.Canonical = CanonicalToolFor(name)
	}
	if id != "" {
		tc.ID = id
	} else {
		tc.ID = SynthesizeToolID(s.ID, s.TurnID, name, tc.Input)
		tc.Synthesized = true
	}
	return tc
}

// Typed events embed Event and add normalized fields.

type SessionStartEvent struct {
	Event
	Source string // "startup", "resume", ... (provider-specific vocabulary)
}

type SessionEndEvent struct {
	Event
	Reason string
}

// MCPServer is one server in the provider's effective MCP configuration.
type MCPServer struct {
	Name    string
	URL     string
	Command string
}

// MCPInventoryEvent reports the known MCP server snapshot independently from
// any provider-native session event. Complete is false when provider discovery
// failed or is still in progress and Servers contains only known
// explicit/config-file entries.
type MCPInventoryEvent struct {
	Event
	Servers  []MCPServer
	Complete bool
}

type PromptEvent struct {
	Event
	Prompt string
}

type ToolPreEvent struct {
	Event
	Tool ToolCall
}

type PermissionEvent struct {
	Event
	Tool ToolCall
}

type ToolPostEvent struct {
	Event
	Tool   ToolCall
	Output json.RawMessage // model-visible tool output; Event.Raw retains the provider payload verbatim
	Failed bool            // true on tool.error and error-carrying tool.post
	Error  string
	// DurationMS is the tool execution time in milliseconds when the provider
	// reports one (Cursor duration/duration_ms), nil otherwise.
	DurationMS *float64
}

// Usage carries the token and cost totals a provider reports at the end of a
// turn. Pointer fields distinguish "reported as zero" from "not reported".
type Usage struct {
	InputTokens      *int
	OutputTokens     *int
	CacheReadTokens  *int
	CacheWriteTokens *int
	Cost             *float64
	LoopCount        *int
	Status           string
}

type StopEvent struct {
	Event
	// PreviouslyContinued surfaces stop_hook_active / loop_count uniformly.
	PreviouslyContinued bool
	// LoopCount is the provider-reported continuation count when available,
	// or 1 when only a boolean guard (stop_hook_active) exists.
	LoopCount int
	// FinalMessage is the last assistant message of the turn when the provider
	// includes it on the stop event (Claude/Codex last_assistant_message).
	FinalMessage string
	// Usage carries end-of-turn token/cost totals when the provider reports
	// them on stop (Cursor), nil otherwise.
	Usage *Usage
}

type SubagentStartEvent struct {
	Event
}

type CompactEvent struct {
	Event
	Trigger      string
	Instructions string
}

type NotificationEvent struct {
	Event
	Message string
}

type FileEditedEvent struct {
	Event
	Path string
}

// ModelEvent covers the experimental model.request/model.response kinds
// (Gemini BeforeModel/AfterModel, OpenCode chat.params). Observe-only in v1.
type ModelEvent struct {
	Event
}

// EventOf returns the embedded envelope of any typed event returned by the
// decoder, or nil if the value is not an agenthooks event. Consumers that
// dispatch on the concrete types (rather than registering handlers) use it to
// reach the shared envelope fields.
func EventOf(typed any) *Event { return eventOf(typed) }

// eventOf returns the embedded envelope of any typed event.
func eventOf(typed any) *Event {
	switch ev := typed.(type) {
	case *Event:
		return ev
	case *SessionStartEvent:
		return &ev.Event
	case *SessionEndEvent:
		return &ev.Event
	case *MCPInventoryEvent:
		return &ev.Event
	case *PromptEvent:
		return &ev.Event
	case *ToolPreEvent:
		return &ev.Event
	case *PermissionEvent:
		return &ev.Event
	case *ToolPostEvent:
		return &ev.Event
	case *StopEvent:
		return &ev.Event
	case *SubagentStartEvent:
		return &ev.Event
	case *CompactEvent:
		return &ev.Event
	case *NotificationEvent:
		return &ev.Event
	case *FileEditedEvent:
		return &ev.Event
	case *ModelEvent:
		return &ev.Event
	}
	return nil
}

// toolOf returns the ToolCall carried by tool-ish events, or nil.
func toolOf(typed any) *ToolCall {
	switch ev := typed.(type) {
	case *ToolPreEvent:
		return &ev.Tool
	case *PermissionEvent:
		return &ev.Tool
	case *ToolPostEvent:
		return &ev.Tool
	}
	return nil
}
