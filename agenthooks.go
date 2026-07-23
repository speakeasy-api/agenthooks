// Package agenthooks lets you author coding-agent hooks once and run them on
// Claude Code, Cursor, OpenAI Codex, Gemini CLI, OpenCode, and Kimi Code.
//
// A hook program is a normal Go binary: register typed handlers on a Runner
// and hand control to Main. The library detects the invoking provider,
// decodes stdin, dispatches, and encodes the response in the provider's
// dialect — including exit code and stderr discipline. Consumers never see
// the wire; they see typed events and return typed decisions.
//
//	func main() {
//		r := agenthooks.New(agenthooks.WithPolicy(agenthooks.Policy{
//			Fail:        agenthooks.FailClosed,
//			AskFallback: agenthooks.FallbackDeny,
//		}))
//		r.OnToolPre(func(ctx context.Context, e *agenthooks.ToolPreEvent) (agenthooks.ToolPreDecision, error) {
//			if e.Tool.Canonical == agenthooks.ToolShell && isDestructive(e.Tool) {
//				return agenthooks.Deny("blocked by policy"), nil
//			}
//			return agenthooks.NoDecision(), nil
//		})
//		agenthooks.Main(r)
//	}
package agenthooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

const maxPayloadBytes = 32 << 20

// Runner holds registered handlers and policy. One handler per unified event
// kind; OnAny is additive (observe-only) and runs regardless. Typed handlers
// gate/mutate; OnAny never does.
type Runner struct {
	policy        PolicyFunc
	logger        *slog.Logger
	now           func() time.Time
	dedupDir      string
	dedupOff      bool
	mcpResolveOff bool
	mcpListOff    bool
	backfillOff   bool
	anyHandlers   []func(context.Context, *Event) error
	otherByName   map[string][]func(context.Context, *Event) error

	hSessionStart  func(context.Context, *SessionStartEvent) (SessionStartDecision, error)
	hSessionEnd    func(context.Context, *SessionEndEvent) error
	hPrompt        func(context.Context, *PromptEvent) (PromptDecision, error)
	hToolPre       func(context.Context, *ToolPreEvent) (ToolPreDecision, error)
	hToolPost      func(context.Context, *ToolPostEvent) (ToolPostDecision, error)
	hToolError     func(context.Context, *ToolPostEvent) (ToolPostDecision, error)
	hPermission    func(context.Context, *PermissionEvent) (ToolPreDecision, error)
	hStop          func(context.Context, *StopEvent) (StopDecision, error)
	hSubagentStart func(context.Context, *SubagentStartEvent) error
	hSubagentStop  func(context.Context, *StopEvent) (StopDecision, error)
	hCompactPre    func(context.Context, *CompactEvent) error
	hCompactPost   func(context.Context, *CompactEvent) error
	hNotification  func(context.Context, *NotificationEvent) error
	hFileEdited    func(context.Context, *FileEditedEvent) error
	hModelRequest  func(context.Context, *ModelEvent) error
	hModelResponse func(context.Context, *ModelEvent) error
}

// Option configures a Runner.
type Option func(*Runner)

// WithPolicy sets a static policy for every event.
func WithPolicy(p Policy) Option {
	return func(r *Runner) { r.policy = func(*Event) Policy { return p } }
}

// WithPolicyFunc resolves policy per event (ratchets, per-tool strictness, ...).
func WithPolicyFunc(f PolicyFunc) Option {
	return func(r *Runner) { r.policy = f }
}

// WithLogger replaces the default logger (stderr, warn level).
func WithLogger(l *slog.Logger) Option {
	return func(r *Runner) { r.logger = l }
}

// WithDedupDir relocates the on-disk cross-process state: the dedup markers
// used for Cursor's duplicate tool events (quirk #2), the per-session
// `claude mcp list` cache (quirk #26), and the prompt-backfill markers
// (quirks #30, #31). Default: os.TempDir().
func WithDedupDir(dir string) Option {
	return func(r *Runner) { r.dedupDir = dir }
}

