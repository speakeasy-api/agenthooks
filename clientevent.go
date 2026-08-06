package agenthooks

import "context"

// DetectionConfidence records how the invoking provider was identified.
// Detection happens on the client, so the value rides ClientEvent rather
// than the core envelope.
type DetectionConfidence string

const (
	DetectionConfig DetectionConfidence = "config" // --provider flag from generated config
	DetectionEnv    DetectionConfidence = "env"    // environment variable sniffing
	DetectionShape  DetectionConfidence = "shape"  // payload shape sniffing
)

// LocalSession carries the machine-local session context: paths and modes
// that are only meaningful on the machine the hook fired on, split out of
// the core SessionInfo so the envelope stays transportable.
type LocalSession struct {
	TranscriptPath string   // "" if unavailable; format is provider-specific (see transcript pkg)
	WorkspaceRoots []string // cursor multi-root; others: [CWD] or project dir
	PermissionMode string   // claude/codex permission_mode; "" elsewhere
}

// ClientEvent pairs one decoded typed event with the client-only context
// the core envelope deliberately does not carry. The client runner builds
// it at decode time and installs it on the handler context; handlers that
// need client-local data reach it via ClientEventFromContext — the mirror
// of a server-side embedder installing request state on the context before
// Decide.
type ClientEvent struct {
	// Typed is the decoded typed event (*ToolPreEvent, *PromptEvent, ...)
	// this client context belongs to.
	Typed any

	// DetectionConfidence records how the invoking provider was identified.
	DetectionConfidence DetectionConfidence

	// Backfilled marks an event the provider never sent: some providers skip
	// events in some modes (quirks #30, #31), and the runner synthesizes the
	// miss on the next delivered event, best-effort. Backfilled events are
	// reporting-only — Raw is nil, Can() reports no capabilities, and any
	// decision returned by the handler is discarded.
	Backfilled bool

	// Session is the machine-local session context.
	Session LocalSession
}

type clientEventKey struct{}

// withClientEvent installs the client context for one dispatch.
func withClientEvent(ctx context.Context, ce *ClientEvent) context.Context {
	return context.WithValue(ctx, clientEventKey{}, ce)
}

// ClientEventFromContext returns the ClientEvent for the dispatch in flight,
// or nil when the handler is not running under the client runner (e.g. a
// server-side embedder driving Decide directly).
func ClientEventFromContext(ctx context.Context) *ClientEvent {
	ce, _ := ctx.Value(clientEventKey{}).(*ClientEvent)
	return ce
}
