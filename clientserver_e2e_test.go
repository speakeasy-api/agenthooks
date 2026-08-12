package agenthooks

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks/internal/ipc"
)

// TestClientAutoSpawnsDetachedServer exercises the real process topology:
// a consumer binary (built from testdata/hookbin) is invoked twice as
// `... agenthooks client`; the first invocation re-execs itself as a
// detached `agenthooks server`, both invocations get their decision over
// the socket/pipe from that one server process, and the server then idles
// out on its own.
func TestClientAutoSpawnsDetachedServer(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "hookbin")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "./testdata/hookbin")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building hookbin: %v", err)
	}

	// The test process and the subprocesses must agree on the rendezvous:
	// t.Setenv covers ipc.Resolve here; hookEnv covers the children (and
	// the detached server they spawn, which inherits it).
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	logPath := filepath.Join(dir, "pids.log")
	hookEnv := append(os.Environ(),
		"XDG_STATE_HOME="+stateDir,
		"HOOKBIN_LOG="+logPath,
		"AGENTHOOKS_SERVER_IDLE_TIMEOUT=1s",
	)

	runClient := func() string {
		cmd := exec.Command(bin, "agenthooks", "client", "--provider=claude-code")
		cmd.Env = hookEnv
		cmd.Stdin = bytes.NewReader(fixture(t, "claude/pre_tool_use.json"))
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("client run: %v (stderr: %s)", err, errb.String())
		}
		return out.String()
	}

	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"denied by hookbin"}}`
	for i := 0; i < 2; i++ {
		if got := runClient(); got != want {
			t.Fatalf("client %d: got %q, want %q", i, got, want)
		}
	}

	// One server pid for both invocations — the second client reused the
	// server the first one spawned; two distinct pids would mean each
	// client spawned its own server.
	pids := readPids(t, logPath)
	if len(pids) != 2 || pids[0] != pids[1] {
		t.Errorf("handler pids = %v, want the same server pid twice", pids)
	}

	// The detached server idles out on its own (1s idle timeout). Probe
	// sparsely: every accepted connection — including a probe — counts as
	// activity, so probing faster than the idle window would keep the
	// server alive forever. The rendezvous derivation must match the
	// clients': they ran with this process's cwd as their location.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := ipc.Resolve(bin, nil, ipc.Location(cwd))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		time.Sleep(3 * time.Second)
		conn, err := ipc.Dial(addr.Endpoint, 200*time.Millisecond)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("detached server never idled out on %s", addr.Endpoint)
		}
	}
}

func readPids(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading pid log: %v", err)
	}
	return strings.Fields(string(data))
}
