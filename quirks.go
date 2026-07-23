package agenthooks

// Quirk is one entry in the machine-readable registry of provider glue this
// library hides. The registry doubles as the conformance-test plan: every
// quirk gets a fixture. Docs markers (⚠) are generated from this table.
type Quirk struct {
	ID         int
	Provider   Provider
	Versions   string // affected version range, free-form ("all", ">=2.0.64", ...)
	Event      EventKind
	Capability Capability // "" when not capability-scoped
	Behavior   string
	Mitigation string
	Reference  string // upstream issue/doc pointer
}

// Quirks returns the full registry, ordered by ID.
func Quirks() []Quirk { return quirkRegistry }

// QuirksFor filters the registry by provider.
func QuirksFor(p Provider) []Quirk {
	var out []Quirk
	for _, q := range quirkRegistry {
		if q.Provider == p {
			out = append(out, q)
		}
	}
	return out
}

var quirkRegistry = []Quirk{
	{ID: 1, Provider: ProviderClaudeCode, Versions: "cowork", Event: KindStop,
		Behavior:   "cowork skips async Stop hooks",
		Mitigation: "config generation forces async:false on Stop",
		Reference:  "observed in production"},
	{ID: 2, Provider: ProviderCursor, Versions: "all", Event: KindToolPre,
		Behavior:   "fires preToolUse AND beforeShellExecution/beforeMCPExecution for the same call",
		Mitigation: "runner dedupes into one tool.pre via a best-effort on-disk marker; both raws remain reachable",
		Reference:  "cursor hooks docs"},
	{ID: 3, Provider: ProviderCursor, Versions: "all", Event: KindToolPre,
		Behavior:   "MCP: tool-name prefix; missing tool_use_id on MCP events",
		Mitigation: "prefix stripped into MCPCall; stable IDs synthesized (hook_synth_<16 hex>)",
		Reference:  "cursor hooks docs"},
	{ID: 4, Provider: ProviderCursor, Versions: ">=2.0.64", Event: KindToolPre,
		Behavior:   "requires snake_case output (user_message); 1.7 used camelCase",
		Mitigation: "codec emits snake_case plus a harmless legacy continue field",
		Reference:  "cursor changelog 2.0.64"},
	{ID: 5, Provider: ProviderCursor, Versions: "all", Event: KindToolPre,
		Behavior:   "tool_input is an object on preToolUse but a JSON-encoded string on MCP events",
		Mitigation: "normalized to an object in ToolCall.Input; RawInput keeps the original",
		Reference:  "cursor hooks docs"},
	{ID: 6, Provider: ProviderCursor, Versions: "CLI <2026-05-20", Event: KindOther,
		Behavior:   "legacy CLI passes payload via argv; Windows CLI double-fires; cloud/CLI fire event subsets",
		Mitigation: "--argv-payload mode; synthesized-id idempotency surface; capability matrix per Variant",
		Reference:  "cursor CLI changelog"},
	{ID: 7, Provider: ProviderCursor, Versions: "all", Event: KindToolPre, Capability: CapDeny,
		Behavior:   "fail-open default: a crashed hook allows the action",
		Mitigation: "generated config sets failClosed:true on decision events when Policy is FailClosed",
		Reference:  "cursor hooks docs"},
	{ID: 8, Provider: ProviderCodex, Versions: "all", Event: KindToolPre, Capability: CapAsk,
		Behavior:   "empty stdout means allow; unknown JSON rejected; ask/approve fail the hook run",
		Mitigation: "codec emits the exact dialect; Ask degrades per Policy.AskFallback",
		Reference:  "codex hooks schema"},
	{ID: 9, Provider: ProviderCodex, Versions: "all", Event: KindSessionStart,
		Behavior:   "hooks require user trust of the definition hash",
		Mitigation: "install pre-seeds [hooks.state] trusted hashes (definition-hash reimplementation)",
		Reference:  "codex hooks trust model"},
	{ID: 10, Provider: ProviderCodex, Versions: "all", Event: KindToolPost,
		Behavior:   "no async hooks: async is parsed but skipped",
		Mitigation: "config generation appends --async; the runner re-execs itself as a detached worker and returns immediately (no shell)",
		Reference:  "codex source"},
	{ID: 11, Provider: ProviderGemini, Versions: "all", Event: KindToolPre,
		Behavior:   "exit codes diverge from docs (any non-zero except 1 blocks); stderr is parsed as the decision when stdout is empty",
		Mitigation: "runner always writes explicit JSON to stdout and never exits bare non-zero",
		Reference:  "gemini-cli source"},
	{ID: 12, Provider: ProviderGemini, Versions: "all", Event: KindToolPre, Capability: CapUpdateInput,
		Behavior:   "hookSpecificOutput.tool_input is shallow-merged: key removal is impossible",
		Mitigation: "ErrLossyUpdate surfaces when a rewrite deletes keys; docs carry a loss marker",
		Reference:  "gemini-cli source"},
	{ID: 13, Provider: ProviderGemini, Versions: "all", Event: KindPromptSubmitted, Capability: CapAddContext,
		Behavior:   "additionalContext HTML-escapes < and >",
		Mitigation: "documented loss; optional pre-encoding by the consumer",
		Reference:  "gemini-cli source"},
	{ID: 14, Provider: ProviderGemini, Versions: "all", Event: KindOther,
		Behavior:   "timeouts are milliseconds where everyone else uses seconds",
		Mitigation: "time.Duration everywhere in the API; codecs convert per dialect",
		Reference:  "gemini-cli settings schema"},
	{ID: 15, Provider: "", Versions: "all", Event: KindToolPre,
		Behavior:   "MCP naming: mcp__s__t (Claude/Codex) vs mcp_s_t (Gemini) vs MCP: (Cursor) vs in-context (OpenCode)",
		Mitigation: "unified MCPCall decode plus the matcher compiler",
		Reference:  "provider docs"},
	{ID: 16, Provider: "", Versions: "all", Event: KindToolPre,
		Behavior:   "hook blind spots: Claude @file reads / Bash file writes / direct /skill bypass tool hooks; Cursor AskQuestion fires no hooks; Codex only intercepts Bash/apply_patch/MCP",
		Mitigation: "documented blind-spot matrix per provider; Can() reflects it",
		Reference:  "provider docs"},
	{ID: 17, Provider: ProviderClaudeCode, Versions: "all", Event: KindToolPre, Capability: CapAllow,
		Behavior:   "empty stdout is NOT allow; NoDecision must not force-allow",
		Mitigation: "explicit NoDecision encoding everywhere ({} on Claude/Cursor/Gemini, empty stdout on Codex)",
		Reference:  "observed in production"},
	{ID: 18, Provider: ProviderOpenCode, Versions: ">=1.5.0", Event: KindPermission,
		Behavior:   "permission.ask plugin hook is typed but dead",
		Mitigation: "permission flow via HTTP reply (POST /session/:id/permissions/:permissionID) plus tool.execute.before throw",
		Reference:  "opencode issue tracker"},
	{ID: 19, Provider: "", Versions: "all", Event: KindStop, Capability: CapContinueAgent,
		Behavior:   "stop-loop guards differ: stop_hook_active (cap 8), loop_count (limit 5), loop_limit null in Cursor Claude-compat",
		Mitigation: "unified PreviouslyContinued/LoopCount plus a library-side continuation cap",
		Reference:  "provider docs"},
	{ID: 20, Provider: "", Versions: "all", Event: KindOther,
		Behavior:   "providers cross-set env: CLAUDE_PLUGIN_ROOT on Codex, CLAUDE_PROJECT_DIR on Cursor/Gemini",
		Mitigation: "flag-first provider detection; env sniffing is fallback only",
		Reference:  "codex/cursor compat layers"},
	{ID: 21, Provider: ProviderKimi, Versions: "all", Event: KindToolPre, Capability: CapDeny,
		Behavior:   "hard fail-open runtime: crash, timeout, and any exit other than 2 all allow; no failClosed switch exists",
		Mitigation: "FailClosed is enforced in-process (handler failure encodes a deny / exit 2); only a crash of the hook binary itself falls open",
		Reference:  "kimi code hooks docs"},
	{ID: 22, Provider: ProviderKimi, Versions: "all", Event: KindToolPre, Capability: CapAsk,
		Behavior:   "JSON output understands permissionDecision deny|allow only: no ask, no updatedInput, no additionalContext",
		Mitigation: "Ask degrades per Policy.AskFallback; input rewrites drop or error per Policy.Unsupported",
		Reference:  "kimi code hooks docs"},
	{ID: 23, Provider: ProviderKimi, Versions: "all", Event: KindPromptSubmitted, Capability: CapDeny,
		Behavior:   "prompt/stop blocking is exit 2 with the reason on stderr; exit-0 stdout is appended to the model context",
		Mitigation: "codec writes reasons to stderr with exit 2 and keeps stdout empty except intentional prompt context; no-op is empty stdout",
		Reference:  "kimi code hooks docs"},
	{ID: 24, Provider: ProviderKimi, Versions: "all", Event: KindStop,
		Behavior:   "identical hook commands are deduplicated per event; Stop re-fires at most once (stop_hook_active)",
		Mitigation: "generated config keeps per-hook argv distinct; PreviouslyContinued/LoopCount surface the native guard",
		Reference:  "kimi code hooks docs"},
	{ID: 25, Provider: "", Versions: "all", Event: KindToolPre,
		Behavior:   "only Cursor MCP events carry server transport (url/command); every other provider ships the tool name alone",
		Mitigation: "runner resolves MCPCall.URL/Command from provider MCP configs (.mcp.json, ~/.claude.json, ~/.codex/config.toml, .cursor/mcp.json, .gemini/settings.json + extension manifests, .kimi-code/mcp.json, opencode.json(c)); ambiguous matches stay empty and FromConfig marks resolved transport",
		Reference:  "provider MCP config formats"},
	{ID: 26, Provider: ProviderClaudeCode, Versions: "all", Event: KindToolPre,
		Behavior:   "plugin- and claude.ai-connector MCP servers appear in no on-disk config; only `claude mcp list` knows their transport, and it health-checks every server (seconds of wall time)",
		Mitigation: "on the first MCP call the config fast path can't attribute, the runner matches against a shared on-disk `claude mcp list` inventory cache (merged additively by server name across sessions/processes; failed/empty runs negative-cached, stale misses re-probe throttled); RefreshClaudeMCPList/WarmClaudeMCPList warm it out of band so the probe leaves the interactive path",
		Reference:  "observed claude CLI behavior"},
	{ID: 27, Provider: ProviderGemini, Versions: "all", Event: KindToolPre,
		Behavior:   "extension-bundled MCP servers live in per-extension gemini-extension.json manifests, not settings.json",
		Mitigation: "resolver also reads <workspace>/.gemini/extensions/*/gemini-extension.json and ~/.gemini/extensions/*/gemini-extension.json",
		Reference:  "gemini-cli extensions docs"},
	{ID: 28, Provider: ProviderOpenCode, Versions: "all", Event: KindToolPre,
		Behavior:   "MCP tools are registered as <server>_<tool> with the configured name verbatim and no reserved prefix, so MCP calls are indistinguishable from native tools by name alone",
		Mitigation: "resolver matches configured server names from opencode.json(c) against tool names — a match detects the MCP call (sets MCPCall, Canonical=mcp) and attaches transport in one step",
		Reference:  "opencode mcp docs; verified against opencode 1.17.8"},
	{ID: 29, Provider: ProviderCursor, Versions: "verified 2026.07.01", Event: KindOther,
		Behavior:   "a scheme:// URL anywhere in any hook command makes Cursor silently drop the ENTIRE hooks.json — no hook in the file fires and nothing is logged; a bare host:port value is accepted",
		Mitigation: "renderCursor rejects commands containing ://; consumers encode endpoints scheme-less (host:port) or move them into a config file the hook binary reads",
		Reference:  "bisected against cursor agent CLI 2026.07.01"},
	{ID: 30, Provider: ProviderKimi, Versions: "observed 0.22.2", Event: KindPromptSubmitted,
		Behavior:   "non-interactive print mode (-p) does not fire UserPromptSubmit; SessionStart/PreToolUse/PostToolUse/Stop fire normally",
		Mitigation: "runner backfills a reporting-only prompt.submitted (Backfilled=true, Raw nil, no capabilities, decisions discarded) before the next event that implies one, once per recovered prompt per session (resumes covered)",
		Reference:  "verified against kimi-code 0.22.2"},
	{ID: 31, Provider: ProviderCursor, Versions: "observed 2026.07.01", Event: KindPromptSubmitted,
		Behavior:   "non-interactive print mode (-p) does not fire beforeSubmitPrompt; sessionStart and tool events fire normally",
		Mitigation: "runner backfills a reporting-only prompt.submitted (Backfilled=true, Raw nil, no capabilities, decisions discarded) before the next event that implies one, once per recovered prompt per session (resumes covered); prompt text recovered from the transcript the payload names, argv fallback",
		Reference:  "verified against cursor agent CLI 2026.07.01"},
}
