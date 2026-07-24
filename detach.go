package agenthooks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const claudeMCPWarmFlag = "--agenthooks-internal-claude-mcp-warm"

const (
	codexMCPWarmFlag       = "--agenthooks-internal-codex-mcp-warm"
	codexLaunchContextFlag = "--agenthooks-internal-codex-launch-context"
	maxLaunchContextBytes  = 1 << 20
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

func claudeMCPWarmCWD(args []string) (string, bool) {
	prefix := claudeMCPWarmFlag + "="
	for _, arg := range args {
		if cwd, ok := strings.CutPrefix(arg, prefix); ok {
			return cwd, true
		}
	}
	return "", false
}

func hasInternalFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func encodeCodexLaunchContext(args []string, stdin io.Reader, launch codexLaunchContext) ([]string, io.Reader, error) {
	payload, err := codexLaunchContextPayload(launch)
	if err != nil {
		return args, stdin, err
	}
	workerArgs := append([]string(nil), args...)
	workerArgs = append(workerArgs, codexLaunchContextFlag)
	return workerArgs, io.MultiReader(payload, stdin), nil
}

func encodeCodexMCPWarm(args []string, launch codexLaunchContext) ([]string, io.Reader, error) {
	payload, err := codexLaunchContextPayload(launch)
	if err != nil {
		return args, nil, err
	}
	workerArgs := append([]string(nil), args...)
	workerArgs = append(workerArgs, codexMCPWarmFlag)
	return workerArgs, payload, nil
}

func codexLaunchContextPayload(launch codexLaunchContext) (io.Reader, error) {
	data, err := json.Marshal(launch)
	if err != nil {
		return nil, err
	}
	if len(data) > maxLaunchContextBytes {
		return nil, fmt.Errorf("codex launch context exceeds %d bytes", maxLaunchContextBytes)
	}
	return bytes.NewReader(append(data, '\n')), nil
}

func decodeCodexLaunchContext(stdin io.Reader) (codexLaunchContext, io.Reader, error) {
	reader := bufio.NewReader(stdin)
	var data []byte
	for {
		part, more, err := reader.ReadLine()
		if err != nil {
			return codexLaunchContext{}, reader, err
		}
		if len(data)+len(part) > maxLaunchContextBytes {
			return codexLaunchContext{}, reader, fmt.Errorf("codex launch context exceeds %d bytes", maxLaunchContextBytes)
		}
		data = append(data, part...)
		if !more {
			break
		}
	}
	var launch codexLaunchContext
	if err := json.Unmarshal(data, &launch); err != nil {
		return codexLaunchContext{}, reader, err
	}
	return launch, reader, nil
}

// detachSelf spawns this binary detached with the given args, streams stdin
// into it, and returns the parent's exit code. The write blocks at most until
// the child starts reading its payload; the provider only waits on the
// parent. Child output goes to the async log used for troubleshooting.
func detachSelf(args []string, stdin io.Reader, stderr io.Writer) int {
	if err := startDetachedSelf(args, stdin); err != nil {
		_, _ = fmt.Fprintf(stderr, "agenthooks: async: %v\n", err)
	}
	return 0
}

// startDetachedSelf starts a copy of the current executable that survives this
// process. A nil stdin is used by internal workers that need no hook payload.
func startDetachedSelf(args []string, stdin io.Reader) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = detachSysProcAttr()

	logPath := filepath.Join(os.TempDir(), "agenthooks-async.log")
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}

	var childStdin io.WriteCloser
	if stdin != nil {
		childStdin, err = cmd.StdinPipe()
		if err != nil {
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if stdin != nil {
		limit := int64(maxPayloadBytes)
		if hasInternalFlag(args, codexLaunchContextFlag) || hasInternalFlag(args, codexMCPWarmFlag) {
			limit += maxLaunchContextBytes + 1
		}
		if _, err := io.Copy(childStdin, io.LimitReader(stdin, limit)); err != nil {
			_ = childStdin.Close()
			_ = cmd.Process.Release()
			return fmt.Errorf("forwarding payload: %w", err)
		}
		_ = childStdin.Close()
	}
	// Deliberately no Wait: the child is the detached worker. Release lets
	// the parent exit without reaping.
	_ = cmd.Process.Release()
	return nil
}
