# Moltis fixture provenance

These payloads target **Moltis 20260902.01** at the official tag commit
[`77b0d0ac52744aa4d59cbfe83db23cb7a6283ccf`](https://github.com/moltis-org/moltis/commit/77b0d0ac52744aa4d59cbfe83db23cb7a6283ccf).
The complete 17-variant wire schema comes from the public `HookPayload` enum in
`crates/common/src/hooks.rs`; Moltis does not version the hook protocol
separately from the application.

`message_received.json`, `before_tool_call.json`, and `after_tool_call.json`
are reduced, sanitized forms of payloads observed while driving an isolated
Moltis Gateway on Linux through a local OpenAI-compatible model. That canary
also proved the native runtime honored all three mutable decisions: prompt
content modification, exit-1 blocking, and tool-argument replacement. The
remaining fixtures are schema-derived examples for events that either were not
needed by that run or have no production dispatch site in the audited source.
They test decoding and forward compatibility, not observed runtime coverage.

| File | Native event | Provenance / coverage |
| --- | --- | --- |
| `message_received.json` | `MessageReceived` | Sanitized live payload; content rewrite observed in persisted history and the model response |
| `before_tool_call.json` | `BeforeToolCall` | Sanitized live `exec` payload; allow, block, and argument replacement observed |
| `after_tool_call.json` | `AfterToolCall` | Sanitized live successful `exec` payload |
| `after_tool_call_failure.json` | `AfterToolCall` | Schema-derived failure case; codec classification coverage |
| `before_agent_start.json` | `BeforeAgentStart` | Schema-derived |
| `after_llm_call.json` | `AfterLLMCall` | Schema-derived |
| `after_compaction.json` | `AfterCompaction` | Schema-derived |
| `message_sending.json` | `MessageSending` | Schema-derived; no production construction/dispatch site found at the pinned commit |
| `message_sent.json` | `MessageSent` | Schema-derived; no production construction/dispatch site found at the pinned commit |
| `tool_result_persist.json` | `ToolResultPersist` | Schema-derived |
| `session_start.json` | `SessionStart` | Schema-derived |
| `session_end.json` | `SessionEnd` | Schema-derived |
| `before_llm_call.json` | `BeforeLLMCall` | Schema-derived |
| `before_compaction.json` | `BeforeCompaction` | Schema-derived |
| `agent_end.json` | `AgentEnd` | Schema-derived; no production construction/dispatch site found at the pinned commit |
| `gateway_start.json` | `GatewayStart` | Schema-derived |
| `gateway_stop.json` | `GatewayStop` | Schema-derived |
| `command.json` | `Command` | Schema-derived |

When re-qualifying a newer Moltis build, compare `HookEvent::ALL` and
`HookPayload`, search the production tree for every payload construction and
registry dispatch site, then rerun `TestMoltisEventsAndDecisions`. An enum
variant is only declared capability; it is not evidence that the runtime emits
the event.

The pinned release fixtures intentionally retain the old no-ID tool payloads.
Forward-compatibility tests separately cover the native `tool_call_id` added by
[moltis-org/moltis#1257](https://github.com/moltis-org/moltis/pull/1257), while
the legacy fixtures keep proving that agenthooks marks fallback identities as
synthesized instead of overstating correlation evidence.
