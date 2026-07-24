//go:build !linux && !darwin && !windows

package agenthooks

import "os"

func tryMCPListLock(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
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
