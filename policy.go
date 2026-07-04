package agenthooks

import "time"

// FailMode governs what a handler error, panic, or timeout means.
type FailMode int

const (
	FailOpen   FailMode = iota // handler error/timeout -> NoDecision
	FailClosed                 // handler error/timeout -> Deny (where possible)
)

// DegradeMode governs unsupported decisions (e.g. Ask on Codex).
type DegradeMode int

const (
	// Degrade maps to the nearest supported intent (Ask->Deny or
	// Ask->NoDecision per AskFallback) and logs the downgrade.
	Degrade DegradeMode = iota
	// Strict treats an unsupported decision as a handler error, which then
	// follows Policy.Fail.
	Strict
)

// AskFallback selects the degradation target when Ask is unsupported.
type AskFallback int

const (
	FallbackNoDecision AskFallback = iota
	FallbackDeny
)

// DefaultContinuationCap bounds ContinueWith loops on providers without a
// native cap (Cursor Claude-compat mode ships loop_limit: null).
const DefaultContinuationCap = 5

// defaultDeadline is used when no --timeout flag was baked into the
// generated config: slightly under the common 60s provider default so the
// runner always answers rather than getting killed mid-write.
const defaultDeadline = 55 * time.Second

// Policy declares how the runner behaves when a handler fails or asks for
// something the provider can't do. FailClosed is enforced with the provider's
// real mechanism per event; on events with no blocking mechanism it
// downgrades to logging (see Can(CapDeny)).
type Policy struct {
	Fail        FailMode
	Unsupported DegradeMode
	AskFallback AskFallback
	// ContinuationCap caps ContinueWith loops; 0 means DefaultContinuationCap.
	ContinuationCap int
	// Timeout bounds handler execution. 0 derives a deadline from the
	// --timeout flag in the generated config (90% of it) or defaultDeadline.
	Timeout time.Duration
}

func (p Policy) continuationCap() int {
	if p.ContinuationCap > 0 {
		return p.ContinuationCap
	}
	return DefaultContinuationCap
}

// PolicyFunc resolves policy per event, enabling consumer patterns like a
// ratchet (fail-open until first success, then fail-closed).
type PolicyFunc func(*Event) Policy
