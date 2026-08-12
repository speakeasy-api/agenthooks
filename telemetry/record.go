package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
)

// maxContentBytes bounds each captured content value (prompt text, tool IO,
// assistant message). Longer values are truncated and the record flagged
// agenthooks.record.truncated=true, keeping single records under the spool's
// per-record cap.
const maxContentBytes = 256 << 10

// buildRecord assembles the OTel log record for one hook event — a wide
// event whose attribute keys reconcile with what gram's pipeline derives
// from hook payloads today (RFC §4.3) — plus the synthetic span context
// carrying the deterministic trace/span identity (§4.4).
func (r *Recorder) buildRecord(hr *hookrecord.Record) (log.Record, trace.SpanContext) {
	b := &recordBuilder{redactor: r.cfg.Redactor}

	var rec log.Record
	rec.SetTimestamp(hr.Time)
	rec.SetEventName(eventName(hr))
	rec.SetSeverity(severityOf(hr))
	rec.SetSeverityText(severityText(severityOf(hr)))

	// Identity and classification. The event name is deliberately emitted
	// twice: as the top-level EventName field (current OTel semconv — the
	// event.name attribute is deprecated in its favor) and as the
	// event.name attribute, because gram's OTLP/JSON ingest schema has no
	// eventName field and its URN deriver reads only the attribute.
	b.str("gram.hook.event", hr.NativeName)
	b.str("gram.hook.source", hr.Provider)
	b.str("event.name", eventName(hr))
	b.str("gram.event.origin", "agenthooks")
	b.str("agenthooks.provider", hr.Provider)
	b.str("agenthooks.variant", hr.Variant)
	b.str("session.id", hr.SessionID)
	b.str("agenthooks.turn.id", hr.TurnID)
	b.str("gen_ai.response.model", hr.Model)
	b.str("user.email", hr.UserEmail)
	if hr.Backfilled {
		b.flag("agenthooks.event.backfilled")
	}
	b.str("agenthooks.subagent.id", hr.SubagentID)
	b.str("agenthooks.subagent.type", hr.SubagentType)
	// Semconv twin: gen_ai.agent.name is the standard home for a
	// human-readable agent name; the subagent type is the closest fit.
	b.str("gen_ai.agent.name", hr.SubagentType)
	b.float("agenthooks.hook.duration_ms", hr.HookDurationMS)

	// Health signals only — the record is observational and never carries
	// the enforcement decision (the enforcement backend's decision-time log
	// is the sole record of decisions, RFC §5.1).
	b.str("agenthooks.handler.error", hr.HandlerErr)
	// error.type (stable semconv) classifies genuine failures with a
	// documented low-cardinality value; policy denies are successful
	// enforcement, not errors, and do not set it.
	b.str("error.type", errorType(hr))

	if t := hr.Tool; t != nil {
		b.str("gen_ai.tool.call.id", t.ID)
		b.str("gram.tool.name", t.Name)
		// Semconv twin of the gram-dialect key, for collector/vendor
		// interop.
		b.str("gen_ai.tool.name", t.Name)
		b.str("agenthooks.tool.canonical", t.Canonical)
		if t.Synthesized {
			b.flag("agenthooks.tool.synthesized")
		}
		if t.DurationMS != nil {
			b.float("agenthooks.tool.duration_ms", *t.DurationMS)
		}
		b.str("gram.hook.error", t.Error)
		if len(t.Input) > 0 {
			if r.cfg.Capture >= CaptureContent {
				b.content("gen_ai.tool.call.arguments", string(t.Input))
			} else {
				b.digest("agenthooks.tool.input", t.Input)
			}
		}
		if len(t.Output) > 0 {
			if r.cfg.Capture >= CaptureContent {
				b.content("gen_ai.tool.call.result", string(t.Output))
			} else {
				b.digest("agenthooks.tool.output", t.Output)
			}
		}
		if m := t.MCP; m != nil {
			b.str("gram.mcp.match", mcpMatch(m))
			b.str("gram.mcp.server_url", redactURL(m.URL))
			b.str("agenthooks.mcp.server", m.Server)
			b.str("agenthooks.mcp.tool", m.Tool)
			b.str("agenthooks.mcp.command", redactCommand(m.Command))
			if m.FromConfig {
				b.flag("agenthooks.mcp.from_config")
			}
		}
	}

	if hr.Kind == "prompt.submitted" {
		// Sizes and digests stand in for text at the default capture level:
		// enough for volume/shape analytics and joins against the
		// enforcement side, which still sees the full decision inputs.
		b.digest("agenthooks.prompt", []byte(hr.Prompt))
	}
	if hr.FinalMessage != "" {
		b.int("agenthooks.message.length", len(hr.FinalMessage))
	}
	if hr.LoopCount > 0 {
		b.int("agenthooks.loop_count", hr.LoopCount)
	}
	if u := hr.Usage; u != nil {
		b.intp("gen_ai.usage.input_tokens", u.InputTokens)
		b.intp("gen_ai.usage.output_tokens", u.OutputTokens)
		b.intp("gen_ai.usage.cache_read.input_tokens", u.CacheReadTokens)
		b.intp("gen_ai.usage.cache_creation.input_tokens", u.CacheWriteTokens)
		if u.Cost != nil {
			b.float("gen_ai.usage.cost", *u.Cost)
		}
	}
	b.str("agenthooks.notification.message", hr.Notification)
	b.str("agenthooks.session.source", hr.SessionSource)
	b.str("agenthooks.session.end_reason", hr.SessionEndReason)
	b.str("agenthooks.compact.trigger", hr.CompactTrigger)
	if r.cfg.Capture >= CaptureContent {
		// Paths are location-revealing, so they ride only at the elevated
		// capture level — same posture as content.
		b.str("agenthooks.session.cwd", hr.CWD)
		b.str("agenthooks.file.path", hr.FilePath)
	}

	// Body: "Hook: <native event name>", matching the synthetic
	// gram.log.body the backend writes for derived rows today. At
	// CaptureContent, prompt and final-message records carry the text as
	// the body instead — the established body destination.
	body := b.redact("body", "Hook: "+nativeOrKind(hr))
	if r.cfg.Capture >= CaptureContent {
		switch {
		case hr.Kind == "prompt.submitted" && hr.Prompt != "":
			body = b.contentValue("body", hr.Prompt)
		case hr.FinalMessage != "":
			body = b.contentValue("body", hr.FinalMessage)
		}
	}
	rec.SetBody(attribute.StringValue(body))

	// Deterministic identity, injected via a synthetic span context on the
	// emit context — no tracer, no spans started (§4.4). With
	// HonorTraceparent and an ambient TRACEPARENT, the launcher's trace ID
	// takes the trace-context field and the deterministic ID rides as an
	// attribute so hash-derived joins keep working; the span ID stays
	// deterministic per event in both cases (replay dedupe relies on it).
	toolCallID, toolName := "", ""
	if hr.Tool != nil {
		toolCallID, toolName = hr.Tool.ID, hr.Tool.Name
	}
	traceID, derived := deriveTraceID(hr.Tool != nil, toolCallID, hr.SessionID, toolName)
	if !derived {
		b.flag("agenthooks.session.unidentified")
	}
	scc := trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  deriveSpanID(hr.SessionID, hr.TurnID, hr.NativeName, toolCallID, hr.Time),
	}
	if r.ambientOK {
		scc.TraceID = r.ambientTrace
		scc.TraceFlags = r.ambientFlags
		if derived {
			b.str("agenthooks.deterministic_trace_id", traceID.String())
		}
	}
	if b.truncated {
		b.flag("agenthooks.record.truncated")
	}
	rec.AddAttributes(b.attrs...)
	return rec, trace.NewSpanContext(scc)
}

