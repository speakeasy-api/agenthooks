package agenthooks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// serve is the long-lived daemon mode behind the in-process-plugin shims
// (OpenCode §8, OpenClaw). Frames are processed sequentially, matching the
// providers' per-session hook semantics (open question #1 resolved
// conservatively). The shim owns the timeout policy the provider lacks; the
// daemon still bounds each handler with the resolved Policy deadline.
func (r *Runner) serve(ctx context.Context, inv *invocation, stdin io.Reader, stdout, stderr io.Writer) int {
	if inv.provider == "" {
		inv.provider = ProviderOpenCode
	}
	if inv.provider == ProviderOpenClaw {
		return r.serveOpenClaw(ctx, inv, stdin, stdout)
	}
	if inv.provider != ProviderOpenCode {
		_, _ = fmt.Fprintf(stderr, "agenthooks: serve mode supports --provider=opencode or --provider=openclaw, got %q\n", inv.provider)
		return 64
	}

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), maxPayloadBytes)
	enc := json.NewEncoder(stdout)

	var serverInfo struct {
		ServerURL string `json:"serverUrl"`
		Directory string `json:"directory"`
		Worktree  string `json:"worktree"`
		MCP       []mcpConfigEntry
		MCPExact  bool
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var fr opencodeFrame
		if err := json.Unmarshal(line, &fr); err != nil {
			r.logger.Error("agenthooks: bad shim frame", "error", err)
			continue
		}
		// The shim's first runtime hook sends server info plus the resolved MCP
		// inventory; omitted MCP falls back to direct config reads.
		if fr.Hook == "initialize" {
			var info struct {
				ServerURL string                      `json:"serverUrl"`
				Directory string                      `json:"directory"`
				Worktree  string                      `json:"worktree"`
				MCP       *map[string]opencodeMCPJSON `json:"mcp"`
			}
			if json.Unmarshal(fr.Input, &info) == nil {
				serverInfo.ServerURL = info.ServerURL
				serverInfo.Directory = info.Directory
				serverInfo.Worktree = info.Worktree
				serverInfo.MCPExact = info.MCP != nil
				serverInfo.MCP = nil
				if info.MCP != nil {
					serverInfo.MCP = openCodeMCPEntries(*info.MCP)
				}
			}
			_ = enc.Encode(opencodeReply{Seq: fr.Seq})
			continue
		}

		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		typed, err := decodeOpenCodeFrame(inv.variant, DetectionConfig, r.now(), &fr, lineCopy)
		if err != nil {
			r.logger.Error("agenthooks: decode failed", "hook", fr.Hook, "error", err)
			_ = enc.Encode(opencodeReply{Seq: fr.Seq})
			continue
		}
		base := eventOf(typed)
		if base.Session.CWD == "" {
			base.Session.CWD = serverInfo.Directory
			base.Session.WorkspaceRoots = rootsFor(serverInfo.Directory)
		}
		tool := toolOf(typed)
		reportInventory := shouldReportMCPInventory(base, tool)
		inventory, inventoryComplete := serverInfo.MCP, serverInfo.MCPExact
		if !inventoryComplete && (tool != nil || reportInventory) {
			inventory = loadMCPConfigEntries(ProviderOpenCode, base.Session.CWD)
		}
		if tool != nil {
			r.resolveMCPWithOpenCodeInventory(typed, &inventory)
			reportInventory = shouldReportMCPInventory(base, tool)
		}
		pol := r.policy(base)
		deadline := pol.Timeout
		if deadline == 0 {
			deadline = defaultDeadline
		}
		if reportInventory {
			inventoryCtx, inventoryCancel := context.WithTimeout(withLogger(ctx, r.logger), deadline)
			err := r.reportMCPInventorySnapshot(inventoryCtx, base, inventory, inventoryComplete)
			inventoryCancel()
			if err != nil {
				r.logger.Error("agenthooks: MCP inventory handler failed", "error", err)
			}
		}
		hctx, cancel := context.WithTimeout(withLogger(ctx, r.logger), deadline)
		core, herr := r.dispatch(hctx, typed)
		cancel()
		if herr != nil {
			r.logger.Error("agenthooks: handler failed", "hook", fr.Hook, "error", herr)
			core = failCore(pol, base)
		}
		core = r.applyPolicy(typed, base, core, pol)

		reply, encErr := encodeOpenCodeReply(typed, base, core)
		if encErr != nil {
			r.logger.Error("agenthooks: encode failed", "hook", fr.Hook, "error", encErr)
			reply = &opencodeReply{}
		}
		reply.Seq = fr.Seq
		if err := enc.Encode(reply); err != nil {
			r.logger.Error("agenthooks: writing reply", "error", err)
			return 1
		}
	}
	if err := sc.Err(); err != nil {
		r.logger.Error("agenthooks: reading shim stream", "error", err)
		return 1
	}
	return 0
}

