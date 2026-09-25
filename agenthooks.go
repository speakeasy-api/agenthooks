// Package agenthooks lets you author coding-agent hooks once and run them on
// Claude Code, Cursor, OpenAI Codex, Gemini CLI, OpenCode, Kimi Code,
// OpenClaw, and Moltis.
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

// Runner holds registered handlers and policy. Handlers per unified event
// kind are ordered stages: registration appends, dispatch runs them in order
// and the first conclusive decision wins (see Any). OnAny is additive
// (observe-only) and runs regardless. Typed handlers gate/mutate; OnAny
// never does. Middleware installed with Use wraps the typed pipeline.
type Runner struct {
	policy             PolicyFunc
	logger             *slog.Logger
	now                func() time.Time
	dedupDir           string
	dedupOff           bool
	mcpResolveOff      bool
	mcpListOff         bool
	backfillOff        bool
	mcpWarmStart       func(string)
	codexLaunchContext *codexLaunchContext
	codexMCPWarmStart  func(codexLaunchContext)
	anyHandlers        []func(context.Context, *Event) error
	otherByName        map[string][]func(context.Context, *Event) error
	interceptors       []Interceptor

	hSessionStart  []func(context.Context, *SessionStartEvent) (SessionStartDecision, error)
	hSessionEnd    []func(context.Context, *SessionEndEvent) error
	hMCPInventory  []func(context.Context, *MCPInventoryEvent) error
	hPrompt        []func(context.Context, *PromptEvent) (PromptDecision, error)
	hToolPre       []func(context.Context, *ToolPreEvent) (ToolPreDecision, error)
	hToolPost      []func(context.Context, *ToolPostEvent) (ToolPostDecision, error)
	hToolError     []func(context.Context, *ToolPostEvent) (ToolPostDecision, error)
	hPermission    []func(context.Context, *PermissionEvent) (ToolPreDecision, error)
	hStop          []func(context.Context, *StopEvent) (StopDecision, error)
	hSubagentStart []func(context.Context, *SubagentStartEvent) error
	hSubagentStop  []func(context.Context, *StopEvent) (StopDecision, error)
	hCompactPre    []func(context.Context, *CompactEvent) error
	hCompactPost   []func(context.Context, *CompactEvent) error
	hNotification  []func(context.Context, *NotificationEvent) error
	hFileEdited    []func(context.Context, *FileEditedEvent) error
	hModelRequest  []func(context.Context, *ModelEvent) error
	hModelResponse []func(context.Context, *ModelEvent) error
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
// used for Cursor's duplicate tool events (quirk #2), launch-context MCP
// inventory caches (quirks #26, #32), and the prompt-backfill markers
// (quirks #30, #31). Without this option, MCP inventories use os.UserCacheDir
// when available; other state uses os.TempDir.
func WithDedupDir(dir string) Option {
	return func(r *Runner) { r.dedupDir = dir }
}

// stateDir roots the best-effort cross-process state files (dedup markers and
// short-lived MCP inventory snapshots).
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

// WithoutMCPListFallback disables provider CLI inventory probes (`claude mcp
// list`, `codex mcp list --json`). Direct config-file resolution stays active;
// launch overrides that cannot be safely reproduced stay unattributed.
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

// Registration is variadic and stacks: repeated calls (and multiple
// arguments) append stages in order, and dispatch runs them with the Any
// semantics — a neutral decision falls through, the first conclusive
// decision wins. Compose stages with Any/All/When for richer pipelines;
// observe-only kinds (session.end, notifications, ...) run every stage.

func (r *Runner) OnSessionStart(hs ...func(context.Context, *SessionStartEvent) (SessionStartDecision, error)) {
	r.hSessionStart = append(r.hSessionStart, hs...)
}
func (r *Runner) OnSessionEnd(hs ...func(context.Context, *SessionEndEvent) error) {
	r.hSessionEnd = append(r.hSessionEnd, hs...)
}
func (r *Runner) OnMCPInventory(hs ...func(context.Context, *MCPInventoryEvent) error) {
	r.hMCPInventory = append(r.hMCPInventory, hs...)
}
func (r *Runner) OnPromptSubmitted(hs ...func(context.Context, *PromptEvent) (PromptDecision, error)) {
	r.hPrompt = append(r.hPrompt, hs...)
}
func (r *Runner) OnToolPre(hs ...func(context.Context, *ToolPreEvent) (ToolPreDecision, error)) {
	r.hToolPre = append(r.hToolPre, hs...)
}
func (r *Runner) OnToolPost(hs ...func(context.Context, *ToolPostEvent) (ToolPostDecision, error)) {
	r.hToolPost = append(r.hToolPost, hs...)
}
func (r *Runner) OnToolError(hs ...func(context.Context, *ToolPostEvent) (ToolPostDecision, error)) {
	r.hToolError = append(r.hToolError, hs...)
}
func (r *Runner) OnPermission(hs ...func(context.Context, *PermissionEvent) (ToolPreDecision, error)) {
	r.hPermission = append(r.hPermission, hs...)
}
func (r *Runner) OnStop(hs ...func(context.Context, *StopEvent) (StopDecision, error)) {
	r.hStop = append(r.hStop, hs...)
}
func (r *Runner) OnSubagentStart(hs ...func(context.Context, *SubagentStartEvent) error) {
	r.hSubagentStart = append(r.hSubagentStart, hs...)
}
func (r *Runner) OnSubagentStop(hs ...func(context.Context, *StopEvent) (StopDecision, error)) {
	r.hSubagentStop = append(r.hSubagentStop, hs...)
}
func (r *Runner) OnCompactPre(hs ...func(context.Context, *CompactEvent) error) {
	r.hCompactPre = append(r.hCompactPre, hs...)
}
func (r *Runner) OnCompactPost(hs ...func(context.Context, *CompactEvent) error) {
	r.hCompactPost = append(r.hCompactPost, hs...)
}
func (r *Runner) OnNotification(hs ...func(context.Context, *NotificationEvent) error) {
	r.hNotification = append(r.hNotification, hs...)
}
func (r *Runner) OnFileEdited(hs ...func(context.Context, *FileEditedEvent) error) {
	r.hFileEdited = append(r.hFileEdited, hs...)
}
func (r *Runner) OnModelRequest(hs ...func(context.Context, *ModelEvent) error) {
	r.hModelRequest = append(r.hModelRequest, hs...)
}
func (r *Runner) OnModelResponse(hs ...func(context.Context, *ModelEvent) error) {
	r.hModelResponse = append(r.hModelResponse, hs...)
}

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
	stdin := io.Reader(os.Stdin)
	if cwd, ok := claudeMCPWarmCWD(os.Args[1:]); ok {
		r.warmClaudeMCP(cwd)
		os.Exit(0)
	}
	if hasInternalFlag(os.Args[1:], codexMCPWarmFlag) {
		launch, _, err := decodeCodexLaunchContext(stdin)
		if err != nil {
			r.logger.Warn("agenthooks: decoding Codex MCP warm context", "error", err)
		} else {
			r.warmCodexMCP(launch)
		}
		os.Exit(0)
	}
	if hasInternalFlag(os.Args[1:], codexLaunchContextFlag) {
		launch, remaining, err := decodeCodexLaunchContext(stdin)
		stdin = remaining
		if err != nil {
			r.logger.Warn("agenthooks: decoding Codex launch context", "error", err)
		} else {
			r.codexLaunchContext = &launch
		}
	}
	if rest, ok := stripAsyncFlag(os.Args[1:]); ok {
		workerArgs, workerStdin := rest, stdin
		if inv, err := parseArgs(rest); err == nil && inv.provider == ProviderCodex {
			if launch, recovered := r.currentCodexLaunchContext(""); recovered {
				if args, input, err := encodeCodexLaunchContext(rest, stdin, launch); err == nil {
					workerArgs, workerStdin = args, input
				}
			}
		}
		os.Exit(detachSelf(workerArgs, workerStdin, os.Stderr))
	}
	mainArgs := append([]string(nil), os.Args[1:]...)
	r.mcpWarmStart = func(cwd string) {
		args := append([]string(nil), mainArgs...)
		args = append(args, claudeMCPWarmFlag+"="+cwd)
		if err := startDetachedSelf(args, nil); err != nil {
			r.logger.Warn("agenthooks: starting Claude MCP inventory warm", "error", err)
		}
	}
	r.codexMCPWarmStart = func(launch codexLaunchContext) {
		args, input, err := encodeCodexMCPWarm(mainArgs, launch)
		if err != nil {
			r.logger.Warn("agenthooks: encoding Codex MCP inventory warm", "error", err)
			return
		}
		if err := startDetachedSelf(args, input); err != nil {
			r.logger.Warn("agenthooks: starting Codex MCP inventory warm", "error", err)
		}
	}
	realStdout := os.Stdout
	if sink, err := logSink(); err == nil {
		os.Stdout = sink
	}
	os.Exit(r.Run(context.Background(), os.Args[1:], stdin, realStdout, os.Stderr))
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
	// The generated vscode-copilot file is loaded by the Copilot CLI too; when
	// the CLI is the runtime, its own env demotes the flag so the session gets
	// the CLI's capability row and flat response schema.
	provider = demoteVSCodeToCLI(provider)
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

	// Start the launch-context inventory probe without delaying SessionStart.
	// A first MCP event arriving before it finishes waits on the same cache
	// lock and consumes the worker's snapshot.
	if provider == ProviderClaudeCode && base.Kind == KindSessionStart && r.mcpWarmStart != nil && r.shouldWarmClaudeMCP(base.Session.CWD) {
		r.mcpWarmStart(base.Session.CWD)
	}
	if provider == ProviderCodex && base.Kind == KindSessionStart && r.codexMCPWarmStart != nil {
		if launch, ok := r.codexMCPWarmContext(base.Session.CWD); ok {
			r.codexMCPWarmStart(launch)
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

	// Attach MCP transport before the matcher filter runs: resolution can
	// repair the Server/Tool split, which MCP globs match against.
	r.resolveMCPContext(hctx, typed)

	// In-process matcher filter for providers whose config dialect can't
	// express the manifest matcher (§7).
	if hctx.Err() == nil && inv.filter != nil {
		if tool := toolOf(typed); tool != nil && !inv.filter.Matches(*tool) {
			wire := noOpResponse(provider)
			_, _ = stdout.Write(wire.Stdout)
			return wire.ExitCode
		}
	}

	// Cursor fires preToolUse AND beforeShellExecution/beforeMCPExecution
	// for the same call (quirk #2): suppress the second sibling.
	if hctx.Err() == nil && provider == ProviderCursor && !r.dedupOff && (base.Kind == KindToolPre || base.Kind == KindToolPost) {
		if r.seenDuplicate(typed) {
			r.logger.Debug("agenthooks: suppressed duplicate cursor event", "native", base.NativeName)
			wire := noOpResponse(provider)
			_, _ = stdout.Write(wire.Stdout)
			return wire.ExitCode
		}
	}

	// SessionStart uses only a ready cache plus cheap explicit/config reads.
	// The first MCP gate waits for provider discovery and inventory delivery.
	// All work shares the hook deadline so timeout policy applies before exit.
	tool := toolOf(typed)
	if hctx.Err() == nil && shouldReportMCPInventory(base, tool) {
		err := r.reportMCPInventory(hctx, base, base.Kind != KindSessionStart)
		if err != nil {
			r.logger.Error("agenthooks: MCP inventory handler failed", "error", err)
		}
	}

	// Best-effort backfill (quirks #30, #31): deliver a reporting-only
	// prompt.submitted before the first event that implies one, for sessions
	// where the provider never fired it.
	if hctx.Err() == nil && !r.backfillOff {
		if pe, ok := typed.(*PromptEvent); ok {
			r.notePromptSeen(base, pe.Prompt)
		} else if promptImplied(base.Kind) {
			r.maybeBackfillPrompt(hctx, base)
		}
	}

	var core decisionCore
	if err := hctx.Err(); err != nil {
		r.logger.Error("agenthooks: handler failed", "kind", base.Kind, "error", err)
		core = failCore(pol, base)
	} else {
		var herr error
		core, herr = r.dispatch(hctx, typed)
		if herr != nil {
			r.logger.Error("agenthooks: handler failed", "kind", base.Kind, "error", herr)
			core = failCore(pol, base)
		}
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

// dispatch runs OnAny/OnOther observers then the middleware-wrapped typed
// pipeline, converting panics to errors and enforcing the context deadline
// even against handlers that ignore ctx. It is the edge entry point: the
// winning decision is reduced to its core for the codecs. Decide is the
// same pipeline without the reduction.
func (r *Runner) dispatch(ctx context.Context, typed any) (decisionCore, error) {
	d, err := r.decideGuarded(ctx, typed)
	if err != nil {
		return decisionCore{}, err
	}
	return coreOfDecision(d), nil
}

// observe delivers the event to OnAny and OnOther observers. Observe-only:
// errors are logged, never gate.
func (r *Runner) observe(ctx context.Context, typed any) {
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
}

// decideGuarded runs the OnAny/OnOther observers, the middleware chain, and
// the typed handlers under the panic/deadline guard. Observers run inside the
// guard so a blocking observer that ignores ctx cannot defeat the deadline.
func (r *Runner) decideGuarded(ctx context.Context, typed any) (Decision, error) {
	type result struct {
		d   Decision
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- result{err: fmt.Errorf("agenthooks: handler panic: %v", p)}
			}
		}()
		r.observe(ctx, typed)
		d, err := r.runPipeline(ctx, typed)
		ch <- result{d: d, err: err}
	}()
	select {
	case res := <-ch:
		return res.d, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("agenthooks: handler deadline exceeded: %w", ctx.Err())
	}
}

// invoke runs the typed-handler stages for the event with the stacked
// registration semantics (see runStages) and returns the winning decision.
func (r *Runner) invoke(ctx context.Context, typed any) (Decision, error) {
	switch ev := typed.(type) {
	case *ToolPreEvent:
		return liftDecision(runStages(ctx, ev, r.hToolPre))
	case *PermissionEvent:
		return liftDecision(runStages(ctx, ev, r.hPermission))
	case *ToolPostEvent:
		if ev.Failed && len(r.hToolError) > 0 {
			return liftDecision(runStages(ctx, ev, r.hToolError))
		}
		return liftDecision(runStages(ctx, ev, r.hToolPost))
	case *PromptEvent:
		return liftDecision(runStages(ctx, ev, r.hPrompt))
	case *StopEvent:
		if ev.Kind == KindSubagentStop {
			return liftDecision(runStages(ctx, ev, r.hSubagentStop))
		}
		if ev.Kind == KindStop {
			return liftDecision(runStages(ctx, ev, r.hStop))
		}
	case *SessionStartEvent:
		return liftDecision(runStages(ctx, ev, r.hSessionStart))
	case *SessionEndEvent:
		return observeStages(ctx, ev, r.hSessionEnd)
	case *MCPInventoryEvent:
		return observeStages(ctx, ev, r.hMCPInventory)
	case *SubagentStartEvent:
		return observeStages(ctx, ev, r.hSubagentStart)
	case *CompactEvent:
		if ev.Kind == KindCompactPre {
			return observeStages(ctx, ev, r.hCompactPre)
		}
		if ev.Kind == KindCompactPost {
			return observeStages(ctx, ev, r.hCompactPost)
		}
	case *NotificationEvent:
		return observeStages(ctx, ev, r.hNotification)
	case *FileEditedEvent:
		return observeStages(ctx, ev, r.hFileEdited)
	case *ModelEvent:
		if ev.Kind == KindModelRequest {
			return observeStages(ctx, ev, r.hModelRequest)
		}
		if ev.Kind == KindModelResponse {
			return observeStages(ctx, ev, r.hModelResponse)
		}
	}
	return coreDecision{}, nil
}

// liftDecision adapts a concrete decision result to the Decision interface.
func liftDecision[D decision](d D, err error) (Decision, error) {
	return any(d).(Decision), err
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
			return decisionCore{kind: DecisionDeny, reason: "agenthooks: hook handler failed (fail-closed policy)"}
		case KindPromptSubmitted:
			return decisionCore{kind: DecisionBlockPrompt, reason: "agenthooks: hook handler failed (fail-closed policy)"}
		}
	}
	return decisionCore{kind: DecisionNoDecision}
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
	case DecisionAsk:
		askOK := set.Has(CapAsk)
		if base.Provider == ProviderCursor && askOK {
			askOK = cursorAskSupported(base.NativeName)
		}
		if !askOK {
			if unsupported("ask") {
				return failCore(pol, base)
			}
			if pol.AskFallback == FallbackDeny {
				d.kind = DecisionDeny
			} else {
				d.kind = DecisionNoDecision
			}
		}
	case DecisionDeny:
		if !set.Has(CapDeny) {
			if unsupported("deny") {
				return failCore(pol, base)
			}
			d.kind = DecisionNoDecision
			d.reason = ""
		}
	case DecisionBlockPrompt:
		if !set.Has(CapDeny) {
			if unsupported("block-prompt") {
				return failCore(pol, base)
			}
			d.kind = DecisionAcceptPrompt
			d.reason = ""
		}
	case DecisionAllow:
		if !set.Has(CapAllow) {
			if unsupported("allow") {
				return failCore(pol, base)
			}
			d.kind = DecisionNoDecision
		}
	case DecisionContinue:
		if !set.Has(CapContinueAgent) {
			if unsupported("continue-agent") {
				return failCore(pol, base)
			}
			d.kind = DecisionFinish
			d.instruction = ""
		} else if st, ok := typed.(*StopEvent); ok && st.LoopCount >= pol.continuationCap() {
			r.logger.Warn("agenthooks: continuation cap reached; finishing instead", "loop_count", st.LoopCount, "cap", pol.continuationCap())
			d.kind = DecisionFinish
			d.instruction = ""
		}
	case DecisionFlagOutput:
		if !set.Has(CapAddContext) {
			if unsupported("flag-output") {
				return failCore(pol, base)
			}
			d.kind = DecisionObserved
			d.reason = ""
		}
	case DecisionReplaceOutput:
		replaceOK := set.Has(CapReplaceOutput)
		if base.Provider == ProviderCursor && replaceOK {
			replaceOK = base.NativeName == "afterMCPExecution" // MCP only (§4.1)
		}
		if !replaceOK {
			if unsupported("replace-output") {
				return failCore(pol, base)
			}
			d.kind = DecisionObserved
			d.hasReplacedOutput = false
			d.replacedOutput = nil
		}
	}

	if d.hasUpdatedInput {
		updateOK := set.Has(CapUpdateInput)
		if base.Provider == ProviderCodex && d.kind != DecisionAllow {
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
