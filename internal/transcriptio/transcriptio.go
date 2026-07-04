// Package transcriptio is the transcript-parsing core shared by the public
// transcript package and the runner's prompt backfill (the root package
// cannot import transcript without a cycle). Providers are identified by
// their string form (agenthooks.Provider values).
package transcriptio

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Entry is one transcript line, normalized best-effort. It mirrors
// transcript.Entry field-for-field so the public package converts directly.
type Entry struct {
	Index int
	Type  string // provider's line type ("user", "assistant", "summary", ...)
	Role  string // message role when present
	Text  string // concatenated text content when extractable
	Raw   json.RawMessage
}

// Read parses transcript JSONL from r. Unparseable lines become raw-only
// entries rather than errors: transcripts are advisory, never authoritative.
func Read(provider string, r io.Reader) ([]Entry, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var out []Entry
	i := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		raw := make([]byte, len(line))
		copy(raw, line)
		e := Entry{Index: i, Raw: raw}
		parseLine(provider, raw, &e)
		out = append(out, e)
		i++
	}
	return out, sc.Err()
}

func parseLine(provider string, line []byte, e *Entry) {
	switch provider {
	case "claude-code", "codex":
		var claude struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &claude) == nil {
			e.Type = claude.Type
			e.Role = claude.Message.Role
			e.Text = contentText(claude.Message.Content)
			return
		}
	case "cursor":
		// Cursor transcript lines carry the role at the top level with the
		// content blocks nested under message.content. User prompts are
		// wrapped in <user_query> tags the hook payloads never carry; strip
		// them so Text matches what beforeSubmitPrompt would have reported.
		var cursor struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &cursor) == nil && (cursor.Role != "" || len(cursor.Message.Content) > 0) {
			e.Type = cursor.Type
			e.Role = cursor.Role
			e.Text = contentText(cursor.Message.Content)
			if cursor.Role == "user" {
				e.Text = stripUserQueryWrapper(e.Text)
			}
			return
		}
	}
	// Generic fallback: probe common keys.
	var generic struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(line, &generic) == nil {
		e.Type = generic.Type
		e.Role = generic.Role
		if generic.Text != "" {
			e.Text = generic.Text
		} else {
			e.Text = contentText(generic.Content)
		}
	}
}

// stripUserQueryWrapper removes Cursor's <user_query> transcript framing and
// trailing blank lines from a user entry's text.
func stripUserQueryWrapper(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<user_query>")
	s = strings.TrimSuffix(strings.TrimRight(s, " \t\n\r"), "</user_query>")
	return strings.Trim(s, "\n")
}

// contentText extracts text from either a plain string or a content-block
// array ({"type":"text","text":...}).
func contentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}
