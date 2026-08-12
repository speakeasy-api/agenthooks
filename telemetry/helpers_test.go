package telemetry

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
)

// testEndpoint is syntactically valid but never contacted: tests swap the
// exporter constructor for an in-memory collector.
const testEndpoint = "http://127.0.0.1:9/v1/logs"

// memExporter collects exported records in memory.
type memExporter struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (m *memExporter) Export(_ context.Context, records []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range records {
		m.recs = append(m.recs, rec.Clone())
	}
	return nil
}

func (m *memExporter) Shutdown(context.Context) error   { return nil }
func (m *memExporter) ForceFlush(context.Context) error { return nil }

// newTestRecorder builds a Recorder through the real New path with the
// in-memory exporter substituted behind the constructor seam.
func newTestRecorder(t *testing.T, mutate func(*Config)) (*Recorder, *memExporter) {
	t.Helper()
	cfg := Config{Endpoint: testEndpoint}
	if mutate != nil {
		mutate(&cfg)
	}
	exp := &memExporter{}
	orig := newExporter
	newExporter = func(Config) (sdklog.Exporter, error) { return exp, nil }
	t.Cleanup(func() { newExporter = orig })
	rec, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rec.Shutdown(ctx)
	})
	return rec, exp
}

// flushed force-flushes the batch processor and returns everything the
// exporter has seen.
func flushed(t *testing.T, rec *Recorder, exp *memExporter) []sdklog.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rec.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	exp.mu.Lock()
	defer exp.mu.Unlock()
	return append([]sdklog.Record(nil), exp.recs...)
}

var testReceiveTime = time.Unix(1700000000, 123456789)

// toolPreRecord is a fully-populated tool.pre snapshot: MCP transport,
// timing.
func toolPreRecord() *hookrecord.Record {
	return &hookrecord.Record{
		Provider:   "claude-code",
		Variant:    "cli",
		NativeName: "PreToolUse",
		Kind:       "tool.pre",
		Time:       testReceiveTime,
		SessionID:  "sess-123",
		TurnID:     "turn-7",
		CWD:        "/work/repo",
		Model:      "claude-sonnet-4-5",
		Tool: &hookrecord.Tool{
			ID:        "toolu_01SsRreQbJuFTsZS9ZszkzNR",
			Name:      "mcp__github__create_issue",
			Canonical: "mcp",
			Input:     json.RawMessage(`{"title":"hi","token":"sk-abcdef1234567890"}`),
			MCP: &hookrecord.MCP{
				Server:  "github",
				Tool:    "create_issue",
				URL:     "https://user:hunter2@mcp.example.com/sse?api_key=abc123&x=1",
				Command: "npx mcp-github --token=ghp_1234567890abcdef",
			},
		},
		HookDurationMS: 12.5,
	}
}

// attrMap flattens an exported record's attributes into Go values for
// assertions.
func attrMap(rec sdklog.Record) map[string]any {
	out := map[string]any{}
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = attrValue(kv.Value)
		return true
	})
	return out
}

func attrValue(v attribute.Value) any {
	switch v.Type() {
	case attribute.BOOL:
		return v.AsBool()
	case attribute.INT64:
		return v.AsInt64()
	case attribute.FLOAT64:
		return v.AsFloat64()
	case attribute.STRING:
		return v.AsString()
	}
	return v.AsInterface()
}

func bodyString(rec sdklog.Record) string {
	return rec.Body().AsString()
}
