//go:build !windows

package agenthooks

import "syscall"

// detachSysProcAttr severs the child from the parent's session so it survives
// the parent's exit and any process-group signals from the provider.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
