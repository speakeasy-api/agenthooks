package telemetry

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	collpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Errorf("empty endpoint must fail at construction")
	}
	if _, err := New(Config{Endpoint: "not a url"}); err == nil {
		t.Errorf("malformed endpoint must fail at construction")
	}
	if _, err := New(Config{Endpoint: "grpc://example.com/v1/logs"}); err == nil {
		t.Errorf("non-http endpoint must fail at construction")
	}
}

func TestCaptureContentLevel(t *testing.T) {
	rec, exp := newTestRecorder(t, func(cfg *Config) { cfg.Capture = CaptureContent })
	hr := toolPreRecord()
	if err := rec.RecordHook(hr); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	prompt := toolPreRecord()
	prompt.Kind, prompt.NativeName = "prompt.submitted", "UserPromptSubmit"
	prompt.Tool = nil
	prompt.Prompt = "deploy with API_TOKEN=supersecret please"
	if err := rec.RecordHook(prompt); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}

	records := flushed(t, rec, exp)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	toolAttrs := attrMap(records[0])
	args, ok := toolAttrs["gen_ai.tool.call.arguments"].(string)
	if !ok {
		t.Fatalf("content level must carry tool arguments")
	}
	if !strings.Contains(args, `"title":"hi"`) {
		t.Errorf("arguments lost benign content: %s", args)
	}
	if strings.Contains(args, "sk-abcdef1234567890") {
		t.Errorf("built-in redaction must scrub token-shaped values from content: %s", args)
	}
	if toolAttrs["agenthooks.session.cwd"] != "/work/repo" {
		t.Errorf("cwd rides at content level, got %v", toolAttrs["agenthooks.session.cwd"])
	}

	body := bodyString(records[1])
	if !strings.Contains(body, "deploy with") {
		t.Errorf("prompt text must ride the body at content level: %q", body)
	}
	if strings.Contains(body, "supersecret") {
		t.Errorf("secret-named env assignment must be scrubbed from the body: %q", body)
	}
}

func TestUserRedactorRunsAfterBuiltinRedaction(t *testing.T) {
	var sawKeys []string
	rec, exp := newTestRecorder(t, func(cfg *Config) {
		cfg.Capture = CaptureContent
		cfg.Redactor = func(key, value string) string {
			sawKeys = append(sawKeys, key)
			if key == "session.id" {
				return "REDACTED-SESSION"
			}
			return strings.ReplaceAll(value, "hi", "**")
		}
	})
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	records := flushed(t, rec, exp)
	attrs := attrMap(records[0])
	if attrs["session.id"] != "REDACTED-SESSION" {
		t.Errorf("Redactor must rewrite attribute values: %v", attrs["session.id"])
	}
	if args, _ := attrs["gen_ai.tool.call.arguments"].(string); strings.Contains(args, "hi") {
		t.Errorf("Redactor must see content values: %s", args)
	}
	joined := strings.Join(sawKeys, ",")
	if !strings.Contains(joined, "body") {
		t.Errorf("Redactor must see the body (key \"body\"); saw %s", joined)
	}
}

func TestPromptDigestAtDefaultLevel(t *testing.T) {
	rec, exp := newTestRecorder(t, nil)
	hr := toolPreRecord()
	hr.Kind, hr.NativeName = "prompt.submitted", "UserPromptSubmit"
	hr.Tool = nil
	hr.Prompt = "refactor the auth middleware"
	if err := rec.RecordHook(hr); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	records := flushed(t, rec, exp)
	attrs := attrMap(records[0])
	if attrs["agenthooks.prompt.length"] != int64(len(hr.Prompt)) {
		t.Errorf("prompt length = %v", attrs["agenthooks.prompt.length"])
	}
	if attrs["agenthooks.prompt.sha256"] != "61d18f121f92c32678dc7bdf69b23794a67d6247aaf7b33b68459d0dfe061660" {
		t.Errorf("prompt sha256 = %v", attrs["agenthooks.prompt.sha256"])
	}
	if body := bodyString(records[0]); strings.Contains(body, "refactor") {
		t.Errorf("prompt text must not leave the process at the default level: %q", body)
	}
	// Non-tool events trace per session (gram rule 3).
	if records[0].TraceID() == [16]byte{} {
		t.Fatalf("trace id missing")
	}
}

