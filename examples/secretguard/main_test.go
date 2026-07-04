package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// AWS's documented example credentials — recognizable shapes, not real keys.
const (
	fakeAWSKey  = "AKIAIOSFODNN7EXAMPLE"
	cleanClaude = `{"session_id":"s","cwd":"/w","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./..."},"tool_use_id":"t1"}`
)

func invoke(t *testing.T, provider, payload string) string {
	t.Helper()
	var out, errb bytes.Buffer
	code := buildRunner().Run(context.Background(),
		[]string{"agenthooks", "run", "--provider=" + provider},
		strings.NewReader(payload), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	return out.String()
}

func secretClaude(t *testing.T) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"session_id": "s", "cwd": "/w", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "t1",
		"tool_input": map[string]any{
			"command": "export AWS_ACCESS_KEY_ID=" + fakeAWSKey + " && aws s3 ls",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func TestSecretPromptsOnClaude(t *testing.T) {
	out := invoke(t, "claude-code", secretClaude(t))
	if !strings.Contains(out, `"permissionDecision":"ask"`) {
		t.Errorf("secret should force a confirmation prompt: %s", out)
	}
	if !strings.Contains(out, "AWS access key ID") || !strings.Contains(out, "accept the risk") {
		t.Errorf("prompt reason should name the finding and the choice: %s", out)
	}
	if strings.Contains(out, fakeAWSKey) {
		t.Errorf("the secret itself must never appear in the response: %s", out)
	}
}

func TestSecretBlocksWhereAskUnsupported(t *testing.T) {
	// Same handler, Codex dialect: no ask support, so the explicit
	// Can(CapAsk) branch denies.
	payload := strings.Replace(secretClaude(t), `"session_id":"s"`, `"session_id":"s","turn_id":"t"`, 1)
	out := invoke(t, "codex", payload)
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("no prompt available: secret must block: %s", out)
	}
	if strings.Contains(out, "Approve to accept") {
		t.Errorf("deny path must not offer a prompt that does not exist: %s", out)
	}
}

func TestSecretBlocksOnKimi(t *testing.T) {
	// Kimi has deny|allow only — no confirmation prompt (quirk #22) — so the
	// same handler must land on the deny branch.
	payload := strings.Replace(secretClaude(t), `"tool_use_id":"t1"`, `"tool_call_id":"t1"`, 1)
	out := invoke(t, "kimi-code", payload)
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("no prompt available: secret must block: %s", out)
	}
	if strings.Contains(out, "Approve to accept") {
		t.Errorf("deny path must not offer a prompt that does not exist: %s", out)
	}
}

func TestCleanInputNoOps(t *testing.T) {
	if out := invoke(t, "claude-code", cleanClaude); out != "{}" {
		t.Errorf("clean input must defer to the provider's normal flow: %s", out)
	}
}

func TestScanRules(t *testing.T) {
	hits := map[string]string{
		"AWS access key ID":       "key=" + fakeAWSKey,
		"GitHub token":            "ghp_" + strings.Repeat("a1", 18),
		"Slack token":             "xoxb-1234567890-abcdef",
		"Anthropic API key":       "sk-ant-" + strings.Repeat("x", 24),
		"OpenAI API key":          "sk-" + strings.Repeat("z", 24),
		"private key block":       "-----BEGIN RSA PRIVATE KEY-----\nMIIB...",
		"JWT":                     "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9P",
		"assigned secret literal": `password = "hunter2hunter2"`,
	}
	for rule, sample := range hits {
		input, _ := json.Marshal(map[string]string{"v": sample})
		findings := Scan(input)
		found := false
		for _, f := range findings {
			if f.Rule == rule {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: not detected in %q (got %v)", rule, sample, findings)
		}
	}

	clean, _ := json.Marshal(map[string]string{"command": "ls -la", "note": "no creds here"})
	if findings := Scan(clean); len(findings) != 0 {
		t.Errorf("clean input flagged: %v", findings)
	}
}

func TestMaskNeverLeaksFullValue(t *testing.T) {
	input, _ := json.Marshal(map[string]string{"k": fakeAWSKey})
	findings := Scan(input)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if findings[0].Masked == fakeAWSKey || !strings.Contains(findings[0].Masked, "…") {
		t.Errorf("masked value leaks: %q", findings[0].Masked)
	}
}
