package install

import (
	"io/fs"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks"
)

func TestRenderMoltisHookPacks(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderMoltis, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}

	paths, err := fs.Glob(fsys, ".moltis/hooks/*/HOOK.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("rendered %d hooks, want 3: %v", len(paths), paths)
	}
	pre := string(readRendered(t, fsys, ".moltis/hooks/myhooks-beforetoolcall/HOOK.md"))
	if !strings.Contains(pre, `events = ["BeforeToolCall"]`) ||
		!strings.Contains(pre, `agenthooks run --provider=moltis`) ||
		!strings.Contains(pre, `--timeout=30s`) ||
		!strings.Contains(pre, `--filter=names=Bash`) ||
		!strings.Contains(pre, `timeout = 30`) {
		t.Errorf("BeforeToolCall HOOK.md wrong:\n%s", pre)
	}
	if strings.Contains(pre, "ProviderCodex") || strings.Contains(pre, "OpenClaw") {
		t.Errorf("Moltis renderer leaked another provider:\n%s", pre)
	}

	post := string(readRendered(t, fsys, ".moltis/hooks/myhooks-aftertoolcall/HOOK.md"))
	if !strings.Contains(post, `events = ["AfterToolCall"]`) {
		t.Errorf("AfterToolCall HOOK.md wrong:\n%s", post)
	}
	stop := string(readRendered(t, fsys, ".moltis/hooks/myhooks-agentend/HOOK.md"))
	if !strings.Contains(stop, `events = ["AgentEnd"]`) {
		t.Errorf("AgentEnd HOOK.md wrong:\n%s", stop)
	}
}

func TestRenderMoltisGroupsPostAndErrorOnce(t *testing.T) {
	m := Manifest{
		Command: []string{"/usr/local/bin/myhooks"},
		Hooks: []HookSpec{
			{Kind: agenthooks.KindToolPost, Timeout: 5 * time.Second, Tools: ToolMatcher{Names: []string{"read"}}},
			{Kind: agenthooks.KindToolError, Blocking: true, Timeout: 9 * time.Second, Tools: ToolMatcher{Canonical: []agenthooks.CanonicalTool{agenthooks.ToolShell}}},
		},
		Identity: Identity{Name: "my hooks"},
	}
	fsys, err := Render(m, Target{Provider: agenthooks.ProviderMoltis, Scope: ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := fs.Glob(fsys, "hooks/*/HOOK.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "hooks/my-hooks-aftertoolcall/HOOK.md" {
		t.Fatalf("AfterToolCall must render once: %v", paths)
	}
	content := string(readRendered(t, fsys, paths[0]))
	filter := `--filter='names=read;canonical=shell'`
	if runtime.GOOS == "windows" {
		filter = `--filter="names=read;canonical=shell"`
	}
	if !strings.Contains(content, `--timeout=9s`) ||
		!strings.Contains(content, filter) ||
		!strings.Contains(content, `timeout = 9`) {
		t.Errorf("grouped HOOK.md wrong:\n%s", content)
	}
}

func TestRenderMoltisDeduplicatesGroupedMatchers(t *testing.T) {
	matcher := ToolMatcher{Names: []string{"tra_capability"}, MCP: []string{"tra/*"}}
	m := Manifest{
		Command: []string{"/usr/local/bin/myhooks"},
		Hooks: []HookSpec{
			{Kind: agenthooks.KindToolPost, Tools: matcher},
			{Kind: agenthooks.KindToolError, Tools: matcher},
		},
	}
	fsys, err := Render(m, Target{Provider: agenthooks.ProviderMoltis, Scope: ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	content := string(readRendered(t, fsys, "hooks/agenthooks-aftertoolcall/HOOK.md"))
	if strings.Count(content, "tra_capability") != 1 || strings.Count(content, "tra/*") != 1 {
		t.Fatalf("grouped matcher contains duplicates:\n%s", content)
	}
}

func TestRenderMoltisGroupedMatchAllDominatesScopedMatchers(t *testing.T) {
	for _, specs := range [][]HookSpec{
		{
			{Kind: agenthooks.KindToolPost, Tools: ToolMatcher{}},
			{Kind: agenthooks.KindToolError, Tools: ToolMatcher{Canonical: []agenthooks.CanonicalTool{agenthooks.ToolShell}}},
		},
		{
			{Kind: agenthooks.KindToolPost, Tools: ToolMatcher{Names: []string{"read"}}},
			{Kind: agenthooks.KindToolError, Tools: ToolMatcher{}},
		},
	} {
		m := Manifest{Command: []string{"/usr/local/bin/myhooks"}, Hooks: specs}
		fsys, err := Render(m, Target{Provider: agenthooks.ProviderMoltis, Scope: ScopeUser})
		if err != nil {
			t.Fatal(err)
		}
		content := string(readRendered(t, fsys, "hooks/agenthooks-aftertoolcall/HOOK.md"))
		if strings.Contains(content, "--filter=") {
			t.Errorf("unscoped grouped spec must remain match-all:\n%s", content)
		}
	}
}

func TestRenderMoltisRejectsPluginScope(t *testing.T) {
	_, err := Render(testManifest(), Target{Provider: agenthooks.ProviderMoltis, Scope: ScopePlugin})
	if err == nil || !strings.Contains(err.Error(), "no plugin hook scope") {
		t.Fatalf("plugin scope error = %v", err)
	}
}
