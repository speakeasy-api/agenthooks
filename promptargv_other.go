//go:build !linux && !darwin && !windows

package agenthooks

import (
	"errors"
	"os"
)

func parentPID() int { return os.Getppid() }

func procArgs(int) ([]string, error) {
	return nil, errors.New("agenthooks: ancestor argv recovery unsupported on this platform")
}

func procPPID(int) (int, error) {
	return 0, errors.New("agenthooks: ancestor ppid lookup unsupported on this platform")
}