// errFrameTooLong marks an NDJSON line that exceeded maxPayloadBytes. The
// serve loop discards the line and continues: killing the daemon over one
// oversized frame would turn every later gate into a shim timeout (and a
// local block under fail-closed shims).
var errFrameTooLong = errors.New("agenthooks: frame exceeds maximum size")

// readBoundedLine returns the next newline-terminated line up to max bytes.
// An over-long line is drained through its newline and reported as
// errFrameTooLong with a nil payload. Returns io.EOF with a nil payload at
// end of stream (a final unterminated line is returned without error).
func readBoundedLine(r *bufio.Reader, maxBytes int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxBytes {
			// Drain the rest of the oversized line. A genuine stream error
			// during the drain must surface as itself: bufio.Reader hands out
			// its stored error only once, so masking it as errFrameTooLong
			// would make the caller continue on a broken stream.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				return nil, err
			}
			return nil, errFrameTooLong
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return bytes.TrimSuffix(buf, []byte("\n")), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if len(buf) > 0 {
				return buf, nil
			}
			return nil, err
		}
	}
}

// openclawObserveQueue is the FIFO between the serve loop and the observe
// worker. Its two constraints pull in opposite directions: a blocking bound
// would let telemetry backpressure stall the loop and delay gate frames,
// while no bound at all could exhaust a long-lived Gateway child if a
// consumer's handlers are persistently slower than the frame rate. So push
// never blocks: past openclawQueueMaxDepth the oldest queued frame is
// dropped, and drops are counted and logged so the loss is visible.
type openclawObserveQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []any
	closed  bool
	dropped int
	// warnedDepth is the high-water backlog depth already logged, so a queue
	// pinned at the cap (depth constant at a warn multiple) logs once, not on
	// every push.
	warnedDepth int
	logger      *slog.Logger
}

const (
	openclawQueueWarnDepth = 1024
	openclawQueueMaxDepth  = 4096
)

func newOpenclawObserveQueue(logger *slog.Logger) *openclawObserveQueue {
	q := &openclawObserveQueue{logger: logger}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *openclawObserveQueue) push(typed any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if len(q.items) >= openclawQueueMaxDepth {
		// Drop the oldest frame rather than block: gates must keep flowing,
		// and the newest telemetry is the most likely to still matter.
		q.items = q.items[1:]
		q.dropped++
		if q.dropped == 1 || q.dropped%openclawQueueWarnDepth == 0 {
			q.logger.Error("agenthooks: observe queue full; dropping oldest frame", "dropped_total", q.dropped, "depth", len(q.items))
		}
	}
	q.items = append(q.items, typed)
	if len(q.items)%openclawQueueWarnDepth == 0 && len(q.items) > q.warnedDepth {
		q.warnedDepth = len(q.items)
		q.logger.Warn("agenthooks: observe queue backlog", "depth", len(q.items))
	}
	q.cond.Signal()
}

// pop blocks until an item is available or the queue is closed and drained.
func (q *openclawObserveQueue) pop() (any, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return nil, false
	}
	typed := q.items[0]
	q.items = q.items[1:]
	return typed, true
}

