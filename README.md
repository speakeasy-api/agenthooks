<div align="center">
 <a href="https://www.speakeasy.com/" target="_blank">
  <img width="1500" height="500" alt="Speakeasy" src="https://github.com/user-attachments/assets/0e56055b-02a3-4476-9130-4be299e5a39c" />
 </a>
 <br />
 <br />
  <div>
   <a href="https://www.speakeasy.com/resources/ai-control-plane" target="_blank"><b>AI Control Plane</b></a>&nbsp;&nbsp;//&nbsp;&nbsp;<a href="https://go.speakeasy.com/slack" target="_blank"><b>Join us on Slack</b></a>
  </div>
 <br />

</div>

<hr />

<p align="center">
  <h1 align="center"><b>agenthooks</b></h1>
  <p align="center">Author coding-agent hooks once in Go; run them on Claude Code, Cursor, OpenAI Codex, Gemini CLI, OpenCode, and Kimi Code.</p>
  <p align="center">
    <!-- Go Doc Badge -->
    <a href="https://pkg.go.dev/github.com/speakeasy-api/agenthooks"><img alt="Go Doc" src="https://img.shields.io/badge/godoc-reference-blue.svg?style=for-the-badge"></a>
    <!-- Release Version Badge -->
    <a href="https://github.com/speakeasy-api/agenthooks/releases/latest"><img alt="Release" src="https://img.shields.io/github/release/speakeasy-api/agenthooks.svg?style=for-the-badge"></a>
    <!-- Go Report Card Badge -->
    <a href="https://goreportcard.com/report/github.com/speakeasy-api/agenthooks"><img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/speakeasy-api/agenthooks?style=for-the-badge"></a>
    <!-- CI Badge -->
    <a href="https://github.com/speakeasy-api/agenthooks/actions/workflows/ci.yaml"><img alt="GitHub Action: CI" src="https://img.shields.io/github/actions/workflow/status/speakeasy-api/agenthooks/ci.yaml?style=for-the-badge"></a>
    <!-- Line Break --><br/>
    <!-- Go Version Badge -->
    <a href="https://golang.org/"><img alt="Go Version" src="https://img.shields.io/badge/go-1.25+-00ADD8.svg?style=for-the-badge&logo=go"></a>
    <!-- Platform Support Badge -->
    <a href="https://github.com/speakeasy-api/agenthooks"><img alt="Platform Support" src="https://img.shields.io/badge/platform-linux%20%7C%20macos%20%7C%20windows-lightgrey.svg?style=for-the-badge"></a>
    <!-- Stars Badge -->
    <a href="https://github.com/speakeasy-api/agenthooks/stargazers"><img alt="GitHub stars" src="https://img.shields.io/github/stars/speakeasy-api/agenthooks.svg?style=for-the-badge&logo=github"></a>
    <!-- Line Break --><br/>
    <!-- Built By Speakeasy Badge -->
    <a href="https://speakeasy.com/"><img alt="Built by Speakeasy" src="https://www.speakeasy.com/assets/badges/built-by-speakeasy.svg" /></a>
    <!-- License Badge -->
    <a href="/LICENSE"><img alt="Software License" src="https://img.shields.io/badge/license-MIT-blue.svg?style=for-the-badge"></a>
  </p>
</p>

One clear interface, zero data-fidelity loss: the library owns the wire (JSON
dialects, exit codes, stderr discipline, provider quirks), the consumer owns
the logic. See [DESIGN.md](DESIGN.md) for the full design and the provider
research behind it.

```go
package main

import (
	"context"

	"github.com/speakeasy-api/agenthooks"
)

func main() {
	r := agenthooks.New(
		agenthooks.WithPolicy(agenthooks.Policy{
			Fail:        agenthooks.FailClosed,
			AskFallback: agenthooks.FallbackDeny,
		}),
	)

	r.OnToolPre(func(ctx context.Context, e *agenthooks.ToolPreEvent) (agenthooks.ToolPreDecision, error) {
		if e.Tool.Canonical == agenthooks.ToolShell {
			return agenthooks.AskUser("shell commands need confirmation"), nil
		}
		return agenthooks.NoDecision(), nil
	})

	// Fidelity escape hatch: every event, mapped or not, raw payload intact.
	r.OnAny(func(ctx context.Context, e *agenthooks.Event) error {
		agenthooks.Logger(ctx).Info("event", "provider", e.Provider, "native", e.NativeName)
		return nil
	})

	agenthooks.Main(r)
}
```

## Packages

| Package | Purpose |
|---|---|
| `agenthooks` | Envelope, decisions, capability matrix, policy, runtime (`Main`/`Run`), quirk registry |
| `provider/{claudecode,codex,cursor,gemini,opencode,kimicode}` | Complete typed native structs with unknown-field capture — the fidelity guarantee |
| `install` | One Go `Manifest` → correct `hooks.json` / `settings.json` / `config.toml` / plugin scaffolding per provider, workarounds baked in |
| `transcript` | Best-effort JSONL transcript readers |
| `agenthookstest` | Fixture corpus, in-process harness, fake-provider spawner |
| `e2e` | Opt-in end-to-end suite driving real local agent CLIs (`AGENTHOOKS_E2E=1 go test ./e2e`) |
| `examples/secretguard` | Working example: detect secrets in tool inputs, prompt the user to accept the risk or block |