// mcpMatch mirrors gram's gram.mcp.match semantics: the server-level
// identifier the matcher resolved — an HTTP/SSE URL, a stdio command, or (as
// fallback) the mcp__<server>__ prefix from the tool name — transport
// redacted before it leaves the machine.
func mcpMatch(m *hookrecord.MCP) string {
	switch {
	case m.URL != "":
		return redactURL(m.URL)
	case m.Command != "":
		return redactCommand(m.Command)
	case m.Server != "":
		return "mcp__" + m.Server + "__"
	}
	return ""
}

func nativeOrKind(hr *hookrecord.Record) string {
	if hr.NativeName != "" {
		return hr.NativeName
	}
	return hr.Kind
}

// eventName is the record's unified event identity — the top-level
// EventName field and the event.name attribute, which gram's URN deriver
// turns into urn:telemetry:agent_hook:log:<event.name>. Mapped kinds pass
// through as-is (tool.pre, agent.stop, ...). Unmapped natives (kind
// "other") would all collapse into one URN type, so they classify as
// "other.<native name>" with the native lowercased and folded to the
// URN-friendly [a-z0-9._-] alphabet; gram.hook.event carries the native
// name verbatim alongside.
func eventName(hr *hookrecord.Record) string {
	if hr.Kind != "other" || hr.NativeName == "" {
		return hr.Kind
	}
	return "other." + urnSafe(hr.NativeName)
}

