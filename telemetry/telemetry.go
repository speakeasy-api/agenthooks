// Package telemetry emits one OpenTelemetry log record (a wide event) per
// hook event, batched in process and shipped over OTLP/HTTP (gzip) in the
// background by the OTel SDK's batch processor.
//
// Wire the recorder into a Runner with agenthooks.WithTelemetry:
//
//	rec, err := telemetry.New(telemetry.Config{
//		Endpoint: "https://app.getgram.ai/rpc/hooks.otel/v1/logs",
//		Headers:  map[string]string{"Gram-Key": key, "Gram-Project": project},
//	})
//	if err != nil { ... }
//	r := agenthooks.New(agenthooks.WithTelemetry(rec))
//
// The recorder is built for the hook server (`mybinary agenthooks server`),
// the long-lived process the client/server architecture runs the pipeline
// in: batches ship on the processor's schedule while the server lives, and
// the server flushes on idle shutdown, SIGINT/SIGTERM, and version-upgrade
// exits via Shutdown. Records buffered in a process that dies without a
// flush are lost — telemetry is best-effort by design; the enforcement rail
// carries the decisions.
//
// The feature is opt-in and fail-open by construction: without the option
// nothing changes; with it, recorder failures degrade to a logged warning
// and never affect the decision path. Any OTLP logs endpoint works; gram is
// one consumer configuration.
package telemetry

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"

	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
)

// CaptureLevel selects how much event content leaves the process.
type CaptureLevel int

const (
	// CaptureAttributes (the default) emits structured attributes only: no
	// prompt text, no tool input/output bodies, no assistant messages, no
	// cwd. Sizes and SHA-256 digests stand in for content.
	CaptureAttributes CaptureLevel = iota
	// CaptureContent additionally records prompt text, tool input/output,
	// and assistant messages — after the built-in transport-credential
	// redaction and the consumer's Redactor.
	CaptureContent
)

// Config configures a Recorder. Misconfiguration fails at New — construction
// time, in the consumer's control — not at event time.
type Config struct {
	// Endpoint is the OTLP/HTTP logs endpoint, e.g.
	// "https://app.getgram.ai/rpc/hooks.otel/v1/logs" or any collector's
	// "/v1/logs". Required.
	Endpoint string
	// Headers are added to every export request (auth: e.g. Gram-Key,
	// Gram-Project).
	Headers map[string]string
	// Resource attributes merged over the library defaults (service.name,
	// service.version, host.name, os.type, host.arch,
	// gram.event.origin=agenthooks, ...).
	Resource map[string]string

	// Capture selects the content level. Default: CaptureAttributes.
	Capture CaptureLevel
	// Redactor rewrites attribute and body values before they enter the
	// export pipeline. It is called with the attribute key (the body uses
	// key "body") and the value, and returns the replacement. The library
	// always applies its built-in transport-credential redaction (URLs,
	// commands, token-shaped values) first; Redactor runs after it.
	Redactor func(key string, value string) string

	// ExportInterval overrides how often the batch processor ships buffered
	// records in the background. Zero keeps the SDK default (1s). Intended
	// for tests and latency tuning — a short interval lets a test observe
	// exports deterministically without racing the default schedule;
	// production consumers rarely need to set it.
	ExportInterval time.Duration

	// HonorTraceparent opts into W3C trace-context parenting: when the
	// recording process carries a valid TRACEPARENT environment variable
	// (read once at construction — in the client/server architecture that
	// is the server process, whose environment came from the client that
	// spawned it), its trace ID and sampled flag replace the deterministic
	// trace ID on emitted records, and the deterministic ID moves to the
	// agenthooks.deterministic_trace_id attribute so hash-derived joins
	// (e.g. gram's) still work. Off by default: the deterministic
	// derivation is what backend joins key on, and an ambient trace ID
	// would silently regroup records. Each record keeps its own
	// deterministic span ID either way.
	HonorTraceparent bool
}

// Recorder captures hook events as OTel log records into an in-process
// batch-export pipeline. Construct with New; install with
// agenthooks.WithTelemetry. Methods are safe for concurrent use.
type Recorder struct {
	cfg      Config
	provider *sdklog.LoggerProvider
	logger   log.Logger

	// Ambient W3C trace context, read once at construction when
	// Config.HonorTraceparent is set and TRACEPARENT is valid.
	ambientTrace trace.TraceID
	ambientFlags trace.TraceFlags
	ambientOK    bool
}