// stateDir roots the best-effort cross-process state files (dedup markers,
// per-session MCP inventory cache).
func (r *Runner) stateDir() string {
	if r.dedupDir != "" {
		return r.dedupDir
	}
	return os.TempDir()
}

// WithoutDedup disables Cursor duplicate-event suppression.
func WithoutDedup() Option {
	return func(r *Runner) { r.dedupOff = true }
}

// WithoutMCPResolution disables filling MCPCall.URL/Command when the payload
// carries no transport info — both the config-file fast path (quirk #25) and
// the `claude mcp list` fallback (quirk #26).
func WithoutMCPResolution() Option {
	return func(r *Runner) { r.mcpResolveOff = true }
}

// WithoutMCPListFallback disables only the once-per-session `claude mcp list`
// probe used to attribute MCP servers that appear in no config file (plugin
// and claude.ai connectors, quirk #26). Config-file resolution (quirk #25)
// stays active.
func WithoutMCPListFallback() Option {
	return func(r *Runner) { r.mcpListOff = true }
}

// WithoutBackfill disables synthesizing reporting-only events for provider
// misses (the prompt.submitted backfill for Kimi/Cursor print modes,
// quirks #30 and #31).
func WithoutBackfill() Option {
	return func(r *Runner) { r.backfillOff = true }
}

