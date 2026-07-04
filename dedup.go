package agenthooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Cursor fires both the generic (preToolUse/postToolUse) and the specific
// (beforeShellExecution/beforeMCPExecution/...) hook for one tool call
// (quirk #2). Each firing is a separate process, so suppression uses
// best-effort on-disk markers: the first arrival wins, the sibling gets the
// provider's no-op form (never a forced allow). Any filesystem error means
// "not a duplicate" — correctness degrades to double-delivery, never to a
// dropped decision.

const dedupTTL = 30 * time.Second

// seenDuplicate records the event's identity and reports whether an
// equivalent sibling was already processed within the TTL.
func (r *Runner) seenDuplicate(typed any) bool {
	base := eventOf(typed)
	tool := toolOf(typed)
	if tool == nil {
		return false
	}
	key := dedupKey(base, tool)
	dir := filepath.Join(r.stateDir(), "agenthooks-dedup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	cleanupStale(dir)
	path := filepath.Join(dir, key)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			if fi, statErr := os.Stat(path); statErr == nil && time.Since(fi.ModTime()) < dedupTTL {
				return true
			}
			// Stale marker: refresh and treat as first arrival.
			_ = os.Chtimes(path, time.Now(), time.Now())
		}
		return false
	}
	_ = f.Close()
	return false
}

// dedupKey identifies the underlying tool call across the differently-shaped
// sibling events: the generic form carries tool_name+tool_input, the specific
// form carries command/file_path, so the key leans on the canonical class and
// the most stable payload facet available.
func dedupKey(base *Event, tool *ToolCall) string {
	facet := ""
	switch tool.Canonical {
	case ToolShell:
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(tool.Input, &in)
		facet = in.Command
	case ToolMCP:
		if tool.MCP != nil {
			facet = tool.MCP.Tool
		}
	case ToolFileRead:
		var in struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(tool.Input, &in)
		facet = in.FilePath
	default:
		facet = tool.Name
	}
	stage := "pre"
	if base.Kind == KindToolPost || base.Kind == KindToolError {
		stage = "post"
	}
	h := sha256.Sum256([]byte(base.Session.ID + "|" + base.Session.TurnID + "|" + stage + "|" + string(tool.Canonical) + "|" + facet))
	return hex.EncodeToString(h[:])[:32]
}

func cleanupStale(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) < 64 {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 10*dedupTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
