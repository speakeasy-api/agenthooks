//go:build !unix && !windows

package agenthooks

import "os/exec"

// configureMCPListProbe only bounds pipe waits where process groups are not
// available: a timeout kills the direct child.
func configureMCPListProbe(cmd *exec.Cmd) {
	cmd.WaitDelay = mcpListProbeWaitDelay
}
