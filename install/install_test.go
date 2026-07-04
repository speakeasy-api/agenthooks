package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks"
)

func testManifest() Manifest {
	return Manifest{
		Command: []string{"/usr/local/bin/myhooks"},
		Hooks: []HookSpec{
			{Kind: agenthooks.KindToolPre, Blocking: true, Timeout: 30 * time.Second,
				Tools: ToolMatcher{Names: []string{"Bash"}}},
			{Kind: agenthooks.KindStop, Blocking: false},
			{Kind: agenthooks.KindToolPost, Blocking: false},
		},
		Identity: Identity{Name: "myhooks", Version: "1.0.0", Description: "test hooks"},
		Fail:     agenthooks.FailClosed,
	}
}

func readRendered(t *testing.T, fsys fs.FS, path string) []byte {
	t.Helper()
	b, err := fs.ReadFile(fsys, path)
	if err != nil {
		t.Fatalf("rendered file %s: %v", path, err)
	}
	return b
}

type claudeConfig struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
			Async   bool   `json:"async"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func TestRenderClaudePlugin(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderClaudeCode, Scope: ScopePlugin})
	if err != nil {
		t.Fatal(err)
	}
	var plugin map[string]string
	if err := json.Unmarshal(readRendered(t, fsys, ".claude-plugin/plugin.json"), &plugin); err != nil {
		t.Fatal(err)
	}
	if plugin["name"] != "myhooks" {
		t.Errorf("plugin.json wrong: %v", plugin)
	}

	var cfg claudeConfig
	if err := json.Unmarshal(readRendered(t, fsys, "hooks/hooks.json"), &cfg); err != nil {
		t.Fatal(err)
	}
	pre := cfg.Hooks["PreToolUse"]
	if len(pre) != 1 || pre[0].Matcher != "Bash" {
		t.Fatalf("PreToolUse entry wrong: %+v", pre)
	}
	cmd := pre[0].Hooks[0]
	if !strings.Contains(cmd.Command, "agenthooks run --provider=claude-code") || cmd.Timeout != 30 || cmd.Async {
		t.Errorf("PreToolUse command wrong: %+v", cmd)
	}
	// quirk #1: Stop is forced synchronous even for telemetry hooks.
	stop := cfg.Hooks["Stop"]
	if len(stop) != 1 || stop[0].Hooks[0].Async {
		t.Errorf("Stop must render async:false (cowork drops async Stop): %+v", stop)
	}
	// Telemetry hooks elsewhere stay async.
	post := cfg.Hooks["PostToolUse"]
	if len(post) != 1 || !post[0].Hooks[0].Async {
		t.Errorf("non-blocking PostToolUse should be async: %+v", post)
	}
}

func TestRenderCursorFailClosed(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderCursor, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Version int `json:"version"`
		Hooks   map[string][]struct {
			Command    string `json:"command"`
			Timeout    int    `json:"timeout"`
			FailClosed bool   `json:"failClosed"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(readRendered(t, fsys, ".cursor/hooks.json"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 {
		t.Errorf("version = %d", cfg.Version)
	}
	// tool.pre expands to the specific + generic events (runner dedupes).
	for _, event := range []string{"beforeShellExecution", "beforeMCPExecution", "beforeReadFile", "preToolUse"} {
		entries := cfg.Hooks[event]
		if len(entries) != 1 {
			t.Fatalf("%s missing", event)
		}
		if !entries[0].FailClosed {
			t.Errorf("%s: decision hooks must render failClosed (quirk #7)", event)
		}
		// Cursor can't express matchers: the filter must ride the argv.
		if !strings.Contains(entries[0].Command, "--filter=") {
			t.Errorf("%s: matcher must compile to --filter: %s", event, entries[0].Command)
		}
	}
	if entries := cfg.Hooks["stop"]; len(entries) != 1 || entries[0].FailClosed {
		t.Errorf("telemetry stop hook must stay fail-open: %+v", entries)
	}
}

// quirk #29: a scheme:// URL in any command makes Cursor silently drop the
// whole hooks.json, so rendering one must fail loudly instead.
func TestRenderCursorRejectsURLInCommand(t *testing.T) {
	m := testManifest()
	m.Command = []string{"/usr/local/bin/myhooks", "--server=https://example.com"}
	_, err := Render(m, Target{Provider: agenthooks.ProviderCursor, Scope: ScopeProject})
	if err == nil || !strings.Contains(err.Error(), "quirk #29") {
		t.Fatalf("expected quirk #29 rejection, got %v", err)
	}
	// Other providers accept URLs in commands.
	if _, err := Render(m, Target{Provider: agenthooks.ProviderClaudeCode, Scope: ScopePlugin}); err != nil {
		t.Fatalf("claude render should accept URL commands: %v", err)
	}
}

func TestRenderGeminiMilliseconds(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderGemini, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int64  `json:"timeout"`
				Name    string `json:"name"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(readRendered(t, fsys, ".gemini/settings.json"), &cfg); err != nil {
		t.Fatal(err)
	}
	pre := cfg.Hooks["BeforeTool"]
	if len(pre) != 1 || pre[0].Hooks[0].Timeout != 30000 {
		t.Errorf("gemini timeout must be milliseconds (quirk #14): %+v", pre)
	}
	if pre[0].Hooks[0].Name != "myhooks:tool.pre" {
		t.Errorf("gemini hooks need names for /hooks UX: %+v", pre[0].Hooks[0])
	}
}

func TestRenderCodexAsyncAndTrust(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderCodex, Scope: ScopeUser, Dir: "/codex-home"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg claudeConfig
	if err := json.Unmarshal(readRendered(t, fsys, "hooks.json"), &cfg); err != nil {
		t.Fatal(err)
	}
	// Codex parses-but-skips async (quirk #10): telemetry hooks get --async
	// so the runner detaches itself; no shell wrapper anywhere.
	post := cfg.Hooks["PostToolUse"][0].Hooks[0]
	if !strings.HasSuffix(post.Command, " --async") || strings.Contains(post.Command, "/bin/sh") {
		t.Errorf("non-blocking codex hook must render --async without a shell: %s", post.Command)
	}
	preEntry := cfg.Hooks["PreToolUse"][0]
	pre := preEntry.Hooks[0]
	if strings.Contains(pre.Command, "--async") {
		t.Errorf("blocking codex hook must stay synchronous: %s", pre.Command)
	}

	// Trust state keys are "<CODEX_HOME>/hooks.json:<event_label>:<group>:<handler>"
	// and land in config.toml inside the managed marker region.
	trust := string(readRendered(t, fsys, "config.toml"))
	if !strings.Contains(trust, `[hooks.state."/codex-home/hooks.json:pre_tool_use:0:0"]`) {
		t.Errorf("trust seeding missing pre_tool_use state key:\n%s", trust)
	}
	if !strings.Contains(trust, `[hooks.state."/codex-home/hooks.json:stop:0:0"]`) {
		t.Errorf("trust seeding missing stop state key:\n%s", trust)
	}
	wantHash := DefinitionHash("PreToolUse", preEntry.Matcher, pre.Command, pre.Timeout)
	if !strings.Contains(trust, `trusted_hash = "`+wantHash+`"`) {
		t.Errorf("trust file must contain the definition hash %s:\n%s", wantHash, trust)
	}
	if !strings.Contains(trust, "BEGIN agenthooks managed hooks") || !strings.Contains(trust, "END agenthooks managed hooks") {
		t.Errorf("codex config.toml must use the managed marker region:\n%s", trust)
	}
}

// TestDefinitionHashCodexAlgorithm pins the exact identity serialization
// Codex hashes (verified against codex-cli 0.142.4 trust state): sha256 over
// compact, key-sorted JSON with HTML escaping disabled.
func TestDefinitionHashCodexAlgorithm(t *testing.T) {
	// Matcher present: {"event_name":"pre_tool_use","hooks":[{"async":false,
	// "command":"run <a> & \"b\"","timeout":30,"type":"command"}],"matcher":"Bash"}
	sum := sha256.Sum256([]byte(`{"event_name":"pre_tool_use","hooks":[{"async":false,"command":"run <a> & \"b\"","timeout":30,"type":"command"}],"matcher":"Bash"}`))
	if got, want := DefinitionHash("PreToolUse", "Bash", `run <a> & "b"`, 30), "sha256:"+hex.EncodeToString(sum[:]); got != want {
		t.Errorf("DefinitionHash = %s, want %s", got, want)
	}
	// Codex forces the matcher absent on Stop; absent timeout defaults to 600.
	sum = sha256.Sum256([]byte(`{"event_name":"stop","hooks":[{"async":false,"command":"cmd","timeout":600,"type":"command"}]}`))
	if got, want := DefinitionHash("Stop", "ignored", "cmd", 0), "sha256:"+hex.EncodeToString(sum[:]); got != want {
		t.Errorf("DefinitionHash(Stop) = %s, want %s", got, want)
	}
}

func TestInstallIdempotentAndMergePreservesForeignEntries(t *testing.T) {
	dir := t.TempDir()
	target := Target{Provider: agenthooks.ProviderClaudeCode, Scope: ScopeProject, Dir: dir}
	m := testManifest()

	// Pre-existing user config with a foreign hook and an unrelated key.
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := `{"env":{"FOO":"1"},"hooks":{"PreToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"other-tool check"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Install(context.Background(), m, target); err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), "other-tool check") || !strings.Contains(string(merged), `"FOO"`) {
		t.Errorf("foreign config must survive merge:\n%s", merged)
	}
	if !strings.Contains(string(merged), "agenthooks run --provider=claude-code") {
		t.Errorf("managed hooks missing after merge:\n%s", merged)
	}

	// Second install: nothing to do.
	changes, err := Diff(m, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.State != StateUnchanged {
			t.Errorf("expected idempotence, got %s for %s", c.State, c.Path)
		}
	}
}

func TestFingerprint(t *testing.T) {
	m := testManifest()
	target := Target{Provider: agenthooks.ProviderClaudeCode, Scope: ScopeProject}
	a, err := Fingerprint(m, target)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Fingerprint(m, target)
	if a != b {
		t.Error("fingerprint must be stable")
	}
	m.Hooks[0].Timeout = 45 * time.Second
	c, _ := Fingerprint(m, target)
	if a == c {
		t.Error("fingerprint must change with the manifest")
	}
}

func TestRenderKimiTOML(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderKimi, Scope: ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	toml := string(readRendered(t, fsys, "config.toml"))
	if !strings.HasPrefix(toml, tomlBeginMarker) || !strings.Contains(toml, tomlEndMarker) {
		t.Errorf("managed markers missing:\n%s", toml)
	}
	if !strings.Contains(toml, `event = "PreToolUse"`) || !strings.Contains(toml, `matcher = "Bash"`) {
		t.Errorf("PreToolUse entry wrong:\n%s", toml)
	}
	if !strings.Contains(toml, "agenthooks run --provider=kimi-code") {
		t.Errorf("argv contract missing:\n%s", toml)
	}
	if !strings.Contains(toml, "timeout = 30") {
		t.Errorf("timeout must render in seconds:\n%s", toml)
	}

	// Kimi reads hooks from the single user-level config.toml only:
	// project scope must fail loudly, not render dead config.
	if _, err := Render(testManifest(), Target{Provider: agenthooks.ProviderKimi, Scope: ScopeProject}); err == nil {
		t.Error("project scope must be rejected for kimi")
	}
}

func TestInstallKimiMergePreservesForeignTOML(t *testing.T) {
	dir := t.TempDir()
	target := Target{Provider: agenthooks.ProviderKimi, Scope: ScopeUser, Dir: dir}
	m := testManifest()

	tomlPath := filepath.Join(dir, "config.toml")
	foreign := "[[hooks]]\nevent = \"Notification\"\ncommand = \"terminal-notifier -title Kimi\"\n"
	if err := os.WriteFile(tomlPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Install(context.Background(), m, target); err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), "terminal-notifier") {
		t.Errorf("foreign hook must survive merge:\n%s", merged)
	}
	if !strings.Contains(string(merged), "agenthooks run --provider=kimi-code") {
		t.Errorf("managed hooks missing after merge:\n%s", merged)
	}

	// Second install: idempotent.
	changes, err := Diff(m, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.State != StateUnchanged {
			t.Errorf("expected idempotence, got %s for %s", c.State, c.Path)
		}
	}

	// Re-install after a manifest change replaces only the managed region.
	m.Hooks[0].Timeout = 45 * time.Second
	if err := Install(context.Background(), m, target); err != nil {
		t.Fatal(err)
	}
	merged, _ = os.ReadFile(tomlPath)
	if !strings.Contains(string(merged), "terminal-notifier") || !strings.Contains(string(merged), "timeout = 45") {
		t.Errorf("managed-region replacement broken:\n%s", merged)
	}
	if strings.Count(string(merged), tomlBeginMarker) != 1 {
		t.Errorf("markers must not duplicate:\n%s", merged)
	}
}

func TestRenderOpenCodeShim(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderOpenCode, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}
	shim := string(readRendered(t, fsys, ".opencode/plugin/agenthooks.ts"))
	if !strings.Contains(shim, `"/usr/local/bin/myhooks"`) {
		t.Errorf("shim must bake in the command:\n%s", shim[:200])
	}
	if !strings.Contains(shim, `"agenthooks", "serve", "--provider=opencode"`) {
		t.Error("shim must spawn serve mode")
	}
}
