// Package transcript provides best-effort JSONL readers for provider
// transcript files (Event.Session.TranscriptPath). Formats are
// provider-specific and unversioned upstream; entries always retain the raw
// line so consumers lose nothing when shapes drift. Capture/dedup pipelines
// are out of scope (DESIGN.md §11) — this package is parsing primitives only.
package transcript

import (
	"encoding/json"
	"io"
	"os"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/transcriptio"
)

// Entry is one transcript line, normalized best-effort.
type Entry struct {
	Index int
	Type  string // provider's line type ("user", "assistant", "summary", ...)
	Role  string // message role when present
	Text  string // concatenated text content when extractable
	Raw   json.RawMessage
}

// ReadFile parses the transcript at path for the given provider.
func ReadFile(p agenthooks.Provider, path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(p, f)
}

// Read parses transcript JSONL from r. Unparseable lines become raw-only
// entries rather than errors: transcripts are advisory, never authoritative.
func Read(p agenthooks.Provider, r io.Reader) ([]Entry, error) {
	core, err := transcriptio.Read(string(p), r)
	out := make([]Entry, len(core))
	for i, e := range core {
		out[i] = Entry(e)
	}
	return out, err
}