// New builds a Runner. Default policy: FailOpen, Degrade, FallbackNoDecision.
func New(opts ...Option) *Runner {
	r := &Runner{
		policy:      func(*Event) Policy { return Policy{} },
		logger:      defaultLogger(),
		now:         time.Now,
		otherByName: map[string][]func(context.Context, *Event) error{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func (r *Runner) OnSessionStart(fn func(context.Context, *SessionStartEvent) (SessionStartDecision, error)) {
	r.hSessionStart = fn
}
func (r *Runner) OnSessionEnd(fn func(context.Context, *SessionEndEvent) error) { r.hSessionEnd = fn }
func (r *Runner) OnPromptSubmitted(fn func(context.Context, *PromptEvent) (PromptDecision, error)) {
	r.hPrompt = fn
}
func (r *Runner) OnToolPre(fn func(context.Context, *ToolPreEvent) (ToolPreDecision, error)) {
	r.hToolPre = fn
}
func (r *Runner) OnToolPost(fn func(context.Context, *ToolPostEvent) (ToolPostDecision, error)) {
	r.hToolPost = fn
}
func (r *Runner) OnToolError(fn func(context.Context, *ToolPostEvent) (ToolPostDecision, error)) {
	r.hToolError = fn
}
func (r *Runner) OnPermission(fn func(context.Context, *PermissionEvent) (ToolPreDecision, error)) {
	r.hPermission = fn
}
func (r *Runner) OnStop(fn func(context.Context, *StopEvent) (StopDecision, error)) { r.hStop = fn }
func (r *Runner) OnSubagentStart(fn func(context.Context, *SubagentStartEvent) error) {
	r.hSubagentStart = fn
}
func (r *Runner) OnSubagentStop(fn func(context.Context, *StopEvent) (StopDecision, error)) {
	r.hSubagentStop = fn
}
func (r *Runner) OnCompactPre(fn func(context.Context, *CompactEvent) error)  { r.hCompactPre = fn }
func (r *Runner) OnCompactPost(fn func(context.Context, *CompactEvent) error) { r.hCompactPost = fn }
func (r *Runner) OnNotification(fn func(context.Context, *NotificationEvent) error) {
	r.hNotification = fn
}
func (r *Runner) OnFileEdited(fn func(context.Context, *FileEditedEvent) error) { r.hFileEdited = fn }
func (r *Runner) OnModelRequest(fn func(context.Context, *ModelEvent) error)    { r.hModelRequest = fn }
func (r *Runner) OnModelResponse(fn func(context.Context, *ModelEvent) error)   { r.hModelResponse = fn }

// OnAny receives EVERY event, mapped or not, with the raw payload — the
// fidelity escape hatch. Observe-only: errors are logged, never gate.
func (r *Runner) OnAny(fn func(context.Context, *Event) error) {
	r.anyHandlers = append(r.anyHandlers, fn)
}

// OnOther receives events whose native name matches, typically ones with no
// unified mapping (Kind == KindOther). Observe-only.
func (r *Runner) OnOther(nativeName string, fn func(context.Context, *Event) error) {
	r.otherByName[nativeName] = append(r.otherByName[nativeName], fn)
}

// Main parses argv/stdin, runs the registered handlers, writes the response,
// and exits with the dialect-correct code. It also redirects the process's
// stdout so stray handler prints can't corrupt the wire (Gemini parses
// stderr as the decision when stdout is empty; Codex rejects unknown JSON).
//
// When argv carries --async (rendered by install for telemetry events on
// providers without native async hooks, quirk #10), Main hands the payload
// to a detached copy of this process and exits immediately; the child runs
// the normal path without the flag. Detaching is process management, so it
// lives here rather than in the testable Run.
func Main(r *Runner) {
	if rest, ok := stripAsyncFlag(os.Args[1:]); ok {
		os.Exit(detachSelf(rest, os.Stdin, os.Stderr))
	}
	realStdout := os.Stdout
	if sink, err := logSink(); err == nil {
		os.Stdout = sink
	}
	os.Exit(r.Run(context.Background(), os.Args[1:], os.Stdin, realStdout, os.Stderr))
}

// Run is the testable core of Main: explicit streams, returns the exit code.
func (r *Runner) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	inv, err := parseArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 64
	}
	if inv.mode == "serve" {
		return r.serve(ctx, inv, stdin, stdout, stderr)
	}

	var payload []byte
	if inv.mode == "notify" || inv.argvPayload {
		payload = []byte(inv.payload)
	} else {
		payload, err = io.ReadAll(io.LimitReader(stdin, maxPayloadBytes))
		if err != nil {
			r.logger.Error("agenthooks: reading stdin", "error", err)
		}
	}

	provider, conf := detectProvider(inv, payload)
	if provider == "" {
		r.logger.Error("agenthooks: cannot detect provider; emitting neutral no-op", "payload_bytes", len(payload))
		_, _ = fmt.Fprint(stdout, "{}")
		return 0
	}
	variant := inv.variant
	if variant == VariantUnknown {
		variant = detectVariant(provider)
	}

	var typed any
	if inv.mode == "notify" {
		typed, err = decodeCodexNotify(variant, conf, r.now(), payload)
	} else {
		typed, err = decodePayload(provider, variant, conf, r.now(), payload)
	}
	if err != nil {
		r.logger.Error("agenthooks: decode failed; emitting no-op", "provider", provider, "error", err)
		wire := noOpResponse(provider)
		_, _ = stdout.Write(wire.Stdout)
		return wire.ExitCode
	}
	base := eventOf(typed)
	pol := r.policy(base)
	if tool := toolOf(typed); tool != nil {
		r.logger.Debug("agenthooks: event decoded", "native", base.NativeName, "kind", string(base.Kind), "tool", tool.Name, "session", base.Session.ID, "turn", base.Session.TurnID)
	} else {
		r.logger.Debug("agenthooks: event decoded", "native", base.NativeName, "kind", string(base.Kind))
	}

	// Attach MCP transport before the matcher filter runs: resolution can
	// repair the Server/Tool split, which MCP globs match against.
	r.resolveMCP(typed)

	// In-process matcher filter for providers whose config dialect can't
	// express the manifest matcher (§7).
	if inv.filter != nil {
		if tool := toolOf(typed); tool != nil && !inv.filter.Matches(*tool) {
			wire := noOpResponse(provider)
			_, _ = stdout.Write(wire.Stdout)
			return wire.ExitCode
		}
	}

	// Cursor fires preToolUse AND beforeShellExecution/beforeMCPExecution
	// for the same call (quirk #2): suppress the second sibling.
	if provider == ProviderCursor && !r.dedupOff && (base.Kind == KindToolPre || base.Kind == KindToolPost) {
		if r.seenDuplicate(typed) {
			r.logger.Debug("agenthooks: suppressed duplicate cursor event", "native", base.NativeName)
			wire := noOpResponse(provider)
			_, _ = stdout.Write(wire.Stdout)
			return wire.ExitCode
		}
	}

	deadline := pol.Timeout
	if deadline == 0 {
		if inv.timeout > 0 {
			deadline = inv.timeout * 9 / 10
		} else {
			deadline = defaultDeadline
		}
	}
	hctx, cancel := context.WithTimeout(withLogger(ctx, r.logger), deadline)
	defer cancel()

	// Best-effort backfill (quirks #30, #31): deliver a reporting-only
	// prompt.submitted before the first event that implies one, for sessions
	// where the provider never fired it.
	if !r.backfillOff {
		if pe, ok := typed.(*PromptEvent); ok {
			r.notePromptSeen(base, pe.Prompt)
		} else if promptImplied(base.Kind) {
			r.maybeBackfillPrompt(hctx, base)
		}
	}

	core, herr := r.dispatch(hctx, typed)
	if herr != nil {
		r.logger.Error("agenthooks: handler failed", "kind", base.Kind, "error", herr)
		core = failCore(pol, base)
	}
	core = r.applyPolicy(typed, base, core, pol)

	wire, encErr := encodeDecision(typed, core)
	if encErr != nil {
		if errors.Is(encErr, ErrLossyUpdate) && pol.Unsupported == Degrade {
			r.logger.Warn("agenthooks: dropping lossy input rewrite (shallow-merge provider)", "native", base.NativeName)
			core.hasUpdatedInput = false
			core.updatedInput = nil
			wire, encErr = encodeDecision(typed, core)
		}
		if encErr != nil {
			r.logger.Error("agenthooks: encode failed", "error", encErr)
			core = failCore(pol, base)
			wire, encErr = encodeDecision(typed, core)
			if encErr != nil {
				wire = noOpResponse(provider)
			}
		}
	}
	if len(wire.Stdout) > 0 {
		_, _ = stdout.Write(wire.Stdout)
	}
	if len(wire.Stderr) > 0 {
		// Kimi's blocking mechanism is exit 2 with the reason on stderr
		// (quirk #23); other dialects never populate Stderr.
		_, _ = stderr.Write(wire.Stderr)
	}
	return wire.ExitCode
}

// dispatch runs OnAny/OnOther observers then the typed handler, converting
// panics to errors and enforcing the context deadline even against handlers
// that ignore ctx.
func (r *Runner) dispatch(ctx context.Context, typed any) (decisionCore, error) {
	base := eventOf(typed)
	for _, h := range r.anyHandlers {
		if err := safeObserve(ctx, h, base); err != nil {
			r.logger.Warn("agenthooks: OnAny handler error", "error", err)
		}
	}
	for _, h := range r.otherByName[base.NativeName] {
		if err := safeObserve(ctx, h, base); err != nil {
			r.logger.Warn("agenthooks: OnOther handler error", "native", base.NativeName, "error", err)
		}
	}

	type result struct {
		core decisionCore
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- result{err: fmt.Errorf("agenthooks: handler panic: %v", p)}
			}
		}()
		core, err := r.invoke(ctx, typed)
		ch <- result{core: core, err: err}
	}()
	select {
	case res := <-ch:
		return res.core, res.err
	case <-ctx.Done():
		return decisionCore{}, fmt.Errorf("agenthooks: handler deadline exceeded: %w", ctx.Err())
	}
}

