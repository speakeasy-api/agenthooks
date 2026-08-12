package ipc

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Request{
		V:     ProtocolVersion,
		Build: "abcdef0123456789",
		Argv:  []string{"--config=/x", "agenthooks", "client", "--provider=claude-code"},
		Stdin: []byte(`{"hook_event_name":"PreToolUse"}`),
		Env:   map[string]string{"TRACEPARENT": "00-11-22-01"},
		CWD:   "/work",
	}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var out Request
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if out.V != in.V || out.Build != in.Build || out.CWD != in.CWD ||
		!bytes.Equal(out.Stdin, in.Stdin) || len(out.Argv) != len(in.Argv) ||
		out.Env["TRACEPARENT"] != "00-11-22-01" {
		t.Errorf("round trip mangled the frame: %+v", out)
	}
	if buf.Len() != 0 {
		t.Errorf("frame left %d trailing bytes", buf.Len())
	}
}

func TestFrameSequenceOnOneConnection(t *testing.T) {
	// One request then one response over the same buffer, like a
	// connection carries them.
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Request{V: 1}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, Response{V: 1, Stdout: []byte("{}"), ExitCode: 2}); err != nil {
		t.Fatal(err)
	}
	var req Request
	if err := ReadFrame(&buf, &req); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := ReadFrame(&buf, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 2 || string(resp.Stdout) != "{}" {
		t.Errorf("second frame wrong: %+v", resp)
	}
}

func TestReadFrameRejectsOversizedLength(t *testing.T) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], MaxFrameBytes+1)
	var out Request
	err := ReadFrame(bytes.NewReader(prefix[:]), &out)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("oversized length prefix must be rejected before allocation: %v", err)
	}
}

func TestReadFrameTruncatedBody(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Request{V: 1}); err != nil {
		t.Fatal(err)
	}
	trunc := buf.Bytes()[:buf.Len()-2]
	var out Request
	if err := ReadFrame(bytes.NewReader(trunc), &out); err == nil {
		t.Errorf("truncated frame must error")
	}
}

func TestIdentity(t *testing.T) {
	base := Identity("/usr/local/bin/speakeasy-hooks", []string{"--config=/a.json"}, "/work/proj")
	if len(base) != 16 {
		t.Fatalf("identity length = %d, want 16", len(base))
	}
	if got := Identity("/usr/local/bin/speakeasy-hooks", []string{"--config=/a.json"}, "/work/proj"); got != base {
		t.Errorf("identity must be deterministic: %s vs %s", got, base)
	}
	if got := Identity("/usr/local/bin/speakeasy-hooks", []string{"--config=/b.json"}, "/work/proj"); got == base {
		t.Errorf("distinct configs must get distinct identities")
	}
	if got := Identity("/other/binary", []string{"--config=/a.json"}, "/work/proj"); got == base {
		t.Errorf("distinct binaries must get distinct identities")
	}
	if got := Identity("/usr/local/bin/speakeasy-hooks", []string{"--config=/a.json"}, "/work/other"); got == base {
		t.Errorf("distinct locations must get distinct identities")
	}
	if got := Identity("/usr/local/bin/speakeasy-hooks", []string{"--config=/a.json"}, ""); got == base {
		t.Errorf("the location-less identity must differ from a located one")
	}
	// The separator must keep the encoding injective across arg boundaries.
	if Identity("/bin/x", []string{"ab", "c"}, "") == Identity("/bin/x", []string{"a", "bc"}, "") {
		t.Errorf("arg boundaries must participate in the identity")
	}
	// ... and across the args/location boundary.
	if Identity("/bin/x", []string{"a"}, "b") == Identity("/bin/x", []string{"a", "b"}, "") {
		t.Errorf("a location must not collide with a trailing pre-sentinel flag")
	}
}

