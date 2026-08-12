package agenthooks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks/internal/ipc"
)

// The in-process client/server suite: servers run as goroutines via
// Runner.Run (the same entry the argv mode uses) and clients connect through
// the real socket/pipe transport in internal/ipc. Auto-spawn is exercised
// through the Runner's spawn seam; the true detached re-exec is covered by
// the subprocess test in clientserver_e2e_test.go.

const denyWire = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"server says no"}}`

// testIdentity isolates one test's rendezvous: a per-test state dir (unix
// sockets and locks) plus unique pre-sentinel args (which hash into the
// endpoint name, so Windows pipes are unique too).
func testIdentity(t *testing.T) []string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return []string{"--config=" + filepath.Join(t.TempDir(), "cfg.json")}
}

func serverRunArgs(preArgs []string, idle string, extra ...string) []string {
	args := append(append([]string(nil), preArgs...), "agenthooks", "server")
	if idle != "" {
		args = append(args, "--idle-timeout="+idle)
	}
	return append(args, extra...)
}

func clientRunArgs(preArgs []string, extra ...string) []string {
	args := append(append([]string(nil), preArgs...), "agenthooks", "client")
	return append(args, extra...)
}

// denyServerRunner is a hermetic server-side Runner that denies tool.pre.
func denyServerRunner(t *testing.T) *Runner {
	t.Helper()
	r := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill())
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("server says no"), nil
	})
	return r
}

// startServer runs the server mode in a goroutine and blocks until it
// accepts connections. The returned channel yields the exit code.
func startServer(t *testing.T, r *Runner, args []string) chan int {
	t.Helper()
	exit := make(chan int, 1)
	go func() {
		exit <- r.Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard)
	}()
	waitForServer(t, args)
	return exit
}

// waitForServer polls the endpoint the args resolve to (explicit --socket,
// or the identity derived from the pre-sentinel flags and this process's
// cwd) until something accepts.
func waitForServer(t *testing.T, args []string) {
	t.Helper()
	inv, err := parseArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := resolveAddress(exe, inv)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := ipc.Dial(addr.Endpoint, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s: %v", addr.Endpoint, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitExit(t *testing.T, exit chan int, what string) int {
	t.Helper()
	select {
	case code := <-exit:
		return code
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never exited", what)
		return -1
	}
}

// explicitEndpoint returns a --socket value valid on this OS: a short
// unix socket path (a fresh short-prefix temp dir keeps it well under the
// sun_path budget regardless of the test TempDir's depth), or a uniquely
// named pipe on Windows.
func explicitEndpoint(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\agenthooks-test-` + strings.ReplaceAll(t.Name(), "/", "-")
	}
	dir, err := os.MkdirTemp("", "ahsock") //nolint:usetesting // t.TempDir paths can exceed the sun_path budget; this stays short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// noSpawn disables auto-spawn so a client test fails fast instead of
// re-execing the test binary.
func noSpawn(r *Runner) {
	r.spawnServer = func([]string, string) error { return errors.New("spawning disabled in this test") }
}

func TestClientServerGatingRoundTrip(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	// The client's own handler would allow: a deny response proves the
	// decision came over the wire from the server, not from this process.
	client := quietRunner()
	noSpawn(client)
	client.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Allow(), nil
	})
	for i := 0; i < 2; i++ { // second request reuses the same server
		out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/pre_tool_use.json"))
		if out != denyWire || code != 0 {
			t.Fatalf("request %d: got %q (exit %d), want the server's deny", i, out, code)
		}
	}
	if code := waitExit(t, exit, "idle server"); code != 0 {
		t.Errorf("server exit = %d, want 0", code)
	}
}

func TestClientRelaysExitCodeAndStderr(t *testing.T) {
	preArgs := testIdentity(t)
	// Kimi's prompt-blocking mechanism is exit 2 with the reason on stderr
	// (quirk #23): the response frame must carry all three channels back.
	server := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill())
	server.OnPromptSubmitted(func(ctx context.Context, e *PromptEvent) (PromptDecision, error) {
		return BlockPrompt("kimi block"), nil
	})
	exit := startServer(t, server, serverRunArgs(preArgs, "5s"))

	client := quietRunner()
	noSpawn(client)
	var out, errb bytes.Buffer
	code := client.Run(context.Background(), clientRunArgs(preArgs, "--provider=kimi-code"),
		bytes.NewReader(kimiPrompt("sess-cs-kimi")), &out, &errb)
	if code != 2 {
		t.Errorf("exit = %d, want kimi's blocking exit 2 (stderr %q)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "kimi block") {
		t.Errorf("stderr must carry the reason: %q", errb.String())
	}
	waitExit(t, exit, "idle server")
}