func urnSafe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// severityOf maps the record's health signals onto log severity: ERROR for
// handler/pipeline failures and failed tool executions, INFO for everything
// else. Decision outcomes never influence severity — records are
// observational, and a deny is successful enforcement, not a fault in the
// hook rail. Gram auto-infers severity when unset, so this mapping only
// refines it.
func severityOf(hr *hookrecord.Record) log.Severity {
	if hr.HandlerErr != "" || (hr.Tool != nil && hr.Tool.Failed) {
		return log.SeverityError
	}
	return log.SeverityInfo
}

// errorType maps genuine failures onto the stable error.type semconv
// attribute. Values are low-cardinality and documented here: "handler_error"
// when the handler pipeline failed, "tool_error" when the tool execution the
// event reports failed. Empty (attribute omitted) otherwise.
func errorType(hr *hookrecord.Record) string {
	switch {
	case hr.HandlerErr != "":
		return "handler_error"
	case hr.Tool != nil && hr.Tool.Failed:
		return "tool_error"
	}
	return ""
}

func severityText(s log.Severity) string {
	if s == log.SeverityError {
		return "ERROR"
	}
	return "INFO"
}

// recordBuilder accumulates attributes, skipping empty values and running
// every string value through the consumer's Redactor before it can touch
// disk (the built-in transport/content redaction runs before that, at the
// call sites that carry credential-prone values).
type recordBuilder struct {
	attrs     []attribute.KeyValue
	redactor  func(key, value string) string
	truncated bool
}

func (b *recordBuilder) redact(key, value string) string {
	if b.redactor == nil || value == "" {
		return value
	}
	return b.redactor(key, value)
}

func (b *recordBuilder) str(key, value string) {
	if value == "" {
		return
	}
	b.attrs = append(b.attrs, attribute.String(key, b.redact(key, value)))
}

// flag attaches a true-valued marker attribute; false markers are expressed
// by omission.
func (b *recordBuilder) flag(key string) {
	b.attrs = append(b.attrs, attribute.Bool(key, true))
}

func (b *recordBuilder) int(key string, v int) {
	b.attrs = append(b.attrs, attribute.Int(key, v))
}

func (b *recordBuilder) intp(key string, v *int) {
	if v == nil {
		return
	}
	b.attrs = append(b.attrs, attribute.Int(key, *v))
}

func (b *recordBuilder) float(key string, v float64) {
	b.attrs = append(b.attrs, attribute.Float64(key, v))
}

// digest stands in for content at the default capture level: byte length
// plus SHA-256, under <prefix>.length / <prefix>.sha256.
func (b *recordBuilder) digest(prefix string, content []byte) {
	sum := sha256.Sum256(content)
	b.attrs = append(b.attrs,
		attribute.Int(prefix+".length", len(content)),
		attribute.String(prefix+".sha256", hex.EncodeToString(sum[:])),
	)
}

// content attaches a captured content value: built-in credential redaction,
// then the consumer's Redactor, then the per-value truncation cap.
func (b *recordBuilder) content(key, value string) {
	if value == "" {
		return
	}
	b.attrs = append(b.attrs, attribute.String(key, b.contentValue(key, value)))
}

func (b *recordBuilder) contentValue(key, value string) string {
	v := b.redact(key, redactContent(value))
	if len(v) > maxContentBytes {
		v = truncateUTF8(v, maxContentBytes)
		b.truncated = true
	}
	return v
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
