//go:build windows

package agenthooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

const createNoWindow = 0x08000000

// configureMCPListProbe keeps the provider CLI off screen and kills its whole
// tree on timeout. The refresh worker is a DETACHED_PROCESS with no console,
// so a console CLI started from it would otherwise open a visible window.
// exec's default cancel terminates only the direct child; taskkill /T also
// ends the stdio MCP servers `claude mcp list` starts for its health check.
// WaitDelay stops a descendant that inherited the output pipe from holding
// the worker past the timeout.
func configureMCPListProbe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	cmd.Cancel = func() error {
		kill := exec.Command(taskkillPath(), "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = mcpListProbeWaitDelay
}

func taskkillPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "taskkill.exe")
	}
	return "taskkill.exe"
}
