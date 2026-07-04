package agenthooks

import (
	"fmt"
	"sort"
	"strings"
)

// ToolMatcher is the unified tool matcher used both by config generation
// (compiled to each provider's matcher dialect where expressible) and by the
// runner's in-process filter (--filter flag) where not.
type ToolMatcher struct {
	Names     []string        // exact native tool names
	Canonical []CanonicalTool // canonical classes
	MCP       []string        // "server/*" or "server/tool" globs
}

// IsEmpty reports whether the matcher matches everything.
func (m ToolMatcher) IsEmpty() bool {
	return len(m.Names) == 0 && len(m.Canonical) == 0 && len(m.MCP) == 0
}

// Matches reports whether the tool call satisfies the matcher. An empty
// matcher matches every tool.
func (m ToolMatcher) Matches(t ToolCall) bool {
	if m.IsEmpty() {
		return true
	}
	for _, n := range m.Names {
		if strings.EqualFold(n, t.Name) {
			return true
		}
	}
	for _, c := range m.Canonical {
		if c == t.Canonical {
			return true
		}
	}
	if t.MCP != nil {
		for _, g := range m.MCP {
			server, tool, ok := strings.Cut(g, "/")
			if !ok {
				server, tool = g, "*"
			}
			if (server == "*" || server == t.MCP.Server) && (tool == "*" || tool == t.MCP.Tool) {
				return true
			}
		}
	}
	return false
}

// Encode serializes the matcher for the --filter flag baked into generated
// configs whose provider dialect can't express it.
func (m ToolMatcher) Encode() string {
	var parts []string
	if len(m.Names) > 0 {
		parts = append(parts, "names="+strings.Join(m.Names, ","))
	}
	if len(m.Canonical) > 0 {
		cs := make([]string, len(m.Canonical))
		for i, c := range m.Canonical {
			cs[i] = string(c)
		}
		parts = append(parts, "canonical="+strings.Join(cs, ","))
	}
	if len(m.MCP) > 0 {
		parts = append(parts, "mcp="+strings.Join(m.MCP, ","))
	}
	return strings.Join(parts, ";")
}

// ParseToolMatcher parses the --filter flag format produced by Encode.
func ParseToolMatcher(s string) (ToolMatcher, error) {
	var m ToolMatcher
	if strings.TrimSpace(s) == "" {
		return m, nil
	}
	for _, part := range strings.Split(s, ";") {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			return m, fmt.Errorf("agenthooks: bad filter segment %q", part)
		}
		items := strings.Split(val, ",")
		switch key {
		case "names":
			m.Names = append(m.Names, items...)
		case "canonical":
			for _, it := range items {
				m.Canonical = append(m.Canonical, CanonicalTool(it))
			}
		case "mcp":
			m.MCP = append(m.MCP, items...)
		default:
			return m, fmt.Errorf("agenthooks: unknown filter key %q", key)
		}
	}
	return m, nil
}

// canonicalNativeNames expands a canonical class to the native names a
// provider uses for it, for matcher-dialect compilation.
func canonicalNativeNames(p Provider, c CanonicalTool) []string {
	type key struct {
		p Provider
		c CanonicalTool
	}
	table := map[key][]string{
		{ProviderClaudeCode, ToolShell}:     {"Bash"},
		{ProviderClaudeCode, ToolFileRead}:  {"Read"},
		{ProviderClaudeCode, ToolFileWrite}: {"Write"},
		{ProviderClaudeCode, ToolFileEdit}:  {"Edit", "MultiEdit", "NotebookEdit"},
		{ProviderClaudeCode, ToolSearch}:    {"Grep", "Glob"},
		{ProviderClaudeCode, ToolFetch}:     {"WebFetch", "WebSearch"},
		{ProviderClaudeCode, ToolTask}:      {"Task"},

		{ProviderCodex, ToolShell}:    {"shell", "local_shell"},
		{ProviderCodex, ToolFileEdit}: {"apply_patch"},

		{ProviderGemini, ToolShell}:     {"run_shell_command"},
		{ProviderGemini, ToolFileRead}:  {"read_file", "read_many_files"},
		{ProviderGemini, ToolFileWrite}: {"write_file"},
		{ProviderGemini, ToolFileEdit}:  {"replace"},
		{ProviderGemini, ToolSearch}:    {"search_file_content", "glob", "list_directory"},
		{ProviderGemini, ToolFetch}:     {"web_fetch", "google_web_search"},

		// Kimi native names are documented sparsely; only the ones the hooks
		// docs themselves use are mapped.
		{ProviderKimi, ToolShell}:     {"Bash"},
		{ProviderKimi, ToolFileWrite}: {"WriteFile"},
		{ProviderKimi, ToolFileEdit}:  {"StrReplaceFile"},
	}
	return table[key{p, c}]
}

// CompileMatcher renders the matcher in the provider's dialect. ok is false
// when the dialect can't express it (e.g. Cursor has no matchers), in which
// case config generation matches broadly and the runner filters in-process.
func CompileMatcher(p Provider, m ToolMatcher) (expr string, ok bool) {
	if m.IsEmpty() {
		return "", true
	}
	switch p {
	case ProviderClaudeCode, ProviderCodex, ProviderGemini, ProviderKimi:
		// Kimi shares Claude's mcp__<server>__<tool> dialect with the
		// configured name verbatim (verified against kimi-code 0.22.2), so
		// its MCP globs compile natively.
		var alts []string
		alts = append(alts, m.Names...)
		for _, c := range m.Canonical {
			alts = append(alts, canonicalNativeNames(p, c)...)
		}
		for _, g := range m.MCP {
			server, tool, cut := strings.Cut(g, "/")
			if !cut {
				server, tool = g, "*"
			}
			sep := "__"
			prefix := "mcp__"
			if p == ProviderGemini {
				sep = "_"
				prefix = "mcp_"
			}
			sp, tp := server, tool
			if sp == "*" {
				sp = ".*"
			}
			if tp == "*" {
				tp = ".*"
			}
			alts = append(alts, prefix+sp+sep+tp)
		}
		if len(alts) == 0 {
			return "", false
		}
		sort.Strings(alts)
		return strings.Join(alts, "|"), true
	default:
		return "", false
	}
}
