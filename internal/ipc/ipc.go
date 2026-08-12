// Package ipc is the transport between the per-hook client process
// (`mybinary agenthooks client`) and the long-running hook server
// (`mybinary agenthooks server`): endpoint derivation from the consumer
// identity and location, length-prefixed JSON framing, and the
// request/response types. Unix domain sockets everywhere except Windows,
// which uses named pipes.
//
// The rendezvous is resolved one of two ways. Resolve derives it from the
// consumer identity — executable path, pre-sentinel flags, and the
// client's normalized working directory — so each deployment gets one
// server per project location (the LSP per-workspace model).
// ResolveEndpoint takes an explicit endpoint (the --socket override)
// verbatim, for externally supervised or machine-wide servers.
//
// The package is internal on purpose: the wire is an implementation detail
// of the runner's client/server modes, versioned by ProtocolVersion, not a
// public API.
package ipc

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ProtocolVersion is the framing/schema version. A server that receives a
// request with a different version answers with an error frame, which the
// client fails open on (exit 0, no output) like any other server failure.
const ProtocolVersion = 1

// MaxFrameBytes bounds one frame. The hook payload cap is 32 MiB; base64
// (JSON []byte encoding) inflates it by 4/3, and the envelope adds argv and
// environment. 64 MiB leaves comfortable headroom without letting a corrupt
// length prefix allocate unbounded memory.
const MaxFrameBytes = 64 << 20

// maxSocketPath conservatively undercuts both sun_path limits (104 bytes on
// macOS/BSD, 108 on Linux); longer derived unix socket paths fall back to
// the system temp dir, whose paths are short by construction. Windows named
// pipes have no such constraint — the constant lives here so cross-platform
// test code compiles everywhere.
const maxSocketPath = 96

// Request is one hook invocation forwarded by the client: the client's full
// argv (the server re-parses it for --provider/--timeout/--filter and the
// payload-carrying positionals of the notify verb), the raw stdin payload,
// and the slice of process state the pipeline needs (allowlisted environment
// variables, working directory).
type Request struct {
	V int `json:"v"`
	// Build fingerprints the client's executable (path, size, mtime). On
	// mismatch with the server's own fingerprint the server finishes
	// in-flight work, flushes telemetry, and exits, so the next spawn runs
	// the new binary — the LSP-style upgrade story.
	Build string            `json:"build,omitempty"`
	Argv  []string          `json:"argv,omitempty"`
	Stdin []byte            `json:"stdin,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	CWD   string            `json:"cwd,omitempty"`
}

// Response carries the provider-dialect wire output back to the client,
// which relays it verbatim: stdout bytes, stderr bytes (Kimi's blocking
// mechanism is exit 2 with the reason on stderr), and the exit code. A
// non-empty Error means the server could not process the request at the
// protocol level; the client treats it like an unreachable server and
// fails open (exit 0, no output).
type Response struct {
	V        int    `json:"v"`
	Error    string `json:"error,omitempty"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// ErrAlreadyRunning reports that another server already owns the endpoint.
var ErrAlreadyRunning = errors.New("ipc: a server is already listening on this endpoint")

// WriteFrame writes one length-prefixed JSON frame: a 4-byte big-endian
// length followed by the JSON body.
func WriteFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ipc: encoding frame: %w", err)
	}
	if len(body) > MaxFrameBytes {
		return fmt.Errorf("ipc: frame of %d bytes exceeds the %d-byte cap", len(body), MaxFrameBytes)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body))) //nolint:gosec // bounded by the MaxFrameBytes check above
	if _, err := w.Write(prefix[:]); err != nil {
		return fmt.Errorf("ipc: writing frame length: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("ipc: writing frame body: %w", err)
	}
	return nil
}

