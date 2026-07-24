package agenthooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openCodeToolFrame(seq int, tool string) string {
	return fmt.Sprintf(`{"seq":%d,"hook":"tool.execute.before","input":{"sessionID":"s","callID":"c%d","tool":%q},"output":{"args":{}}}`, seq, seq, tool)
}

func TestServeLoop(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		if e.Tool.Name == "bash" {
			return Deny("no bash in this session"), nil
		}
		return NoDecision(), nil
	})

	lines := []string{
		`{"seq":1,"hook":"initialize","input":{"serverUrl":"http://127.0.0.1:1","directory":"/work","worktree":""}}`,
		strings.TrimSpace(string(fixture(t, "opencode/tool_execute_before.json"))), // seq 3, bash
		strings.TrimSpace(string(fixture(t, "opencode/session_idle.json"))),        // seq 9
	}
	var out, errb bytes.Buffer
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=opencode"},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}

	replies := parseReplies(t, out.String())
	if len(replies) != 3 {
		t.Fatalf("expected 3 replies, got %d: %s", len(replies), out.String())
	}
	if replies[0].Seq != 1 || replies[0].Error != "" {
		t.Errorf("initialize reply wrong: %+v", replies[0])
	}
	if replies[1].Seq != 3 || replies[1].Error != "no bash in this session" {
		t.Errorf("deny must ride the error field (shim re-throws): %+v", replies[1])
	}
	if replies[2].Seq != 9 || replies[2].Error != "" || len(replies[2].Output) != 0 {
		t.Errorf("session.idle no-op wrong: %+v", replies[2])
	}
}

func TestServeUpdateInput(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision().WithUpdatedInput(map[string]any{"command": "make test -j4", "timeout": 120}), nil
	})
	var out, errb bytes.Buffer
	line := strings.TrimSpace(string(fixture(t, "opencode/tool_execute_before.json")))
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=opencode"},
		strings.NewReader(line+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d", code)
	}
	replies := parseReplies(t, out.String())
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	args, ok := replies[0].Output["args"].(map[string]any)
	if !ok || args["command"] != "make test -j4" {
		t.Errorf("updated input must ride output.args: %+v", replies[0].Output)
	}
}

func TestServeOpenCodeActiveMCPInventory(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, "opencode.json"), `{"mcp":{"disk_only":{"type":"remote","url":"https://disk.example.com/mcp"}}}`)
	r := quietRunner()
	seen := map[string]ToolCall{}
	r.OnToolPre(func(_ context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		seen[e.Tool.Name] = e.Tool
		if strings.Contains(string(e.Raw), "active.example.com") {
			t.Error("active inventory leaked into Event.Raw")
		}
		return NoDecision(), nil
	})
	lines := []string{
		fmt.Sprintf(`{"seq":1,"hook":"initialize","input":{"directory":%q,"mcp":{"active":{"type":"remote","url":"https://active.example.com/mcp"},"local":{"type":"local","command":["node","server.js"]},"disabled":{"type":"remote","url":"https://off.example.com/mcp","enabled":false}}}}`, cwd),
		openCodeToolFrame(2, "active_run"),
		openCodeToolFrame(3, "local_do"),
		openCodeToolFrame(4, "disabled_run"),
		openCodeToolFrame(5, "disk_only_run"),
	}
	var out, errb bytes.Buffer
	if code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=opencode"},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb); code != 0 {
		t.Fatalf("serve exit %d: %s", code, errb.String())
	}
	if got := seen["active_run"].MCP; got == nil || got.URL != "https://active.example.com/mcp" || !got.FromConfig {
		t.Fatalf("active remote MCP = %+v", got)
	}
	if got := seen["local_do"].MCP; got == nil || got.Command != "node server.js" || !got.FromConfig {
		t.Fatalf("active local MCP = %+v", got)
	}
	for _, name := range []string{"disabled_run", "disk_only_run"} {
		if seen[name].MCP != nil {
			t.Fatalf("%s resolved outside active inventory: %+v", name, seen[name].MCP)
		}
	}
}

func TestServeOpenCodeMCPInventoryFallbackAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mcpField  string
		disabled  bool
		wantMatch bool
	}{
		{name: "omitted falls back", wantMatch: true},
		{name: "empty is authoritative", mcpField: `,"mcp":{}`},
		{name: "resolution disabled", mcpField: `,"mcp":{"disk":{"type":"remote","url":"https://active.example.com/mcp"}}`, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			cwd := t.TempDir()
			writeConfig(t, filepath.Join(cwd, "opencode.json"), `{"mcp":{"disk":{"type":"remote","url":"https://disk.example.com/mcp"}}}`)
			var opts []Option
			if tc.disabled {
				opts = append(opts, WithoutMCPResolution())
			}
			r := quietRunner(opts...)
			var call ToolCall
			r.OnToolPre(func(_ context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
				call = e.Tool
				return NoDecision(), nil
			})
			lines := []string{
				fmt.Sprintf(`{"seq":1,"hook":"initialize","input":{"directory":%q%s}}`, cwd, tc.mcpField),
				openCodeToolFrame(2, "disk_run"),
			}
			var out, errb bytes.Buffer
			if code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=opencode"},
				strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb); code != 0 {
				t.Fatalf("serve exit %d: %s", code, errb.String())
			}
			if tc.wantMatch {
				if call.MCP == nil || call.MCP.URL != "https://disk.example.com/mcp" {
					t.Fatalf("disk fallback = %+v", call.MCP)
				}
			} else if call.MCP != nil {
				t.Fatalf("unexpected MCP resolution = %+v", call.MCP)
			}
		})
	}
}

type testReply struct {
	Seq    int64          `json:"seq"`
	Output map[string]any `json:"output"`
	Error  string         `json:"error"`
}

func parseReplies(t *testing.T, s string) []testReply {
	t.Helper()
	var out []testReply
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var r testReply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad reply line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}
