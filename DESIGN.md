# agenthooks — Design

A Go library for authoring coding-agent hooks once and running them everywhere:
Claude Code, Cursor (IDE + CLI + cloud), OpenAI Codex, Gemini CLI, OpenCode,
and Kimi Code.

The core promise: **one clear interface, zero data-fidelity loss**. The library
owns the per-provider glue, hacks, and workarounds so consumers don't have to.

Research verified against provider docs, source, and changelogs as of
**2026-07-02** (Claude Code 2.1.x, Codex hooks post-v0.124, Cursor 2.4 / CLI
May-2026+, Gemini CLI v0.26+ GA, OpenCode 1.17.x).

---

## 1. Landscape findings (what we're unifying)

The single most important finding: **Claude Code's hook contract is the
de-facto industry standard.**

| Provider | Relationship to Claude contract |
|---|---|
| Claude Code | The reference. 30 events, stdin snake_case JSON in, camelCase JSON out, exit 0/2/other, `hookSpecificOutput`, matchers, settings.json / plugin `hooks/hooks.json`. |
| Codex | **Deliberate Claude dialect.** Same event names, same `hookSpecificOutput`/`permissionDecision` shapes, even exports `CLAUDE_PLUGIN_ROOT`/`CLAUDE_PLUGIN_DATA` for compat. 10 events. Machine-readable JSON schemas published in-repo. |
| Gemini CLI | Claude-inspired with renames (`PreToolUse`→`BeforeTool`, `Stop`→`AfterAgent`), same base input fields plus `timestamp`, top-level `decision` instead of `hookSpecificOutput.permissionDecision`. Adds model-level hooks Claude lacks. |
| Cursor | Own dialect (camelCase event names, per-event output schemas, `permission`/`user_message`/`agent_message`, `followup_message`) **plus** a Claude-compat layer that reads `.claude/settings.json` and accepts Claude response shapes. |
| OpenCode | The outlier: in-process JS/TS plugins mutating shared objects, no spawned-process protocol at all. Requires a shim (§8). |

(Kimi Code, added after the initial research, ships another Claude-shaped
dialect with renamed keys and a narrower response surface — see quirk registry
entries #21–24.)

Consequence: the unified contract should be **Claude-shaped semantics with
typed extensions**, not a lowest-common-denominator invention. Three of five
providers natively converge on it; Cursor half-converges; only OpenCode needs a
real bridge.

Secondary findings that shape the design (full quirk registry in §9):

- Every provider disagrees on *something* mechanical: timeout units (s vs ms),
  MCP tool naming (`mcp__srv__tool` vs `mcp_srv_tool` vs `MCP:` prefix),
  `tool_input` type (object vs JSON-stringified string), empty-stdout meaning
  (Claude: no decision; Codex: allow; Gemini: stderr gets parsed instead),
  fail-open vs fail-closed defaults, exit-code semantics (Gemini blocks on any
  non-zero except 1, despite its docs).
- Providers ship bugs and drift: Cursor broke camelCase output fields in
  2.0.64, fires hooks differently across IDE/CLI/cloud, double-fires MCP tools;
  Claude cowork skips async `Stop` hooks; Gemini's `SessionStart` has an open
  not-firing regression. The library must be a place where these get encoded
  once, tested, and versioned.

---

## 2. Design principles

1. **Fidelity first.** The raw provider payload is always retained verbatim and
   reachable. Normalization is a *projection over* the raw data, never a
   replacement for it. Unknown fields are never dropped.
2. **Claude-shaped canon.** Unified names and semantics follow Claude Code
   where a mapping exists; provider deltas are explicit, typed extensions.
3. **Open taxonomy.** The unified event set is not closed. Any native event
   with no mapping still reaches the consumer (as `Kind == KindOther` with the
   native name and raw payload). New provider events degrade gracefully, they
   don't error.
4. **Explicit capability degradation.** When a consumer asks for something a
   provider can't do (e.g. `Ask` on Codex, deny on a fire-and-forget Cursor
   event), the library never silently discards it — behavior is governed by a
   declared policy (§6).