func TestIdentityLocationlessFallbackIsStable(t *testing.T) {
	// An empty location must reproduce the historic exe+preArgs bytes, so
	// deployments without a usable working directory do not re-key their
	// rendezvous across library upgrades.
	h := sha256.New()
	h.Write([]byte("/usr/local/bin/myhooks"))
	h.Write([]byte{0})
	h.Write([]byte("--config=/a.json"))
	want := hex.EncodeToString(h.Sum(nil))[:16]
	if got := Identity("/usr/local/bin/myhooks", []string{"--config=/a.json"}, ""); got != want {
		t.Errorf("location-less identity = %s, want the plain exe+preArgs hash %s", got, want)
	}
}

func TestLocation(t *testing.T) {
	if got := Location(""); got != "" {
		t.Errorf("empty cwd must stay empty, got %q", got)
	}
	// Nonexistent paths cannot resolve symlinks; cleaning still applies.
	sep := string(filepath.Separator)
	messy := filepath.Join(sep+"nonexistent-agenthooks-test", "proj") + sep + "." + sep + "sub" + sep + ".."
	if got, want := Location(messy), filepath.Join(sep+"nonexistent-agenthooks-test", "proj"); got != want {
		t.Errorf("Location(%q) = %q, want the cleaned path %q", messy, got, want)
	}
	// Symlinked and physical spellings of one directory must agree.
	dir := t.TempDir()
	physical := filepath.Join(dir, "real")
	if err := os.Mkdir(physical, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(physical, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err) // e.g. unprivileged windows
	}
	if Location(link) != Location(physical) {
		t.Errorf("Location must resolve symlinks: %q vs %q", Location(link), Location(physical))
	}
}

func TestBuildStamp(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin")
	if err := os.WriteFile(exe, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := BuildStamp(exe)
	if a == "" {
		t.Fatalf("stamp empty for existing file")
	}
	if b := BuildStamp(exe); b != a {
		t.Errorf("stamp must be stable for an unchanged file")
	}
	// Replacing the binary (new size or mtime) changes the stamp.
	if err := os.WriteFile(exe, []byte("v2-bigger"), 0o755); err != nil {
		t.Fatal(err)
	}
	if b := BuildStamp(exe); b == a {
		t.Errorf("stamp must change when the executable is replaced")
	}
	if got := BuildStamp(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing file must stamp empty, got %q", got)
	}
}

func TestResolveDerivesEndpointAndLocks(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
	}
	addr, err := Resolve("/usr/local/bin/myhooks", []string{"--config=/a.json"}, "/work/proj")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if addr.ID == "" || addr.Endpoint == "" || addr.ServerLock == "" || addr.SpawnLock == "" {
		t.Fatalf("incomplete address: %+v", addr)
	}
	if !strings.Contains(addr.Endpoint, addr.ID) {
		t.Errorf("endpoint must embed the identity: %+v", addr)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(addr.Endpoint, `\\.\pipe\agenthooks-`) {
			t.Errorf("windows endpoint must be a named pipe: %s", addr.Endpoint)
		}
	} else {
		if !strings.HasSuffix(addr.Endpoint, ".sock") || len(addr.Endpoint) > maxSocketPath {
			t.Errorf("unix endpoint must be a bounded socket path: %s (%d bytes)", addr.Endpoint, len(addr.Endpoint))
		}
		if fi, err := os.Stat(filepath.Dir(addr.ServerLock)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("state dir must exist with 0700: %v %v", fi, err)
		}
	}
	if addr.ServerLock == addr.SpawnLock {
		t.Errorf("server and spawn locks must differ: %+v", addr)
	}

	other, err := Resolve("/usr/local/bin/myhooks", []string{"--config=/b.json"}, "/work/proj")
	if err != nil {
		t.Fatal(err)
	}
	if other.Endpoint == addr.Endpoint {
		t.Errorf("distinct configs must rendezvous on distinct endpoints")
	}
	elsewhere, err := Resolve("/usr/local/bin/myhooks", []string{"--config=/a.json"}, "/work/other")
	if err != nil {
		t.Fatal(err)
	}
	if elsewhere.Endpoint == addr.Endpoint {
		t.Errorf("distinct locations must rendezvous on distinct endpoints")
	}
}

