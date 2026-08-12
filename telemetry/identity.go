package telemetry

import (
	"crypto/rand"
	"crypto/sha256"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Deterministic trace-context identity, reproducing gram's derivation
// byte-for-byte so agent-emitted and server-derived rows for the same event
// share trace IDs and existing joins keep working (RFC §4.4). The reference
// implementation is gram's canonicalTraceID / hashToolCallIDToTraceID /
// syntheticToolCallID (server/internal/hooks/ingest_hooks.go,
// server/internal/hooks/impl.go):
//
//  1. tool events with a per-call id      → SHA-256(toolCallID)[:16]
//  2. tool events without one             → SHA-256(len(sessionID) + "|" +
//     sessionID + "|" + toolName)[:16]
//  3. everything else with a session id   → SHA-256(sessionID)[:16]
//  4. last resort                         → random
//
// In this library rule 1 covers effectively every tool event — ToolCall.ID
// is the native id or the synthesized hook_synth_* id, the same value the
// relay sends today — but the full ladder is reproduced so any input hashes
// identically to gram's.

// deriveTraceID returns the trace ID for an event and whether it was
// deterministically derived. ok is false only on the random fallback
// (empty session id on a non-tool event), which callers flag with
// agenthooks.session.unidentified=true.
func deriveTraceID(isTool bool, toolCallID, sessionID, toolName string) (trace.TraceID, bool) {
	switch {
	case isTool && toolCallID != "":
		return traceIDFrom(toolCallID), true
	case isTool && sessionID != "" && toolName != "":
		// gram's syntheticToolCallID: the session id is length-prefixed so
		// the encoding is injective even when session ids contain "|".
		return traceIDFrom(strconv.Itoa(len(sessionID)) + "|" + sessionID + "|" + toolName), true
	case sessionID != "":
		return traceIDFrom(sessionID), true
	}
	var id trace.TraceID
	_, _ = rand.Read(id[:])
	return id, false
}

// traceIDFrom is gram's hashToolCallIDToTraceID: the first 16 bytes of the
// key's SHA-256.
func traceIDFrom(key string) trace.TraceID {
	sum := sha256.Sum256([]byte(key))
	var id trace.TraceID
	copy(id[:], sum[:16])
	return id
}

// parseTraceparent parses a W3C trace-context traceparent value
// ("00-<32 hex trace id>-<16 hex parent span id>-<2 hex flags>"). ok is
// false for malformed values, the invalid all-zero IDs, and the reserved
// version ff. Only the trace ID and flags are consumed by this library: the
// parent span ID identifies the launcher's span, not this event, and each
// record keeps its own deterministic span identity (§4.4).
func parseTraceparent(v string) (trace.TraceID, trace.TraceFlags, bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) < 4 || !isHexByte(parts[0]) || strings.EqualFold(parts[0], "ff") {
		return trace.TraceID{}, 0, false
	}
	traceID, err := trace.TraceIDFromHex(strings.ToLower(parts[1]))
	if err != nil || !traceID.IsValid() {
		return trace.TraceID{}, 0, false
	}
	if _, err := trace.SpanIDFromHex(strings.ToLower(parts[2])); err != nil {
		return trace.TraceID{}, 0, false
	}
	flags, err := strconv.ParseUint(parts[3], 16, 8)
	if err != nil || len(parts[3]) != 2 {
		return trace.TraceID{}, 0, false
	}
	return traceID, trace.TraceFlags(flags), true
}

// isHexByte reports whether s is exactly two hex digits (a traceparent
// version field).
func isHexByte(s string) bool {
	if len(s) != 2 {
		return false
	}
	_, err := strconv.ParseUint(s, 16, 8)
	return err == nil
}

// deriveSpanID is deterministic per event: the first 8 bytes of the SHA-256
// of "agenthooks|event" followed by the length-prefixed session ID, turn ID,
// native name, tool-call ID, and receive-time nanos. Length prefixes keep
// the encoding injective — field values are provider-controlled and may
// contain the separator — so two distinct events can never collide onto one
// key (the same reasoning as gram's syntheticToolCallID). Identical
// double-fires and spool replays still collide onto the same
// (trace_id, span_id) and dedupe at the storage layer; nothing joins on
// span ids today (gram's are random).
func deriveSpanID(sessionID, turnID, nativeName, toolCallID string, receive time.Time) trace.SpanID {
	var key strings.Builder
	key.WriteString("agenthooks|event")
	for _, part := range []string{sessionID, turnID, nativeName, toolCallID, strconv.FormatInt(receive.UnixNano(), 10)} {
		key.WriteString("|")
		key.WriteString(strconv.Itoa(len(part)))
		key.WriteString("|")
		key.WriteString(part)
	}
	sum := sha256.Sum256([]byte(key.String()))
	var id trace.SpanID
	copy(id[:], sum[:8])
	return id
}
