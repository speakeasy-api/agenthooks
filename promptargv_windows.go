//go:build windows

package agenthooks

import (
	"fmt"
	"os"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func parentPID() int { return os.Getppid() }

func procArgs(pid int) ([]string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)

	var size uint32
	_ = windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, nil, 0, &size)
	if size < uint32(unsafe.Sizeof(windows.NTUnicodeString{})) {
		return nil, fmt.Errorf("agenthooks: invalid command line size for pid %d", pid)
	}
	buf := make([]byte, size)
	if err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), size, &size); err != nil {
		return nil, err
	}
	value := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
	if value.Buffer == nil || value.Length == 0 || value.Length%2 != 0 {
		return nil, fmt.Errorf("agenthooks: invalid command line for pid %d", pid)
	}
	commandLine := string(utf16.Decode(unsafe.Slice(value.Buffer, int(value.Length/2))))
	return windows.DecomposeCommandLine(commandLine)
}

func procPPID(int) (int, error) {
	return 0, fmt.Errorf("agenthooks: ancestor ppid lookup unsupported on windows")
}
