package agenthooks

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	lpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
	"github.com/speakeasy-api/agenthooks/telemetry"
)

// newTestTelemetry stands up a real OTLP/HTTP collector and a Recorder
// pointed at it. collect() force-flushes the recorder's batch pipeline and
// returns every log record received so far.
func newTestTelemetry(t *testing.T) (*telemetry.Recorder, func() []*lpb.LogRecord) {
	t.Helper()
	var mu sync.Mutex
	var records []*lpb.LogRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip reader: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer func() { _ = gz.Close() }()
			body = gz
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("reading export body: %v", err)
		}
		var req collpb.ExportLogsServiceRequest
		if err := proto.Unmarshal(raw, &req); err != nil {
			t.Errorf("decoding export body: %v", err)
		}
		mu.Lock()
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				records = append(records, sl.GetLogRecords()...)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rec, err := telemetry.New(telemetry.Config{Endpoint: srv.URL + "/v1/logs"})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rec.Shutdown(ctx)
	})
	collect := func() []*lpb.LogRecord {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := rec.ForceFlush(ctx); err != nil {
			t.Fatalf("ForceFlush: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]*lpb.LogRecord(nil), records...)
	}
	return rec, collect
}

func telemetryAttrs(pr *lpb.LogRecord) map[string]any {
	out := map[string]any{}
	for _, kv := range pr.GetAttributes() {
		switch val := kv.GetValue().GetValue().(type) {
		case *cpb.AnyValue_BoolValue:
			out[kv.GetKey()] = val.BoolValue
		case *cpb.AnyValue_IntValue:
			out[kv.GetKey()] = val.IntValue
		case *cpb.AnyValue_DoubleValue:
			out[kv.GetKey()] = val.DoubleValue
		default:
			out[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
	}
	return out
}

// requireNoDecisionAttrs asserts the observational contract: records never
// carry the enforcement decision (the enforcement backend's decision-time
// log is the sole record of decisions).
func requireNoDecisionAttrs(t *testing.T, attrs map[string]any) {
	t.Helper()
	for _, key := range []string{
		"gram.hook.decision", "agenthooks.decision.reason",
		"agenthooks.decision.blocking", "agenthooks.decision.source",
	} {
		if _, ok := attrs[key]; ok {
			t.Errorf("decision attribute %s must not be emitted: %v", key, attrs)
		}
	}
}

func TestWithTelemetryRecordsEvent(t *testing.T) {
	rec, collect := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked"), nil
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked"}}`
	if out != want || code != 0 {
		t.Fatalf("telemetry must not change the wire: got %q (exit %d)", out, code)
	}

	records := collect()
	if len(records) != 1 {
		t.Fatalf("recorded records = %d, want 1", len(records))
	}
	pr := records[0]
	attrs := telemetryAttrs(pr)
	if attrs["gram.hook.event"] != "PreToolUse" || attrs["event.name"] != "tool.pre" {
		t.Errorf("record identity wrong: %v", attrs)
	}
	if attrs["gen_ai.tool.call.id"] != "toolu_01ABC" || attrs["gram.tool.name"] != "Bash" {
		t.Errorf("tool identity wrong: %v", attrs)
	}
	if attrs["session.id"] != "sess-claude-1" {
		t.Errorf("session id wrong: %v", attrs)
	}
	requireNoDecisionAttrs(t, attrs)
	// gram's hashToolCallIDToTraceID("toolu_01ABC").
	if got := hex.EncodeToString(pr.GetTraceId()); got != "7661011023ab0fab264a729fccde4ff1" {
		t.Errorf("trace id = %s, want gram derivation for toolu_01ABC", got)
	}
	// The handler denied, but a deny is successful enforcement, not a rail
	// fault: severity stays INFO.
	if pr.GetSeverityText() != "INFO" {
		t.Errorf("severity = %q, want INFO", pr.GetSeverityText())
	}
}

func TestWithTelemetryRecordStaysObservationalUnderPolicy(t *testing.T) {
	rec, collect := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec), WithPolicy(Policy{Unsupported: Degrade, AskFallback: FallbackDeny}))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return AskUser("confirm?"), nil
	})
	out, _ := runWith(t, r, []string{"agenthooks", "run", "--provider=codex"}, fixture(t, "codex/pre_tool_use.json"))
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("ask should degrade to deny on codex: %q", out)
	}

	records := collect()
	if len(records) != 1 {
		t.Fatalf("recorded records = %d, want 1", len(records))
	}
	// Neither the handler's ask nor the degraded deny reaches the record:
	// it stays a pure observation of the event.
	attrs := telemetryAttrs(records[0])
	requireNoDecisionAttrs(t, attrs)
	if attrs["event.name"] != "tool.pre" {
		t.Errorf("event identity wrong: %v", attrs)
	}
	// gram's hashToolCallIDToTraceID("call_9").
	if got := hex.EncodeToString(records[0].GetTraceId()); got != "5e447f59d541311dada70f8d9d26d0e3" {
		t.Errorf("trace id = %s, want gram derivation for call_9", got)
	}
}

