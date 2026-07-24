package agenthooks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// serve is the long-lived daemon mode behind the OpenCode shim (§8). Frames
// are processed sequentially, matching OpenCode's per-session hook semantics
// (open question #1 resolved conservatively). The shim owns the timeout
// policy OpenCode lacks; the daemon still bounds each handler with the
// resolved Policy deadline.
func (r *Runner) serve(ctx context.Context, inv *invocation, stdin io.Reader, stdout, stderr io.Writer) int {
	if inv.provider == "" {
		inv.provider = ProviderOpenCode
	}
	if inv.provider != ProviderOpenCode {
		_, _ = fmt.Fprintf(stderr, "agenthooks: serve mode supports --provider=opencode, got %q\n", inv.provider)
		return 64
	}

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), maxPayloadBytes)
	enc := json.NewEncoder(stdout)

	var serverInfo struct {
		ServerURL string `json:"serverUrl"`
		Directory string `json:"directory"`
		Worktree  string `json:"worktree"`
		MCP       []mcpConfigEntry
		MCPExact  bool
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var fr opencodeFrame
		if err := json.Unmarshal(line, &fr); err != nil {
			r.logger.Error("agenthooks: bad shim frame", "error", err)
			continue
		}
		// The shim's first runtime hook sends server info plus the resolved MCP
		// inventory; omitted MCP falls back to direct config reads.
		if fr.Hook == "initialize" {
			var info struct {
				ServerURL string                      `json:"serverUrl"`
				Directory string                      `json:"directory"`
				Worktree  string                      `json:"worktree"`
				MCP       *map[string]opencodeMCPJSON `json:"mcp"`
			}
			if json.Unmarshal(fr.Input, &info) == nil {
				serverInfo.ServerURL = info.ServerURL
				serverInfo.Directory = info.Directory
				serverInfo.Worktree = info.Worktree
				serverInfo.MCPExact = info.MCP != nil
				serverInfo.MCP = nil
				if info.MCP != nil {
					serverInfo.MCP = openCodeMCPEntries(*info.MCP)
				}
			}
			_ = enc.Encode(opencodeReply{Seq: fr.Seq})
			continue
		}

		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		typed, err := decodeOpenCodeFrame(inv.variant, DetectionConfig, r.now(), &fr, lineCopy)
		if err != nil {
			r.logger.Error("agenthooks: decode failed", "hook", fr.Hook, "error", err)
			_ = enc.Encode(opencodeReply{Seq: fr.Seq})
			continue
		}
		base := eventOf(typed)
		if base.Session.CWD == "" {
			base.Session.CWD = serverInfo.Directory
			base.Session.WorkspaceRoots = rootsFor(serverInfo.Directory)
		}
		if serverInfo.MCPExact {
			r.resolveMCPWithOpenCodeInventory(typed, &serverInfo.MCP)
		} else {
			r.resolveMCP(typed)
		}
		pol := r.policy(base)
		deadline := pol.Timeout
		if deadline == 0 {
			deadline = defaultDeadline
		}
		hctx, cancel := context.WithTimeout(withLogger(ctx, r.logger), deadline)
		core, herr := r.dispatch(hctx, typed)
		cancel()
		if herr != nil {
			r.logger.Error("agenthooks: handler failed", "hook", fr.Hook, "error", herr)
			core = failCore(pol, base)
		}
		core = r.applyPolicy(typed, base, core, pol)

		reply, encErr := encodeOpenCodeReply(typed, base, core)
		if encErr != nil {
			r.logger.Error("agenthooks: encode failed", "hook", fr.Hook, "error", encErr)
			reply = &opencodeReply{}
		}
		reply.Seq = fr.Seq
		if err := enc.Encode(reply); err != nil {
			r.logger.Error("agenthooks: writing reply", "error", err)
			return 1
		}
	}
	if err := sc.Err(); err != nil {
		r.logger.Error("agenthooks: reading shim stream", "error", err)
		return 1
	}
	return 0
}
