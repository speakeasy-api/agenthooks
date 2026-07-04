package transcript

import (
	"strings"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

func TestReadClaudeTranscript(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Bash"}]}}`,
		`this line is not JSON`,
	}, "\n")
	entries, err := Read(agenthooks.ProviderClaudeCode, strings.NewReader(jsonl))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Role != "user" || entries[0].Text != "hello" {
		t.Errorf("entry 0 wrong: %+v", entries[0])
	}
	if entries[1].Type != "assistant" || entries[1].Text != "hi" {
		t.Errorf("entry 1 wrong: %+v", entries[1])
	}
	if string(entries[2].Raw) != "this line is not JSON" || entries[2].Text != "" {
		t.Errorf("unparseable lines must survive raw-only: %+v", entries[2])
	}
}

func TestReadCursorTranscriptStripsUserQueryWrapper(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nread the file\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"<user_query> mentioned in passing"}]}}`,
	}, "\n")
	entries, err := Read(agenthooks.ProviderCursor, strings.NewReader(jsonl))
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Role != "user" || entries[0].Text != "read the file" {
		t.Errorf("user entry must lose the <user_query> framing: %+v", entries[0])
	}
	if entries[1].Text != "<user_query> mentioned in passing" {
		t.Errorf("assistant text must stay verbatim: %+v", entries[1])
	}
}