func TestClientSpawnsServerOnDemand(t *testing.T) {
	preArgs := testIdentity(t)
	server := denyServerRunner(t)
	exit := make(chan int, 1)

	var spawns atomic.Int32
	client := quietRunner()
	client.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Allow(), nil
	})
	client.spawnServer = func(gotPre []string, endpoint string) error {
		spawns.Add(1)
		if len(gotPre) != len(preArgs) || gotPre[0] != preArgs[0] {
			t.Errorf("spawn must preserve pre-sentinel flags: %v", gotPre)
		}
		if endpoint == "" {
			t.Errorf("spawn must hand the resolved endpoint to the server")
		}
		// The server binds exactly the endpoint it is told, like the real
		// detached spawn ("agenthooks server --socket=<endpoint>") does.
		go func() {
			exit <- server.Run(context.Background(), serverRunArgs(gotPre, "5s", "--socket="+endpoint), strings.NewReader(""), io.Discard, io.Discard)
		}()
		return nil
	}

	out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/pre_tool_use.json"))
	if out != denyWire || code != 0 {
		t.Fatalf("got %q (exit %d), want the spawned server's deny", out, code)
	}
	if got := spawns.Load(); got != 1 {
		t.Errorf("spawns = %d, want 1", got)
	}
	waitExit(t, exit, "idle server")
}

func TestClientSpawnRaceStartsOneServer(t *testing.T) {
	preArgs := testIdentity(t)
	server := denyServerRunner(t)
	exit := make(chan int, 1)

	var spawns atomic.Int32
	spawn := func(gotPre []string, endpoint string) error {
		if spawns.Add(1) > 1 {
			return errors.New("second spawn attempted; the spawn lock failed")
		}
		go func() {
			exit <- server.Run(context.Background(), serverRunArgs(gotPre, "5s", "--socket="+endpoint), strings.NewReader(""), io.Discard, io.Discard)
		}()
		return nil
	}

	const clients = 4
	var wg sync.WaitGroup
	results := make([]string, clients)
	codes := make([]int, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := quietRunner()
			c.spawnServer = spawn
			var out, errb bytes.Buffer
			codes[i] = c.Run(context.Background(), clientRunArgs(preArgs, "--provider=claude-code"),
				bytes.NewReader(fixture(t, "claude/pre_tool_use.json")), &out, &errb)
			results[i] = out.String()
		}(i)
	}
	wg.Wait()

	for i := range results {
		if results[i] != denyWire || codes[i] != 0 {
			t.Errorf("client %d: got %q (exit %d), want the server's deny", i, results[i], codes[i])
		}
	}
	// The spawn lock admits one spawner; racing losers reconnect instead.
	if got := spawns.Load(); got != 1 {
		t.Errorf("spawns = %d, want 1 (spawn lock must serialize the herd)", got)
	}
	waitExit(t, exit, "idle server")
}

// Clients running in two different project directories derive two
// different rendezvous and therefore two servers — the LSP per-workspace
// model. Spawns are in-process via the seam; each spawned server is handed
// the client's endpoint via --socket exactly like the detached re-exec.
func TestClientPerLocationServers(t *testing.T) {
	preArgs := testIdentity(t)
	exits := make(chan int, 2)
	var mu sync.Mutex
	var endpoints []string
	spawn := func(gotPre []string, endpoint string) error {
		mu.Lock()
		endpoints = append(endpoints, endpoint)
		mu.Unlock()
		srv := denyServerRunner(t)
		go func() {
			exits <- srv.Run(context.Background(), serverRunArgs(gotPre, "5s", "--socket="+endpoint), strings.NewReader(""), io.Discard, io.Discard)
		}()
		return nil
	}

	payload := fixture(t, "claude/pre_tool_use.json") // load before leaving the package dir
	for _, dir := range []string{t.TempDir(), t.TempDir()} {
		t.Chdir(dir)
		c := quietRunner()
		c.spawnServer = spawn
		out, code := runWith(t, c, clientRunArgs(preArgs, "--provider=claude-code"), payload)
		if out != denyWire || code != 0 {
			t.Fatalf("client in %s: got %q (exit %d), want a per-location server's deny", dir, out, code)
		}
	}

	mu.Lock()
	got := append([]string(nil), endpoints...)
	mu.Unlock()
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("distinct locations must spawn distinct servers, got endpoints %v", got)
	}
	for i := 0; i < 2; i++ {
		waitExit(t, exits, "per-location server")
	}
}