## Installing hooks

```go
m := install.Manifest{
	Command: []string{"/usr/local/bin/myhooks"},
	Hooks: []install.HookSpec{
		{Kind: agenthooks.KindToolPre, Blocking: true, Timeout: 30 * time.Second},
		{Kind: agenthooks.KindStop, Blocking: true},
	},
	Identity: install.Identity{Name: "myhooks", Version: "1.0.0"},
	Fail:     agenthooks.FailClosed,
}
err := install.Install(ctx, m, install.Target{
	Provider: agenthooks.ProviderClaudeCode,
	Scope:    install.ScopeProject,
	Dir:      repoRoot,
})
```

Generated configs bake in the argv contract (`mybinary agenthooks run
--provider=...`), per-provider timeout units, async workarounds (sync `Stop`
on Claude cowork, backgrounder wrapper on Codex), Cursor `failClosed`, and
Codex trust-hash pre-seeding.

## Semantics worth knowing

- `NoDecision() != Allow()`: an empty-opinion response defers to the
  provider's own permission flow; it never force-allows.
- Capability degradation is explicit: `Policy.Unsupported` chooses `Degrade`
  (nearest supported intent, logged) or `Strict` (handler error →
  `Policy.Fail`). Check `e.Can(agenthooks.CapAsk)` when you care.
- `Event.Raw` is the verbatim provider payload, always. Unknown fields decode
  into `Extra` on every native struct. Normalization is a projection.
- Provider gaps are backfilled best-effort: Kimi's and Cursor's print modes
  never fire their prompt-submitted hooks, so the runner synthesizes a
  reporting-only `prompt.submitted` before the next event that implies one —
  once per recovered prompt per session, so resumed headless turns
  (`-p --resume`) backfill again. Backfilled events carry `Backfilled: true`,
  a nil `Raw` (nothing is fabricated), no capabilities, and any returned
  decision is discarded — the prompt already reached the model. To gate on a
  recovered prompt, record what the `PromptSubmitted` handler saw and deny in
  the triggering event's handler: the backfill dispatches in the same process
  immediately before it. The prompt text is recovered best-effort — from
  Kimi's session store; on Cursor from the session transcript every hook
  payload names (covering stdin-piped prompts), falling back to the agent
  process's own argv. Disable with `WithoutBackfill()`.
- MCP tool calls carry the target server's transport: `MCPCall.URL`/`Command`
  come from the payload on Cursor MCP events and are otherwise resolved from
  the provider's own MCP config files (`.mcp.json`, `~/.claude.json`,
  `~/.codex/config.toml`, `.cursor/mcp.json`, `.gemini/settings.json` plus
  extension manifests, `~/.kimi/mcp.json`). On Claude Code, servers absent
  from config files
  (plugins, claude.ai connectors) are attributed via `claude mcp list`, run
  in the launching process's project and configuration context. On Claude Code
  2.1.214+, launch-only `--mcp-config`, `--strict-mcp-config`, `--settings`,
  `--setting-sources`, `--plugin-dir`, `--bare`, and
  `--safe-mode` semantics are recovered through `CLAUDE_PID`; older versions
  fall back to the ordinary on-disk context. Inventories are cached briefly by
  a secret-safe context digest and replaced on refresh so removals propagate.
  Remote `--plugin-url` servers stay unattributed rather than being refetched
  with potentially different code during hook dispatch.
  Config-resolved transport is flagged `FromConfig` and is best-effort —
  ambiguous or unrecoverable matches stay empty.
  Disable with `WithoutMCPResolution()` (everything) or
  `WithoutMCPListFallback()` (just the CLI probe).
- The quirk registry (`agenthooks.Quirks()`) is the machine-readable list of
  provider glue this library hides, and doubles as the conformance-test plan.

## Contributing

This repository is maintained by Speakeasy, but we welcome and encourage
contributions from the community to help improve its capabilities and
stability.

### How to Contribute

1. **Discussions**: Have a feature request or want to discuss the roadmap? Use
   [GitHub Discussions](https://github.com/speakeasy-api/agenthooks/discussions)
   to share your ideas and engage with the community.

2. **Issues**: Found a bug or technical issue? Open a
   [GitHub Issue](https://github.com/speakeasy-api/agenthooks/issues) to report
   it with details about the problem. Provider-behavior reports are especially
   valuable — the [quirk registry](quirks.go) only grows through observation.

3. **Pull Requests**: We welcome pull requests! If you'd like to contribute
   code:
   - Fork the repository
   - Create a new branch for your feature/fix
   - Submit a PR with a clear description of the changes and any related issues

4. **Feedback**: Share your experience using the library or suggest
   improvements.

All contributions, whether they're feature requests, bug reports, or code
changes, help make this project better for everyone. Please ensure your
contributions adhere to the existing code style and include appropriate tests
where applicable.

## License

Released under the [MIT License](LICENSE).
