# RFC: Decoupled agent-side telemetry (OTel logs) and backend enforcement logging

- **Status:** Draft, for review (rev 3 — client/server delivery, superseding the spool/exporter draft)
- **Author:** agenthooks maintainers (drafted from joint research of `agenthooks` and `gram`)
- **Scope:** Both repos — `github.com/speakeasy-api/agenthooks` (this repo) and the `gram` backend (`/Users/subomi/code/gram`, referenced read-only)
- **Date:** 2026-08-05

> **Rev 3 note:** delivery moved from a disk spool + externally supervised
> exporter daemon (rev 2's §4.5/§4.7) to the library's **client/server
> architecture**: hooks are installed as a lightweight `agenthooks client`
> that forwards each event to a long-running, auto-spawned `agenthooks
> server` hosting the full pipeline — and the telemetry recorder now lives
> in that server as an ordinary in-process OTel batch pipeline
> (`BatchProcessor` + `otlploghttp`), flushed on the server's shutdown
> points. The record model (§4.3), deterministic identity (§4.4), capture
> levels (§4.6), and everything gram-side (§5, §6) carry over unchanged.
> The durability boundary moves from disk to the server process: records
> lost to a server crash are accepted as best-effort observability.

> **Rev 2 note:** the primary signal is now **OTel log records (wide events)**
> shipped over OTLP to gram's **existing** `/rpc/hooks.otel/v1/logs` ingest,
> instead of spans to a new traces endpoint. Gram has substantially built out
> its logs processing (attribute normalization, attribution, `telemetry_logs`
> wide-event storage, URN classification); logs-first reuses all of it and
> eliminates most of the backend work the traces draft required. Traces move
> to §8 (alternatives / future evolution); the design keeps deterministic
> trace-context identity so spans can be added later without re-keying.

---

## 1. Background & problem: observability is coupled to enforcement

Today the hook binary built on this library (`speakeasy-hooks`, at
`gram/hooks/cmd/speakeasy-hooks/main.go`) projects **every** hook event into
the Gram ingest contract and POSTs it synchronously to the backend. The
backend uses that one request for two unrelated jobs:

1. **Enforcement** — deciding allow/deny/warn for gating events.
2. **Observability** — deriving essentially all coding-agent telemetry
   (ClickHouse wide events, Postgres session transcripts, usage metrics,
   MCP inventory, skill observations) from the same payload.

Concrete code paths showing the coupling:

| Path | What it shows |
|---|---|
| `gram/hooks/relay/runner.go:64-66` | The consumer's own comment of record: *"gating events (prompt.submitted, tool.requested) POST synchronously and honor deny; every other event is relayed as fire-and-forget telemetry."* "Fire-and-forget" means *ignore the verdict*, not *skip the network* — observe-only events still make a synchronous HTTP call (45 s budget, `gram/hooks/relay/client.go:22-28`) unless the provider config detached the whole hook process. |
| `gram/hooks/relay/envelope.go:138-161` (`buildEnvelope`) | One envelope (`hook.ingest.v1`, `IngestRequestBody`) carries both decision inputs and observability freight: `source`, `session`, `event`, feature blocks (`prompt`, `tool_call`, `mcp`, `mcp_inventory`, `usage`, `message`, `skill`, `notification`, `mcp_attribution`) and the scrubbed `raw` provider payload. |
| `gram/server/design/hooks/design.go:267-288` | `IngestHookPayload` / `IngestHookResult`: the *enforcement response* (`decision: allow|deny`) is returned from the same endpoint that receives the observability payload (`POST /rpc/hooks.ingest`). |
| `gram/server/internal/hooks/ingest_hooks.go:55` (`Ingest`) → `:410` (`evaluateCanonicalHook`) → `:622` (`recordCanonicalHook`) → `:734` (`writeCanonicalTelemetry`) → `:868` (`logHookTelemetry`) | The handler evaluates enforcement first, then converts **the same payload** into ClickHouse `telemetry_logs` attributes and Postgres rows. Observability is literally a post-processing step of the enforcement request. |
| `gram/server/internal/hooks/session_capture.go`, `pending_helpers.go`, `claude_hooks.go`, `cursor_hooks.go`, `codex_hooks.go` | The legacy provider endpoints (`/rpc/hooks.claude`, `.cursor`, `.codex`) follow the same dual-use pattern, including `writeClaudeBlockToClickHouse` writing a *block* observability row that shares a hash-derived trace ID with the tool event row. |

Consequences of the coupling:

- **Latency:** every observe-only event (tool.post, stop, session.end,
  notifications) pays a synchronous round-trip to the control plane from
  inside the hook process, bounded only by provider timeouts and the 45 s
  send budget.
- **Availability entanglement:** observability data is lost or spooled
  whenever the enforcement endpoint is down, and enforcement request size /
  schema is held hostage by observability needs (prompt text, assistant
  messages, usage, raw payloads all ride the decision request).
- **Second-hand telemetry:** the observability record is whatever the
  backend can reconstruct from an enforcement request, stamped server-side
  (`writeCanonicalTelemetry`), rather than a first-class record emitted at
  the source with real timing.
- **Enforcement is under-logged in its own right:** the decision verdict
  lives as attributes stamped onto observability rows derived from the
  request (`gram.hook.block_reason` on the event row; `gram.hook.decision`
  only on duration *metrics*, `gram/server/internal/attr/conventions.go:336-339`),
  plus `tool_call_blocks` in Postgres for denies. If the observability
  derivation goes away, enforcement currently loses most of its audit trail.

### Why logs-first

Gram already runs a mature OTLP **logs** ingest for coding agents:
`POST /rpc/hooks.otel/v1/logs` (`gram/server/design/hooks/design.go:440-462`),
handled by `Service.Logs` → `writeClaudeOTELLogsToClickHouse`
(`gram/server/internal/hooks/otel.go:25`, `:354`). That path already does,
per log record:

- attribute normalization into the shared conventions —
  `normalizeClaudeLogAttributes` (`otel.go:550-563`) maps `session.id` →
  `gen_ai.conversation.id`, `model` → `gen_ai.response.model`, and attribute-
  or field-borne trace context onto `trace.id` / `span.id`;
- stamping of `gram.event.source=hook`, project/org, `gram.hook.source`
  (product surface), and account attribution (Redis-cached per session,
  `otel.go:56-160`);
- body → `gram.log.body`, scope name/version capture, native OTLP
  `traceId`/`spanId` fields → `trace.id`/`span.id` attributes
  (`otel.go:442-447`);
- bulk write into ClickHouse `telemetry_logs` — the same wide-event table
  every hook-derived observability row lands in today
  (`gram/server/internal/telemetry/README.md`);
- URN classification per row (`gram/server/internal/telemetry/event_urn.go`).

Emitting **one OTel log record per hook event, shaped to these conventions**,
lets the agent-side stream drop into the built-out pipeline nearly unchanged:
same table, same materialized columns, same downstream readers. No new
ingest path, no new storage model.

### What this RFC proposes

1. **Agent-side telemetry becomes a built-in, opt-in feature of this OSS
   library**: a `telemetry` package plus a runner option that emits one
   **OpenTelemetry log record (wide event)** per hook event from the
   long-running **hook server** process (`agenthooks server`, the
   singleton the per-hook `agenthooks client` forwards events to),
   batched in memory and shipped over **OTLP/HTTP** in the background —
   never adding latency to the hook's critical path, which only ever
   waits on the decision. (DESIGN.md §11's non-goals will simply be
   rewritten when the package lands — nothing has shipped, so no formal
   amendment is carried in this RFC.)
2. **The gram backend logs enforcement itself, at decision time,
   independently of the agent-side telemetry stream**, and the enforcement
   request slims down to decision-relevant data only.
3. **Cutover, not additive:** once telemetry ships, non-gating events stop
   flowing through the enforcement endpoint, and the backend stops deriving
   observability from enforcement requests.

Correlation between the two streams uses the **existing session ID and turn
ID** (`SessionInfo.ID` / `SessionInfo.TurnID`, `event.go:105-114`) plus
trace-context fields derived **exactly** the way gram already derives its
pseudo trace IDs (§4.4). No new correlation ID is introduced.

---

## 2. Goals / non-goals

### Goals

- **G1** — Hook telemetry is emitted from the agent side as OTel log
  records, with zero added latency on the hook critical path (recorded
  after the response is on the wire, batched and shipped off-path in the
  hook server; fail-open, never blocks).
- **G2** — The telemetry stream carries (at minimum) everything gram derives
  from hook payloads today for observability, per the inventory in §3, so
  the backend can stop deriving it from enforcement requests without losing
  data.
- **G3** — Emitted records are **schema-compatible with gram's existing
  logs pipeline**: attribute keys reconcile with the `gram.hook.*` /
  `gen_ai.*` conventions the backend derives today, and trace-context
  fields reproduce gram's existing hash derivation, so downstream readers
  and joins need minimal changes.
- **G4** — Enforcement decisions get their own self-contained log in gram,
  written at decision time, correlatable with agent-side records by
  session ID + turn ID (and tool call ID / derived trace ID where present).
- **G5** — The enforcement request payload shrinks to decision-relevant
  fields; non-gating events stop being sent to the enforcement endpoint at
  all.
- **G6** — The library feature is vendor-neutral: any OTLP logs endpoint
  works; gram is one consumer configuration.
- **G7** — Deterministic identity makes the pipeline replay- and
  double-fire-safe (Cursor duplicate events, provider re-fires, retried
  exports) and forward-compatible with a future traces signal (§8).

### Non-goals

- **N1** — Traces or metrics as the v1 signal. Log records are the primary
  signal; structured data rides attributes (gram's wide-event model is
  built for exactly this). Spans are a possible future evolution (§8) and
  gram may derive metrics server-side.
- **N2** — Replacing gram's enforcement transport. Gating events keep their
  synchronous request/response; only its payload slims.
- **N3** — Shipping a general transcript-capture product in the library.
  Content capture (prompt/tool-IO/assistant text) is an explicit, opt-in
  capture level with redaction hooks — off by default (§4.6).
- **N4** — Per-record durability. There is no disk spool: the hook
  server's memory is the buffer and its shutdown flush points (§4.7) are
  the durability boundary. Records buffered in a server that crashes —
  or recorded in a per-hook process (plain `run`), which exits before a
  batch ships — are lost, and that is accepted:
  telemetry is best-effort observability, and the enforcement rail (§5.1)
  carries everything decision-critical.
- **N5** — Auth/login flows. The library takes endpoint + headers as config;
  acquiring credentials stays a consumer concern (speakeasy-hooks already
  has this stack: `gram/hooks/relay/auth.go`).

---

## 3. Inventory: what gram gathers from hook data today → replacement

Sources: `gram/server/design/hooks/design.go` (contract),
`gram/server/internal/hooks/ingest_hooks.go` (`writeCanonicalTelemetry`,
`recordCanonicalHook`), `gram/server/internal/telemetry/README.md` +
`gram/server/clickhouse/schema.sql` (storage),
`gram/server/internal/attr/conventions.go` (attribute keys).

Legend for **Replacement**: `record attr` = attribute on the hook-event log
record (§4.3); `resource attr` = OTel resource attribute stamped once per
batch; `enforce` = stays on the slimmed enforcement request (§5.3);
`capture` = only present at an elevated content-capture level (§4.6).

Where possible the replacement uses **the same attribute key gram's
pipeline already writes or normalizes**, so `telemetry_logs` materialized
columns and existing queries keep working (G3).

| # | Ingest field (hook.ingest.v1) | Where gram persists it today | Replacement in the decoupled design |
|---|---|---|---|
| 1 | `source.adapter` (provider slug) | `gram.hook.source` attr → CH materialized `hook_source`; `skill_observations.provider`; `tool_call_blocks.provider` | record attr `gram.hook.source` (same key, emitted at source; Claude surface refinement stays a server-side enrichment, §5.2). Also resource attr `agenthooks.provider` + `agenthooks.variant`. Still on `enforce` for the decision log. |
| 2 | `source.adapter_version` | (mostly unset) | resource attrs `telemetry.sdk.*`, `service.version`, `agenthooks.version` |
| 3 | `source.raw_event_name` | `gram.hook.event` attr → CH | record attr `gram.hook.event` (native name, same key) + `event.name` = unified kind (the producer-key convention the URN deriver already reads, `event_urn.go:28-31`; dual-emitted with the top-level EventName field — §4.3) |
| 4 | `source.hostname` | `gram.hook.hostname` attr; Redis session metadata | resource attr `host.name` (OTel semconv); gram maps to `gram.hook.hostname` at ingest |
| 5 | `source.user_email` | actor resolution → PG messages, `user_email` CH column | record attr `user.email` — the logs handler already resolves and attributes by email (`otel.go:106-116`, `:454-457`); still on `enforce` for attribution of decisions |
| 6 | `session.id` | `gen_ai.conversation.id` attr → CH `chat_id`; Redis `session:metadata:*`; PG `chats` UUID derivation | record attr `session.id` — deliberately the key the logs pipeline already normalizes into `gen_ai.conversation.id` and reads for session attribution (`normalizeClaudeLogAttributes`, `extractSessionMetadata`); still on `enforce` |
| 7 | `session.turn_id` | accepted by the API but **not stamped** as a telemetry attribute today | record attr `agenthooks.turn.id`; still on `enforce`, where gram's enforcement log now stamps it too (§5.1). A strict improvement: turn ID becomes first-class. |
| 8 | `session.cwd` | not written to CH attrs (used transiently) | record attr `agenthooks.session.cwd`, default **off** (privacy); `enforce` keeps it only if policy engines need it |
| 9 | `session.model` | `gen_ai.response.model` attr | record attr `gen_ai.response.model` directly — the current semconv key and the one gram's pipeline already stores; the flat `model` spelling is Claude-dialect input to `normalizeClaudeLogAttributes`, not something this rail emits |
| 10 | `event.type` + `occurred_at` | dispatch + `time_unix_nano` | log record `Timestamp` = event receive time, `ObservedTimestamp` = emit time; record attr `agenthooks.hook.duration_ms` = dispatch-to-response duration (timing today's point events never had; namespaced under `.hook.` to keep it distinct from tool-execution duration — §4.9) |
| 11 | `data.prompt.text` | risk-scan input; PG `chat_messages` (user role) | **`enforce`** (it is the decision input). Record carries `agenthooks.prompt.length` + `agenthooks.prompt.sha256` by default; full text only at `capture` level |
| 12 | `data.tool_call.id/name` | `gen_ai.tool.call.id`, `gram.tool.name` attrs; PG tool messages | record attrs `gen_ai.tool.call.id`, `gram.tool.name` (same keys) plus the semconv twin `gen_ai.tool.name`, `agenthooks.tool.canonical`, `agenthooks.tool.synthesized`; `enforce` keeps id+name+input for gating events |
| 13 | `data.tool_call.input` | `gen_ai.tool.call.arguments` attr; risk-scan input; PG | **`enforce`** for tool.pre/permission (decision input). Record: size + hash by default; `gen_ai.tool.call.arguments` at `capture` level |
| 14 | `data.tool_call.output` / `error` / `is_interrupt` / `duration_ms` / `status` | `gen_ai.tool.call.result`, `gram.hook.error`, `gram.hook.is_interrupt` attrs; PG tool result messages | record attrs on the tool.post/tool.error record: `gram.hook.error` (same key) with the stable-semconv twin `error.type` (`tool_error`), `agenthooks.tool.duration_ms`; severity ERROR on failure (§4.3); `gen_ai.tool.call.result` at `capture` level. `is_interrupt` has no agenthooks event-model equivalent today, so no record attribute carries it (gram keeps deriving `gram.hook.is_interrupt` for rails that supply it). **Not sent to enforcement at all** (tool.post stops being POSTed). |
| 15 | `data.mcp.*` (server_name, server_identity, url, command, result_json) | `gram.mcp.match`, `gram.mcp.server_url`, `gram.tool_call.source` attrs; shadow-MCP evidence | record attrs `gram.mcp.match`, `gram.mcp.server_url` (same keys, transport redacted exactly like `gram/hooks/relay/redact.go`) + `agenthooks.mcp.server/tool/from_config`; **`enforce`** keeps url/command/identity for shadow-MCP gating |
| 16 | `data.mcp_inventory[]` | CH `shadow_mcp_inventory_urls` (schema.sql:285-297) | **`enforce`** — inventory is shadow-MCP *enforcement evidence*, not just observability. Additionally mirrored (redacted) as attributes on the session.start record for self-contained telemetry. |
| 17 | `data.usage.*` (tokens, cost, loop_count, status) | `gen_ai.usage.*` attrs; usage-metric rows (Cursor) | record attrs `gen_ai.usage.input_tokens`, `.output_tokens`, `.cache_read.input_tokens`, `.cache_creation.input_tokens` (semconv-exact, matching gram's keys) and `gen_ai.usage.cost` (gram extension, no semconv equivalent) on the stop record. Gram keeps synthesizing its usage-metric rows (`agent_hook:metric:usage`) from these attrs server-side, or the library emits a second usage record — open question O5. Not sent to enforcement. |
| 18 | `data.message` (assistant text, role, duration) | PG `chat_messages` (assistant role) | record body carries the text at `capture` level (`gram.log.body` is the established body destination); `agenthooks.message.length` otherwise. Not sent to enforcement. |
| 19 | `data.skill.*` | PG `skill_observations` + Skill telemetry row; content-upload side channel | record attrs `agenthooks.skill.name/source/...`; **but** the skill-capture product pipeline (content-required effects → `uploadSkillContent`) stays on the ingest rail for now (open question O6) |
| 20 | `data.notification` | light hook-event log row | its own log record with `agenthooks.notification.type/message` |
| 21 | `data.mcp_attribution[]` | Redis tuples that un-redact staged Claude OTEL rows (`telemetry_logs_staging`) | unchanged for now — this exists to repair *Claude's native OTEL* stream, orthogonal to this RFC (open question O6) |
| 22 | `raw` (scrubbed provider payload) | stored for debugging only (design.go:276: "The backend does not use this for feature behavior") | **dropped from the wire entirely.** Debugging moves to record attributes; local debugging uses `AGENTHOOKS_LOG`. |
| 23 | `idempotency_key` / `replayed` | Redis dedup claim; `gram.hook.replayed` attr | deterministic trace/span identity (§4.4) enables storage-level dedup of double-fires and retried exports; this rail no longer emits `gram.hook.replayed` (there is no drain path) |
| 24 | Device headers `X-Gram-Device-*` | `gram.hook.device.{os,arch,binary_version,harness,...}` attrs on endpoint spans (conventions.go:359-372) | resource attrs `os.type`, `host.arch`, `service.name`, `service.version`, `agenthooks.harness`, `agenthooks.harness.variant`, `agenthooks.harness.version` |
| 25 | Decision verdict returned to agent | `gram.hook.block_reason` on event row + companion block row; `gram.hook.decision` on metrics; PG `tool_call_blocks` | **Enforcement log only:** gram's enforcement log, written at decision time (§5.1), is the sole record of decisions (it carries `gram.hook.decision` and keeps `gram.hook.block_reason`). Agent-side records are purely observational — they log the event, never the verdict — so dual-emit parity diffs (§6) exclude decision fields. |
| 26 | hash-derived `trace_id`/`span_id` on hook rows | CH `trace_id`, `span_id` columns (trace: `hashToolCallIDToTraceID`; span: random) | log record `TraceId` field populated with **gram's exact existing derivation** (§4.4) so joins and parity diffs work; `SpanId` becomes deterministic per event (an improvement over today's random `generateSpanID`; nothing joins on span_id) |

Items that gram gathers from **other channels** (Claude/Codex native OTEL via
`/rpc/hooks.otel/v1/{logs,metrics}`, Cursor Admin API polling) are out of
scope: they are already decoupled from enforcement and classified separately
by the URN origin vocabulary (`provider_otel`, `provider_api` —
`gram/server/internal/urn/telemetry_event.go:17-33`).

---

## 4. Agent-side design (this library)

### 4.1 Package & API surface

New package `telemetry` (root-adjacent, like `install` and `transcript`),
plus one runner option in the root package:

```go
// package agenthooks
//
// TelemetryRecorder is the runner-facing surface of a telemetry recorder;
// *telemetry.Recorder implements it. Defined in the root package so the root
// package never imports telemetry: only binaries that construct a recorder
// link the OTel SDK dependency tree. Its methods take an internal type
// (internal/hookrecord.Record), so external implementations are not
// possible — the interface is a linkage boundary, not an extension point.
type TelemetryRecorder interface {
	// RecordHook enqueues one hook event on the in-process export pipeline.
	RecordHook(hr *hookrecord.Record) error
	// Shutdown flushes buffered records and stops the pipeline. The hook
	// server calls it on idle shutdown, SIGINT/SIGTERM, and before a
	// version-upgrade exit (§4.7).
	Shutdown(ctx context.Context) error
}

func WithTelemetry(rec TelemetryRecorder) Option

// package telemetry
type Config struct {
	// OTLP/HTTP logs endpoint, e.g. "https://app.getgram.ai/rpc/hooks.otel/v1/logs"
	// or any collector's "/v1/logs". Required.
	Endpoint string
	// Headers added to every export request (auth: e.g. Gram-Key, Gram-Project).
	Headers map[string]string
	// Resource attributes merged over the library defaults
	// (service.name/version, host.name, os.type, agenthooks.provider,
	// gram.event.origin=agenthooks, ...).
	Resource map[string]string

	// Capture selects the content level: CaptureAttributes (default),
	// CaptureContent (prompt text, tool IO, assistant messages on the
	// record, post-Redactor).
	Capture CaptureLevel
	// Redactor rewrites attribute/body values before they enter the
	// export pipeline. The library always applies built-in
	// transport-credential redaction (URLs, commands) first; Redactor
	// runs after it.
	Redactor func(key string, value string) string
	// HonorTraceparent opts into W3C TRACEPARENT env-var parenting (§4.4).
	// Off by default: deterministic trace IDs are what backend joins key on.
	HonorTraceparent bool
}

func New(cfg Config) (*Recorder, error)

// Recorder methods beyond the interface: ForceFlush exports everything
// buffered without stopping the pipeline (tests, checkpoints).
func (r *Recorder) ForceFlush(ctx context.Context) error
```

Consumer usage (what speakeasy-hooks would do):

```go
rec, _ := telemetry.New(telemetry.Config{
	Endpoint: cfg.ServerURL + "/rpc/hooks.otel/v1/logs",
	Headers:  map[string]string{"Gram-Key": key, "Gram-Project": project},
})
r := agenthooks.New(agenthooks.WithTelemetry(rec), ...)
```

Opt-in and fail-open by construction: without the option, nothing changes;
with it, a recorder failure degrades to a logged warning, never an error on
the pipeline (G1).

Delivery needs no entry point of its own: the same Runner construction
serves every argv mode, and the recorder only becomes meaningful in the
mode that lives long enough to ship — the **hook server**
(`mybinary agenthooks server`), the singleton the per-hook
`agenthooks client` forwards events to and auto-spawns on demand (§4.7).
When the server is unreachable the client fails open (exit 0, no output)
without running the pipeline, so no record is produced on that path at
all; a per-hook `run` process constructs the recorder but exits before a
batch ships, and that loss is accepted (N4). Nothing needs external
supervision or provisioning: the server is spawned by hook traffic and
retires itself when idle.

#### Dependencies: the OTel-Go SDK, configured for this shape

The `telemetry` package **uses the OTel-Go libraries internally** and
accepts them as dependencies of this opt-in feature:
`go.opentelemetry.io/otel` (API), `otel/log` + `otel/sdk/log` (the logs
bridge API and SDK pipeline), `otel/sdk/resource`,
`exporters/otlp/otlploghttp` (pure `net/http` — **no gRPC**), and their
transitive `go.opentelemetry.io/proto/otlp` + `google.golang.org/protobuf`.
Rationale:

- **The backend is already designed to ingest OTLP.** Gram's ingest, and
  any generic collector a customer points the recorder at, speak OTLP; the
  SDK guarantees wire-format correctness — Resource semantics, typed
  attribute encoding, severity mapping, trace-context fields on log
  records — instead of this library re-implementing and re-validating that
  surface. Protobuf encoding comes for free.
- **The SDK's shape now matches the process shape.** The SDK's processing
  defaults (in-memory `BatchProcessor` + network exporter) target
  long-lived processes — which is exactly what the hook server is. The
  recorder is the textbook pipeline: `BatchProcessor` feeding the
  **official `otlploghttp` exporter** (gzip), flushed at the server's
  shutdown points (§4.7). No custom exporter, no custom persistence. All
  locked constraints hold: fail-open, zero network on the hook's
  response path.
- **go.mod footprint, honestly.** The module's `go.mod` (essentially
  dependency-free today) gains the OTel SDK and its transitive closure.
  Binaries that never import `telemetry` do not *link* any of it (the Go
  linker only includes imported packages), and with module-graph pruning,
  downstream modules that don't import the package don't vendor or verify
  the SDK's own dependencies — the cost to non-users is go.mod noise, not
  binary size or supply-chain surface. To make that guarantee structural,
  `WithTelemetry` takes the root-package `TelemetryRecorder` interface
  (above) rather than `*telemetry.Recorder`: the **root package itself has
  no telemetry import** (`go list -deps` on the root package shows zero
  OTel packages), so nothing links the SDK until the consumer's own import
  of `telemetry` does. The server's shutdown flush rides the same
  boundary — the runner holds only the interface's `Shutdown` method and
  calls it when a recorder was installed. **Recommendation:** keep
  `telemetry` in the main module for v1 (a nested `telemetry/go.mod`
  submodule would keep the root `go.mod` pristine but adds multi-module
  versioning/tagging friction that isn't warranted yet; revisit if
  consumers object to the requirement entries).

### 4.2 Where the recorder taps in

`OnAny` observers run **before** the handler pipeline
(`agenthooks.go:450-462`, `decideGuarded`), so they cannot see the full
processing timing or a handler failure. Telemetry needs both: the record's
duration must cover dispatch-to-response-encoded, and records must fire even
when handlers error. The recorder therefore taps a new **internal
end-of-processing hook** invoked by `Runner.Run` after `applyPolicy` and
wire encoding (`agenthooks.go:400-432`), by the OpenCode `serve` loop after
`encodeOpenCodeReply` (`serve.go:94-107`), and by the backfill dispatch for
synthesized reporting-only events:

```go
// internal; WithTelemetry installs one.
type afterEvent func(typed any, base *Event, timing recordTiming, herr error)
```

The tap deliberately does **not** read the decision: records are purely
observational — the event plus the hook rail's own health (timing, handler
errors) — and gram's decision-time enforcement log (§5.1) is the sole
record of decisions. Recorded per event: receive time (record timestamp),
dispatch duration, and any handler error (severity + error attrs). The
recorder builds an OTel `log.Record` and `Emit`s it through the package's
`sdk/log` `LoggerProvider`, whose pipeline is a **batch processor feeding
the OTLP/HTTP exporter** (§4.5) — so the call is wrapped in the same panic
guard as observers and bounded: **one in-memory enqueue**, no I/O, no
network, no retries on the critical path. The tap fires identically in
every execution mode that runs the pipeline (server-handled requests,
plain `run`, the OpenCode serve loop); what differs is only
whether the process lives long enough for the batch to ship (§4.7, N4).

