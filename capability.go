package agenthooks

// Capability names one thing a decision can express. The matrix below encodes
// which providers honor it on which events, so degradation is explicit and
// typed instead of silent.
type Capability string

const (
	CapDeny          Capability = "deny"
	CapAsk           Capability = "ask"
	CapAllow         Capability = "allow"
	CapUpdateInput   Capability = "update-input"
	CapAddContext    Capability = "add-context"
	CapReplaceOutput Capability = "replace-output"
	CapContinueAgent Capability = "continue-agent"
	CapStopAgent     Capability = "stop-agent"
	CapSystemMessage Capability = "system-message"
)

// CapSet is the set of capabilities available on one provider+event pair.
type CapSet map[Capability]bool

func (s CapSet) Has(c Capability) bool { return s[c] }

func caps(cs ...Capability) CapSet {
	s := make(CapSet, len(cs))
	for _, c := range cs {
		s[c] = true
	}
	return s
}

// capMatrix is the normative capability table (DESIGN.md §4.1). Fire-and-forget
// events get an empty set: pretending to block on them is worse than honesty.
var capMatrix = map[Provider]map[EventKind]CapSet{
	ProviderClaudeCode: {
		KindToolPre:         caps(CapDeny, CapAsk, CapAllow, CapUpdateInput, CapAddContext, CapSystemMessage, CapStopAgent),
		KindPermission:      caps(CapDeny, CapAllow, CapUpdateInput, CapSystemMessage, CapStopAgent),
		KindPromptSubmitted: caps(CapDeny, CapAddContext, CapSystemMessage, CapStopAgent),
		KindToolPost:        caps(CapAddContext, CapReplaceOutput, CapSystemMessage, CapStopAgent),
		KindToolError:       caps(CapAddContext, CapSystemMessage, CapStopAgent),
		KindStop:            caps(CapContinueAgent, CapSystemMessage),
		KindSubagentStop:    caps(CapContinueAgent, CapSystemMessage),
		KindSessionStart:    caps(CapAddContext, CapSystemMessage),
		KindCompactPre:      caps(CapSystemMessage),
	},
	ProviderCodex: {
		KindToolPre:         caps(CapDeny, CapAllow, CapUpdateInput, CapAddContext, CapSystemMessage, CapStopAgent),
		KindPermission:      caps(CapDeny, CapAllow, CapSystemMessage, CapStopAgent),
		KindPromptSubmitted: caps(CapDeny, CapAddContext, CapSystemMessage, CapStopAgent),
		KindToolPost:        caps(CapAddContext, CapSystemMessage),
		KindStop:            caps(CapContinueAgent, CapSystemMessage),
		KindSubagentStop:    caps(CapContinueAgent, CapSystemMessage),
		KindSessionStart:    caps(CapAddContext),
	},
	ProviderCursor: {
		// Ask is enforced only on shell/MCP events, ignored on preToolUse,
		// and beforeReadFile is allow/deny only — the runner refines this
		// per native event on top of the kind-level matrix.
		KindToolPre:         caps(CapDeny, CapAsk, CapAllow, CapUpdateInput, CapAddContext, CapSystemMessage),
		KindPromptSubmitted: caps(CapDeny),
		KindToolPost:        caps(CapReplaceOutput), // updated_mcp_tool_output, MCP only
		KindStop:            caps(CapContinueAgent), // followup_message
		KindSubagentStop:    caps(CapContinueAgent),
	},
	ProviderGemini: {
		KindToolPre:         caps(CapDeny, CapAsk, CapAllow, CapUpdateInput, CapAddContext, CapStopAgent),
		KindPromptSubmitted: caps(CapDeny, CapAddContext, CapStopAgent),
		KindToolPost:        caps(CapAddContext, CapReplaceOutput),
		KindToolError:       caps(CapAddContext),
		KindStop:            caps(CapContinueAgent, CapStopAgent),
		KindSessionStart:    caps(CapAddContext),
	},
	ProviderOpenCode: {
		KindToolPre:         caps(CapDeny, CapUpdateInput),
		KindPromptSubmitted: caps(CapDeny, CapAddContext),
		KindToolPost:        caps(CapReplaceOutput),
		KindToolError:       caps(CapReplaceOutput),
		KindPermission:      caps(CapAllow, CapDeny), // via HTTP reply (§8)
		// session.idle cannot continue the agent: empty set on KindStop.
	},
	ProviderOpenClaw: {
		// before_tool_call returns {block, blockReason, requireApproval,
		// params}; requireApproval resolves headless via timeoutBehavior
		// (verified 2026.6.34), so Ask is honest. before_agent_run is the
		// blockable prompt gate ({outcome: "block"}); its result carries no
		// context channel. Everything else is observe-only — after_tool_call,
		// agent_end, session_*, llm_* returns are ignored by the Gateway.
		KindToolPre:         caps(CapDeny, CapAsk, CapUpdateInput),
		KindPromptSubmitted: caps(CapDeny),
	},
	ProviderMoltis: {
		// MessageReceived can block or rewrite the inbound content. Context is
		// appended to that content by the codec because Moltis has no distinct
		// additional-context response field. BeforeToolCall can block or replace
		// its arguments. Moltis has no hook-level forced-allow or ask decision;
		// all post/lifecycle events are observation-only.
		KindPromptSubmitted: caps(CapDeny, CapAddContext),
		KindToolPre:         caps(CapDeny, CapUpdateInput),
	},
	ProviderCopilotCLI: {
		// preToolUse and permissionRequest are the only decision-capable
		// events: deny was observed enforced end to end, and it fires even
		// under --allow-all/--yolo. prompt.submitted is deliberately an empty
		// set — Copilot DROPS command-hook output for userPromptSubmitted
		// (modifiedPrompt is SDK-programmatic only), so pretending to block
		// there would silently allow. Everything else is observation-only.
		KindToolPre:      caps(CapDeny, CapAsk, CapAllow, CapUpdateInput),
		KindPermission:   caps(CapDeny, CapAllow),
		KindStop:         caps(CapContinueAgent),
		KindSubagentStop: caps(CapContinueAgent),
		KindSessionStart: caps(CapAddContext),
	},
	ProviderVSCodeCopilot: {
		// Verified against the extension source, not the docs, which contradict
		// themselves on placement. PreToolUse is the only event whose decision
		// rides hookSpecificOutput; PostToolUse and UserPromptSubmit block via
		// TOP-LEVEL decision/reason; Stop/SubagentStop block via NESTED
		// decision/reason (encodeVSCode moves them). SessionStart/SubagentStart
		// are processed with ignoreErrors and drop stopReason silently, so no
		// CapStopAgent there. No CapReplaceOutput anywhere: updatedToolOutput is
		// a Claude extension VS Code does not read. No CapAsk outside ToolPre.
		// PostToolUse CapAddContext is contract-level: VS Code 1.135 accepts it
		// and appends it to the tool result, but its panel path starts that async
		// work without awaiting it (quirk #49). Keep the capability so the same
		// documented payload works once the upstream race is fixed.
		// permission.request, session.end, tool.error and notification are absent:
		// VS Code never fires them.
		//
		// SubagentStart and CompactPre are observe-only in the public runner,
		// so their upstream output channels are not capabilities here. Stop and
		// SubagentStop cannot advertise CapStopAgent because StopDecision has no
		// operation that sets it.
		KindToolPre:         caps(CapDeny, CapAsk, CapAllow, CapUpdateInput, CapAddContext, CapSystemMessage, CapStopAgent),
		KindToolPost:        caps(CapAddContext, CapSystemMessage, CapStopAgent),
		KindPromptSubmitted: caps(CapDeny, CapAddContext, CapSystemMessage, CapStopAgent),
		KindSessionStart:    caps(CapAddContext, CapSystemMessage),
		KindStop:            caps(CapContinueAgent, CapSystemMessage),
		KindSubagentStop:    caps(CapContinueAgent, CapSystemMessage),
	},
	ProviderKimi: {
		// Only UserPromptSubmit, PreToolUse and Stop are blockable; JSON
		// output understands deny|allow only — no ask, no updatedInput, no
		// additionalContext (quirk #22). Prompt context rides plain exit-0
		// stdout; prompt/stop blocking is exit 2 + stderr (quirk #23).
		// PermissionRequest, PostToolUse and the rest are observation-only:
		// empty sets, so pretend-blocking degrades honestly.
		KindToolPre:         caps(CapDeny, CapAllow),
		KindPromptSubmitted: caps(CapDeny, CapAddContext),
		KindStop:            caps(CapContinueAgent),
	},
}

// Capabilities reports what a decision can express for the given
// provider/variant/event combination.
func Capabilities(p Provider, v Variant, k EventKind) CapSet {
	byKind, ok := capMatrix[p]
	if !ok {
		return CapSet{}
	}
	s, ok := byKind[k]
	if !ok {
		return CapSet{}
	}
	return s
}

// Can reports whether the event's provider honors the capability. Backfilled
// events are reporting-only: the provider never sent them, so nothing can be
// gated or mutated.
func (e *Event) Can(c Capability) bool {
	if e.Backfilled {
		return false
	}
	return Capabilities(e.Provider, e.Variant, e.Kind).Has(c)
}

// cursorAskSupported refines CapAsk per native Cursor event: enforced only on
// shell/MCP confirmations; ignored on preToolUse; beforeReadFile is
// allow/deny only; treated as deny on subagentStart.
func cursorAskSupported(nativeName string) bool {
	switch nativeName {
	case "beforeShellExecution", "beforeMCPExecution":
		return true
	}
	return false
}