func TestWithTelemetryRecordsHandlerError(t *testing.T) {
	rec, collect := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision(), errors.New("boom: handler exploded")
	})
	// Default FailOpen policy: the wire response stays a no-op.
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 {
		t.Fatalf("fail-open handler error must not change the exit code: %q (exit %d)", out, code)
	}

	records := collect()
	if len(records) != 1 {
		t.Fatalf("records must fire even when the handler errors: got %d", len(records))
	}
	pr := records[0]
	attrs := telemetryAttrs(pr)
	if got, _ := attrs["agenthooks.handler.error"].(string); !strings.Contains(got, "handler exploded") {
		t.Errorf("agenthooks.handler.error = %v, want the handler failure", attrs["agenthooks.handler.error"])
	}
	if attrs["error.type"] != "handler_error" {
		t.Errorf("error.type = %v, want handler_error", attrs["error.type"])
	}
	if pr.GetSeverityText() != "ERROR" {
		t.Errorf("handler-error severity = %q, want ERROR", pr.GetSeverityText())
	}
	requireNoDecisionAttrs(t, attrs)
}

func TestWithTelemetryRecordsUnmappedNative(t *testing.T) {
	rec, collect := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec))
	if out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/setup.json")); code != 0 {
		t.Fatalf("unmapped native must no-op cleanly: %q (exit %d)", out, code)
	}

	records := collect()
	if len(records) != 1 {
		t.Fatalf("recorded records = %d, want 1", len(records))
	}
	attrs := telemetryAttrs(records[0])
	// The native name rides verbatim; the unified identity classifies as
	// other.<sanitized native> so unmapped natives do not collapse into a
	// single gram URN type.
	if attrs["gram.hook.event"] != "Setup" || attrs["event.name"] != "other.setup" {
		t.Errorf("unmapped-native identity wrong: %v", attrs)
	}
	if records[0].GetEventName() != "other.setup" {
		t.Errorf("EventName field = %q, want other.setup", records[0].GetEventName())
	}
	if attrs["session.id"] != "sess-claude-1" {
		t.Errorf("session id wrong: %v", attrs)
	}
	requireNoDecisionAttrs(t, attrs)
}

func TestWithTelemetryRecordsBackfilledPrompt(t *testing.T) {
	rec, collect := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec), WithDedupDir(t.TempDir()))
	if out, code := runWith(t, r, []string{"agenthooks", "run", "--provider=kimi-code"}, kimiPre("sess-tel-bf")); code != 0 {
		t.Fatalf("run failed: %q (exit %d)", out, code)
	}

	// The synthesized reporting-only prompt.submitted records before the
	// triggering tool.pre, flagged as backfilled (nil Raw, no prompt text
	// recovered here — the record still forms).
	records := collect()
	if len(records) != 2 {
		t.Fatalf("recorded records = %d, want 2 (backfilled prompt + tool.pre)", len(records))
	}
	prompt := telemetryAttrs(records[0])
	if prompt["event.name"] != "prompt.submitted" || prompt["agenthooks.event.backfilled"] != true {
		t.Errorf("backfilled prompt record wrong: %v", prompt)
	}
	requireNoDecisionAttrs(t, prompt)
	toolPre := telemetryAttrs(records[1])
	if toolPre["event.name"] != "tool.pre" {
		t.Errorf("triggering event record wrong: %v", toolPre)
	}
	if _, ok := toolPre["agenthooks.event.backfilled"]; ok {
		t.Errorf("real events must not carry the backfilled flag: %v", toolPre)
	}
}

// captureRecorder is a minimal TelemetryRecorder for tap-boundary tests.
type captureRecorder struct {
	records  atomic.Int32
	shutdown atomic.Bool
	fail     bool
}

func (c *captureRecorder) RecordHook(*hookrecord.Record) error {
	c.records.Add(1)
	if c.fail {
		return errors.New("recorder unavailable")
	}
	return nil
}

func (c *captureRecorder) Shutdown(context.Context) error {
	c.shutdown.Store(true)
	return nil
}

func TestWithTelemetryFailingRecorderKeepsWire(t *testing.T) {
	rec := &captureRecorder{fail: true}
	r := quietRunner(WithTelemetry(rec))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked"), nil
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked"}}`
	if out != want || code != 0 {
		t.Errorf("failing recorder must never change the wire response: %q (exit %d)", out, code)
	}
	if rec.records.Load() != 1 {
		t.Errorf("tap must still deliver to the failing recorder")
	}
}

func TestTelemetryTapPanicIsContained(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked"), nil
	})
	r.afterEvent = func(any, *Event, recordTiming, error) {
		panic("recorder bug")
	}
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || !strings.Contains(out, `"deny"`) {
		t.Errorf("tap panic must not leak into the wire: %q (exit %d)", out, code)
	}
}

func TestWithTelemetryTypedNilRecorderIsNoOp(t *testing.T) {
	var rec *telemetry.Recorder
	r := quietRunner(WithTelemetry(rec))
	if r.afterEvent != nil || r.telemetryShutdown != nil {
		t.Errorf("typed-nil recorder must not install the tap or the shutdown hook")
	}
}

func TestServeLoopTapsTelemetry(t *testing.T) {
	rec, collect := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("no bash in this session"), nil
	})
	lines := []string{
		`{"seq":1,"hook":"initialize","input":{"serverUrl":"http://127.0.0.1:1","directory":"/work","worktree":""}}`,
		strings.TrimSpace(string(fixture(t, "opencode/tool_execute_before.json"))),
	}
	var out, errb strings.Builder
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=opencode"},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}

	records := collect()
	if len(records) != 1 {
		t.Fatalf("recorded records = %d, want 1 (initialize is not a hook event)", len(records))
	}
	attrs := telemetryAttrs(records[0])
	if attrs["event.name"] != "tool.pre" || attrs["gram.hook.source"] != "opencode" {
		t.Errorf("serve-loop record wrong: %v", attrs)
	}
	requireNoDecisionAttrs(t, attrs)
}