// scopeName identifies this package as the instrumentation scope of every
// emitted record.
const scopeName = "github.com/speakeasy-api/agenthooks/telemetry"

// newExporter builds the OTLP/HTTP exporter; a variable so tests can
// substitute an in-memory exporter behind the real construction path.
var newExporter = func(cfg Config) (sdklog.Exporter, error) {
	return otlploghttp.New(context.Background(),
		otlploghttp.WithEndpointURL(cfg.Endpoint),
		otlploghttp.WithHeaders(cfg.Headers),
		otlploghttp.WithCompression(otlploghttp.GzipCompression),
	)
}

// New builds a Recorder: it validates the endpoint and stands up the
// sdk/log pipeline — a batch processor feeding a gzip-compressed OTLP/HTTP
// exporter. Construction performs no network I/O; the first export happens
// on the processor's schedule (or on ForceFlush/Shutdown).
func New(cfg Config) (*Recorder, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, errors.New("telemetry: Config.Endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.New("telemetry: Config.Endpoint is not a valid URL: " + err.Error())
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("telemetry: Config.Endpoint must be an absolute http(s) URL")
	}
	cfg.Endpoint = endpoint

	res, err := buildResource(cfg.Resource)
	if err != nil {
		return nil, errors.New("telemetry: building resource: " + err.Error())
	}
	exporter, err := newExporter(cfg)
	if err != nil {
		return nil, errors.New("telemetry: building exporter: " + err.Error())
	}
	var procOpts []sdklog.BatchProcessorOption
	if cfg.ExportInterval > 0 {
		procOpts = append(procOpts, sdklog.WithExportInterval(cfg.ExportInterval))
	}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter, procOpts...)),
	)
	r := &Recorder{
		cfg:      cfg,
		provider: provider,
		logger:   provider.Logger(scopeName, log.WithInstrumentationVersion(agenthooksVersion())),
	}
	if cfg.HonorTraceparent {
		r.ambientTrace, r.ambientFlags, r.ambientOK = parseTraceparent(os.Getenv("TRACEPARENT"))
	}
	return r, nil
}

// RecordHook captures one hook event at end of processing: it builds the
// observational log record (event identity, payload shape, hook-rail health
// — never the enforcement decision), injects the deterministic trace/span
// identity via a synthetic span context on the emit context, and enqueues
// the record on the batch processor — no synchronous network I/O.
//
// RecordHook is invoked by the runner tap agenthooks.WithTelemetry installs.
// Its parameter type lives in an internal package, so it is not callable by
// external consumers.
func (r *Recorder) RecordHook(hr *hookrecord.Record) error {
	rec, sc := r.buildRecord(hr)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	r.logger.Emit(ctx, rec)
	return nil
}

// ForceFlush exports every buffered record without stopping the pipeline.
func (r *Recorder) ForceFlush(ctx context.Context) error {
	return r.provider.ForceFlush(ctx)
}

// Shutdown flushes buffered records and stops the pipeline; further
// RecordHook calls become no-ops. The hook server calls it on idle
// shutdown, on SIGINT/SIGTERM, and before a version-upgrade exit.
func (r *Recorder) Shutdown(ctx context.Context) error {
	return r.provider.Shutdown(ctx)
}

// buildResource merges the library defaults with the consumer's overrides.
func buildResource(extra map[string]string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", serviceName()),
		attribute.String("os.type", runtime.GOOS),
		attribute.String("host.arch", runtime.GOARCH),
		attribute.String("gram.event.origin", "agenthooks"),
		attribute.String("agenthooks.version", agenthooksVersion()),
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		attrs = append(attrs, attribute.String("host.name", host))
	}
	if v := binaryVersion(); v != "" {
		attrs = append(attrs, attribute.String("service.version", v))
	}
	for k, v := range extra {
		attrs = append(attrs, attribute.String(k, v))
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
}

func serviceName() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "agenthooks"
	}
	return strings.TrimSuffix(filepath.Base(exe), ".exe")
}

// agenthooksVersion reports this module's version as built into the consumer
// binary, or "unknown" outside module builds.
func agenthooksVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	const module = "github.com/speakeasy-api/agenthooks"
	if bi.Main.Path == module && bi.Main.Version != "" {
		return bi.Main.Version
	}
	for _, dep := range bi.Deps {
		if dep.Path == module && dep.Version != "" {
			return dep.Version
		}
	}
	return "unknown"
}

// binaryVersion reports the consumer binary's own module version, "" when
// unavailable.
func binaryVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return ""
	}
	return bi.Main.Version
}