5. **The library owns the wire, the consumer owns the logic.** Consumers never
   see exit codes, stdout JSON dialects, stderr discipline, or matcher regex
   flavors. They see typed events and return typed decisions.
6. **Config is generated, not hand-written.** One Go `Manifest` produces
   correct `hooks.json` / `settings.json` / `config.toml` / plugin scaffolding
   per provider, with the per-provider timing/async/fail-mode workarounds baked
   in (§7).

Non-goals for v1 (§11): auth/login flows, HTTP transport to a decision server,
transcript capture pipelines. These are consumer concerns layered *on top of*
this library, not inside it.

---

## 3. Core contract

Module: `github.com/speakeasy-api/agenthooks` (root package `agenthooks`).

### 3.1 Entry point and handler registration

A hook program is a normal Go binary. The library detects the invoking
provider (from generated-config argv, with env/shape sniffing as fallback),
decodes stdin, dispatches to the registered handler, and encodes the response
in the provider's dialect — including exit code and stderr discipline.

```go
func main() {
	r := agenthooks.New(
		agenthooks.WithPolicy(agenthooks.Policy{
			Fail:        agenthooks.FailClosed, // what a handler error/timeout means
			AskFallback: agenthooks.FallbackDeny, // when Ask is unsupported
		}),
	)

	r.OnToolPre(func(ctx context.Context, e *agenthooks.ToolPreEvent) (agenthooks.ToolPreDecision, error) {
		if e.Tool.Canonical == agenthooks.ToolShell && isDestructive(e.Tool) {
			return agenthooks.Deny("blocked by policy"), nil
		}
		return agenthooks.NoDecision(), nil
	})

	r.OnPromptSubmitted(func(ctx context.Context, e *agenthooks.PromptEvent) (agenthooks.PromptDecision, error) {
		return agenthooks.AcceptPrompt().WithContext("today is " + today()), nil
	})

	// Fidelity escape hatch: receives EVERY event, mapped or not, with raw payload.
	r.OnAny(func(ctx context.Context, e *agenthooks.Event) error {
		return telemetry.Send(ctx, e.Provider, e.NativeName, e.Raw)
	})

	agenthooks.Main(r) // parses argv/stdin, runs, writes response, sets exit code
}
```

Notes:

- One handler per unified event kind; `OnAny` is additive (observe-only) and
  runs regardless. Typed handlers gate/mutate; `OnAny` never does.
- `Main` never lets handler panics/errors leak as garbage on stdout. Outcome on
  error follows `Policy.Fail` and the provider's actual blocking mechanism
  (exit 2, deny body, `failClosed` flag — see §6).
- **Logging discipline is enforced**: the runner redirects the process's
  stdout usage by handlers (and offers `agenthooks.Logger(ctx)`) to a state
  file / stderr only where stderr is safe. This exists because Gemini parses
  stderr as the decision when stdout is empty, and Codex rejects unknown JSON
  on stdout. The runner always emits an explicit well-formed response.

### 3.2 The envelope

```go
type Provider string

const (
	ProviderClaudeCode Provider = "claude-code" // incl. cowork/desktop/web variants
	ProviderCursor     Provider = "cursor"      // incl. cursor-agent CLI, cloud agents
	ProviderCodex      Provider = "codex"
	ProviderGemini     Provider = "gemini"
	ProviderOpenCode   Provider = "opencode"
)

// Variant refines Provider where runtime behavior genuinely differs.
type Variant string // e.g. "cowork", "cli", "ide", "cloud", "" (unknown)

type Event struct {
	Provider   Provider
	Variant    Variant
	NativeName string    // "PreToolUse", "beforeShellExecution", "BeforeTool", ...
	Kind       EventKind // normalized; KindOther when unmapped
	Time       time.Time // library receive time; Gemini also supplies its own

	Session SessionInfo
	Agent   *AgentInfo // non-nil inside a subagent context

	// Backfilled marks a synthesized event for a provider miss (e.g. print
	// modes that skip prompt hooks): reporting-only, nil Raw, no capabilities.
	Backfilled bool

	// Raw is the verbatim provider payload. Never normalized, never trimmed.
	Raw json.RawMessage
}

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

type AgentInfo struct {
	ID   string
	Type string // provider's subagent type name
}
```