func (q *openclawObserveQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

// serveOpenClaw is the NDJSON loop behind the generated OpenClaw shim plugin.
// It differs from the OpenCode loop in wire semantics only: replies are hook
// return values rather than mutable-output merges, there is no initialize
// frame (every frame's ctx carries its own identity), and the per-connection
// state backfills workspaceDir/model onto tool-scope frames and flips the
// after_tool_call of a denied call to a failure (quirk #37).
//
// Gating frames (tool.pre, prompt.submitted) dispatch inline so their reply
// carries the decision; every other frame is acknowledged immediately and
// dispatched on a single background worker, so a slow telemetry handler
// cannot delay a queued gate. The worker queue is unbounded (handlers are
// deadline-bounded, so it always drains) — a bounded queue would reintroduce
// gate blocking through telemetry backpressure. Observe frames keep their
// relative order on the worker; a gate may run before an earlier observe
// handler finishes.
func (r *Runner) serveOpenClaw(ctx context.Context, inv *invocation, stdin io.Reader, stdout io.Writer) int {
	br := bufio.NewReaderSize(stdin, 64<<10)
	enc := json.NewEncoder(stdout)
	st := newOpenclawServeState()

	// gateDeadline applies to gate frames only: observe frames have no shim
	// deadline to respect (their reply is already sent), so the worker bounds
	// them with the policy timeout or the default instead.
	gateDeadline := func(pol Policy, frameTimeout int64) time.Duration {
		// Gate frames carry the shim's per-hook deadline; without it a
		// handler could keep burning long after the shim gave up. The serve
		// invocation's --timeout (the max gate deadline) is the fallback.
		var shim time.Duration
		if frameTimeout > 0 {
			shim = time.Duration(frameTimeout) * time.Millisecond * 9 / 10
		} else if inv.timeout > 0 {
			shim = inv.timeout * 9 / 10
		}
		// The policy timeout can only tighten the shim deadline, never extend
		// it: once the shim has given up, a gate decision is unusable.
		switch {
		case pol.Timeout > 0 && shim > 0:
			return min(pol.Timeout, shim)
		case pol.Timeout > 0:
			return pol.Timeout
		case shim > 0:
			return shim
		}
		return defaultDeadline
	}

	observe := newOpenclawObserveQueue(r.logger)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			typed, ok := observe.pop()
			if !ok {
				return
			}
			base := eventOf(typed)
			pol := r.policy(base)
			deadline := pol.Timeout
			if deadline == 0 {
				deadline = defaultDeadline
			}
			hctx, cancel := context.WithTimeout(withLogger(ctx, r.logger), deadline)
			if _, err := r.dispatch(hctx, typed); err != nil {
				r.logger.Error("agenthooks: handler failed", "hook", base.NativeName, "error", err)
			}
			cancel()
		}
	}()
	defer workers.Wait()
	defer observe.close()

	for {
		line, err := readBoundedLine(br, maxPayloadBytes)
		if err != nil {
			if errors.Is(err, errFrameTooLong) {
				r.logger.Error("agenthooks: dropping oversized shim frame", "max_bytes", maxPayloadBytes)
				continue
			}
			if !errors.Is(err, io.EOF) {
				r.logger.Error("agenthooks: reading shim stream", "error", err)
				return 1
			}
			break
		}
		if len(line) == 0 {
			continue
		}
		var fr openclawFrame
		if err := json.Unmarshal(line, &fr); err != nil {
			r.logger.Error("agenthooks: bad shim frame", "error", err)
			continue
		}
		// The shim reports a gate it had to fail-close locally (consumer
		// unreachable or over deadline) so the denied call's after_tool_call
		// still decodes as blocked (quirks #36, #37).
		if fr.Hook == "gate_timeout" {
			var in struct {
				ToolCallID string `json:"toolCallId"`
				Reason     string `json:"reason"`
			}
			_ = json.Unmarshal(fr.Event, &in)
			if in.ToolCallID != "" {
				reason := in.Reason
				if reason == "" {
					reason = "agenthooks: hook timed out (fail-closed)"
				}
				st.blockedCalls[in.ToolCallID] = reason
			}
			_ = enc.Encode(openclawReply{Seq: fr.Seq})
			continue
		}
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		typed, err := decodeOpenClawFrame(inv.variant, DetectionConfig, r.now(), &fr, lineCopy, st)
		if err != nil {
			r.logger.Error("agenthooks: decode failed", "hook", fr.Hook, "error", err)
			_ = enc.Encode(openclawReply{Seq: fr.Seq})
			continue
		}
		base := eventOf(typed)

		if base.Kind != KindToolPre && base.Kind != KindPromptSubmitted {
			if err := enc.Encode(openclawReply{Seq: fr.Seq}); err != nil {
				r.logger.Error("agenthooks: writing reply", "error", err)
				return 1
			}
			observe.push(typed)
			continue
		}

		pol := r.policy(base)
		deadline := gateDeadline(pol, fr.TimeoutMS)
		hctx, cancel := context.WithTimeout(withLogger(ctx, r.logger), deadline)
		started := time.Now()
		core, herr := r.dispatch(hctx, typed)
		elapsed := time.Since(started)
		cancel()
		if herr != nil {
			r.logger.Error("agenthooks: handler failed", "hook", fr.Hook, "error", herr)
			core = failCore(pol, base)
		}
		// Gate latency is the number that decides whether real deployments
		// live inside the shim's wall; surface it before it becomes a timeout.
		if elapsed > deadline*4/5 {
			r.logger.Warn("agenthooks: gate ran close to its deadline",
				"hook", fr.Hook, "elapsed_ms", elapsed.Milliseconds(), "deadline_ms", deadline.Milliseconds())
		} else {
			r.logger.Debug("agenthooks: gate dispatched",
				"hook", fr.Hook, "elapsed_ms", elapsed.Milliseconds(), "deadline_ms", deadline.Milliseconds())
		}
		core = r.applyPolicy(typed, base, core, pol)

		toolCallID := ""
		if tool := toolOf(typed); tool != nil && !tool.Synthesized {
			toolCallID = tool.ID
		}
		reply := encodeOpenClawReply(base, core, st, toolCallID)
		reply.Seq = fr.Seq
		if err := enc.Encode(reply); err != nil {
			r.logger.Error("agenthooks: writing reply", "error", err)
			return 1
		}
	}
	return 0
}
