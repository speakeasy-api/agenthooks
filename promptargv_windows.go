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

func procExecutable(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return string(utf16.Decode(buf[:size])), nil
}

func procPPID(pid int) (int, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, err
	}
	for {
		if entry.ProcessID == uint32(pid) {
			return int(entry.ParentProcessID), nil
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			return 0, err
		}
	}
}