Provider-specific fields that don't generalize are reached two ways, both
lossless:

```go
// Typed views (generated from provider schemas; nil if wrong provider/event):
cc, ok := claudecode.PreToolUse(e)   // *claudecode.PreToolUseInput
cx, ok := codex.PreToolUse(e)
cu, ok := cursor.BeforeShellExecution(e)

// Or generic:
v := e.RawField("stop_hook_active") // gjson-style path into Raw
```

The `provider/*` packages ship **complete typed structs for every native event
and every native field**, kept in sync with upstream schemas (Codex publishes
JSON Schema; Claude/Gemini/Cursor structs are maintained against docs/source
with golden fixtures, §10). The unified layer is convenience; the typed native
layer is the fidelity guarantee.

### 3.3 Unified event kinds

```go
type EventKind string

const (
	KindSessionStart    EventKind = "session.start"
	KindSessionEnd      EventKind = "session.end"
	KindPromptSubmitted EventKind = "prompt.submitted"
	KindToolPre         EventKind = "tool.pre"      // gate/rewrite before execution
	KindToolPost        EventKind = "tool.post"
	KindToolError       EventKind = "tool.error"
	KindPermission      EventKind = "permission.request"
	KindStop            EventKind = "agent.stop"    // turn finished
	KindSubagentStart   EventKind = "subagent.start"
	KindSubagentStop    EventKind = "subagent.stop"
	KindCompactPre      EventKind = "compact.pre"
	KindCompactPost     EventKind = "compact.post"
	KindNotification    EventKind = "notification"
	KindFileEdited      EventKind = "file.edited"   // post-hoc file-change reports
	KindModelRequest    EventKind = "model.request" // gemini BeforeModel, opencode chat.params
	KindModelResponse   EventKind = "model.response"
	KindOther           EventKind = "other"         // any unmapped native event
)
```

Typed event structs embed `Event` and add normalized fields, e.g.:

```go
type ToolPreEvent struct {
	Event
	Tool ToolCall
}

type ToolCall struct {
	ID          string        // native id, or synthesized (see Synthesized)
	Synthesized bool          // true when provider omitted an id (Cursor MCC/MCP cases)
	Name        string        // native tool name, verbatim
	Canonical   CanonicalTool // ToolShell | ToolFileRead | ToolFileWrite | ToolFileEdit |
	                          // ToolSearch | ToolFetch | ToolTask | ToolMCP | ToolOther
	MCP         *MCPCall      // non-nil when the call targets an MCP tool
	Input       json.RawMessage // ALWAYS a JSON object (library un-stringifies Cursor's string form)
	RawInput    json.RawMessage // the input exactly as the provider sent it
}

type MCPCall struct {
	Server string // decoded from mcp__s__t / mcp_s_t / MCP:t + context; "" if undecodable
	Tool   string // tool name as the MCP server knows it
	URL    string // cursor/gemini transport info when provided
	Command string
}
```

`ToolCall.ID` synthesis (`hash(session|turn|tool|input)` →
`hook_synth_<16 hex>`) is deterministic so pre/post records correlate on
providers that omit ids.

### 3.4 Event mapping table (normative)