func TestResolveEndpointOverride(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
	}
	explicit := `\\.\pipe\agenthooks-test-override`
	if runtime.GOOS != "windows" {
		// A short parent keeps the explicit path within the sun_path budget
		// regardless of how deep the test's own TempDir nests.
		dir, err := os.MkdirTemp("", "ahsock") //nolint:usetesting // t.TempDir paths can exceed the sun_path budget; this stays short
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		explicit = filepath.Join(dir, "s.sock")
	}

	addr, err := ResolveEndpoint(explicit)
	if err != nil {
		t.Fatalf("ResolveEndpoint: %v", err)
	}
	if addr.Endpoint != explicit {
		t.Errorf("explicit endpoint must be used verbatim: %q, want %q", addr.Endpoint, explicit)
	}
	if addr.ServerLock == "" || addr.SpawnLock == "" || addr.ServerLock == addr.SpawnLock {
		t.Errorf("locks must derive from the endpoint and differ: %+v", addr)
	}
	again, err := ResolveEndpoint(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if again != addr {
		t.Errorf("resolution must be deterministic so client and server agree: %+v vs %+v", again, addr)
	}

	// The explicit rendezvous must not collide with any derived one for
	// the same string (domain separation of the lock hash).
	derived, err := Resolve(explicit, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if derived.ServerLock == addr.ServerLock {
		t.Errorf("explicit and derived rendezvous must not share lock files")
	}
}

func TestResolveEndpointRejectsOverlongUnixPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes have no path-length constraint")
	}
	long := "/" + strings.Repeat("x", maxSocketPath) + "/server.sock"
	_, err := ResolveEndpoint(long)
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("overlong socket path must be rejected with a clear error, got %v", err)
	}
}

func TestSocketPathLengthFallsBackToTempDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes have no path-length constraint")
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), strings.Repeat("deep", 30)))
	addr, err := Resolve("/usr/local/bin/myhooks", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(addr.Endpoint) > maxSocketPath {
		t.Errorf("endpoint exceeds sun_path budget: %s (%d bytes)", addr.Endpoint, len(addr.Endpoint))
	}
}

func TestListenDialRoundTrip(t *testing.T) {
	addr := testAddress(t)
	ln, err := Listen(addr.Endpoint)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		var req Request
		if err := ReadFrame(conn, &req); err != nil {
			done <- err
			return
		}
		done <- WriteFrame(conn, Response{V: 1, Stdout: []byte("ok"), ExitCode: 0})
	}()

	conn, err := Dial(addr.Endpoint, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := WriteFrame(conn, Request{V: 1}); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Stdout) != "ok" {
		t.Errorf("response = %+v", resp)
	}
	if err := <-done; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

func TestListenDetectsLiveServer(t *testing.T) {
	addr := testAddress(t)
	ln, err := Listen(addr.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	// Keep the listener accepting so the probe's dial succeeds.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if _, err := Listen(addr.Endpoint); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Listen = %v, want ErrAlreadyRunning", err)
	}
}

func TestListenSweepsStaleSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pipe instances die with their process; no stale files on windows")
	}
	addr := testAddress(t)
	ln, err := Listen(addr.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: the socket file stays behind with nobody listening.
	if ul, ok := ln.(interface{ SetUnlinkOnClose(bool) }); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = ln.Close()
	if _, err := os.Stat(addr.Endpoint); err != nil {
		t.Skipf("platform unlinked the socket on close: %v", err)
	}

	ln2, err := Listen(addr.Endpoint)
	if err != nil {
		t.Fatalf("Listen must sweep a stale socket file: %v", err)
	}
	_ = ln2.Close()
}

// testAddress resolves an Address rooted in a per-test state dir (unix) or
// with a unique identity (windows, where pipes are process-scoped anyway).
func testAddress(t *testing.T) Address {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
	}
	addr, err := Resolve("/test/bin/agenthooks", []string{"--test-id=" + t.Name(), "--nonce=" + t.TempDir()}, "")
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