// A server started with --socket binds exactly that endpoint and a client
// passing the same --socket reaches it — no identity derivation on either
// side — while the derived rendezvous stays untouched.
func TestClientServerExplicitSocket(t *testing.T) {
	preArgs := testIdentity(t)
	endpoint := explicitEndpoint(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s", "--socket="+endpoint))

	client := quietRunner()
	noSpawn(client)
	out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code", "--socket="+endpoint), fixture(t, "claude/pre_tool_use.json"))
	if out != denyWire || code != 0 {
		t.Fatalf("got %q (exit %d), want the --socket server's deny", out, code)
	}

	// A socket-less sibling derives the identity rendezvous, where nothing
	// listens: it must fail open, proving the override really is a separate
	// rendezvous rather than an alias of the derived one.
	plain := quietRunner()
	noSpawn(plain)
	var pout, perr bytes.Buffer
	pcode := plain.Run(context.Background(), clientRunArgs(preArgs, "--provider=claude-code"),
		bytes.NewReader(fixture(t, "claude/pre_tool_use.json")), &pout, &perr)
	if pcode != 0 || pout.Len() != 0 || perr.Len() != 0 {
		t.Errorf("derived-rendezvous client must fail open: %q %q (exit %d)", pout.String(), perr.String(), pcode)
	}
	waitExit(t, exit, "idle server")
}

// When a --socket client must spawn, the spawned server is handed the same
// --socket, so both land on the operator-chosen endpoint.
func TestClientSpawnPassesExplicitSocket(t *testing.T) {
	preArgs := testIdentity(t)
	endpoint := explicitEndpoint(t)
	exit := make(chan int, 1)

	client := quietRunner()
	client.spawnServer = func(gotPre []string, gotEndpoint string) error {
		if gotEndpoint != endpoint {
			t.Errorf("spawn endpoint = %q, want the client's --socket %q", gotEndpoint, endpoint)
		}
		srv := denyServerRunner(t)
		go func() {
			exit <- srv.Run(context.Background(), serverRunArgs(gotPre, "5s", "--socket="+gotEndpoint), strings.NewReader(""), io.Discard, io.Discard)
		}()
		return nil
	}
	out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code", "--socket="+endpoint), fixture(t, "claude/pre_tool_use.json"))
	if out != denyWire || code != 0 {
		t.Fatalf("got %q (exit %d), want the spawned server's deny", out, code)
	}
	waitExit(t, exit, "idle server")
}

// An overlong --socket value cannot be bound or dialed: the client fails
// open rather than surfacing an error to the provider.
func TestClientFailsOpenOnRejectedSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes have no path-length constraint")
	}
	client := quietRunner()
	noSpawn(client)
	long := "--socket=/" + strings.Repeat("x", 200) + "/s.sock"
	var out, errb bytes.Buffer
	code := client.Run(context.Background(), clientRunArgs(nil, "--provider=claude-code", long),
		bytes.NewReader(fixture(t, "claude/pre_tool_use.json")), &out, &errb)
	if code != 0 || out.Len() != 0 || errb.Len() != 0 {
		t.Errorf("rejected --socket must fail open: %q %q (exit %d)", out.String(), errb.String(), code)
	}
}

// The server side of the same mistake is loud: exit 1 with the validation
// error on stderr, at startup, before any bind.
func TestServerRejectsOverlongSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes have no path-length constraint")
	}
	srv := quietRunner()
	var out, errb bytes.Buffer
	code := srv.Run(context.Background(), serverRunArgs(nil, "", "--socket=/"+strings.Repeat("x", 200)+"/s.sock"),
		strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "at most") {
		t.Errorf("overlong --socket must fail server startup loudly: exit %d, stderr %q", code, errb.String())
	}
}

func TestClientFailsOpenWhenServerUnavailable(t *testing.T) {
	preArgs := testIdentity(t)
	client := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill())
	noSpawn(client)
	// A gating deny handler that must never run: in client mode the server
	// is a hard dependency, and an unreachable server means fail open —
	// never a silent in-process run of the pipeline.
	client.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		t.Error("client mode must never run the pipeline in-process")
		return Deny("must not run"), nil
	})

	start := time.Now()
	var out, errb bytes.Buffer
	code := client.Run(context.Background(), clientRunArgs(preArgs, "--provider=claude-code"),
		bytes.NewReader(fixture(t, "claude/pre_tool_use.json")), &out, &errb)
	if code != 0 || out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("unreachable server must fail open (exit 0, no output): stdout %q, stderr %q (exit %d)",
			out.String(), errb.String(), code)
	}
	// The fail-open must be prompt: a failed spawn errors the seam
	// immediately, so at most the connect/spawn retry budget (plus slack)
	// precedes the exit.
	if elapsed := time.Since(start); elapsed > clientSpawnBudget+3*time.Second {
		t.Errorf("fail-open took %s, want under the spawn budget plus slack", elapsed)
	}
}

