//go:build unix

package agenthooks

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A timed-out probe must take its descendants with it: `claude mcp list`
// starts stdio MCP servers, and one left holding the output pipe would keep
// the refresh worker alive long after the timeout.
func TestMCPListProbeTimeoutKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 & echo $!; wait")
	configureMCPListProbe(cmd)
	start := time.Now()
	out, err := cmd.Output()
	if err == nil {
		t.Fatal("probe outlived its timeout")
	}
	if elapsed := time.Since(start); elapsed >= mcpListProbeWaitDelay {
		t.Fatalf("probe returned after %v: a descendant held its output", elapsed)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("grandchild pid %q: %v", out, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the probe timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