| Unified | Claude Code | Codex | Cursor | Gemini | OpenCode (shim) |
|---|---|---|---|---|---|
| session.start | SessionStart | SessionStart | sessionStart | SessionStart | plugin init + `session.created` |
| session.end | SessionEnd | — | sessionEnd | SessionEnd | `server.instance.disposed` (best-effort) |
| prompt.submitted | UserPromptSubmit | UserPromptSubmit | beforeSubmitPrompt | BeforeAgent | `chat.message` |
| tool.pre | PreToolUse | PreToolUse | preToolUse + beforeShellExecution + beforeMCPExecution + beforeReadFile (deduped, §9) | BeforeTool | `tool.execute.before` |
| tool.post | PostToolUse | PostToolUse | postToolUse + afterShellExecution + afterMCPExecution + afterFileEdit (deduped) | AfterTool | `tool.execute.after` |
| tool.error | PostToolUseFailure | — | postToolUseFailure | — (AfterTool w/ error field) | `tool.execute.after` (error state) |
| permission.request | PermissionRequest | PermissionRequest | — | — (`ask` decision on BeforeTool only) | `permission.asked` event + HTTP reply (§8) |
| agent.stop | Stop | Stop | stop | AfterAgent | `session.idle` |
| subagent.start | SubagentStart | SubagentStart | subagentStart | — | `session.created` (child) |
| subagent.stop | SubagentStop | SubagentStop | subagentStop | — | `session.idle` (child) |
| compact.pre | PreCompact | PreCompact | preCompact | PreCompress | `experimental.session.compacting` |
| compact.post | PostCompact | PostCompact | — | — | `session.compacted` |
| notification | Notification | — | — | Notification | `tui.toast.show` (observe) |
| file.edited | FileChanged | — | afterFileEdit / afterTabFileEdit | — | `file.edited` |
| model.request | — | — | — | BeforeModel / BeforeToolSelection | `chat.params` / `chat.headers` |
| model.response | MessageDisplay (approx.) | — | afterAgentResponse / afterAgentThought | AfterModel | `experimental.text.complete` |
| other | remaining ~15 events (Setup, TaskCreated, Elicitation, Worktree*, …) | — | remaining (workspaceOpen, Tab events, …) | — | remaining bus events |

Everything in the last row is still fully deliverable via `OnAny` /
`OnOther(nativeName, …)` with typed native structs — "unmapped" never means
"unavailable".

---

## 4. Decision model

Each gating event kind has a decision type with constructors. Decisions carry
*intent*; the provider codec translates intent into that provider's mechanism.

```go
// tool.pre / permission.request
func NoDecision() ToolPreDecision            // defer to the provider's normal flow (NEVER a forced allow)
func Allow() ToolPreDecision                 // skip the permission prompt where supported
func Deny(reason string) ToolPreDecision
func AskUser(reason string) ToolPreDecision  // force a confirmation prompt
func (d ToolPreDecision) WithUpdatedInput(v any) ToolPreDecision // rewrite tool args
func (d ToolPreDecision) WithContext(s string) ToolPreDecision   // inject context for the model

// prompt.submitted
func AcceptPrompt() PromptDecision
func BlockPrompt(reason string) PromptDecision
func (d PromptDecision) WithContext(s string) PromptDecision

// agent.stop / subagent.stop
func Finish() StopDecision
func ContinueWith(instruction string) StopDecision // claude decision:block+reason, cursor followup_message, codex continuation prompt, gemini retry

// tool.post
func Observed() ToolPostDecision
func FlagOutput(reason string) ToolPostDecision            // feedback shown to the model
func ReplaceOutput(v any) ToolPostDecision                 // claude updatedToolOutput, cursor updated_mcp_tool_output (MCP only), gemini reason-replace
func (d ToolPostDecision) WithContext(s string) ToolPostDecision

// universal modifiers on all decisions
func (d T) WithSystemMessage(s string) T // user-facing note where supported
func (d T) StopAgent(reason string) T    // continue:false where supported
```

Important semantics the constructors encode:

- **`NoDecision` ≠ `Allow`.** An empty-body response must let the provider's
  own permission flow run, not force-allow. The codecs emit the correct "no
  opinion" form per provider (`{}` on Claude/Cursor, empty stdout on Codex,
  `{}` on Gemini to avoid stderr-parsing).
- **`Allow` never loosens.** On Claude, hook-allow still respects deny rules;
  the library documents (and tests) that `Allow` means "skip the ask", not
  "bypass policy" — matching every provider's actual behavior.
