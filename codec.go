package agenthooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrUnsupportedDecision is returned (under Policy.Unsupported == Strict)
// when a handler decision cannot be expressed on the invoking provider.
var ErrUnsupportedDecision = errors.New("agenthooks: decision unsupported by provider")

// ErrLossyUpdate is returned when an input rewrite removes keys on a provider
// whose update mechanism is a shallow merge (Gemini): the removal cannot be
// expressed, so honoring the rewrite would silently corrupt the tool call.
var ErrLossyUpdate = errors.New("agenthooks: updated input removes keys; provider merge is shallow (lossy)")

// wireResponse is what a codec hands back to the runtime: stdout/stderr bytes
// plus the dialect-correct exit code. Stderr is only populated on dialects
// whose blocking mechanism is exit-2-with-stderr-reason (Kimi, quirk #23).
type wireResponse struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// noOpResponse is each provider's correct "no opinion" form. Claude/Cursor/
// Gemini get an explicit {} (Gemini parses stderr when stdout is empty,
// quirk #11; Claude empty stdout is not a decision, quirk #17). Codex treats
// empty stdout as "no opinion" and rejects unknown JSON (quirk #8). Kimi
// appends exit-0 stdout to the model context, so its no-op must be empty
// stdout (quirk #23).
func noOpResponse(p Provider) wireResponse {
	if p == ProviderCodex || p == ProviderKimi {
		return wireResponse{}
	}
	return wireResponse{Stdout: []byte("{}")}
}

func joinContext(ss []string) string { return strings.Join(ss, "\n") }

// decodePayload turns a provider payload into a typed event.
func decodePayload(p Provider, v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	switch p {
	case ProviderClaudeCode:
		return decodeClaude(v, conf, now, payload)
	case ProviderCodex:
		return decodeCodex(v, conf, now, payload)
	case ProviderCursor:
		return decodeCursor(v, conf, now, payload)
	case ProviderGemini:
		return decodeGemini(v, conf, now, payload)
	case ProviderOpenCode:
		return decodeOpenCodeLine(v, conf, now, payload)
	case ProviderKimi:
		return decodeKimi(v, conf, now, payload)
	}
	return nil, fmt.Errorf("agenthooks: unknown provider %q", p)
}

// encodeDecision translates a decision into the provider's dialect.
func encodeDecision(typed any, d decisionCore) (wireResponse, error) {
	base := eventOf(typed)
	if base == nil {
		return wireResponse{}, errors.New("agenthooks: nil event")
	}
	switch base.Provider {
	case ProviderClaudeCode:
		return encodeClaude(base, d)
	case ProviderCodex:
		return encodeCodex(base, d)
	case ProviderCursor:
		return encodeCursor(base, d)
	case ProviderGemini:
		return encodeGemini(typed, base, d)
	case ProviderOpenCode:
		reply, err := encodeOpenCodeReply(typed, base, d)
		if err != nil {
			return wireResponse{}, err
		}
		out, err := json.Marshal(reply)
		if err != nil {
			return wireResponse{}, err
		}
		return wireResponse{Stdout: out}, nil
	case ProviderKimi:
		return encodeKimi(base, d)
	}
	return wireResponse{}, fmt.Errorf("agenthooks: unknown provider %q", base.Provider)
}

// lossyUpdate reports whether updated removes keys present in the original
// input object — inexpressible under shallow-merge semantics (quirk #12).
func lossyUpdate(original json.RawMessage, updated any) (bool, error) {
	var orig map[string]json.RawMessage
	if err := json.Unmarshal(original, &orig); err != nil {
		return false, nil //nolint:nilerr // a non-object original has no keys to lose
	}
	upBytes, err := json.Marshal(updated)
	if err != nil {
		return false, err
	}
	var up map[string]json.RawMessage
	if err := json.Unmarshal(upBytes, &up); err != nil {
		return false, fmt.Errorf("agenthooks: updated input is not a JSON object: %w", err)
	}
	for k := range orig {
		if _, ok := up[k]; !ok {
			return true, nil
		}
	}
	return false, nil
}
