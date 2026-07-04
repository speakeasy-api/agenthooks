package agenthooks

// Decisions carry *intent*; the provider codec translates intent into that
// provider's mechanism (JSON dialect, exit code, thrown error). Consumers
// never see the wire.

type decisionKind int

const (
	decNoDecision decisionKind = iota // defer to the provider's normal flow (NEVER a forced allow)
	decAllow
	decDeny
	decAsk
	decAcceptPrompt
	decBlockPrompt
	decFinish
	decContinue
	decObserved
	decFlagOutput
	decReplaceOutput
	decContinueSession
)

type decisionCore struct {
	kind              decisionKind
	reason            string
	instruction       string // ContinueWith payload
	context           []string
	systemMessage     string
	stopAgent         bool
	stopReason        string
	updatedInput      any
	hasUpdatedInput   bool
	replacedOutput    any
	hasReplacedOutput bool
}

func (c decisionCore) withContext(s string) decisionCore {
	c.context = append(append([]string(nil), c.context...), s)
	return c
}

// ToolPreDecision gates tool.pre and permission.request events.
type ToolPreDecision struct{ core decisionCore }

// NoDecision defers to the provider's normal permission flow. It is NEVER a
// forced allow: the codecs emit each provider's correct "no opinion" form.
func NoDecision() ToolPreDecision { return ToolPreDecision{decisionCore{kind: decNoDecision}} }

// Allow skips the permission prompt where supported. It never loosens policy:
// provider-side deny rules still apply.
func Allow() ToolPreDecision { return ToolPreDecision{decisionCore{kind: decAllow}} }

// Deny blocks the tool call with feedback for the model.
func Deny(reason string) ToolPreDecision {
	return ToolPreDecision{decisionCore{kind: decDeny, reason: reason}}
}

// AskUser forces a confirmation prompt where the provider supports one.
// Behavior on providers without ask is governed by Policy.AskFallback.
func AskUser(reason string) ToolPreDecision {
	return ToolPreDecision{decisionCore{kind: decAsk, reason: reason}}
}

// WithUpdatedInput rewrites the tool arguments before execution.
func (d ToolPreDecision) WithUpdatedInput(v any) ToolPreDecision {
	d.core.updatedInput = v
	d.core.hasUpdatedInput = true
	return d
}

// WithContext injects context for the model where supported.
func (d ToolPreDecision) WithContext(s string) ToolPreDecision {
	d.core = d.core.withContext(s)
	return d
}

// WithSystemMessage attaches a user-facing note where supported.
func (d ToolPreDecision) WithSystemMessage(s string) ToolPreDecision {
	d.core.systemMessage = s
	return d
}

// StopAgent requests continue:false where supported.
func (d ToolPreDecision) StopAgent(reason string) ToolPreDecision {
	d.core.stopAgent = true
	d.core.stopReason = reason
	return d
}

// PromptDecision gates prompt.submitted events.
type PromptDecision struct{ core decisionCore }

func AcceptPrompt() PromptDecision { return PromptDecision{decisionCore{kind: decAcceptPrompt}} }

func BlockPrompt(reason string) PromptDecision {
	return PromptDecision{decisionCore{kind: decBlockPrompt, reason: reason}}
}

func (d PromptDecision) WithContext(s string) PromptDecision {
	d.core = d.core.withContext(s)
	return d
}

func (d PromptDecision) WithSystemMessage(s string) PromptDecision {
	d.core.systemMessage = s
	return d
}

func (d PromptDecision) StopAgent(reason string) PromptDecision {
	d.core.stopAgent = true
	d.core.stopReason = reason
	return d
}

// StopDecision responds to agent.stop / subagent.stop.
type StopDecision struct{ core decisionCore }

// Finish lets the agent stop.
func Finish() StopDecision { return StopDecision{decisionCore{kind: decFinish}} }

// ContinueWith keeps the agent working: claude decision:block+reason, cursor
// followup_message, codex continuation prompt, gemini retry. The runner
// enforces Policy.ContinuationCap so consumers can't build infinite loops on
// providers without native caps.
func ContinueWith(instruction string) StopDecision {
	return StopDecision{decisionCore{kind: decContinue, instruction: instruction}}
}

func (d StopDecision) WithSystemMessage(s string) StopDecision {
	d.core.systemMessage = s
	return d
}

// ToolPostDecision responds to tool.post / tool.error.
type ToolPostDecision struct{ core decisionCore }

// Observed acknowledges the event with no opinion.
func Observed() ToolPostDecision { return ToolPostDecision{decisionCore{kind: decObserved}} }

// FlagOutput sends feedback about the tool output to the model.
func FlagOutput(reason string) ToolPostDecision {
	return ToolPostDecision{decisionCore{kind: decFlagOutput, reason: reason}}
}

// ReplaceOutput substitutes the tool output where supported: claude
// updatedToolOutput, cursor updated_mcp_tool_output (MCP only), gemini
// tool_output, opencode output mutation.
func ReplaceOutput(v any) ToolPostDecision {
	return ToolPostDecision{decisionCore{
		kind:              decReplaceOutput,
		replacedOutput:    v,
		hasReplacedOutput: true,
	}}
}

func (d ToolPostDecision) WithContext(s string) ToolPostDecision {
	d.core = d.core.withContext(s)
	return d
}

func (d ToolPostDecision) WithSystemMessage(s string) ToolPostDecision {
	d.core.systemMessage = s
	return d
}

func (d ToolPostDecision) StopAgent(reason string) ToolPostDecision {
	d.core.stopAgent = true
	d.core.stopReason = reason
	return d
}

// SessionStartDecision responds to session.start.
type SessionStartDecision struct{ core decisionCore }

func ContinueSession() SessionStartDecision {
	return SessionStartDecision{decisionCore{kind: decContinueSession}}
}

func (d SessionStartDecision) WithContext(s string) SessionStartDecision {
	d.core = d.core.withContext(s)
	return d
}

func (d SessionStartDecision) WithSystemMessage(s string) SessionStartDecision {
	d.core.systemMessage = s
	return d
}
