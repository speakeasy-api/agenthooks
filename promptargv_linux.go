package agenthooks

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func parentPID() int { return os.Getppid() }

// procArgs reads /proc/<pid>/cmdline (NUL-separated argv).
func procArgs(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("agenthooks: empty cmdline for pid %d", pid)
	}
	if data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}
	fields := bytes.Split(data, []byte{0})
	args := make([]string, 0, len(fields))
	for _, f := range fields {
		args = append(args, string(f))
	}
	return args, nil
}

func procExecutable(pid int) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
}

// procPPID reads the parent pid from /proc/<pid>/stat. The comm field may
// contain spaces and parens, so parsing starts after the last ')'.
func procPPID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("agenthooks: malformed stat for pid %d", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("agenthooks: malformed stat for pid %d", pid)
	}
	return strconv.Atoi(fields[1])
}
