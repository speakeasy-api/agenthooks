// Package hookrecord carries the runner's end-of-processing view of one
// hook event across the agenthooks ↔ telemetry package boundary. It is
// internal on purpose: the telemetry recorder's tap methods take these
// types, which keeps them callable by the runner but not by external
// consumers, without growing the public API of either package.
package hookrecord

import (
	"encoding/json"
	"time"
)

// Record is the observational snapshot of a hook event the runner hands to
// the telemetry recorder: identity, the normalized payload fields telemetry
// needs, and processing-health signals (dispatch duration, handler errors).
// Deliberately absent: the enforcement decision — telemetry logs the event,
// never the verdict; gram's decision-time enforcement log is the sole
// record of decisions.
type Record struct {
	Provider   string
	Variant    string
	NativeName string
	Kind       string    // unified kind, e.g. "tool.pre"
	Time       time.Time // library receive time
	Backfilled bool

	SessionID string
	TurnID    string
	CWD       string
	Model     string
	UserEmail string

	SubagentID   string
	SubagentType string

	Tool *Tool

	// Kind-specific payloads; zero-valued when the kind does not carry them.
	Prompt           string
	FinalMessage     string
	LoopCount        int
	Usage            *Usage
	Notification     string
	FilePath         string
	SessionSource    string
	SessionEndReason string
	CompactTrigger   string

	HandlerErr string // non-empty when the handler pipeline failed

	// HookDurationMS is dispatch-to-response-encoded time in milliseconds —
	// the hook's own overhead, distinct from tool execution time.
	HookDurationMS float64
}

// Tool mirrors the normalized ToolCall plus the post-execution fields of
// tool.post/tool.error events.
type Tool struct {
	ID          string
	Synthesized bool
	Name        string
	Canonical   string
	Input       json.RawMessage
	Output      json.RawMessage
	Failed      bool
	Error       string
	DurationMS  *float64
	MCP         *MCP
}

// MCP mirrors MCPCall: decoded identity plus transport.
type MCP struct {
	Server     string
	Tool       string
	URL        string
	Command    string
	FromConfig bool
}

// Usage mirrors the end-of-turn token/cost totals.
type Usage struct {
	InputTokens      *int
	OutputTokens     *int
	CacheReadTokens  *int
	CacheWriteTokens *int
	Cost             *float64
}