func (r *Runner) invoke(ctx context.Context, typed any) (decisionCore, error) {
	switch ev := typed.(type) {
	case *ToolPreEvent:
		if r.hToolPre != nil {
			d, err := r.hToolPre(ctx, ev)
			return d.core, err
		}
	case *PermissionEvent:
		if r.hPermission != nil {
			d, err := r.hPermission(ctx, ev)
			return d.core, err
		}
	case *ToolPostEvent:
		if ev.Failed && r.hToolError != nil {
			d, err := r.hToolError(ctx, ev)
			return d.core, err
		}
		if r.hToolPost != nil {
			d, err := r.hToolPost(ctx, ev)
			return d.core, err
		}
	case *PromptEvent:
		if r.hPrompt != nil {
			d, err := r.hPrompt(ctx, ev)
			return d.core, err
		}
	case *StopEvent:
		if ev.Kind == KindSubagentStop && r.hSubagentStop != nil {
			d, err := r.hSubagentStop(ctx, ev)
			return d.core, err
		}
		if ev.Kind == KindStop && r.hStop != nil {
			d, err := r.hStop(ctx, ev)
			return d.core, err
		}
	case *SessionStartEvent:
		if r.hSessionStart != nil {
			d, err := r.hSessionStart(ctx, ev)
			return d.core, err
		}
	case *SessionEndEvent:
		if r.hSessionEnd != nil {
			return decisionCore{kind: decObserved}, r.hSessionEnd(ctx, ev)
		}
	case *SubagentStartEvent:
		if r.hSubagentStart != nil {
			return decisionCore{kind: decObserved}, r.hSubagentStart(ctx, ev)
		}
	case *CompactEvent:
		if ev.Kind == KindCompactPre && r.hCompactPre != nil {
			return decisionCore{kind: decObserved}, r.hCompactPre(ctx, ev)
		}
		if ev.Kind == KindCompactPost && r.hCompactPost != nil {
			return decisionCore{kind: decObserved}, r.hCompactPost(ctx, ev)
		}
	case *NotificationEvent:
		if r.hNotification != nil {
			return decisionCore{kind: decObserved}, r.hNotification(ctx, ev)
		}
	case *FileEditedEvent:
		if r.hFileEdited != nil {
			return decisionCore{kind: decObserved}, r.hFileEdited(ctx, ev)
		}
	case *ModelEvent:
		if ev.Kind == KindModelRequest && r.hModelRequest != nil {
			return decisionCore{kind: decObserved}, r.hModelRequest(ctx, ev)
		}
		if ev.Kind == KindModelResponse && r.hModelResponse != nil {
			return decisionCore{kind: decObserved}, r.hModelResponse(ctx, ev)
		}
	}
	return decisionCore{kind: decNoDecision}, nil
}

