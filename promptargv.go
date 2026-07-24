package agenthooks

import (
	"path/filepath"
	"strings"
)

// Cursor prompt recovery, argv fallback. The primary recovery reads the
// session transcript named in the hook payload (backfill.go); this path
// covers payloads whose transcript is missing or unreadable. Backfill only
// ever happens in print mode (quirk #31) — and in print mode the prompt is
// usually in the agent process's own argv: the hook is a descendant of
// `cursor-agent -p ... "<prompt>"`. The runner walks its ancestor processes
// and reads the prompt from the cursor-agent command line, using only
// facilities the OS itself guarantees (/proc on Linux, sysctl on macOS).
// Interactive sessions — where argv carries no prompt — fire the real
// beforeSubmitPrompt and never reach this path. Prompts piped via stdin
// instead of argv stay unrecovered here (the transcript covers those).

// backfillMaxAncestors bounds the walk; hooks typically sit one shell below
// the agent process.
const backfillMaxAncestors = 6

func cursorArgvPrompt() string {
	args := findAncestorArgs(isCursorAgentArgs)
	if args == nil {
		return ""
	}
	return cursorPromptFromArgs(args)
}

// findAncestorArgs climbs the process tree until match accepts a process's
// argv. procPPID/procArgs are per-OS (promptargv_*.go).
func findAncestorArgs(match func([]string) bool) []string {
	args, _ := findAncestorProcess(match)
	return args
}

func findAncestorProcess(match func([]string) bool) ([]string, int) {
	pid := parentPID()
	for depth := 0; depth < backfillMaxAncestors && pid > 1; depth++ {
		args, err := procArgs(pid)
		if err != nil {
			return nil, 0
		}
		if match(args) {
			return args, pid
		}
		next, err := procPPID(pid)
		if err != nil || next == pid {
			return nil, 0
		}
		pid = next
	}
	return nil, 0
}

// isCursorAgentArgs recognizes a cursor-agent print-mode invocation. The
// print-mode requirement is a safety latch: an interactive ancestor's
// positionals are not prompts.
func isCursorAgentArgs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	named := false
	for _, a := range args[:min(2, len(args))] {
		if strings.Contains(filepath.Base(a), "cursor-agent") {
			named = true
			break
		}
	}
	if !named {
		return false
	}
	for _, a := range args[1:] {
		if a == "-p" || a == "--print" {
			return true
		}
	}
	return false
}

// cursorValueFlags are cursor-agent flags that consume the next argument, so
// their values are not mistaken for the prompt.
var cursorValueFlags = map[string]bool{
	"--api-key":       true,
	"-H":              true,
	"--header":        true,
	"--output-format": true,
	"--model":         true,
	"--mode":          true,
	"--workspace":     true,
	"--sandbox":       true,
}

// cursorPromptFromArgs extracts the positional prompt from a cursor-agent
// argv (`cursor-agent [options] [prompt...]`); multiple positionals join
// with spaces, matching the CLI's variadic prompt.
func cursorPromptFromArgs(args []string) string {
	var parts []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if cursorValueFlags[a] {
				i++
			}
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}
