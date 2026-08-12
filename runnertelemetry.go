package agenthooks

import (
	"context"
	"reflect"
	"time"

	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
)

// recordTiming carries the tap's timing view of one event: the library
// receive time and the dispatch-to-response-encoded duration — hook
// overhead, distinct from tool execution time.
type recordTiming struct {
	receive  time.Time
	duration time.Duration
}

// afterEvent is the internal end-of-processing hook invoked by Runner.Run
// after the response is encoded, and by the OpenCode serve loop after the
// reply is encoded. Unlike OnAny observers — which run before the handler
// pipeline — it sees the full processing timing and any handler error. It
// deliberately does not see the decision: telemetry records are purely
// observational (the enforcement rail owns decision logging), so the tap
// carries the event, the timing, and the error signal — nothing about the
// verdict. WithTelemetry installs one.
type afterEvent func(typed any, base *Event, timing recordTiming, herr error)

// TelemetryRecorder is the runner-facing surface of a telemetry recorder;
// *telemetry.Recorder implements it. It exists so this root package does not
// import the telemetry package: only binaries that construct a recorder (and
// therefore import telemetry themselves) link the OTel SDK dependency tree —
// consumers that never opt in pay nothing. The method signatures use an
// internal type on purpose, which keeps the recorder tap callable by the
// runner but not implementable or invokable by external consumers.
type TelemetryRecorder interface {
	// RecordHook captures one hook event into the recorder's in-process
	// export pipeline.
	RecordHook(hr *hookrecord.Record) error
	// Shutdown flushes buffered records to the endpoint and stops the
	// pipeline. The hook server calls it on idle shutdown, on
	// SIGINT/SIGTERM, and before a version-upgrade exit.
	Shutdown(ctx context.Context) error
}

// WithTelemetry installs rec as the runner's telemetry recorder: one OTel
// log record per hook event, captured after the response is on the wire.
// Records are observational — they describe the event and the hook rail's
// own health (timing, errors), never the enforcement decision, which is
// logged by the enforcement backend at decision time.
//
// The recorder batches in process and ships over OTLP/HTTP in the
// background, so it is only meaningful in a long-lived process: the hook
// server (`mybinary agenthooks server`), which flushes it on idle shutdown
// and on signals. In a per-hook process (plain `run`) the process usually
// exits before a batch ships — telemetry there is best-effort by design
// and the loss is accepted.
//
// Opt-in and fail-open by construction — without the option nothing
// changes; with it, a recorder failure degrades to a logged warning, never
// an error on the pipeline. See the telemetry package for configuration:
//
//	rec, err := telemetry.New(telemetry.Config{Endpoint: ...})
//	if err != nil { ... }
//	r := agenthooks.New(agenthooks.WithTelemetry(rec))
//
// Runner.Decide does not record telemetry: it has no wire edge and its
// callers own their own observability.
func WithTelemetry(rec TelemetryRecorder) Option {
	return func(r *Runner) {
		if rec == nil {
			return
		}
		// A typed-nil recorder (a nil *telemetry.Recorder from a New whose
		// error went unchecked) makes the interface non-nil; treat it as
		// absent rather than installing a tap that can only fail.
		if v := reflect.ValueOf(rec); v.Kind() == reflect.Pointer && v.IsNil() {
			return
		}
		r.telemetryShutdown = rec.Shutdown
		r.afterEvent = func(typed any, base *Event, timing recordTiming, herr error) {
			if err := rec.RecordHook(buildHookRecord(typed, base, timing, herr)); err != nil {
				r.logger.Warn("agenthooks: telemetry record failed", "error", err)
			}
		}
	}
}

// tapAfterEvent delivers the end-of-processing snapshot to the telemetry
// recorder. encodedAt is sampled at the encoding boundary so the duration
// measures dispatch-to-response-encoded, not the provider write. Fail-open,
// always: the tap runs after the response is written, is panic-guarded like
// observers, and its work is bounded I/O with no network — recorder errors
// log a warning and never change a decision, delay a response, or surface
// as a hook failure.
func (r *Runner) tapAfterEvent(typed any, base *Event, herr error, encodedAt time.Time) {
	if r.afterEvent == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			r.logger.Warn("agenthooks: telemetry tap panic", "panic", p)
		}
	}()
	timing := recordTiming{receive: base.Time, duration: encodedAt.Sub(base.Time)}
	r.afterEvent(typed, base, timing, herr)
}

// buildHookRecord projects the typed event into the flat record the
// telemetry package consumes.
func buildHookRecord(typed any, base *Event, timing recordTiming, herr error) *hookrecord.Record {
	hr := &hookrecord.Record{
		Provider:       string(base.Provider),
		Variant:        string(base.Variant),
		NativeName:     base.NativeName,
		Kind:           string(base.Kind),
		Time:           timing.receive,
		Backfilled:     base.Backfilled,
		SessionID:      base.Session.ID,
		TurnID:         base.Session.TurnID,
		CWD:            base.Session.CWD,
		Model:          base.Session.Model,
		UserEmail:      base.Session.UserEmail,
		HookDurationMS: float64(timing.duration) / float64(time.Millisecond),
	}
	if herr != nil {
		hr.HandlerErr = herr.Error()
	}
	if base.Agent != nil {
		hr.SubagentID = base.Agent.ID
		hr.SubagentType = base.Agent.Type
	}
	switch ev := typed.(type) {
	case *PromptEvent:
		hr.Prompt = ev.Prompt
	case *ToolPreEvent:
		hr.Tool = toolHookRecord(&ev.Tool, nil)
	case *PermissionEvent:
		hr.Tool = toolHookRecord(&ev.Tool, nil)
	case *ToolPostEvent:
		hr.Tool = toolHookRecord(&ev.Tool, ev)
	case *StopEvent:
		hr.FinalMessage = ev.FinalMessage
		hr.LoopCount = ev.LoopCount
		if u := ev.Usage; u != nil {
			hr.Usage = &hookrecord.Usage{
				InputTokens:      u.InputTokens,
				OutputTokens:     u.OutputTokens,
				CacheReadTokens:  u.CacheReadTokens,
				CacheWriteTokens: u.CacheWriteTokens,
				Cost:             u.Cost,
			}
		}
	case *SessionStartEvent:
		hr.SessionSource = ev.Source
	case *SessionEndEvent:
		hr.SessionEndReason = ev.Reason
	case *CompactEvent:
		hr.CompactTrigger = ev.Trigger
	case *NotificationEvent:
		hr.Notification = ev.Message
	case *FileEditedEvent:
		hr.FilePath = ev.Path
	}
	return hr
}

func toolHookRecord(t *ToolCall, post *ToolPostEvent) *hookrecord.Tool {
	tr := &hookrecord.Tool{
		ID:          t.ID,
		Synthesized: t.Synthesized,
		Name:        t.Name,
		Canonical:   string(t.Canonical),
		Input:       t.Input,
	}
	if m := t.MCP; m != nil {
		tr.MCP = &hookrecord.MCP{
			Server:     m.Server,
			Tool:       m.Tool,
			URL:        m.URL,
			Command:    m.Command,
			FromConfig: m.FromConfig,
		}
	}
	if post != nil {
		tr.Output = post.Output
		tr.Failed = post.Failed
		tr.Error = post.Error
		tr.DurationMS = post.DurationMS
	}
	return tr
}
