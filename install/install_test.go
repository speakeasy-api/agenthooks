package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

func TestShellQuoteBodyQuotesCommandSeparators(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell quoting assertion")
	}
	if got, want := shellQuoteBody("names=read;canonical=shell"), "'names=read;canonical=shell'"; got != want {
		t.Fatalf("shellQuoteBody() = %q, want %q", got, want)
	}
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
	m := testManifest()
	m.Hooks = append(m.Hooks, HookSpec{Kind: agenthooks.KindSessionEnd, Blocking: false, Timeout: 60 * time.Second})
	fsys, err := Render(m, Target{Provider: agenthooks.ProviderCodex, Scope: ScopeUser, Dir: "/codex-home"})
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
	sessionEndEntry := cfg.Hooks["SessionEnd"][0]
	sessionEnd := sessionEndEntry.Hooks[0]
	if sessionEnd.Timeout != 3 || !strings.HasSuffix(sessionEnd.Command, " --async") {
		t.Errorf("SessionEnd must detach within Codex's 3-second teardown budget: %+v", sessionEnd)
	}

	// Trust state keys are "<CODEX_HOME>/hooks.json:<event_label>:<group>:<handler>"
	// and land in config.toml inside the managed marker region. The source
	// path is OS-native (filepath.Join, like Codex's own canonicalization),
	// so build the expectation the same way and TOML-escape it.
	trust := string(readRendered(t, fsys, "config.toml"))
	source := filepath.Join("/codex-home", "hooks.json")
	if !strings.Contains(trust, `[hooks.state.`+tomlString(source+":pre_tool_use:0:0")+`]`) {
		t.Errorf("trust seeding missing pre_tool_use state key:\n%s", trust)
	}
	if !strings.Contains(trust, `[hooks.state.`+tomlString(source+":stop:0:0")+`]`) {
		t.Errorf("trust seeding missing stop state key:\n%s", trust)
	}
	if !strings.Contains(trust, `[hooks.state.`+tomlString(source+":session_end:0:0")+`]`) {
		t.Errorf("trust seeding missing session_end state key:\n%s", trust)
	}
	wantHash := DefinitionHash("PreToolUse", preEntry.Matcher, pre.Command, pre.Timeout)
	if !strings.Contains(trust, `trusted_hash = "`+wantHash+`"`) {
		t.Errorf("trust file must contain the definition hash %s:\n%s", wantHash, trust)
	}
	wantSessionEndHash := DefinitionHash("SessionEnd", sessionEndEntry.Matcher, sessionEnd.Command, sessionEnd.Timeout)
	if !strings.Contains(trust, `trusted_hash = "`+wantSessionEndHash+`"`) {
		t.Errorf("trust file must contain the SessionEnd definition hash %s:\n%s", wantSessionEndHash, trust)
	}
	if !strings.Contains(trust, "BEGIN agenthooks managed hooks") || !strings.Contains(trust, "END agenthooks managed hooks") {
		t.Errorf("codex config.toml must use the managed marker region:\n%s", trust)
	}
}

