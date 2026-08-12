package agenthooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/speakeasy-api/agenthooks/internal/filelock"
	"github.com/speakeasy-api/agenthooks/internal/ipc"
)

// The `agenthooks server` mode: a long-running singleton per rendezvous
// that hosts the full pipeline — handlers, caches, warm HTTP connections,
// and the in-process telemetry recorder — across hook invocations. Each
// connection carries exactly one framed request (a hook event with its
// argv, payload, and environment snapshot) and receives one framed
// response (the provider-dialect stdout/stderr/exit code), which the
// paired `agenthooks client` relays to the provider.
//
// The rendezvous comes from `--socket=<endpoint>` when given — clients
// always pass it when they spawn, and external supervisors should always
// pass it too, rather than relying on the server's own working directory.
// Without the flag the server derives it the same way a client does:
// executable, pre-sentinel flags, and its own normalized cwd (one server
// per project location, see internal/ipc).
//
// The server is spawned on demand by the first client that finds no
// listener, stays up while hooks keep arriving, and shuts itself down —
// flushing telemetry — after an idle period, on SIGINT/SIGTERM, or when a
// client built from a different binary shows up (the upgrade path: finish
// in-flight work, exit, let the respawn run the new code).
//
// Handlers registered on the Runner run concurrently here (one goroutine
// per connection), so consumer handlers used in client/server installs must
// be safe for concurrent use. The library's own cross-invocation state
// already is: dedup markers, backfill markers, and MCP inventory caches are
// file-based with O_EXCL/flock semantics, which serialize across goroutines
// exactly as they do across processes.

// defaultIdleTimeout is how long the server lingers without a connection
// before shutting down; override with --idle-timeout=<dur> or the
// AGENTHOOKS_SERVER_IDLE_TIMEOUT environment variable (flag wins).
const defaultIdleTimeout = 10 * time.Minute

// serveConnTimeout bounds one connection end to end: frame read, pipeline
// (whose handler deadlines are the same as run mode's), frame write.
const serveConnTimeout = 5 * time.Minute

// shutdownFlushTimeout bounds the telemetry flush on shutdown.
const shutdownFlushTimeout = 10 * time.Second

// serverMain implements the `agenthooks server` argv mode. It returns 0
// when another server already owns the endpoint (the spawn raced), after a
// clean shutdown, and 1 only for hard setup failures.
func (r *Runner) serverMain(ctx context.Context, inv *invocation, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agenthooks: server: resolving executable: %v\n", err)
		return 1
	}
	addr, err := resolveAddress(exe, inv)
	if err != nil {
		// Covers a rejected --socket value (e.g. over the unix sun_path
		// budget) as well as state-dir failures: a hard, loud setup error —
		// the client side of the same mistake fails open instead.
		_, _ = fmt.Fprintf(stderr, "agenthooks: server: %v\n", err)
		return 1
	}

	// The endpoint bind is the real mutual exclusion; the file lock is
	// belt-and-braces (and lets Listen distinguish a crashed server's stale
	// socket from a live one). A lock error degrades to bind-only.
	release, locked, err := filelock.TryLock(addr.ServerLock)
	if err != nil {
		r.logger.Warn("agenthooks: server lock unavailable; relying on the endpoint bind", "error", err)
	} else if !locked {
		return 0 // another server is already running
	}
	releaseOnce := sync.OnceFunc(release)
	defer releaseOnce()

	ln, err := ipc.Listen(addr.Endpoint)
	if errors.Is(err, ipc.ErrAlreadyRunning) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agenthooks: server: binding %s: %v\n", addr.Endpoint, err)
		return 1
	}

	idle := inv.idleTimeout
	if idle <= 0 {
		if env := os.Getenv("AGENTHOOKS_SERVER_IDLE_TIMEOUT"); env != "" {
			idle, _ = time.ParseDuration(env)
		}
	}
	if idle <= 0 {
		idle = defaultIdleTimeout
	}

	srv := &hookServer{
		runner:       r,
		stamp:        ipc.BuildStamp(exe),
		lastActivity: time.Now(),
		shutdown:     make(chan struct{}),
	}
	r.logger.Debug("agenthooks: server listening", "endpoint", addr.Endpoint, "idle_timeout", idle)
	srv.run(ctx, ln, idle)

	// Release the rendezvous before the flush so an upgrade respawn can
	// bind while this process drains its telemetry queue.
	_ = ln.Close() // net.UnixListener unlinks the socket file on Close
	releaseOnce()
	srv.wg.Wait()
	if r.telemetryShutdown != nil {
		fctx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
		defer cancel()
		if err := r.telemetryShutdown(fctx); err != nil {
			r.logger.Warn("agenthooks: telemetry flush on shutdown failed", "error", err)
		}
	}
	return 0
}