- **Loop-guard awareness.** `StopEvent.PreviouslyContinued` surfaces
  `stop_hook_active` / `loop_count` uniformly; `ContinueWith` refuses to exceed
  a configurable continuation cap so consumers can't accidentally build
  infinite loops on providers without native caps (Cursor Claude-compat mode
  has `loop_limit: null`).

### 4.1 Capability matrix and degradation

```go
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

func Capabilities(p Provider, v Variant, k EventKind) CapSet
func (e *Event) Can(c Capability) bool
```

Selected divergences the matrix encodes (from research):

| Capability | Divergence |
|---|---|
| Ask on tool.pre | Claude ✓; Gemini ✓ (undocumented); Cursor: enforced only on shell/MCP events, ignored on `preToolUse`, treated as *deny* on `subagentStart`; Codex ✗ (fails the hook run) |
| UpdateInput | Claude: full replace; Codex: replace, allow-only; Cursor: `updated_input` on preToolUse only; Gemini: **shallow merge** (key removal impossible — library surfaces `ErrLossyUpdate` if the rewrite removes keys); OpenCode: mutate args ✓ |
| Deny on file reads | Cursor `beforeReadFile` is allow/deny only (no ask) |
| StopAgent | Claude/Gemini/Codex `continue:false`; Cursor ✗ (except via deny paths) |
| tool.error | Codex folds failures into PostToolUse; Gemini reports via `tool_response.error` |

When a handler returns an unsupported decision, `Policy` decides:
`Degrade` (map to the nearest supported intent: Ask→Deny or Ask→NoDecision per
`AskFallback`; log it) or `Strict` (treat as handler error → `Policy.Fail`).

### 4.2 Failure policy

```go
type FailMode int
const (
	FailOpen   FailMode = iota // handler error/timeout → NoDecision
	FailClosed                 // handler error/timeout → Deny (where possible)
)
```

`FailClosed` is enforced with the provider's real mechanism, which the library
knows per event: exit 2 (Claude/Codex/Cursor), deny JSON body (Cursor stdout),
`failClosed: true` in generated Cursor config, deny decision (Gemini), thrown
error (OpenCode shim). On events with no blocking mechanism, `FailClosed`
downgrades to logging (and says so via `Can(CapDeny)`), because pretending to
block on a fire-and-forget event is worse than being honest.

Consumers who need a ratchet (fail-open until first success, then
fail-closed) implement it in the handler; the library provides the primitives
(`Policy` is resolvable per event: `PolicyFunc func(*Event) Policy`).

---

## 5. Fidelity guarantees (normative)

1. `Event.Raw` is byte-identical to what the provider sent (post transport
   framing only — e.g. argv-decoding for legacy Cursor CLI / Codex notify).
2. Native typed structs (`provider/*`) decode with unknown-field capture:
   unrecognized JSON keys land in an `Extra map[string]json.RawMessage` on
   every struct, never dropped.
3. Normalization documents its losses. Where a projection is lossy (Gemini's
   `additionalContext` HTML-escaping, Gemini shallow-merge updates, Cursor
   stringified `tool_input` re-parsing, `MessageDisplay`≈model.response), the
   typed API exposes both the normalized and native forms and the docs carry a
   ⚠ marker generated from the quirk registry (§9).
4. Round-trip property: for every fixture in the corpus,
   `decode(payload) → encode(NoDecision)` produces the provider-correct no-op,
   and `Raw` equals the input.

---

## 6. Runtime & transports

One binary, several invocation modes, selected by argv the generated configs
control:

```
mybinary agenthooks run    --provider=claude-code   # process-per-event, stdin JSON (claude, codex, gemini, cursor)
mybinary agenthooks run    --provider=cursor --argv-payload  # legacy cursor-agent CLI (<2026-05-20): payload in argv
mybinary agenthooks notify --provider=codex          # legacy codex notify: kebab-case JSON in argv[1]
mybinary agenthooks serve  --provider=opencode       # long-lived daemon for the OpenCode shim (§8)
```