`Runner.Decide` (the embedded entry point) does **not** record telemetry:
it has no wire edge and its callers own their own observability.

### 4.3 Log record model

**One OTLP log record per hook event** — a wide event, matching gram's
storage philosophy (`telemetry/README.md`: "store each event as a single,
richly-attributed row"). This is deliberately the same shape
`writeCanonicalTelemetry` synthesizes server-side today, so the agent-emitted
record is a drop-in replacement for the derived row.

Record anatomy:

- **Timestamp** = library receive time (`Event.Time`);
  **ObservedTimestamp** = time the record was emitted to the pipeline.
- **Body** = `"Hook: <native event name>"` — matching the synthetic
  `gram.log.body` the backend writes today (research: `writeCanonicalTelemetry`
  emits e.g. `"Hook: PreToolUse"`), so body-based queries keep working. At
  `CaptureContent`, prompt/assistant-message records carry the text as the
  body instead (the established body destination, item 18 of §3).
- **SeverityText/Number** — `INFO` for ordinary events; `ERROR` for tool
  failures and handler errors — health signals only. Decision outcomes
  never influence severity: records do not see decisions at all, and a
  deny is successful enforcement, not a fault in the hook rail. Gram
  auto-infers severity when unset (`telemetry/README.md`), so this mapping
  only refines it (open question O4 confirms the exact table).
- **TraceId / SpanId** (native OTLP log fields) — §4.4. The logs handler
  already lifts these onto the `trace.id`/`span.id` attributes
  (`otel.go:442-447`).
- **EventName (top-level field) *and* `event.name` attribute** = unified
  kind (`tool.pre`, `agent.stop`, ...), deliberately **dual-emitted**.
  Current OTel conventions moved the event name from the `event.name`
  attribute (now deprecated) to the top-level `EventName` LogRecord field,
  and the SDK/`otlploghttp` emit it there — but gram's OTLP/JSON ingest
  schema has no `eventName` field (`otel_types.go:41-47`; goa drops unknown
  fields) and its URN deriver reads only the *attribute*
  (`event_urn.go:28-31`). So the recorder sets both: the field for semconv
  correctness and generic collectors, the attribute for gram's pipeline.
  (Gram's protobuf decode branch, §5.2 item 4, should also lift proto
  `event_name` into the attribute for future SDK-only producers.)
  Unmapped natives (unified kind `other`) classify as `other.<native>` —
  the native event name lowercased and folded to the URN-friendly
  `[a-z0-9._-]` alphabet (e.g. Claude's `Setup` → `other.setup`) — so they
  do not all collapse into one `urn:telemetry:agent_hook:log:other` type;
  `gram.hook.event` carries the native name verbatim alongside.
- **Attributes** (default capture level) — keys chosen to **reconcile with
  what gram derives today** (§3), i.e. the record arrives pre-normalized.
  `gen_ai.*` keys follow the current registry, which lives in the
  [semantic-conventions-genai repo](https://github.com/open-telemetry/semantic-conventions-genai)
  (all Development stability):

  | Attribute | Source | Existing gram key? |
  |---|---|---|
  | `gram.hook.event` | `Event.NativeName` | yes (conventions.go:340) |
  | `gram.hook.source` | `Event.Provider` (+ variant refinement) | yes (conventions.go:343, CH mat column) |
  | `event.name` | `Event.Kind` | producer convention read by URN deriver (dual-emitted with the EventName field, above) |
  | `gram.event.origin` | fixed `"agenthooks"` | new; the plugin-rail origin marker (see below) |
  | `session.id` | `Session.ID` | normalized → `gen_ai.conversation.id` by `normalizeClaudeLogAttributes` (otel.go:551-553) |
  | `agenthooks.turn.id` | `Session.TurnID` | new (turn ID is dropped today — §3 row 7; no semconv turn concept exists) |
  | `gen_ai.response.model` | `Session.Model` | yes — the current semconv key gram's pipeline already normalizes Claude's flat `model` into (otel.go:554-556); emitted directly, no flat `model` dialect key |
  | `gen_ai.tool.call.id`, `gram.tool.name`, `gen_ai.tool.name` | `ToolCall` | id + gram name: yes. `gen_ai.tool.name` is the semconv twin carried alongside the gram-dialect key for collector/vendor interop. The id value is the provider's native tool-call id — on Claude this is the same `tool_use_id` its native OTEL events carry ("matches the `tool_use_id` passed to hooks", per Claude's monitoring docs), making it the cross-rail join key (§4.9) |
  | `agenthooks.tool.canonical`, `.synthesized` | `ToolCall` | new |
  | `agenthooks.tool.duration_ms` | `ToolPostEvent` duration | new (§3 row 14); agent-side counterpart of the Claude-native `tool_result.duration_ms` |
  | `gram.mcp.match`, `gram.mcp.server_url`, `agenthooks.mcp.*` | `MCPCall` (redacted) | gram keys: yes (conventions.go:377-387). Note the `mcp.*` namespace is now reserved by the MCP semconv — this library never mints `mcp.*` keys |
  | `gram.hook.error`, `agenthooks.handler.error`, `error.type` | `ToolPostEvent` / handler failure | `gram.hook.error`: yes (conventions.go:341). `error.type` is the stable-semconv twin, set only for genuine failures with documented low-cardinality values (`tool_error`, `handler_error`) — never for policy denies, which are successful enforcement. These are the record's *health* attributes; no decision attribute exists (§3 row 25 — the enforcement log owns the verdict) |
  | `gen_ai.usage.input_tokens`, `.output_tokens`, `.cache_read.input_tokens`, `.cache_creation.input_tokens`, `gen_ai.usage.cost`, `agenthooks.loop_count` | `StopEvent.Usage` / `LoopCount` | yes. Token keys match the current semconv registry exactly (which gram's `conventions.go:439-440` already mirrors); `gen_ai.usage.cost` is a gram extension with no semconv equivalent, kept for pipeline compat. If reasoning tokens are ever carried, the semconv key is `gen_ai.usage.reasoning.output_tokens` — not gram's legacy `gen_ai.usage.reasoning_tokens`, which gram should map at ingest |
  | `agenthooks.prompt.length`, `.prompt.sha256` | `PromptEvent` | new (text itself only at `CaptureContent`) |
  | `agenthooks.hook.duration_ms` | dispatch timing (receive → response encoded) | new. **Changed** from `agenthooks.duration_ms`: namespaced under `.hook.` so it cannot be confused with Claude's flat `duration_ms`, which measures tool execution, not hook overhead (§4.9) |
  | `agenthooks.event.backfilled` | `Event.Backfilled` | new. `true` on synthesized reporting-only events (the prompt backfill for Kimi/Cursor print modes): `Raw` is nil and any handler decision was discarded, but the record still forms — same shape, backfill-flagged |
  | `agenthooks.subagent.id/type`, `gen_ai.agent.name` | `AgentInfo` when present | new. `gen_ai.agent.name` carries the subagent type as its semconv twin (the registry's "human-readable name of the agent"); the per-invocation subagent *id* stays custom — `gen_ai.agent.id` means a stable hosted-agent resource, which this is not |
  | `user.email` | consumer-supplied (resolver hook) | read by logs attribution (otel.go:454) |

- **Resource attributes** — `service.name` (consumer binary),
  `service.version`, `host.name`, `os.type`, `host.arch`,
  `agenthooks.provider`, `agenthooks.variant`, `agenthooks.harness.*`, and
  `gram.event.origin=agenthooks` (also stamped per-record for readers that
  only see flattened attributes).

**`gram.event.origin`.** Every record carries
`gram.event.origin = "agenthooks"`, matching the established taxonomy from
prior design discussions (`gram`, `claude`, `codex`, `copilot`,
`agenthooks`). The key lives in gram's dialect namespace — alongside the
existing `gram.event.source` — rather than the bare `event.` namespace an
earlier draft used: `event.` is an existing OTel semconv namespace (its one
member, `event.name`, is deprecated), and the naming guidelines recommend
against minting custom attributes inside semconv namespaces, where a future
`event.origin` definition would collide. Gram's persisted classifier is
coarser — `urn:telemetry:<origin>:<kind>:<type>` with origins
`provider_otel | provider_api | agent_hook | gram_service | unknown`
(`gram/server/internal/urn/telemetry_event.go:10-42`, which deliberately
keeps producer identity in attributes like `gram.hook.source`). Agent-emitted
records classify as **`agent_hook` / `kind=log`** — the same origin and kind
as today's derived hook rows (`deriveHookEventURN`, `event_urn.go:91-93`),
which is exactly what makes downstream readers indifferent to the cutover;
`gram.event.origin=agenthooks` + resource attrs distinguish emitted from
derived rows during the dual-emit window (§6).

### 4.4 Deterministic trace-context identity — matching gram's derivation exactly

Log records still group into session/tool "traces" via the native
`TraceId`/`SpanId` fields — **without any traces ingest path**, because
gram's logs pipeline already lifts those fields onto the `trace.id`/`span.id`
attributes and `telemetry_logs` columns (`otel.go:442-447`, schema).

Gram already derives pseudo trace IDs for hook rows, and **joins depend on
the exact derivation**: the shadow-MCP provenance lookup resolves a recorded
chat tool-call id to its telemetry rows via
`trace_id = hashToolCallIDToTraceID(recorded id)`
(`gram/server/internal/hooks/impl.go:256-284`,
`gram/server/internal/telemetry/repo/mcp_match_lookup.go`, DNO-604). The
derivation (`canonicalTraceID`, `ingest_hooks.go:1190-1203`) is:

1. tool events with a per-call id → `hex(SHA-256(toolCallID)[:16])`;
2. tool events without one → `hex(SHA-256(len(sessionID) + "|" + sessionID +
   "|" + toolName)[:16])` (`syntheticToolCallID`, `impl.go:279-284`);
3. everything else → `hex(SHA-256(sessionID)[:16])`;
4. last resort → random.

**The agent side reproduces this derivation verbatim** (G3):

- `TraceId`: tool events use rule 1 with `ToolCall.ID` — which is the same
  value the relay sends today (native id, or the library's synthesized
  `hook_synth_*` id, `event.go:269-280`), so agent-emitted and
  server-derived rows for the same event get **identical trace IDs**, and
  dual-emit parity diffs can join on `(trace_id, event.name)`. Non-tool
  events use rule 3 (session hash), so a session's prompt/stop/session
  records share one trace — the "session trace" grouping.
- `SpanId`: deterministic per event —
  `hex(SHA-256("agenthooks|event" + the length-prefixed sessionID, turnID,
  nativeName, toolCallID, and receiveTimeNanos)[:8])` (length prefixes keep
  the encoding injective for separator-bearing values, the same reasoning
  as `syntheticToolCallID`). Today gram
  generates *random* span ids (`generateSpanID`, `impl.go:287-291`) and
  nothing joins on them, so determinism is a strict improvement: identical
  double-fires and retried exports collide onto the same
  `(trace_id, span_id)` and dedupe at the storage layer.
- Turn structure is carried by the `agenthooks.turn.id` **attribute** (there
  are no parent spans in a logs-only model). Gram's enforcement log stamps
  the same attribute (§5.1), keeping the two streams joinable at session,
  turn, and tool-call granularity.
- Sessions with an empty session ID (rare, provider bugs) fall back to
  random IDs and are flagged `agenthooks.session.unidentified=true`.

**Mechanism through the SDK.** The OTel logs API populates a record's
trace-context fields from the `context.Context` passed to `Logger.Emit`
(the spec requires implementations to "resolve trace context from the
provided context argument" — `otel/log/DESIGN.md`). The recorder therefore
constructs a synthetic span context carrying the derived IDs and injects it
into the emit context — no tracer, no spans started:

```go
sc := trace.NewSpanContext(trace.SpanContextConfig{
	TraceID: derivedTraceID, // canonicalTraceID reproduction, above
	SpanID:  derivedSpanID,  // deterministic per event, above
})
logger.Emit(trace.ContextWithSpanContext(ctx, sc), rec)
```

The `sdk/log` pipeline stamps `TraceID`/`SpanID` onto the record from that
span context, and they survive unchanged through the batch processor and
`otlploghttp` (§4.5) onto the OTLP wire.

This also keeps the door open for a future traces signal (§8): the same
derivation can later mint real spans with unchanged IDs, so historical log
records and future spans would share identity.

**Ambient `TRACEPARENT` — opt-in, deterministic IDs stay the default.**
Launch environments that already run inside a distributed trace (CI
pipelines, orchestrators) may export a W3C `TRACEPARENT` env var into the
hook process. Honoring it wholesale would silently **break gram's trace-ID
joins** — the shadow-MCP provenance lookup and the session grouping both
compute `hashToolCallIDToTraceID(...)` and expect the record's `TraceId` to
match — so the precedence is deliberately conservative:

1. **Default (`HonorTraceparent` unset): TRACEPARENT is ignored.** Records
   carry the deterministic derivation above. This is the only mode where
   gram's joins work unmodified, so it is the only safe default.
2. **`Config.HonorTraceparent = true`:** if the hook process carries a
   *valid* traceparent (version ≠ `ff`, non-zero IDs), its **trace ID and
   sampled flag** take the record's trace-context fields, parenting the
   record into the ambient trace, and the deterministic trace ID moves to
   the `agenthooks.deterministic_trace_id` attribute so a gram-side join
   can still be recovered by mapping. Malformed or absent values fall back
   to the deterministic derivation.
3. In both modes the **span ID stays deterministic per event** — the
   traceparent's span ID names the *launcher's* span, not this event, and
   replay dedupe relies on per-event span identity.

The opt-in is thus double-gated: the consumer sets the config knob *and*
the launching environment sets the env var. Neither alone changes anything.

### 4.5 Delivery: the in-process batch pipeline

Delivery is the OTel SDK's own long-lived-process pipeline, running inside
the hook server:

- **Pipeline:** `sdk/log.LoggerProvider` → `BatchProcessor` (SDK defaults:
  bounded queue, periodic export interval) → **`otlploghttp`** exporter
  (`WithEndpointURL`, `WithHeaders`, gzip compression). `RecordHook` is an
  in-memory enqueue; batches ship in the background on the processor's
  schedule while the server lives.
- **Buffering & backpressure:** the batch processor's queue is bounded
  (SDK default 2048 records); overflow drops oldest-first inside the SDK.
  No disk artifact exists at any point — content-capture payloads are
  still truncated per record (max 1 MiB, flagged
  `agenthooks.record.truncated=true`) so single records can't dominate
  the queue or the export request.
- **Flush points:** the server calls `Recorder.Shutdown` — which force-
  flushes the queue — on idle shutdown, on SIGINT/SIGTERM, and before a
  version-upgrade exit (§4.7). A per-hook process (plain `run`) exits
  without a flush; those records are lost by design (N4).
- **Retry:** transient export failures (network down, 5xx, 429 with
  `Retry-After`) are retried by the SDK exporter's built-in policy;
  records survive retries in memory only. Endpoint downtime longer than
  the server's lifetime loses the buffered tail — accepted (N4). Gram-
  unreachable durability for *enforcement* traffic remains the relay's
  offline ingest spool, which is orthogonal to this rail.

### 4.6 Capture levels, privacy, redaction

- **Default (`CaptureAttributes`):** no prompt text, no tool input/output
  bodies, no assistant messages, no cwd. Sizes + SHA-256 digests stand in,
  which is enough for volume/shape analytics and joins against
  enforcement-side data (which still sees the full decision inputs).
- **`CaptureContent`:** prompt text, tool input/output
  (`gen_ai.tool.call.arguments` / `.result`), and assistant messages (record
  body) attach after (1) the built-in transport-credential redaction
  (ported from `gram/hooks/relay/redact.go`: URL userinfo/query secrets,
  env assigns, `Authorization`/token-shaped values) and (2) the consumer's
  `Redactor`. Redaction happens before the record enters the export
  pipeline, so unredacted content never leaves the tap.
- Captured content now exists only in the server's memory between emit and
  export — a strictly smaller at-rest surface than rev 2's `0600` spool
  files.
- `Event.Raw` is **never** exported at any level (fidelity stays local; the
  backend explicitly does not use raw for behavior today, design.go:276).

### 4.7 Server lifecycle: where the recorder lives and flushes

The recorder's host process is the **hook server** — the client/server
architecture's long-running singleton, not a telemetry-specific daemon.
Its lifecycle is owned by hook traffic, with telemetry riding along:

- **Placement.** Generated configs install
  `mybinary [flags] agenthooks client --provider=X` as the hook command.
  The client forwards each event over a per-consumer-identity,
  per-project-location unix socket (Windows: named pipe) to
  `mybinary [flags] agenthooks server`; when no server answers, the
  client re-execs itself as one (detached, spawn-lock serialized,
  handed the resolved endpoint via `--socket` so both always agree on
  the rendezvous) and retries. The server constructs the consumer's
  Runner — including `WithTelemetry` — once, and hosts it across
  invocations: handlers, caches, warm HTTP connections, and the
  recorder's batch pipeline all live exactly as long as the server.
- **Singleton.** One server per consumer identity and location
  (executable + pre-sentinel flags such as `--config=...` + the
  client's normalized working directory — one server per project, the
  LSP per-workspace model; no usable cwd degrades to the location-less
  identity), enforced by the endpoint bind plus a `internal/filelock`
  server lock. Distinct configs get distinct servers, so telemetry
  credentials never cross deployments; distinct project directories get
  distinct servers, so cwd-dependent best-effort state (MCP config
  discovery, inventories) is derived per project.
- **Shutdown = flush.** The server shuts down — and calls
  `TelemetryRecorder.Shutdown`, which force-flushes the batch queue —
  on three triggers:
  1. **Idle timeout** (default 10 min without a connection;
     `--idle-timeout=DUR` / `AGENTHOOKS_SERVER_IDLE_TIMEOUT` override):
     the server retires itself between working sessions, flushing on the
     way out, so records never sit in memory unboundedly on an idle
     machine.
  2. **SIGINT/SIGTERM:** graceful drain — stop accepting, finish
     in-flight requests, flush, exit 0.
  3. **Version upgrade:** every client request carries a fingerprint of
     the client's executable (path/size/mtime). On mismatch the server
     answers that request, then drains and exits so the next client's
     respawn runs the new binary — the LSP-style upgrade story. The
     rendezvous (socket + lock) is released *before* the final flush so
     the replacement server can bind immediately.
- **No fallback path.** If the client cannot reach or spawn a server
  within its ~2 s budget, it fails open — exit 0 with no output, a
  warning on the debug log — without running the pipeline: the server is
  a hard dependency of client mode. No record is produced for such an
  event; the gap shows up server-side as missing traffic rather than as
  a silent degraded mode.
- **No external supervision.** Nothing needs provisioning, boot
  registration, or a service manager: the server exists exactly when hook
  traffic exists. (Consumers running under external supervision anyway —
  e.g. a device agent — can keep a server warm by running
  `mybinary [flags] agenthooks server --socket=<endpoint>`; supervisors
  should always pin the rendezvous with an explicit `--socket` rather
  than relying on the supervised server's own working directory, and
  point clients at it with the same flag. That is an optimization, not
  a requirement.)

**Encoding note:** `otlploghttp` emits OTLP **protobuf**
(`application/x-protobuf`) — the Go OTLP/HTTP exporters do not offer a JSON
mode. Gram's endpoint parses OTLP/JSON today, so the SDK decision adds one
small gram-side item: a protobuf decode branch at ingest (§5.2 item 4,
resolving O10).

Latency accounting: the hook's response path gains exactly one in-memory
enqueue — no disk, no network, ever; the network happens on the server's
background export schedule. Deployment accounting: nothing to provision —
the server (and with it, delivery) is spawned by the first hook and
retires itself when idle.

### 4.8 Failure behavior (normative)

- Telemetry is **fail-open, always**: recorder errors, full batch queues,
  dead endpoints, export failures, and server crashes must never change a
  decision, delay a response, or surface as a hook failure. This mirrors
  and strengthens the fail-open discipline of OnAny observers
  (`agenthooks.go`, `decideGuarded`).
- The tap is guarded by the same panic-to-error conversion as observers,
  and its work is one in-memory enqueue — no I/O, no network.
- Misconfiguration (empty or non-http(s) endpoint) fails at
  `telemetry.New` — construction time, in the consumer's control — not at
  event time.
- Memory is bounded by the SDK batch processor's queue cap plus the
  per-record truncation cap; overflow drops records inside the SDK, never
  errors on the pipeline.
- Delivery failures are quiet by design: the SDK retries transient
  failures per request, `Shutdown` bounds the final flush with a timeout
  (a dead endpoint cannot wedge server shutdown), and anything unshipped
  when the flush window closes is dropped. There is no loud failure mode
  on this rail — the enforcement rail owns anything that must not be
  silently lost.
- The server itself is hook infrastructure, not telemetry infrastructure:
  after a server crash, in-flight clients fail open and the next hook
  invocation respawns the server; hook events in that window produce no
  records.

### 4.9 Positioning vs Claude Code's native OTEL events (dual-rail)

Claude Code ships its own OpenTelemetry monitoring rail
([monitoring docs](https://code.claude.com/docs/en/monitoring-usage#events)):
with `CLAUDE_CODE_ENABLE_TELEMETRY=1` and `OTEL_LOGS_EXPORTER=otlp`, the CLI
emits `claude_code.*` log events plus cost/token/session metrics. That rail
**already feeds gram**: `Service.Logs` writes every `claude_code.*` record
into `telemetry_logs` (no event allowlist — `otel.go:354-498`), normalized by
`normalizeClaudeLogAttributes` (`session.id`→`gen_ai.conversation.id`,
`model`→`gen_ai.response.model`, trace-context lifting) and classified
`urn:telemetry:provider_otel:log:<event.name>` (`event_urn.go:70-80`). The
agenthooks record schema is therefore positioned *relative to* an existing
neighbor in the same table, not designed in a vacuum.

**Event-by-event matrix.** "gram today" = what gram does with the Claude
event beyond storing it (everything is stored).

| Claude native event (`claude_code.*`) | gram today | Closest agenthooks record (§4.3) | Overlap & join |
|---|---|---|---|
| `user_prompt` (`prompt_length`; `prompt` text only with `OTEL_LOG_USER_PROMPTS=1`) | stored + schedules the prompt-correlation workflow (`otel.go:511-532`) | `prompt.submitted` (`agenthooks.prompt.length/.sha256`; text at `CaptureContent`) | both see the prompt; join on `session.id`. Content gating differs: Claude's is a per-device env flag, agenthooks' an org-distributed capture level (§4.6) |
| `assistant_response` (`response`, `model`, `request_id`, `message.uuid`) | stored; no feature consumer | `agent.stop` message capture (§3 row 18) | Claude adds `message.uuid` (transcript join) and per-response `request_id`; agenthooks sees final text only where the provider exposes it |
| `tool_result` (`tool_name`, `tool_use_id`, `success`, `duration_ms`, sizes, `decision_source`) | stored | `tool.post` / `tool.error` | strongest overlap. Join: `tool_use_id` = `gen_ai.tool.call.id` (same underlying id, per Claude's docs). Claude adds I/O sizes + decision provenance; agenthooks adds `gram.mcp.*` resolution, `error.type` failure classing, and output content under org-controlled capture |
| `tool_decision` (`decision` accept/reject, `source` config/hook/user_*) | stored | **none** — agenthooks records are observational and carry no verdict; gram's enforcement log (§5.1) owns decision logging | Claude's `source` taxonomy covers config/user decisions that never reach a hook; for hook-made decisions the enforcement log carries the reason/policy detail Claude flattens to accept/reject. Correlate the observational `tool.pre` record via `tool_use_id` = `gen_ai.tool.call.id` |
| `api_request`, `api_error`, `api_refusal`, `api_retries_exhausted`, `api_{request,response}_body` | stored; `api_request` feeds identity extraction (`otel.go:320-343`) and MCP-attribution staging when redacted (`otel.go:503-509`); cost/token **metrics** feed usage rollups (`pending_helpers.go:535-547`) | **none** — hooks never see API internals | Claude-only: per-request cost, tokens, cache, model, request ids, errors/refusals. agenthooks' `gen_ai.usage.*` on `agent.stop` is turn-grained and provider-dependent — a different measurement, not a substitute |
| `mcp_server_connection` (`status`, `transport_type`; `server_name` only with `OTEL_LOG_TOOL_DETAILS=1`) | stored | `session.start` MCP inventory + per-call `gram.mcp.*` | complementary angles: Claude sees connection lifecycle; agenthooks resolves server identity/URL/command per call (shadow-MCP evidence), unredacted by default |
| `skill_activated`, `compaction`, `permission_mode_changed`, `auth`, `internal_error`, `plugin_installed/loaded`, `at_mention`, `feedback_survey` | stored; no feature consumers | `compact.pre/.post` (trigger only); skill via §3 row 19; `permission.request` partially; rest: none | mostly Claude-only product internals |
| `hook_registered`, `hook_execution_start/complete`, `hook_plugin_metrics` | stored | **none — these observe agenthooks itself** | an independent external monitor of the hook rail: registration inventory, per-event `total_duration_ms`, `num_blocking`. Useful for verifying agenthooks health/latency without trusting agenthooks' own telemetry |

Conversely, agenthooks emits kinds Claude's OTEL has no event for:
`session.start`/`session.end` (Claude has only a session *counter* metric),
`subagent.start`/`subagent.stop`, `notification`, `file.edited`, and
`model.request`/`model.response` on providers that expose them — and, the
structural difference, **the same record shape across all six providers**,
where the Claude rail covers exactly one.

**What each rail uniquely provides:**

- *Claude native*: model-side economics and internals (per-request cost,
  tokens, cache hits, request ids, errors/refusals/retries), transcript
  identity (`message.uuid`, `prompt.id`), authenticated user identity
  attributes (`user.email`, `organization.id` — which gram's attribution
  already consumes), decisions made by config rules or the human at the
  permission prompt (paths where no hook fires), and meta-telemetry about
  hook execution itself.
- *agenthooks*: provider-uniform coverage, MCP transport resolution and
  shadow-MCP evidence, turn identity (`agenthooks.turn.id`), deterministic
  trace-context aligned with gram's joins (§4.4), hook-rail health (dispatch
  duration, handler errors), and org-controlled (rather than per-device
  env-flag) content capture with built-in redaction. Full decision detail
  (deny/ask/rewrite + reason + policy source, not just accept/reject) lives
  on gram's enforcement log (§5.1), not on the agent-side records.

**Naming positioning.** The agenthooks record deliberately uses gram's
derived-row dialect (`gram.hook.*`, `gen_ai.*`, §4.3) rather than Claude's
flat producer keys (`tool_name`, `duration_ms`, `success`): in
`telemetry_logs` the flat keys *are* the provider-OTEL dialect, and reusing
them would blur the rail boundary readers scope on. Alignment happens at the
value level instead: shared `session.id`, shared tool-call id, and the same
`gen_ai.*` targets post-normalization. Two cheap alignments were made in
§4.3 as a result of this comparison: `agenthooks.hook.duration_ms` (renamed
from `agenthooks.duration_ms`, so hook overhead can't be misread as Claude's
tool-execution `duration_ms`) and an explicit `agenthooks.tool.duration_ms`
mirroring `tool_result.duration_ms`. One optional gram-side alignment:
extend `normalizeClaudeLogAttributes` to map `tool_use_id` →
`gen_ai.tool.call.id` so the cross-rail join key lands in the same
materialized column for both rails (§5.2).

**Dual-rail overlap (both rails pointed at gram).** Recommendation:
**coexistence with origin scoping — no ingest-time dedup.**

- Separation is already structural: Claude rows classify
  `provider_otel:log:*` (writer URN `claude-code:otel:logs`), agenthooks
  rows `agent_hook:log:*` (§5.2), and the URN is materialized for
  filtering. Note `gram.event.origin=agenthooks` marks only the agenthooks
  rows — Claude's CLI emits no such attribute — so **readers should scope on
  the URN origin**, with `gram.event.origin` as the finer producer marker
  within `agent_hook`.
- The rails are complementary, not duplicates: per the matrix, only
  `tool_result`/`tool.post` and `tool_decision`/`tool.pre` overlap
  semantically, and their attribute sets differ enough that dedup would be
  lossy. Correlate instead: `tool_use_id` = `gen_ai.tool.call.id` joins the
  two views of one tool call.
- The reader rule that follows: any metric counting "tool calls",
  "sessions", or "denies" must draw from **one rail per metric** (scoped by
  URN origin) — the same discipline the dual-emit window already requires
  (O2). Cost/token metrics have no conflict: only the Claude rail carries
  them, and O5's server-side usage synthesis must stay scoped to providers
  without a native usage rail so it never double-counts
  `provider_otel:metric:usage` rows.
- One decision can legitimately produce **two records**: gram's enforcement
  log row (§5.1) and Claude's `tool_decision` (`source="hook"`). Precedence
  for readers: the enforcement log is authoritative for *what policy
  decided*; `tool_decision` is provider-side provenance, and the only
  record for config/user decisions hooks never see. The agenthooks event
  record deliberately carries no verdict — join it in by trace ID or
  tool-call id when the observational context around a decision (timing,
  MCP resolution, payload shape) is needed. This is
  documentation/MV-guidance, not a pipeline change (O11).

---

## 5. Backend-side design (gram)

Going logs-first **eliminates** the largest backend items from the traces
draft: no new `/rpc/hooks.otel/v1/traces` endpoint, no span storage
decision, no `TelemetryEventKind` extension, no span→row mapping, no new URN
kinds, no trace-view query work. What remains is small and listed below.

### 5.1 Enforcement logging (unchanged from rev 1 — still required)

Enforcement gets a first-class, self-contained record written **at decision
time** by the decision sites themselves — `evaluateCanonicalHook`
(`ingest_hooks.go:410`), the legacy PreToolUse/UserPromptSubmit handlers,
and the shadow-MCP evaluator — instead of decision fields being stamped
onto observability rows derived from the request. This log is the **sole
record of decisions across both rails**: agent-side telemetry records are
purely observational and never carry the verdict (§4.2, §4.3), so nothing
else re-records what policy decided.

- **ClickHouse:** one `telemetry_logs` row per *evaluated* gating event —
  allow and deny both, and allow-row logging is **required**, not merely
  cheap: with no agent-side decision attributes, an unlogged allow would
  be recorded nowhere (it also keeps deny-rate queries self-contained).
  Classified
  `urn:telemetry:gram_service:log:hook.decision`. Attributes:
  `gram.hook.decision` (allow/deny/warn — key exists,
  `conventions.go:339`), `gram.hook.block_reason`, `gram.hook.event`,
  `gram.hook.source`, `gram.tool.name`, `gram.mcp.match`,
  `gram.risk.policy_id` / rule id / engine, org/project/user, scan latency,
  `gen_ai.conversation.id`, **`agenthooks.turn.id`** (new: stamp the turn
  ID, which the API already accepts but drops — §3 row 7), and
  **`trace_id`/`span_id` via the existing `canonicalTraceID` derivation** —
  which now provably matches the agent-side records (§4.4), so a deny and
  the agent's view of that deny land in the same trace.
- **Postgres:** `tool_call_blocks` (denies) and `risk_policy_challenges`
  (warn acks) stay as-is — they are already decision-time, decision-owned
  records.
- **Metrics:** `hooks.event.duration` with `gram.hook.decision` /
  `gram.hook.risk_scanned` stays as-is (`internal/hooks/metrics.go`).

This makes the enforcement audit trail independent of whether any
observability derivation happens afterward — the precondition for deleting
that derivation in the cutover.

### 5.2 Logs ingest: what exists, what changes

**Exists and is reused as-is** (the point of logs-first):

- Endpoint + auth: `POST /rpc/hooks.otel/v1/logs`, `Gram-Key`/`Gram-Project`
  (`design/hooks/design.go:440-462`).
- Per-record pipeline: attribute normalization
  (`normalizeClaudeLogAttributes`), `gram.event.source=hook` stamping,
  project/org stamping, session attribution with Redis caching, body/scope/
  trace-context lifting, `LogBulk` → `telemetry_logs`
  (`otel.go:354-498`).
- Storage, materialized columns, TTL, downstream MVs, URN classification
  (`telemetry/README.md`, `event_urn.go`).

**Small changes required:**

1. **Recognize agenthooks payloads.** `Service.Logs` currently branches
   Claude vs Codex (`isCodexLogsPayload`, `otel.go:49-54`) and applies
   Claude-specific surface resolution. Add a branch keyed on the resource
   attr `gram.event.origin=agenthooks` (or `service.name`): skip the
   Claude-surface machinery (the records carry `gram.hook.source` already,
   §4.3), reuse the generic normalization + attribution + bulk write, and
   stamp a new writer URN constant (e.g. `agenthooks:otel:logs`) alongside
   the existing `claudeOTELLogsURN` (`otel.go:22`).
2. **URN mapping.** Extend `deriveHookEventURN`
   (`telemetry/event_urn.go:68-94`): rows with the `agenthooks:otel:logs`
   writer URN classify as `urn:telemetry:agent_hook:log:<event.name>` —
   the **same** origin/kind/type today's derived hook rows get, which is
   what keeps existing readers working (§4.3).
3. **Turn ID persistence.** Accept and persist `agenthooks.turn.id` (a
   conventions.go key + optionally a materialized column) — new capability,
   small.
4. **Protobuf decode branch (resolves O10).** The SDK's `otlploghttp`
   exporter emits OTLP **protobuf**; gram's endpoint parses OTLP/JSON. The
   hooks request decoder (`newHooksRequestDecoder`,
   `claude_hooks.go:62-148`) already buffers the whole body, decompresses
   gzip, and branches per content type — add one branch: on
   `application/x-protobuf` for the OTLP paths, `proto.Unmarshal` into
   `go.opentelemetry.io/proto/otlp` `ExportLogsServiceRequest`, then
   **protojson-transcode to canonical OTLP/JSON** and hand it to the
   existing stock JSON decoder. One enrichment while transcoding: **lift
   the proto `event_name` field into an `event.name` attribute** when the
   attribute is absent — gram's JSON schema has no `eventName` field and
   the URN deriver reads the attribute, so this future-proofs the endpoint
   for standard SDK producers that only set the top-level field (this
   library dual-emits both, §4.3, so it works either way). The rest of the
   downstream pipeline is untouched — the decode tests already guard
   exactly the canonical shape protojson produces (`otel_decode_test.go`:
   collector-style stringified ints), and `google.golang.org/protobuf` is
   already in the server module. Small, contained, and independently
   useful (any standard OTLP client can then target the endpoint).
5. **(Optional, one line) Cross-rail tool-call join.** Extend
   `normalizeClaudeLogAttributes` (`otel.go:550-563`) to also map Claude's
   `tool_use_id` → `gen_ai.tool.call.id`, so the Claude-native and
   agenthooks views of the same tool call share a materialized join key
   (§4.9). Strictly additive; nothing depends on it.
6. **Re-source product features.** ClickHouse-reading features need no
   change (same table, same keys). The **Postgres** products fed by ingest
   handlers today — session capture (`chats`/`chat_messages`) and usage
   rollup writes — need a consumer of agenthooks-emitted rows (or of the
   OTLP payload at ingest time, mirroring how `persistCanonicalConversationEvent`
   consumes ingest payloads) if they are to survive the cutover; session
   capture additionally requires `CaptureContent` (open question O1).

### 5.3 Slimmed enforcement request

After cutover, the enforcement rail carries **only gating events** and only
decision-relevant fields. Per the enforcement analysis
(`evaluateCanonicalHook`, `risk.Scanner.ScanForEnforcement` at
`internal/risk/scanner.go:234`, shadow-MCP validator), the decision needs:
identity, event type, tool identity, the scanned text, and MCP evidence.

Proposed `hook.enforce.v1` (successor to `hook.ingest.v1` on the same
endpoint or a sibling `/rpc/hooks.enforce`):

| Block | Keeps | Drops (moves to telemetry) |
|---|---|---|
| `source` | `adapter`, `adapter_version`, `hostname`, `user_email` | `raw_event_name` (optional keep for decision logs) |
| `session` | `id`, `turn_id`, `model`¹, `cwd`¹ | — |
| `event` | `type` (gating types only: `prompt.submitted`, `tool.requested`), `occurred_at` | all non-gating types — **no longer POSTed at all** |
| `data.prompt` | `text` (scan input) | — |
| `data.tool_call` | `id`, `name`, `input` (scan input), `permission_type` | `output`, `error`, `duration_ms`, `status`, `is_interrupt` |
| `data.mcp` | `server_name`, `server_identity`, `url`, `command` | `result_json` |
| `data.mcp_inventory` | kept (shadow-MCP enforcement evidence, sent on session start / config change)² | — |
| `data.usage`, `data.message`, `data.notification` | — | dropped entirely |
| `data.skill`, `data.mcp_attribution` | see open questions O6 | — |
| `raw` | — | dropped entirely |

¹ `model`/`cwd` retained only if policy predicates use them; audit before
final schema.
² Inventory delivery needs a gating carrier once `session.started` stops
being an ingest event; either keep `session.started` on the enforcement rail
as a non-gating-but-synchronous event (it already piggybacks org-settings
`effects` — see O3), or attach inventory to the first gating event of a
session.

The `IngestHookResult` response shape (decision/reason/message/effects) is
unchanged — the `effects` channel (org fail-open settings, skill content
requests) continues to ride the gating exchanges.

**Client-side effect:** `buildEnvelope` + `Relay.deliver` stop constructing
and sending envelopes for non-gating events entirely; observe handlers
(`onToolPost`, `onStop`, `onObserve` in `gram/hooks/relay/runner.go`) reduce
to telemetry-only. This removes a synchronous control-plane HTTP call from
every observe event — the largest latency win of the whole change (§1,
research finding 3: "in-process `deliver` is still a blocking HTTP call
unless the provider/`--async` detached the worker").

The relay's existing enforcement spool (`spool.go`) shrinks with the
payload; the drain replay path is unchanged for gating events.

---

## 6. Cutover plan

Ordered; each step ships independently and is verifiable before the next.
Dual-emit is the compatibility window; the end state is a hard cutover.
Re-sequenced from rev 1: with no new ingest path to build, the gram-side
prerequisite phase is much smaller, and parity verification gets easier
because both streams land in the **same table with the same trace-ID
derivation**.

**Phase 0 — library (this repo)**
1. Land the client/server architecture (`agenthooks client`/`server`
   verbs, `internal/ipc`, install-package command-line change), the
   `telemetry` package with its in-process batch pipeline, `WithTelemetry`
   + the recorder `Shutdown` flush points, and the internal
   end-of-processing tap. Opt-in; no consumer change yet. Update
   DESIGN.md to reflect both. Fixture-test record determinism per
   provider (golden corpus already exists in `agenthookstest`), including
   a cross-check that the emitted `TraceId` matches gram's
   `canonicalTraceID` for the same inputs.

**Phase 1 — gram, small and additive**
2. Add the agenthooks branch to `Service.Logs` + the `agenthooks:otel:logs`
   writer URN + `deriveHookEventURN` mapping + `agenthooks.turn.id`
   persistence + the protobuf decode branch in `newHooksRequestDecoder`
   (§5.2 items 1–4). No new endpoint, no schema change beyond an attribute
   key (and optional materialized column).
3. Make enforcement logging self-contained (§5.1): decision-time rows with
   `gram.hook.decision`, turn ID, `canonicalTraceID`-derived trace context.
   From this point the enforcement audit trail no longer depends on
   `writeCanonicalTelemetry`.

**Phase 2 — consumer, dual emit**
4. speakeasy-hooks enables `WithTelemetry` pointed at
   `/rpc/hooks.otel/v1/logs` with `Gram-Key` headers (config from the
   existing `speakeasy.json`/env/auth-cache stack). Bump the pinned
   `hooksBinaryVersion` in `server/internal/plugins/hooks_bootstrap.go` so
   org plugins roll forward. **Both** streams now flow into
   `telemetry_logs`: request-derived rows (URN `agent_hook:log:*`, no
   `gram.event.origin`) and agent-emitted rows (same URN class,
   `gram.event.origin=agenthooks`).
5. **Parity verification** (the compatibility window's exit criterion):
   same-table diffs joined on `(trace_id, event.name / gram.hook.event,
   gen_ai.conversation.id)` — possible precisely because the agent side
   reproduces `canonicalTraceID` (§4.4). Dashboards for the inventory items
   in §3. Fix gaps while both streams exist.

**Phase 3 — gram readers switch**
6. ClickHouse readers mostly need nothing (same table/keys); audit queries
   that would double-count during dual-emit and scope them by
   `gram.event.origin` where needed. Stand up the span-free replacements for the
   Postgres products (session capture / usage rollups fed from the emitted
   stream, per §5.2 item 6, if approved under O1). Request-derived rows are
   marked deprecated to catch stragglers.

**Phase 4 — slim the wire (the cutover)**
7. Ship `hook.enforce.v1` server support (accepting v1 *and* the slim
   schema; `schema_version` already exists for exactly this).
8. Ship the consumer binary that (a) stops POSTing non-gating events,
   (b) sends the slim payload for gating events. Bootstrap pin bump again.
   Old binaries keep working against the v1 acceptor during the fleet
   rollout window (bootstrap-pinned fleets converge on next session).
9. Remove `writeCanonicalTelemetry`'s observability derivation (and the
   legacy per-provider endpoints' equivalents) once v1 traffic drains to
   zero; `Ingest` keeps only: evaluate → enforcement log → respond.

**Phase 5 — cleanup**
10. Drop dead payload fields from the Goa design, regenerate SDKs, delete
    the parity dashboards, remove the dual-write flags.

**Verifying nothing observability-critical is lost:** the §3 inventory is
the checklist; step 5's keyed same-table diff is the mechanism; the ordering
guarantees there is never a moment where an inventory item has no producer
(derivation is deleted only after record parity is proven *and* readers have
switched).

---

## 7. Open questions for review

- **O1 — Content capture & session capture.** PG `chats`/`chat_messages`
  (the session-transcript product) currently gets prompt and assistant text
  from enforcement requests. Keeping it requires running the fleet at
  `CaptureContent` (with redaction) so records carry text, plus a gram-side
  consumer that writes PG from the emitted stream (§5.2 item 6). Are we
  comfortable putting conversation content on the telemetry rail (held
  in server memory until export), or should session capture be
  re-scoped/dropped at cutover?
- **O2 — Record volume & dual-emit scoping.** One emitted record per hook
  event is roughly today's derived-row volume, but the dual-emit window
  doubles hook-row volume in `telemetry_logs`, and both streams share the
  URN class `agent_hook:log:*` by design. Is `gram.event.origin` scoping enough
  for every reader during the window (metrics MVs aggregate without origin
  filters today), or do some MVs need a temporary filter / do we sample the
  emitted stream until Phase 4?
- **O3 — Org-settings distribution.** Cached fail-open posture refreshes via
  ingest `effects` on *any* successful exchange
  (`gram/hooks/relay/orgsettings.go`). With only gating events POSTing,
  refresh frequency drops on observe-heavy sessions. Is gating-event
  frequency sufficient, or does the posture need a dedicated (rare) refresh
  call?
- **O4 — Severity mapping.** Proposed: INFO default; ERROR for tool
  failure and handler error — health signals only, since records carry no
  decisions (§4.3) and a deny is successful enforcement, not a fault.
  Gram auto-infers severity when unset — should the library set severity at
  all, or leave inference to the backend so agent- and server-derived rows
  can't disagree during dual-emit?
- **O5 — Usage-metric rows.** Today Cursor stop events also yield synthetic
  usage rows classified `agent_hook:metric:usage`
  (`event_urn.go:85-88`). Post-cutover: should gram synthesize them
  server-side from the emitted stop record's `gen_ai.usage.*` attrs (keeps
  the library signal-pure), or should the library emit a second, dedicated
  usage record per stop? Leaning: server-side synthesis at ingest.
- **O6 — Skill capture & `mcp_attribution`.** `skill.activated` +
  content-required effects + `uploadSkillContent` is a product pipeline
  with a synchronous effects handshake; `mcp_attribution` repairs Claude's
  redacted native-OTEL rows via Redis staging. Keep both on the (gating)
  enforcement rail, carry them as record attributes read at logs-ingest
  time, or redesign as exporter-adjacent side channels? Leaning: keep on
  the enforcement rail initially; revisit after cutover.
- **O7 — OpenCode serve mode: resolved by rev 3.** The serve daemon is
  long-lived, so its in-process recorder batches and ships exactly like
  the hook server's — one uniform pipeline everywhere. (The serve process
  should gain the same flush-on-signal discipline as the server; today it
  flushes when the consumer's process exits cleanly.)
- **O8 — Delivery entry point: resolved into the design (rev 3).** Rev 2
  answered this with a supervised `agenthooks exporter` daemon; rev 3
  removes the dedicated entry point entirely — the **hook server is the
  delivery point**, spawned by hook traffic and retiring on idle, with
  flush-on-shutdown replacing continuous spool polling. Idle machines
  hold no unshipped backlog because the idle shutdown flushes before
  exit. No decision left open.
- **O9 — Enforcement-allow row volume.** §5.1 logs allows as well as
  denies. At fleet scale this multiplies gating-event rows — but sampling
  allows (keep all denies/warns) is now lossy in a way earlier drafts
  weren't: the enforcement log is the sole record of decisions (§5.1,
  agent-side records carry no verdict), so a sampled-out allow is recorded
  nowhere. Needs a gram capacity check before any sampling is considered.
- **O10 — OTLP encoding: resolved into the design.** With the OTel-Go SDK
  decision (§4.1 Dependencies), the exporter ships via `otlploghttp`,
  which emits OTLP **protobuf** — so gram's logs endpoint gains a small
  protobuf→protojson transcode branch in its request decoder (§5.2 item 4,
  Phase 1 of the cutover). Gram's existing JSON parsing is untouched and
  keeps serving Claude Code and collector traffic; generic collectors work
  out of the box since protobuf is the OTLP/HTTP default. No decision left
  open.
- **O11 — Dual-rail reader guidance.** §4.9 recommends coexistence with
  URN-origin scoping and one-rail-per-metric, but today's metrics MVs
  aggregate `telemetry_logs` without origin filters. Do we (a) audit and
  add origin predicates to the MVs that count tool calls/sessions/denies,
  (b) publish the two-record decision precedence (enforcement log
  authoritative; Claude `tool_decision` as provider-side provenance, the
  agenthooks record carrying no verdict at all) as a documented convention,
  and (c) constrain O5's usage-row synthesis to providers with no native
  usage rail so it can't double-count `provider_otel:metric:usage`? All
  three look necessary before recommending customers enable both rails.

---

## 8. Alternatives considered

- **Traces-first (rev 1 of this RFC)** — one span per hook event with
  synthesized turn/session parents, shipped to a **new**
  `/rpc/hooks.otel/v1/traces` ingest. Rejected for v1: gram has no traces
  ingest or span storage today, so it required a new endpoint, a
  `TelemetryEventKind` extension, span→row mapping, and reader migration —
  all eliminated by reusing the built-out logs pipeline. **Deliberately not
  precluded:** the deterministic trace/span identity in §4.4 is chosen so a
  future traces signal mints spans with the *same* IDs, letting historical
  log records and future spans share identity; the server's in-process SDK
  pipeline would gain a second OTLP signal path, not a rewrite.
- **Hand-rolled OTLP structs instead of the OTel-Go SDK** — considered
  (briefly the draft position) to keep `go.mod` dependency-free: hand-write
  the OTLP/JSON mapping and skip `sdk/log`/`otlploghttp` entirely.
  Rejected: the backend is already designed around OTLP ingestion, and the
  SDK guarantees wire-format correctness (Resource/attribute/severity/
  trace-context semantics) plus free protobuf support, where a hand-rolled
  encoder is a parallel implementation to keep conformant forever. Under
  the client/server model the SDK's long-lived-process defaults fit the
  host process exactly (§4.1 Dependencies, §4.5, §4.7).
- **Disk spool + externally supervised exporter daemon (rev 2 of this
  RFC, built and reviewed as PR #8)** — the hook process appended
  protojson records to a capped NDJSON spool; a separate
  `agenthooks exporter` daemon (supervised by a service manager or device
  agent) tailed, checkpointed, and shipped it. Superseded by the
  client/server architecture: once the full pipeline runs in a
  long-running server anyway, the spool's reason to exist — surviving
  process-per-event — disappears, and with it the exporter daemon, its
  supervision requirement, the checkpoint/tail/sweep machinery, and the
  at-rest content surface. The durability trade (disk → server memory,
  flush-on-shutdown) was accepted deliberately: telemetry is best-effort,
  and the enforcement rail carries everything that must survive crashes.
- **Metrics as the primary signal** — rejected: pre-aggregation discards
  the wide-event dimensionality gram's model is built on; gram already
  derives metrics from rows server-side.
- **Ship telemetry synchronously with a short timeout** — rejected: any
  network on the critical path eventually bites (provider timeouts are
  unforgiving, §6 of DESIGN.md); spool-and-drain is already proven in this
  ecosystem (relay drain).
- **Debounced detached shipper spawned from hook activity (rev 2 of this
  RFC, briefly implemented)** — each hook event could re-exec the binary as
  a short-lived detached shipper behind a 30 s debounce, the MCP-warm
  pattern. Replaced by the supervised exporter: hooks doing process
  management meant fork/exec on the hook path (however debounced),
  credentials passed over stdin between processes, ad-hoc run budgets
  instead of a real lifecycle, and idle machines whose spool tails only
  shipped on the *next* session. A daemon under external supervision is
  operationally boring in all four dimensions, and the argv contract
  already had a natural place for the verb.
- **Keep telemetry in the consumer binary only (status quo of DESIGN.md
  §11)** — rejected by prior decision: every consumer would rebuild the
  spool/exporter/record-model machinery; the library owns the wire and the
  event taxonomy, so it is the right owner of the record schema.
- **New correlation ID** — rejected by prior decision: session ID + turn ID
  already exist on both rails (`SessionInfo`, `HookIngestSession`) and
  reproducing gram's existing `canonicalTraceID` derivation makes them
  sufficient (and keeps the shadow-MCP provenance join intact).