// hookServer is the accept-loop state of one server process.
type hookServer struct {
	runner *Runner
	stamp  string // this process's executable fingerprint

	wg           sync.WaitGroup
	shutdownOnce sync.Once
	shutdown     chan struct{}

	mu           sync.Mutex
	active       int
	lastActivity time.Time
}

func (s *hookServer) requestShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

func (s *hookServer) connOpened() {
	s.mu.Lock()
	s.active++
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *hookServer) connClosed() {
	s.mu.Lock()
	s.active--
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *hookServer) idleSince(limit time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active == 0 && time.Since(s.lastActivity) >= limit
}

// run accepts connections until a shutdown trigger fires: idle timeout,
// SIGINT/SIGTERM, context cancellation, or a version-mismatch drain.
func (s *hookServer) run(ctx context.Context, ln net.Listener, idle time.Duration) {
	// Every exit path marks the server as shutting down so the accept
	// goroutine can never stay blocked handing over a late connection.
	defer s.requestShutdown()
	conns := make(chan net.Conn)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed (shutdown) or fatal accept error
			}
			select {
			case conns <- conn:
			case <-s.shutdown:
				_ = conn.Close()
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	tick := max(idle/4, 10*time.Millisecond)
	idleTicker := time.NewTicker(tick)
	defer idleTicker.Stop()

	for {
		select {
		case conn := <-conns:
			s.connOpened()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer s.connClosed()
				s.serveConn(ctx, conn)
			}()
		case <-idleTicker.C:
			if s.idleSince(idle) {
				s.runner.logger.Debug("agenthooks: server idle; shutting down")
				return
			}
		case <-sig:
			s.requestShutdown()
			return
		case <-ctx.Done():
			return
		case <-s.shutdown:
			return
		case <-acceptDone:
			return
		}
	}
}

// serveConn handles one framed request/response exchange.
func (s *hookServer) serveConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	defer func() {
		// The pipeline guards handler panics itself; this guard keeps a
		// decode/transport panic on one connection from killing the server.
		if p := recover(); p != nil {
			s.runner.logger.Error("agenthooks: connection handler panic", "panic", p)
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(serveConnTimeout))

	var req ipc.Request
	if err := ipc.ReadFrame(conn, &req); err != nil {
		s.runner.logger.Warn("agenthooks: reading request frame", "error", err)
		return
	}
	if req.V != ipc.ProtocolVersion {
		s.respond(conn, ipc.Response{
			V:        ipc.ProtocolVersion,
			Error:    fmt.Sprintf("protocol version mismatch: server speaks %d, client sent %d", ipc.ProtocolVersion, req.V),
			ExitCode: 1,
		})
		return
	}
	if req.Build != "" && s.stamp != "" && req.Build != s.stamp {
		// The client runs a different build of this binary: serve this
		// request, then drain and exit so the next spawn picks up the new
		// executable — the LSP-style upgrade story.
		s.runner.logger.Debug("agenthooks: client build differs; draining after this request", "client", req.Build, "server", s.stamp)
		defer s.requestShutdown()
	}

	inv, err := parseArgs(req.Argv)
	if err != nil {
		s.respond(conn, ipc.Response{V: ipc.ProtocolVersion, Stderr: []byte(err.Error() + "\n"), ExitCode: 64})
		return
	}
	var payload []byte
	if inv.mode == "notify" || inv.argvPayload {
		payload = []byte(inv.payload)
	} else {
		payload = req.Stdin
		if len(payload) > maxPayloadBytes {
			payload = payload[:maxPayloadBytes]
		}
	}

	acked := false
	opts := runOpts{
		getenv: func(key string) string { return req.Env[key] },
		earlyAck: func(wire wireResponse) {
			acked = true
			s.respond(conn, ipc.Response{
				V:        ipc.ProtocolVersion,
				Stdout:   wire.Stdout,
				Stderr:   wire.Stderr,
				ExitCode: wire.ExitCode,
			})
			_ = conn.Close() // the client is free; processing continues here
		},
	}
	var stdout, stderrBuf bytes.Buffer
	code := s.runner.runEvent(ctx, inv, payload, opts, &stdout, &stderrBuf)
	if acked {
		return
	}
	s.respond(conn, ipc.Response{
		V:        ipc.ProtocolVersion,
		Stdout:   stdout.Bytes(),
		Stderr:   stderrBuf.Bytes(),
		ExitCode: code,
	})
}

func (s *hookServer) respond(conn net.Conn, resp ipc.Response) {
	if err := ipc.WriteFrame(conn, resp); err != nil {
		s.runner.logger.Warn("agenthooks: writing response frame", "error", err)
	}
}
