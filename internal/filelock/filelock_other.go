//go:build !linux && !darwin && !windows

package filelock

import (
	"os"
	"time"
)

// staleAfter bounds how long an O_EXCL marker left by a crashed process can
// block new lock holders on platforms without advisory locks.
const staleAfter = 5 * time.Minute

// TryLock attempts a best-effort exclusive lock on path using an O_EXCL
// marker file. It returns a release func, whether the lock was acquired, and
// any unexpected error. A held lock elsewhere yields (noop, false, nil).
func TryLock(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > staleAfter {
			if removeErr := os.Remove(path); removeErr == nil {
				return TryLock(path)
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
