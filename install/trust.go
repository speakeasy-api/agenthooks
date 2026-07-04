package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// codexEventLabel is the snake_case label Codex uses in hook state keys and
// in the normalized identity it hashes (codex-rs hook_event_key_label).
var codexEventLabel = map[string]string{
	"PreToolUse":        "pre_tool_use",
	"PermissionRequest": "permission_request",
	"PostToolUse":       "post_tool_use",
	"PreCompact":        "pre_compact",
	"PostCompact":       "post_compact",
	"SessionStart":      "session_start",
	"UserPromptSubmit":  "user_prompt_submit",
	"SubagentStart":     "subagent_start",
	"SubagentStop":      "subagent_stop",
	"Stop":              "stop",
}

// DefinitionHash reimplements Codex's hook definition fingerprint so installs
// can pre-seed trusted hashes (quirk #9). Codex hashes a normalized identity
// rather than source text (codex-rs hooks/engine/discovery.rs
// command_hook_hash + config/fingerprint.rs version_for_toml): sha256 over
// the compact, recursively key-sorted JSON of
//
//	{"event_name": <label>, "matcher": <m>?, "hooks": [
//	  {"async": false, "command": <cmd>, "timeout": <secs>, "type": "command"}]}
//
// prefixed "sha256:". The matcher is included only when present in the
// rendered config and never for UserPromptSubmit/Stop (Codex forces it absent
// there); an absent timeout defaults to 600 and is clamped to >= 1. Verified
// against codex-cli 0.142.4 trust state.
func DefinitionHash(event, matcher, command string, timeoutSeconds int) string {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 600
	}
	identity := map[string]any{
		"event_name": codexEventLabel[event],
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": command,
			"timeout": timeoutSeconds,
			"async":   false,
		}},
	}
	if matcher != "" && event != "UserPromptSubmit" && event != "Stop" {
		identity["matcher"] = matcher
	}
	// encoding/json sorts map keys; serde_json does not HTML-escape, so
	// neither may we.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(identity); err != nil {
		// Marshal of plain strings/ints cannot fail; keep the signature clean.
		panic(err)
	}
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return "sha256:" + hex.EncodeToString(sum[:])
}