func TestRecordHookAfterShutdownIsNoOp(t *testing.T) {
	rec, exp := newTestRecorder(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rec.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatalf("RecordHook after Shutdown must not error: %v", err)
	}
	exp.mu.Lock()
	defer exp.mu.Unlock()
	if len(exp.recs) != 0 {
		t.Errorf("records after shutdown = %d, want 0", len(exp.recs))
	}
}

// TestExportIntervalReachesBatchProcessor asserts the Config.ExportInterval
// seam is plumbed through: with a short interval the record ships in the
// background — no ForceFlush — well before the SDK's 1s default schedule
// would have fired, so a dropped option fails the test.
func TestExportIntervalReachesBatchProcessor(t *testing.T) {
	rec, exp := newTestRecorder(t, func(cfg *Config) { cfg.ExportInterval = 10 * time.Millisecond })
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	deadline := time.Now().Add(900 * time.Millisecond)
	for {
		exp.mu.Lock()
		n := len(exp.recs)
		exp.mu.Unlock()
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("record never exported on the custom interval (records = %d)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestShutdownShipsOverOTLP exercises the real export path end to end: a
// record is enqueued, Shutdown flushes it to an OTLP/HTTP endpoint with
// gzip compression and the configured auth headers.
func TestShutdownShipsOverOTLP(t *testing.T) {
	type export struct {
		header http.Header
		req    *collpb.ExportLogsServiceRequest
	}
	exports := make(chan export, 16)
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
		exports <- export{header: r.Header.Clone(), req: &req}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec, err := New(Config{
		Endpoint: srv.URL + "/v1/logs",
		Headers:  map[string]string{"Gram-Key": "key-1", "Gram-Project": "proj-1"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := rec.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case got := <-exports:
		if got.header.Get("Content-Encoding") != "gzip" {
			t.Errorf("Content-Encoding = %q, want gzip", got.header.Get("Content-Encoding"))
		}
		if got.header.Get("Gram-Key") != "key-1" || got.header.Get("Gram-Project") != "proj-1" {
			t.Errorf("auth headers missing: %v", got.header)
		}
		var events []string
		for _, rl := range got.req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					for _, kv := range lr.GetAttributes() {
						if kv.GetKey() == "gram.hook.event" {
							events = append(events, kv.GetValue().GetStringValue())
						}
					}
				}
			}
		}
		if len(events) != 1 || events[0] != "PreToolUse" {
			t.Errorf("shipped gram.hook.event values = %v, want [PreToolUse]", events)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Shutdown never delivered the record")
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://user:pass@host.example.com/path", "https://host.example.com/path"},
		// url.Values.Encode percent-encodes the mask, matching the relay
		// implementation this is ported from.
		{"https://host.example.com/sse?api_key=abc&x=1", "https://host.example.com/sse?api_key=%2A%2A%2A&x=1"},
		{"https://host.example.com/p?signature=zzz", "https://host.example.com/p?signature=%2A%2A%2A"},
		{"https://host.example.com/p#frag", "https://host.example.com/p"},
		// Unparseable URLs could hide credentials anywhere: dropped whole.
		{"https://u:p@host/%zz", "***"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := redactURL(tt.in); got != tt.want {
			t.Errorf("redactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactCommand(t *testing.T) {
	tests := []struct{ in, want string }{
		{"npx server --api-key=abc123", "npx server --api-key=***"},
		{"npx server --token abc123", "npx server --token ***"},
		{"OPENAI_API_KEY=sk-123 npx server", "OPENAI_API_KEY=*** npx server"},
		{`curl -H "Authorization: Bearer abc.def" https://api.example.com`, "curl -H Authorization: Bearer *** https://api.example.com"},
		{"npx mcp-remote https://u:p@srv.example.com/mcp", "npx mcp-remote https://srv.example.com/mcp"},
		{"npx server ghp_0123456789abcdef", "npx server ***"},
	}
	for _, tt := range tests {
		if got := redactCommand(tt.in); got != tt.want {
			t.Errorf("redactCommand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactContent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"use sk-abcdef1234567890 for auth", "use *** for auth"},
		{"push with ghp_0123456789abcdef now", "push with *** now"},
		{"MY_API_KEY=hunter2 ./run", "MY_API_KEY=*** ./run"},
		{"see https://user:pw@host.example.com/x", "see https://***@host.example.com/x"},
		{"Authorization: Bearer abc12345678", "Authorization: Bearer ***"},
		{"plain text stays intact", "plain text stays intact"},
	}
	for _, tt := range tests {
		if got := redactContent(tt.in); got != tt.want {
			t.Errorf("redactContent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