func safeObserve(ctx context.Context, h func(context.Context, *Event) error, e *Event) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return h(ctx, e)
}

// failCore maps a handler failure to the policy outcome, using a mechanism
// the provider actually has. Where no blocking mechanism exists, FailClosed
// downgrades to logging — pretending to block on a fire-and-forget event is
// worse than being honest (§4.2).
func failCore(pol Policy, base *Event) decisionCore {
	if pol.Fail == FailClosed && Capabilities(base.Provider, base.Variant, base.Kind).Has(CapDeny) {
		switch base.Kind {
		case KindToolPre, KindPermission:
			return decisionCore{kind: decDeny, reason: "agenthooks: hook handler failed (fail-closed policy)"}
		case KindPromptSubmitted:
			return decisionCore{kind: decBlockPrompt, reason: "agenthooks: hook handler failed (fail-closed policy)"}
		}
	}
	return decisionCore{kind: decNoDecision}
}

// applyPolicy degrades (or strictly rejects) decisions the provider can't
// express, per the capability matrix and the per-native-event refinements.
func (r *Runner) applyPolicy(typed any, base *Event, d decisionCore, pol Policy) decisionCore {
	set := Capabilities(base.Provider, base.Variant, base.Kind)
	unsupported := func(what string) bool {
		if pol.Unsupported == Strict {
			r.logger.Error("agenthooks: unsupported decision under Strict policy", "what", what, "provider", base.Provider, "kind", base.Kind)
			return true
		}
		r.logger.Warn("agenthooks: degrading unsupported decision", "what", what, "provider", base.Provider, "kind", base.Kind)
		return false
	}

	switch d.kind {
	case decAsk:
		askOK := set.Has(CapAsk)
		if base.Provider == ProviderCursor && askOK {
			askOK = cursorAskSupported(base.NativeName)
		}
		if !askOK {
			if unsupported("ask") {
				return failCore(pol, base)
			}
			if pol.AskFallback == FallbackDeny {
				d.kind = decDeny
			} else {
				d.kind = decNoDecision
			}
		}
	case decDeny:
		if !set.Has(CapDeny) {
			if unsupported("deny") {
				return failCore(pol, base)
			}
			d.kind = decNoDecision
			d.reason = ""
		}
	case decBlockPrompt:
		if !set.Has(CapDeny) {
			if unsupported("block-prompt") {
				return failCore(pol, base)
			}
			d.kind = decAcceptPrompt
			d.reason = ""
		}
	case decAllow:
		if !set.Has(CapAllow) {
			if unsupported("allow") {
				return failCore(pol, base)
			}
			d.kind = decNoDecision
		}
	case decContinue:
		if !set.Has(CapContinueAgent) {
			if unsupported("continue-agent") {
				return failCore(pol, base)
			}
			d.kind = decFinish
			d.instruction = ""
		} else if st, ok := typed.(*StopEvent); ok && st.LoopCount >= pol.continuationCap() {
			r.logger.Warn("agenthooks: continuation cap reached; finishing instead", "loop_count", st.LoopCount, "cap", pol.continuationCap())
			d.kind = decFinish
			d.instruction = ""
		}
	case decFlagOutput:
		if !set.Has(CapAddContext) {
			if unsupported("flag-output") {
				return failCore(pol, base)
			}
			d.kind = decObserved
			d.reason = ""
		}
	case decReplaceOutput:
		replaceOK := set.Has(CapReplaceOutput)
		if base.Provider == ProviderCursor && replaceOK {
			replaceOK = base.NativeName == "afterMCPExecution" // MCP only (§4.1)
		}
		if !replaceOK {
			if unsupported("replace-output") {
				return failCore(pol, base)
			}
			d.kind = decObserved
			d.hasReplacedOutput = false
			d.replacedOutput = nil
		}
	}

	if d.hasUpdatedInput {
		updateOK := set.Has(CapUpdateInput)
		if base.Provider == ProviderCodex && d.kind != decAllow {
			updateOK = false // Codex updates are allow-only (§4.1)
		}
		if base.Provider == ProviderCursor && base.NativeName != "preToolUse" {
			updateOK = false // updated_input rides preToolUse only (§4.1)
		}
		if !updateOK {
			if unsupported("update-input") {
				return failCore(pol, base)
			}
			d.hasUpdatedInput = false
			d.updatedInput = nil
		}
	}
	if len(d.context) > 0 && !set.Has(CapAddContext) {
		if unsupported("add-context") {
			return failCore(pol, base)
		}
		d.context = nil
	}
	if d.systemMessage != "" && !set.Has(CapSystemMessage) {
		r.logger.Debug("agenthooks: dropping system message (unsupported)", "provider", base.Provider)
		d.systemMessage = ""
	}
	if d.stopAgent && !set.Has(CapStopAgent) {
		if unsupported("stop-agent") {
			return failCore(pol, base)
		}
		d.stopAgent = false
		d.stopReason = ""
	}
	return d
}
