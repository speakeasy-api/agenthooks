//go:build unix

package agenthooks

import (
	"os/exec"
	"syscall"
)

// configureMCPListProbe runs the provider CLI in its own process group so a
// timeout kills the whole tree, including the stdio MCP servers `claude mcp
// list` starts for its health check. The refresh worker is already a session
// leader (detachSysProcAttr), so without a group of its own the probe would
// share the worker's group. WaitDelay stops a descendant that inherited the
// output pipe from holding the worker past the timeout.
func configureMCPListProbe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = mcpListProbeWaitDelay
}
