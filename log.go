package agenthooks

import (
	"context"
	"log/slog"
	"os"
)

// Logging discipline (§3.1): hook stdout is a wire protocol, so handlers must
// never print to it. Main swaps os.Stdout for a sink before running, and
// handlers get a structured logger via Logger(ctx). stderr is used only where
// safe: the runner always writes an explicit response to the real stdout, so
// Gemini's stderr-parses-as-decision behavior (quirk #11) can't trigger.

type ctxKey int

const loggerKey ctxKey = 0

// Logger returns the runner's logger from a handler context.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return defaultLogger()
}

func withLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

func defaultLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// logSink returns where stray handler stdout goes: $AGENTHOOKS_LOG when set,
// /dev/null otherwise.
func logSink() (*os.File, error) {
	if p := os.Getenv("AGENTHOOKS_LOG"); p != "" {
		return os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	}
	return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
}
