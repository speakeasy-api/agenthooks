//go:build linux || darwin

// Package filelock provides a best-effort, non-blocking advisory file lock,
// used to enforce the hook server singleton and to serialize client
// auto-spawns.
package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// TryLock attempts a non-blocking exclusive lock on path, creating the file
// if needed. It returns a release func, whether the lock was acquired, and
// any unexpected error. A held lock elsewhere yields (noop, false, nil).
func TryLock(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return func() {}, false, nil
		}
		return func() {}, false, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, true, nil
}