(Consumers embed this by calling `agenthooks.Main(r)`; the subcommand surface
is provided by the library so any consumer binary gets it for free. A
standalone `agenthooks` CLI that loads handlers is explicitly out of scope —
this is a library.)

Runtime responsibilities per mode:

- **stdin mode**: read payload, enforce a deadline slightly under the
  provider-configured timeout (so we always answer rather than get killed
  mid-write), decode → dispatch → encode, exit with the dialect-correct code.
- **stderr/stdout discipline** as in §3.1.
- **Provider detection**: primary = `--provider` flag baked into generated
  config; fallback = env sniffing (`CLAUDE_PLUGIN_ROOT`/`CLAUDE_PROJECT_DIR`,
  `CURSOR_VERSION`, `GEMINI_CWD`, `CODEX_HOME`) then payload-shape sniffing
  (`hook_event_name` casing, `conversation_id` presence). Detection result is
  on the event (`Provider`, `Variant`, plus `DetectionConfidence` for
  observability). Note Codex/Cursor deliberately export `CLAUDE_*` compat vars,
  so env sniffing alone is insufficient — flag-first is a hard rule.
- **Variant detection**: cowork = cmux `local_<rid>.json` adjacency /
  `CLAUDE_PROJECT_DIR` shape; remote = `CLAUDE_CODE_REMOTE`; cursor cloud/CLI
  = payload capability probing.

---

## 7. Config generation & installation

```go
pkg install

type Manifest struct {
	Command  []string          // how to invoke the consumer binary (abs path or PATH lookup)
	Hooks    []HookSpec
	Identity Identity          // plugin name/version/description for plugin-based installs
}

type HookSpec struct {
	Kind     agenthooks.EventKind
	Tools    ToolMatcher // unified matcher (names, canonical classes, MCP globs)
	Blocking bool        // decision path vs telemetry path
	Timeout  time.Duration
}

func Render(m Manifest, target Target) (fs.FS, error) // Target = provider (+ scope: user/project/plugin)
func Install(ctx context.Context, m Manifest, target Target, opts ...InstallOption) error
func Diff(...) // idempotent re-install support, fingerprint-based
```

Per-target rendering encodes the workaround knowledge:

- **Claude Code**: plugin layout (`.claude-plugin/plugin.json` +
  `hooks/hooks.json` — *must* be at `hooks/hooks.json`, not plugin root) or
  settings.json fragments. `Blocking: false` renders `async: true` **except
  `Stop`, which is forced synchronous** (cowork drops async Stop hooks).
  Timeouts in seconds; `SessionStart` interactive flows get raised timeouts.
- **Cursor**: `hooks.json` v1. Decision hooks get `"failClosed": true` when
  `Policy` is FailClosed; telemetry hooks stay fail-open. Timeout in seconds;
  the library warns when `Timeout` × retry budget exceeds the hook timeout.
  MCP double-fire handled in-binary (not via `^(?!MCP:)` matchers) so the
  correct empty-response shape is still emitted.
- **Codex**: `hooks.json` (or `config.toml` tables) + **trust pre-seeding**:
  reimplementation of Codex's definition-hash fingerprint so installs can
  write `[hooks.state]` trusted hashes. `Blocking: false` renders the
  tee-to-tmpfile backgrounder wrapper (Codex parses-but-skips `async: true`).
  Emits nothing on stdout for allow.
- **Gemini**: `settings.json` fragment with `name`/`description` (enables
  `/hooks enable|disable` UX), timeouts converted to **milliseconds**, matcher
  dialect `mcp_server_tool`.
- **OpenCode**: writes the self-contained `.opencode/plugin/agenthooks.ts`
  shim pointing at the consumer binary (§8).

Matchers: `ToolMatcher` compiles to the provider dialect where expressible
(Claude regex/exact-list rules incl. the hyphen/comma version gates, Gemini
regex-with-literal-fallback, Cursor tool-type strings, Codex regex). Where not
expressible, the generated config matches broadly and the runner filters
in-process — correctness over per-call process savings, with a `Strictness`
knob for hot paths.

---