func TestServerEarlyAcksNonGatingEvents(t *testing.T) {
	preArgs := testIdentity(t)
	handlerDone := make(chan struct{})
	release := make(chan struct{})
	server := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill())
	server.OnNotification(func(ctx context.Context, e *NotificationEvent) error {
		<-release
		close(handlerDone)
		return nil
	})
	exit := startServer(t, server, serverRunArgs(preArgs, "5s"))

	client := quietRunner()
	noSpawn(client)
	out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/notification.json"))
	if code != 0 || out != "{}" {
		t.Fatalf("early-ack must return the provider no-op: %q (exit %d)", out, code)
	}
	select {
	case <-handlerDone:
		t.Fatalf("handler finished before the ack returned — not early-acked")
	default:
	}
	// The handler is still parked: the client got its answer first.
	close(release)
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("handler never completed after the ack")
	}
	waitExit(t, exit, "idle server")
}

func TestServerGatingEventsWaitForHandlers(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	client := quietRunner()
	noSpawn(client)
	// tool.pre is gating: the response must be the handler's decision, not
	// an early no-op.
	out, _ := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/pre_tool_use.json"))
	if out != denyWire {
		t.Errorf("gating event must carry the decision: %q", out)
	}
	waitExit(t, exit, "idle server")
}

func TestServerIdleShutdown(t *testing.T) {
	preArgs := testIdentity(t)
	start := time.Now()
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "300ms"))
	if code := waitExit(t, exit, "idle server"); code != 0 {
		t.Errorf("idle shutdown exit = %d, want 0", code)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("idle shutdown took %s", elapsed)
	}
}

func TestServerSingleton(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	// A second server for the same identity yields immediately with 0.
	second := quietRunner()
	code := second.Run(context.Background(), serverRunArgs(preArgs, "5s"), strings.NewReader(""), io.Discard, io.Discard)
	if code != 0 {
		t.Errorf("second server exit = %d, want 0 (already running)", code)
	}
	waitExit(t, exit, "idle server")
}

func TestServerVersionMismatchDrains(t *testing.T) {
	preArgs := testIdentity(t)
	// Idle long enough that only the mismatch can explain the exit.
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "2m"))

	resp := rawRequest(t, preArgs, ipc.Request{
		V:     ipc.ProtocolVersion,
		Build: "different-build-stamp",
		Argv:  clientRunArgs(preArgs, "--provider=claude-code"),
		Stdin: fixture(t, "claude/pre_tool_use.json"),
	})
	if resp.Error != "" || string(resp.Stdout) != denyWire {
		t.Fatalf("the mismatched request must still be served: %+v", resp)
	}
	if code := waitExit(t, exit, "draining server"); code != 0 {
		t.Errorf("upgrade drain exit = %d, want 0", code)
	}
}

func TestServerRejectsProtocolMismatch(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	resp := rawRequest(t, preArgs, ipc.Request{
		V:     99,
		Argv:  clientRunArgs(preArgs, "--provider=claude-code"),
		Stdin: fixture(t, "claude/pre_tool_use.json"),
	})
	if resp.Error == "" {
		t.Errorf("unknown protocol version must produce an error frame: %+v", resp)
	}
	waitExit(t, exit, "idle server")
}

func TestServerReportsBadArgv(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	resp := rawRequest(t, preArgs, ipc.Request{
		V:    ipc.ProtocolVersion,
		Argv: clientRunArgs(preArgs, "--provider=claude-code", "--timeout=bogus"),
	})
	if resp.ExitCode != 64 || !strings.Contains(string(resp.Stderr), "--timeout") {
		t.Errorf("bad argv must round-trip as exit 64 + stderr: %+v", resp)
	}
	waitExit(t, exit, "idle server")
}

func TestServerFlushesTelemetryOnShutdown(t *testing.T) {
	preArgs := testIdentity(t)
	rec := &captureRecorder{}
	server := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill(), WithTelemetry(rec))
	server.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("server says no"), nil
	})
	exit := startServer(t, server, serverRunArgs(preArgs, "500ms"))

	client := quietRunner()
	noSpawn(client)
	if out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/pre_tool_use.json")); code != 0 || out != denyWire {
		t.Fatalf("request failed: %q (exit %d)", out, code)
	}
	waitExit(t, exit, "idle server")
	if rec.records.Load() == 0 {
		t.Errorf("server-side events must reach the recorder")
	}
	if !rec.shutdown.Load() {
		t.Errorf("idle shutdown must flush the recorder via Shutdown")
	}
}

// rawRequest opens one connection to the test server and performs a framed
// exchange, bypassing clientMain (for protocol-level assertions).
func rawRequest(t *testing.T, preArgs []string, req ipc.Request) ipc.Response {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := resolveAddress(exe, &invocation{preArgs: preArgs})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := ipc.Dial(addr.Endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := ipc.WriteFrame(conn, req); err != nil {
		t.Fatal(err)
	}
	var resp ipc.Response
	if err := ipc.ReadFrame(conn, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}