// ReadFrame reads one length-prefixed JSON frame into v.
func ReadFrame(r io.Reader, v any) error {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return fmt.Errorf("ipc: reading frame length: %w", err)
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n > MaxFrameBytes {
		return fmt.Errorf("ipc: frame of %d bytes exceeds the %d-byte cap", n, MaxFrameBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("ipc: reading frame body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("ipc: decoding frame: %w", err)
	}
	return nil
}

// Identity fingerprints one consumer deployment at one location: the
// executable path, the consumer flags that precede the "agenthooks"
// sentinel in the hook command (e.g. --config=/path/speakeasy.json), and
// the client's normalized working directory (see Location). Distinct
// binaries, distinct configs, or distinct project directories get distinct
// identities — and therefore distinct servers, the LSP per-workspace
// model; the per-hook flags after the sentinel (--provider, --timeout)
// deliberately do not participate. An empty location contributes nothing,
// so invocations without a usable working directory fall back to the plain
// exe+preArgs identity — byte-identical to the pre-location derivation.
func Identity(exe string, preArgs []string, location string) string {
	h := sha256.New()
	h.Write([]byte(exe))
	for _, a := range preArgs {
		h.Write([]byte{0})
		h.Write([]byte(a))
	}
	if location != "" {
		// A distinct separator keeps the location from colliding with a
		// trailing pre-sentinel flag of the same spelling.
		h.Write([]byte{1})
		h.Write([]byte(location))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Location normalizes a client working directory for identity derivation:
// filepath.Clean plus best-effort symlink resolution, so different
// spellings of the same project directory rendezvous on the same server.
// When symlinks cannot be resolved the cleaned path is used as is; an
// empty cwd stays empty (the location-less identity fallback).
func Location(cwd string) string {
	if cwd == "" {
		return ""
	}
	cleaned := filepath.Clean(cwd)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	return cleaned
}

// BuildStamp fingerprints the executable file behind path — path, size, and
// mtime — cheaply enough to compute per hook invocation. Replacing the
// binary on disk changes the stamp, which is what triggers the server's
// upgrade shutdown. Returns "" when the file cannot be inspected; empty
// stamps never trigger a mismatch.
func BuildStamp(exe string) string {
	fi, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(exe))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(fi.Size(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(fi.ModTime().UnixNano(), 10)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Address is the resolved rendezvous for one consumer identity.
type Address struct {
	// ID is the consumer identity hash (see Identity).
	ID string
	// Endpoint is the unix socket path, or the named-pipe name on Windows.
	Endpoint string
	// ServerLock is held for the server's lifetime — belt-and-braces
	// singleton enforcement alongside the endpoint bind itself.
	ServerLock string
	// SpawnLock serializes client auto-spawns so a thundering herd of hook
	// invocations starts one server, not dozens.
	SpawnLock string
}

// Resolve derives the Address for a consumer identity at a location (the
// client's normalized working directory — see Location; empty means
// location-less) and ensures the state directory exists (0700). The
// endpoint embeds only the identity hash, never the location path itself,
// so derived unix socket paths stay within sun_path limits no matter how
// deep the project directory is.
func Resolve(exe string, preArgs []string, location string) (Address, error) {
	dir, err := ensureStateDir()
	if err != nil {
		return Address{}, err
	}
	id := Identity(exe, preArgs, location)
	return Address{
		ID:         id,
		Endpoint:   endpoint(dir, id),
		ServerLock: lockPath(dir, id, ".lock"),
		SpawnLock:  lockPath(dir, id, ".spawn"),
	}, nil
}

// ResolveEndpoint is the --socket override: the Address for an explicitly
// chosen endpoint (a unix socket path; a named-pipe name on Windows),
// used verbatim. Identity derivation is bypassed entirely; the lock files
// derive from a hash of the endpoint in the state dir, so a client and a
// server handed the same --socket agree on the whole rendezvous without
// consulting their own process state. Unix endpoints beyond the sun_path
// budget are rejected here, before any bind or dial.
func ResolveEndpoint(explicit string) (Address, error) {
	if err := validateEndpoint(explicit); err != nil {
		return Address{}, err
	}
	dir, err := ensureStateDir()
	if err != nil {
		return Address{}, err
	}
	// Domain-separated from Identity so an explicit endpoint can never
	// collide with a derived identity's lock files.
	sum := sha256.Sum256([]byte("endpoint\x00" + explicit))
	id := hex.EncodeToString(sum[:])[:16]
	return Address{
		ID:         id,
		Endpoint:   explicit,
		ServerLock: lockPath(dir, id, ".lock"),
		SpawnLock:  lockPath(dir, id, ".spawn"),
	}, nil
}

// ensureStateDir resolves the state directory for sockets and locks and
// creates it (0700) if needed.
func ensureStateDir() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", fmt.Errorf("ipc: resolving state dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ipc: creating state dir: %w", err)
	}
	return dir, nil
}

// dialProbe reports whether something answers on the endpoint right now.
func dialProbe(endpoint string) bool {
	conn, err := Dial(endpoint, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