## 8. OpenCode bridge

OpenCode has no out-of-process hook protocol, so agenthooks ships two pieces:

1. **A generated shim plugin** (`.opencode/plugin/agenthooks.ts`, ~100 lines,
   rendered by the `install` package with the consumer command baked in, so
   there is no npm dependency): an OpenCode plugin that spawns the consumer
   binary in `agenthooks serve --provider=opencode` mode at plugin init and
   proxies hook invocations over **NDJSON on stdio**: request
   `{seq, hook, input, output}` → response `{seq, output, error?}`. The shim
   `Object.assign`s the returned output (arrays replaced wholesale, preserving
   OpenCode's mutation semantics) and re-throws `error` to keep block-the-tool
   behavior. `dispose` terminates the daemon. The shim adds the timeout policy
   OpenCode lacks.
2. **`provider/opencode`** in Go: maps shim frames into unified events. The
   daemon also receives `serverUrl`/`directory`/`worktree` at startup and gets
   an optional typed client for OpenCode's HTTP API (permission replies via
   `POST /session/:id/permissions/:permissionID` — the *only* working
   permission mechanism, since the `permission.ask` plugin hook is dead in
   ≥1.5.0; context injection via `session.prompt` with `noReply`).

This keeps the promise: consumers write the same `OnToolPre` handler; on
OpenCode it arrives via the shim with `Raw` = the exact hook input JSON.

---

## 9. Quirk registry (the glue we hide)

Machine-readable registry (`quirks.go` + generated docs), each entry:
provider, version range, affected event/capability, behavior, mitigation,
upstream reference. Seeded from provider research and production observation:

| # | Quirk | Mitigation |
|---|---|---|
| 1 | Claude cowork skips async `Stop` hooks | config gen forces `async:false` on Stop |
| 2 | Cursor fires `preToolUse` **and** `beforeShellExecution`/`beforeMCPExecution` for the same call | runner dedupes into one `tool.pre`, keeps both raws |
| 3 | Cursor `MCP:` tool-name prefix; missing `tool_use_id` on MCP events | strip prefix into `MCPCall`; synthesize stable IDs |
| 4 | Cursor 2.0.64+ requires snake_case output (`user_message`); 1.7 used camelCase | emit snake_case + harmless legacy `continue` field |
| 5 | Cursor `tool_input` object on preToolUse, JSON-string on MCP events | normalize to object; `RawInput` keeps original |
| 6 | Cursor CLI <2026-05-20 passes payload via argv; Windows CLI double-fires; cloud/CLI fire event subsets | argv mode; idempotency key surface for consumers; capability matrix per Variant |
| 7 | Cursor fail-open default; crashed hook allows action | `failClosed` in generated config on decision events |
| 8 | Codex: empty stdout = allow; unknown JSON rejected; `ask`/`approve` fail the hook run | codec emits exact dialect; Ask degrades per policy |
| 9 | Codex hooks require user trust of definition hash | install pre-seeds `[hooks.state]` trusted hashes |
| 10 | Codex has no async hooks (`async` parsed-but-skipped) | generated backgrounder wrapper for telemetry events |
| 11 | Gemini: exit codes ≠ docs (any non-zero except 1 blocks); stderr parsed as decision when stdout empty | runner always writes explicit JSON to stdout; never bare non-zero exits |
| 12 | Gemini `hookSpecificOutput.tool_input` is shallow-merge | `ErrLossyUpdate` when a rewrite deletes keys; docs marker |
| 13 | Gemini `additionalContext` HTML-escapes `<`/`>` | documented loss; optional pre-encoding |
| 14 | Gemini timeouts in ms vs everyone's seconds | `time.Duration` everywhere; codecs convert |
| 15 | MCP naming: `mcp__s__t` (Claude/Codex) vs `mcp_s_t` (Gemini) vs `MCP:` (Cursor) vs in-context (OpenCode) | unified `MCPCall`; matcher compiler |
| 16 | Claude `@file` reads / Bash file writes / direct `/skill` bypass tool hooks; Cursor AskQuestion tool fires no hooks; Codex only intercepts Bash/apply_patch/MCP | documented blind-spot matrix per provider; `Can()` reflects it |
| 17 | Claude empty stdout ≠ allow; `NoDecision` must not force-allow | explicit NoDecision encoding everywhere |
| 18 | OpenCode `permission.ask` typed but dead ≥1.5.0 | permission flow via HTTP reply + `tool.execute.before` throw |
| 19 | Stop-loop guards differ (`stop_hook_active`, cap 8 / `loop_count`, `loop_limit` 5 or null) | unified `PreviouslyContinued` + library-side continuation cap |
| 20 | Providers cross-set env (`CLAUDE_PLUGIN_ROOT` on Codex, `CLAUDE_PROJECT_DIR` on Cursor/Gemini) | flag-first provider detection |

