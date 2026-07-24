package gemini

import (
	"encoding/json"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

func TestBeforeToolMCPContext(t *testing.T) {
	raw := json.RawMessage(`{
		"session_id":"s","cwd":"/work","hook_event_name":"BeforeTool","tool_name":"mcp_srv_run",
		"mcp_context":{"server_name":"srv","tool_name":"run","command":"node","args":["server.js"],"cwd":"/srv","url":"https://example.com/mcp","tcp":"localhost:9000"}
	}`)
	in, ok := BeforeTool(&agenthooks.Event{Provider: agenthooks.ProviderGemini, NativeName: "BeforeTool", Raw: raw})
	if !ok || in.MCPContext == nil {
		t.Fatal("BeforeTool MCP context not decoded")
	}
	ctx := in.MCPContext
	if ctx.ServerName != "srv" || ctx.ToolName != "run" || ctx.Command != "node" || len(ctx.Args) != 1 ||
		ctx.CWD != "/srv" || ctx.URL != "https://example.com/mcp" || ctx.TCP != "localhost:9000" {
		t.Fatalf("MCP context = %+v", ctx)
	}
}
