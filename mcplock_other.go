//go:build !linux && !darwin && !windows

package agenthooks

import (
	"os"
	"time"
)

// mcpListLockStaleAfter lets this lock-file fallback (platforms without flock
// or LockFileEx) recover a lock whose refresh worker died mid-probe.
const mcpListLockStaleAfter = mcpListProbeTimeout + mcpListProbeWaitDelay + 30*time.Second

func tryMCPListLock(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > mcpListLockStaleAfter {
			if removeErr := os.Remove(path); removeErr == nil {
				return tryMCPListLock(path)
			}
		}
		return func() {}, false, nil
	}
	if err != nil {
		return func() {}, false, err
	}
	return func() {
		_ = f.Close()
		_ = os.Remove(path)
	}, true, nil
}