The registry doubles as the conformance-test plan: every quirk gets a fixture.

---

## 10. Package layout

```
agenthooks/
├── agenthooks.go            // Runner, Main, handler registration
├── event.go                 // Event, SessionInfo, ToolCall, kinds
├── decision.go              // decision types + constructors
├── capability.go            // capability matrix + policy
├── quirks.go                // quirk registry (source of truth for docs/tests)
├── provider/
│   ├── claudecode/          // native typed structs + codec (30 events)
│   ├── codex/               // codec generated from upstream JSON schemas
│   ├── cursor/
│   ├── gemini/
│   └── opencode/            // shim wire protocol + typed hook frames
├── install/                 // Manifest → rendered configs, trust seeding, fingerprint diffing
├── transcript/              // best-effort JSONL readers per provider (claude/cursor formats)
├── agenthookstest/          // fixture corpus, fake-provider harness, round-trip assertions
└── e2e/                     // opt-in suite driving real local agent CLIs end to end
```

Testing strategy: golden fixture corpus per provider *and version* (captured
payloads + expected normalized event + expected encoded responses per
decision), round-trip property tests (§5.4), and a `fakeagent` harness that
spawns the consumer binary exactly like each provider does (stdin/argv, env,
timeout, exit-code interpretation) so consumer hooks can be integration-tested
in CI without the actual agents.

---

## 11. Non-goals (v1)

- **Auth, login, identity** (browser flows, device agents, credential
  caches): consumer concerns built *on* this library. agenthooks provides the
  hook I/O substrate those flows plug into.
- **HTTP/decision-server transport**: a server-authoritative decision model
  is a consumer of this library — the handler body does the POST. (Claude's
  native `http` hook type is a possible later `install` target.)
- **Transcript capture/dedup pipelines**: the `transcript` package gives
  parsing primitives; pipelines belong to consumers.
- **A standalone hooks CLI / config-file DSL**: library-first; Go is the DSL.
- **In-process Agent-SDK hooks** (Claude Agent SDK callbacks): different
  runtime model; possible future `sdkbridge` package.

## 12. Open questions

1. **Handler concurrency**: one event per process makes this moot except in
   OpenCode serve mode — serialize per session (matching OpenCode's sequential
   semantics) or allow parallel with consumer opt-in?
2. **Version pinning**: do we gate dialect features on detected provider
   versions (Cursor camelCase era, Claude matcher version gates §Claude 2.1.19x)
   or always emit the modern + harmless-legacy superset? Proposal: superset
   where harmless, version-gate otherwise; revisit per quirk.
3. **`model.request`/`model.response` in v1?** Only Gemini/OpenCode support
   them and they're hot-path (Gemini AfterModel fires per streamed chunk).
   Proposal: ship types, mark experimental, exclude from config gen defaults.
4. **Cursor Claude-compat as an install target**: rendering Claude-format
   config that both agents read is tempting (one file, two agents) but hits
   the duplicate-firing quirk and loses Cursor-only events. Proposal: native
   configs per provider, always.
5. **Codex `notify` support**: worth a `notification` mapping for orgs on old
   Codex, or hooks-only? Proposal: ship the argv decode (it's tiny), don't
   advertise.
