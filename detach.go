package agenthooks

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Self-backgrounding for providers that run every hook synchronously (Codex
// parses-but-skips async, quirk #10). The rendered command for a telemetry
// event carries --async: the parent re-execs this binary without the flag as
// a detached child, hands over the stdin payload, and exits so the provider
// unblocks immediately. Threads cannot outlive their process, so surviving
// the parent's exit requires a second process — and the only executable
// guaranteed to exist is this one.

// stripAsyncFlag reports whether args request asynchronous delivery and
// returns the args for the worker child, with every --async removed so the
// child runs the normal synchronous path.
func stripAsyncFlag(args []string) ([]string, bool) {
	found := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--async" {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, found
}

// detachSelf spawns this binary detached with the given args, streams stdin
// into it, and returns the parent's exit code. The write blocks at most until
// the child starts reading its payload; the provider only waits on the
// parent. Child output goes to the async log used for troubleshooting.
func detachSelf(args []string, stdin io.Reader, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agenthooks: async: %v\n", err)
		return 0 // fail open: telemetry loss must not block the agent
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = detachSysProcAttr()

	logPath := filepath.Join(os.TempDir(), "agenthooks-async.log")
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}

	childStdin, err := cmd.StdinPipe()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agenthooks: async: %v\n", err)
		return 0
	}
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(stderr, "agenthooks: async: %v\n", err)
		return 0
	}
	if _, err := io.Copy(childStdin, io.LimitReader(stdin, maxPayloadBytes)); err != nil {
		_, _ = fmt.Fprintf(stderr, "agenthooks: async: forwarding payload: %v\n", err)
	}
	_ = childStdin.Close()
	// Deliberately no Wait: the child is the detached worker. Release lets
	// the parent exit without reaping.
	_ = cmd.Process.Release()
	return 0
}
