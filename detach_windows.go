//go:build windows

package agenthooks

import "syscall"

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// detachSysProcAttr severs the child from the parent's console and process
// group so it survives the parent's exit and any ctrl-event broadcasts from
// the provider.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}
