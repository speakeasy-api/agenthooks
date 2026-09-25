# OpenCode 2 fixture provenance

These frames were captured from **OpenCode 2.0.16** (`@opencode/cli`, macOS
arm64, the free `opencode/big-pickle` model) on 2026-09-24. They are the
NDJSON frames the generated shim's OpenCode 2 path (`setup()` in
`install/render_opencode.go`) wrote to a recording stand-in for
`agenthooks serve`. That is exactly what the codec decodes, so they pin the
V2-to-V1 frame translation, not OpenCode's raw hook and bus payloads.
Paths are rewritten to `/work`.

Record the version here whenever the fixtures are re-recorded.

| File | Frame | Notes |
| --- | --- | --- |
| `initialize.json` | `initialize` | no `mcp`: the shim omits the inventory when its transform snapshot can't see every server, so serve falls back to config files |
| `session_created.json` | `session.created` | from a separate e2e run; see below |
| `chat_message.json` | `chat.message` | from `session.hook("prompt")`; prompt text as a single text part |
| `chat_params.json` | `chat.params` | from `session.hook("context")` |
| `tool_execute_before.json` / `tool_execute_after.json` | tool pre/post | `read` on an existing file |
| `message_part_updated_tool_error.json` | `message.part.updated` | a failed `read`, kept in V1's tool-part shape so it decodes as a tool error |
| `tool_execute_before_code_mode.json` | `tool.execute.before` | code mode's `execute` tool (quirk #50) |
| `tool_execute_before_mcp_nested.json` / `tool_execute_after_mcp_nested.json` | tool pre/post | the MCP call nested inside `execute`; it reuses the outer call's ID |
| `session_idle.json` | `session.idle` | from `session.execution.succeeded`, with `finalMessage` from `session.text.ended` and usage summed across steps |

## Known gap

`session.created` is often missing from V2 captures. A location's first
session is created before `setup()` has subscribed to the event bus, so the
shim never sees the event. `session_created.json` comes from an earlier e2e run
where the event did arrive.

## Re-recording

Install the rendered shim (with `COMMAND` pointing at a script that appends
each stdin line to a file and replies `{"seq":N}`), then drive
`opencode run --standalone --auto` through: a successful and a failing tool
call, and one MCP call (code mode). Standalone runs are the reliable path;
runs through the shared background service have intermittently hung before
any hook fired.