func TestRenderCodexTrustSeedsLexicalAndResolvedSymlinkHomes(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "archive", "codex-home")
	if err := os.MkdirAll(realHome, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(root, "codex-home")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	fsys, err := Render(testManifest(), Target{
		Provider: agenthooks.ProviderCodex,
		Scope:    ScopeUser,
		Dir:      linkedHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	trust := string(readRendered(t, fsys, "config.toml"))
	for _, source := range []string{
		filepath.Join(linkedHome, "hooks.json"),
		filepath.Join(realHome, "hooks.json"),
	} {
		key := source + ":pre_tool_use:0:0"
		if !strings.Contains(trust, `[hooks.state.`+tomlString(key)+`]`) {
			t.Errorf("trust seeding missing symlink identity %q:\n%s", key, trust)
		}
	}
}

func TestRenderCodexTrustResolvesSymlinkParentForMissingHome(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "archive")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	linkedHome := filepath.Join(linkedParent, "nested", "codex-home")
	realHome := filepath.Join(realParent, "nested", "codex-home")

	fsys, err := Render(testManifest(), Target{
		Provider: agenthooks.ProviderCodex,
		Scope:    ScopeUser,
		Dir:      linkedHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	trust := string(readRendered(t, fsys, "config.toml"))
	for _, source := range []string{
		filepath.Join(linkedHome, "hooks.json"),
		filepath.Join(realHome, "hooks.json"),
	} {
		key := source + ":pre_tool_use:0:0"
		if !strings.Contains(trust, `[hooks.state.`+tomlString(key)+`]`) {
			t.Errorf("trust seeding missing fresh-home identity %q:\n%s", key, trust)
		}
	}
	if _, err := os.Stat(realHome); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Render created the target directory or returned an unexpected error: %v", err)
	}
}

func TestInstallCodexTrustResolvesSymlinkParentForMissingHome(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "archive")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	linkedHome := filepath.Join(linkedParent, "nested", "codex-home")
	realHome := filepath.Join(realParent, "nested", "codex-home")
	target := Target{Provider: agenthooks.ProviderCodex, Scope: ScopeUser, Dir: linkedHome}

	if err := Install(context.Background(), testManifest(), target); err != nil {
		t.Fatal(err)
	}
	trust, err := os.ReadFile(filepath.Join(realHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{
		filepath.Join(linkedHome, "hooks.json"),
		filepath.Join(realHome, "hooks.json"),
	} {
		key := source + ":pre_tool_use:0:0"
		if !strings.Contains(string(trust), `[hooks.state.`+tomlString(key)+`]`) {
			t.Errorf("installed trust missing fresh-home identity %q:\n%s", key, trust)
		}
	}
}

func TestInstallCodexReplacesStaleTrustOutsideManagedRegion(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "archive", "codex-home")
	if err := os.MkdirAll(realHome, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(root, "codex-home")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	logicalKey := filepath.Join(linkedHome, "hooks.json") + ":pre_tool_use:1:0"
	existing := fmt.Sprintf(`[unrelated]
value = "preserved"

[hooks.state.%s]
trusted_hash = "sha256:stale"
`, tomlString(logicalKey))
	if err := os.WriteFile(filepath.Join(realHome, "config.toml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignHooks := `{"hooks":{"PreToolUse":[{"matcher":"Read","hooks":[{"type":"command","command":"foreign-hook check","timeout":10}]}]}}`
	if !json.Valid([]byte(foreignHooks)) {
		t.Fatalf("test fixture is not valid JSON: %q", foreignHooks)
	}
	if err := os.WriteFile(filepath.Join(realHome, "hooks.json"), []byte(foreignHooks), 0o600); err != nil {
		t.Fatal(err)
	}

	target := Target{Provider: agenthooks.ProviderCodex, Scope: ScopeUser, Dir: linkedHome}
	if err := Install(context.Background(), testManifest(), target); err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(filepath.Join(realHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	header := `[hooks.state.` + tomlString(logicalKey) + `]`
	if count := strings.Count(string(merged), header); count != 1 {
		t.Fatalf("logical trust table count = %d, want 1:\n%s", count, merged)
	}
	if strings.Contains(string(merged), "sha256:stale") {
		t.Fatalf("stale trust hash survived:\n%s", merged)
	}
	if !strings.Contains(string(merged), `[unrelated]`) || !strings.Contains(string(merged), `value = "preserved"`) {
		t.Fatalf("foreign TOML was not preserved:\n%s", merged)
	}
	if strings.Contains(string(merged), filepath.Join(linkedHome, "hooks.json")+":pre_tool_use:0:0") {
		t.Fatalf("managed trust used the standalone rather than merged group index:\n%s", merged)
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
	for _, want := range []string{
		"ctx.client.config.get()", "?.data", "mcp === undefined",
		"type: server?.type", "command: server?.command", "url: server?.url", "enabled: server?.enabled",
		"await initialize()",
	} {
		if !strings.Contains(shim, want) {
			t.Errorf("shim must carry resolved MCP config via %q", want)
		}
	}
	if strings.Contains(shim, "server?.headers") || strings.Contains(shim, "server?.environment") {
		t.Error("shim must not forward MCP credentials")
	}
}

// TestRenderCopilotPlugin pins the three things that silently break Copilot
// telemetry rather than failing loudly:
//   - a matcher key at all (an empty one is a validation error that discards
//     this plugin's ENTIRE hook config),
//   - a second copy of the config at <root>/hooks.json (Copilot parses both
//     paths, so shipping both double-registers every hook),
//   - bash/powershell keys (Copilot fills both from command; splitting them
//     here would render the argv twice with no test on the second copy).
func TestRenderCopilotPlugin(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderCopilotCLI, Scope: ScopePlugin})
	if err != nil {
		t.Fatal(err)
	}
	var plugin map[string]string
	if err := json.Unmarshal(readRendered(t, fsys, "plugin.json"), &plugin); err != nil {
		t.Fatal(err)
	}
	if plugin["name"] != "myhooks" {
		t.Errorf("plugin.json must sit at the package root with the manifest name: %v", plugin)
	}
	if _, err := fs.ReadFile(fsys, "hooks.json"); err == nil {
		t.Error("hooks.json at the package root double-registers every hook")
	}

	raw := readRendered(t, fsys, "hooks/hooks.json")
	if bytes.Contains(raw, []byte(`"matcher"`)) {
		t.Errorf("matcher key present; an empty matcher discards the whole hook config:\n%s", raw)
	}
	var cfg struct {
		Version int `json:"version"`
		Hooks   map[string][]struct {
			Type       string          `json:"type"`
			Command    string          `json:"command"`
			TimeoutSec int             `json:"timeoutSec"`
			Bash       json.RawMessage `json:"bash"`
			PowerShell json.RawMessage `json:"powershell"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 {
		t.Errorf("version = %d, want 1", cfg.Version)
	}
	// testManifest declares ToolPre, Stop and ToolPost; each maps to exactly one
	// native Copilot event, so there is no double-fire to dedupe.
	if len(cfg.Hooks) != 3 {
		t.Errorf("hooks = %v, want one entry per declared kind", cfg.Hooks)
	}
	pre := cfg.Hooks["preToolUse"]
	if len(pre) != 1 {
		t.Fatalf("preToolUse entries = %+v", pre)
	}
	if pre[0].Type != "command" || pre[0].TimeoutSec != 30 {
		t.Errorf("preToolUse entry wrong: %+v", pre[0])
	}
	if !strings.Contains(pre[0].Command, "agenthooks run --provider=copilot-cli") {
		t.Errorf("command wrong: %q", pre[0].Command)
	}
	if len(pre[0].Bash) > 0 || len(pre[0].PowerShell) > 0 {
		t.Error("bash/powershell must be absent; Copilot copies command into both")
	}
	for _, event := range []string{"agentStop", "postToolUse"} {
		if len(cfg.Hooks[event]) != 1 {
			t.Errorf("%s not registered: %v", event, cfg.Hooks)
		}
	}
}

func TestRenderCopilotScopes(t *testing.T) {
	// Project scope goes to .github/hooks/, user scope to hooks/hooks.json
	// under Target.Dir (~/.copilot). Plugin scope has nowhere to put the name.
	proj, err := Render(testManifest(), Target{Provider: agenthooks.ProviderCopilotCLI, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}
	readRendered(t, proj, ".github/hooks/agenthooks.json")

	user, err := Render(testManifest(), Target{Provider: agenthooks.ProviderCopilotCLI, Scope: ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	readRendered(t, user, "hooks/hooks.json")

	m := testManifest()
	m.Identity.Name = ""
	if _, err := Render(m, Target{Provider: agenthooks.ProviderCopilotCLI, Scope: ScopePlugin}); err == nil {
		t.Error("plugin scope with no Identity.Name must fail, not emit a nameless package")
	}
}

// TestRenderVSCodeScopes pins the two paths and the basename. Both directories
// are globbed by VS Code AND by the Copilot CLI, so the basename is the only
// thing keeping this file from colliding with render_copilot.go's — and
// agenthooks-vscode.json is neither settings.json nor hooks.json, so the file
// stays whole-file owned instead of being merged into.
func TestRenderVSCodeScopes(t *testing.T) {
	proj, err := Render(testManifest(), Target{Provider: agenthooks.ProviderVSCodeCopilot, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}
	readRendered(t, proj, ".github/hooks/agenthooks-vscode.json")

	user, err := Render(testManifest(), Target{Provider: agenthooks.ProviderVSCodeCopilot, Scope: ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	raw := readRendered(t, user, "hooks/agenthooks-vscode.json") // Target.Dir is ~/.copilot
	if isMergeableJSON("hooks/agenthooks-vscode.json") {
		t.Error("agenthooks-vscode.json must not be merge-eligible; a merge would fold it into the CLI's config")
	}

	if _, err := Render(testManifest(), Target{Provider: agenthooks.ProviderVSCodeCopilot, Scope: ScopePlugin}); err == nil {
		t.Error("plugin scope must fail: VS Code loads plugin hooks through ~/.copilot, which user scope already covers")
	}

	// PascalCase event keys, one per declared kind. A camelCase key here would
	// still resolve in VS Code (it accepts the CLI's names) but would pick up
	// the CLI's event vocabulary, where Stop is spelled agentStop.
	var cfg struct {
		Hooks map[string][]struct {
			Type       string `json:"type"`
			Command    string `json:"command"`
			Timeout    int    `json:"timeout"`
			TimeoutSec int    `json:"timeoutSec"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"PreToolUse", "Stop", "PostToolUse"} {
		if len(cfg.Hooks[event]) != 1 {
			t.Fatalf("%s not registered: %v", event, cfg.Hooks)
		}
	}
	if len(cfg.Hooks) != 3 {
		t.Errorf("hooks = %v, want one entry per declared kind", cfg.Hooks)
	}
	pre := cfg.Hooks["PreToolUse"][0]
	if pre.Type != "command" {
		t.Errorf("type = %q, want command", pre.Type)
	}
	if !strings.Contains(pre.Command, "agenthooks run --provider=vscode-copilot") {
		t.Errorf("command wrong: %q", pre.Command)
	}
	// VS Code parses matcher values and ignores them, so --filter is the only
	// enforcement that is actually true.
	if !strings.Contains(pre.Command, "--filter=") {
		t.Errorf("no --filter in %q; VS Code ignores matchers, so a scoped hook would fire on every tool", pre.Command)
	}
}

// TestRenderVSCodeOmissions pins the keys whose wrong presence or absence fails
// silently rather than loudly: a matcher that reads as enforcement VS Code does
// not perform, a version key no VS Code example carries (an unknown key is a
// schema-validation risk), bash/powershell keys VS Code does not understand at
// all (its platform override is windows), and the two unreconciled timeout
// spellings — the reference table says timeout, a usage example says
// timeoutSec, and reading the missing one silently means the 30s default.
func TestRenderVSCodeOmissions(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderVSCodeCopilot, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}
	raw := readRendered(t, fsys, ".github/hooks/agenthooks-vscode.json")
	for _, key := range []string{`"matcher"`, `"version"`, `"bash"`, `"powershell"`, `"osx"`} {
		if bytes.Contains(raw, []byte(key)) {
			t.Errorf("%s key present:\n%s", key, raw)
		}
	}
	var cfg struct {
		Hooks map[string][]struct {
			Timeout    *int `json:"timeout"`
			TimeoutSec *int `json:"timeoutSec"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	pre := cfg.Hooks["PreToolUse"][0]
	if pre.Timeout == nil || pre.TimeoutSec == nil || *pre.Timeout != 30 || *pre.TimeoutSec != 30 {
		t.Errorf("both timeout spellings must carry the same value, got timeout=%v timeoutSec=%v", pre.Timeout, pre.TimeoutSec)
	}
	stop := cfg.Hooks["Stop"][0]
	if stop.Timeout == nil || *stop.Timeout != 60 {
		t.Errorf("Stop timeout = %v, want the 60s default", stop.Timeout)
	}
}

// TestHookCommandQuotesSpacedBinary pins the quoting of a consumer binary whose
// path contains spaces. The dialect follows the HOST OS, not the provider
// (shellQuote, install.go:355-374): configs are rendered on the machine that
// runs them, cmd.exe has no single-quote syntax and POSIX shells have no
// cmd-style escaping.
//
// The Windows form is cmd.exe's, and deliberately so: PowerShell parses a
// statement that begins with a quote as an expression, so it needs a leading
// call operator (& "C:\Program Files\...") that cmd.exe in turn rejects — no
// single string is valid in both. Both shells are reachable on Windows (the
// Copilot CLI copies command into its powershell key, render_copilot.go:34-36),
// so if a spaced path is ever observed failing under PowerShell the fix is a
// per-shell rendering for those providers, not a change to this quoting.
func TestHookCommandQuotesSpacedBinary(t *testing.T) {
	m := testManifest()
	m.Command = []string{"/opt/My Hooks/myhooks"}
	want := `'/opt/My Hooks/myhooks' agenthooks run --provider=vscode-copilot`
	if runtime.GOOS == "windows" {
		want = `"/opt/My Hooks/myhooks" agenthooks run --provider=vscode-copilot`
	}
	got := hookCommand(m, agenthooks.ProviderVSCodeCopilot, m.Hooks[0])
	if !strings.HasPrefix(got, want) {
		t.Errorf("spaced binary path unquoted or wrong dialect:\n got %s\nwant prefix %s", got, want)
	}
}
