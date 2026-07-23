package agenthooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// MCP transport resolution (quirk #25). Only Cursor's beforeMCPExecution/
// afterMCPExecution events carry the target server's url/command in the hook
// payload; every other provider ships the tool name alone. To keep
// MCPCall.URL/Command part of the tool-call contract everywhere, the runner
// resolves them from the provider's own MCP config files, mirroring each
// provider's name-sanitization and prefix-matching rules (several are
// undocumented upstream and were inferred from observed tool names).
// Ambiguous matches resolve to nothing: misattributing a server is worse
// than admitting ignorance.
//
// Resolution runs before the in-process matcher filter, so the Server/Tool
// split repairs it performs (Codex names whose sanitized prefix contains
// "__", Gemini names whose server contains "_") also fix MCP glob matching.
//
// Codecs stay pure (payload bytes -> typed event); this file is the only
// place the library touches provider config on disk during dispatch.

// mcpConfigEntry is one MCP server flattened out of a provider config file or
// `claude mcp list` inventory line.
type mcpConfigEntry struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	Command string `json:"command,omitempty"`
	// Prefix is the pre-computed tool-name prefix, set when the source knows
	// better than sanitize(Name): `claude mcp list` entries carry source-
	// dependent prefixes (claude_ai_*, plugin_*_*). Empty means derive.
	Prefix string `json:"prefix,omitempty"`
}

// resolveMCP fills MCPCall.URL/Command (and refines Server/Tool where the
// match disambiguates the name split) for MCP tool calls whose payload
// carried no transport info. On OpenCode it additionally performs MCP
// detection itself: <server>_<tool> names carry no reserved marker, so only
// a config match can tell an MCP call apart from a native tool (quirk #28).
func (r *Runner) resolveMCP(typed any) {
	if r.mcpResolveOff {
		return
	}
	tc := toolOf(typed)
	if tc == nil {
		return
	}
	base := eventOf(typed)
	if base.Provider == ProviderOpenCode {
		r.resolveOpenCodeMCP(base, tc)
		return
	}
	if tc.MCP == nil || tc.MCP.URL != "" || tc.MCP.Command != "" {
		return
	}
	entries := loadMCPConfigEntries(base.Provider, base.Session.CWD)

	var (
		matched      *mcpConfigEntry
		server, tool string
	)
	switch base.Provider {
	case ProviderClaudeCode:
		// mcp__<prefix>__<tool>, prefix = claude-sanitized config name.
		matched, server, tool = matchSanitizedPrefix(entries, tc.Name, "mcp__", "__", claudeSanitizeMCPName)
		if matched == nil && !r.mcpListOff {
			// Slow path (quirk #26): plugin- and claude.ai-connector servers
			// appear in no config file; match against the shared `claude mcp
			// list` inventory. On a miss, re-probe (throttled) in case the
			// server was installed since the last refresh, then match again.
			matched, server, tool = matchSanitizedPrefix(r.mcpListEntries(), tc.Name, "mcp__", "__", claudeSanitizeMCPName)
			if matched == nil {
				if refreshed, ran := r.mcpListEntriesOnMiss(); ran {
					matched, server, tool = matchSanitizedPrefix(refreshed, tc.Name, "mcp__", "__", claudeSanitizeMCPName)
				}
			}
		}
	case ProviderKimi:
		// mcp__<server>__<tool> with the configured name verbatim — hyphens
		// survive unsanitized (verified against kimi-code 0.22.2).
		matched, server, tool = matchSanitizedPrefix(entries, tc.Name, "mcp__", "__", verbatimMCPName)
	case ProviderCodex:
		// Codex's sanitizer preserves consecutive underscores, so the prefix
		// itself can contain "__" and the naive first-"__" split is wrong;
		// longest-prefix matching also repairs Server/Tool.
		matched, server, tool = matchSanitizedPrefix(entries, tc.Name, "mcp__", "__", codexSanitizeMCPName)
	case ProviderGemini:
		// mcp_<server>_<tool> with a single-underscore separator: the split
		// is ambiguous whenever the server name contains "_" (quirk #15), so
		// match configured names longest-first.
		matched, server, tool = matchSanitizedPrefix(entries, tc.Name, "mcp_", "_", verbatimMCPName)
	case ProviderCursor:
		// MCP:<tool> carries no server identity (quirk #3). Attribution is
		// only sound when exactly one server is configured.
		if len(entries) == 1 {
			matched, server, tool = &entries[0], entries[0].Name, tc.MCP.Tool
		}
	}
	if matched == nil {
		return
	}
	tc.MCP.URL = matched.URL
	tc.MCP.Command = matched.Command
	tc.MCP.FromConfig = true
	if server != "" {
		tc.MCP.Server = server
	}
	if tool != "" {
		tc.MCP.Tool = tool
	}
}

// resolveOpenCodeMCP detects and resolves OpenCode MCP calls. OpenCode
// registers MCP tools as <server>_<tool> with the configured name verbatim
// (verified against opencode 1.17.8) and no reserved prefix, so the codec
// cannot classify them; a configured-name match both detects the call and
// attaches transport.
func (r *Runner) resolveOpenCodeMCP(base *Event, tc *ToolCall) {
	if tc.MCP != nil && (tc.MCP.URL != "" || tc.MCP.Command != "") {
		return
	}
	entries := loadMCPConfigEntries(ProviderOpenCode, base.Session.CWD)
	matched, server, tool := matchSanitizedPrefix(entries, tc.Name, "", "_", verbatimMCPName)
	if matched == nil {
		return
	}
	tc.MCP = &MCPCall{Server: server, Tool: tool, URL: matched.URL, Command: matched.Command, FromConfig: true}
	tc.Canonical = ToolMCP
}

// matchSanitizedPrefix finds the unique config entry whose sanitized name,
// followed by sep, prefixes the tool name's post-namePrefix remainder.
// Longest match wins; a tie is ambiguous and matches nothing. Returns the
// entry plus the repaired server prefix and tool name.
func matchSanitizedPrefix(entries []mcpConfigEntry, name, namePrefix, sep string, sanitize func(string) string) (*mcpConfigEntry, string, string) {
	rest, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return nil, "", ""
	}
	var (
		best      *mcpConfigEntry
		bestP     string
		ambiguous bool
	)
	for i := range entries {
		p := entries[i].Prefix
		if p == "" {
			p = sanitize(entries[i].Name)
		}
		if p == "" || !strings.HasPrefix(rest, p+sep) {
			continue
		}
		switch {
		case best == nil || len(p) > len(bestP):
			best, bestP, ambiguous = &entries[i], p, false
		case len(p) == len(bestP):
			ambiguous = true
		}
	}
	if best == nil || ambiguous {
		return nil, "", ""
	}
	return best, bestP, rest[len(bestP)+len(sep):]
}

// claudeSanitizeMCPName mirrors the tool-name prefix Claude Code derives from
// a configured server name (undocumented upstream; rules inferred from
// observed tool names): spaces become "_", parens are dropped, consecutive
// "_" collapse, leading/trailing "_" are trimmed.
func claudeSanitizeMCPName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch r {
		case ' ':
			b.WriteByte('_')
		case '(', ')':
			// drop
		default:
			b.WriteRune(r)
		}
	}
	s := b.String()
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	return strings.Trim(s, "_")
}

// codexSanitizeMCPName mirrors codex-rs sanitize_responses_api_tool_name:
// every character outside [A-Za-z0-9_] becomes "_". Codex appends a hash
// suffix when two sanitized names collide; such entries fail the ambiguity
// check here and stay unresolved.
func codexSanitizeMCPName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// verbatimMCPName covers providers that key MCP tools by the configured
// server name as-is (Gemini settings.json names; Kimi and OpenCode, verified
// empirically — hyphens survive). Only spaces are normalized, since no
// underscore-dialect tool name can carry one.
func verbatimMCPName(name string) string {
	return strings.ReplaceAll(name, " ", "_")
}

// loadMCPConfigEntries reads the provider's MCP server config files. Missing
// or malformed files contribute nothing. More specific scopes come first and
// win name collisions (project over user).
func loadMCPConfigEntries(p Provider, cwd string) []mcpConfigEntry {
	home, _ := os.UserHomeDir()
	var groups [][]mcpConfigEntry
	switch p {
	case ProviderClaudeCode:
		var local, user []mcpConfigEntry
		if home != "" {
			local, user = readClaudeUserConfig(filepath.Join(home, ".claude.json"), cwd)
		}
		groups = append(groups, local)
		if cwd != "" {
			groups = append(groups, readMCPServersJSON(filepath.Join(cwd, ".mcp.json")))
		}
		groups = append(groups, user)
	case ProviderCodex:
		dir := os.Getenv("CODEX_HOME")
		if dir == "" && home != "" {
			dir = filepath.Join(home, ".codex")
		}
		if dir != "" {
			groups = append(groups, readCodexConfigTOML(filepath.Join(dir, "config.toml")))
		}
	case ProviderCursor:
		if cwd != "" {
			groups = append(groups, readMCPServersJSON(filepath.Join(cwd, ".cursor", "mcp.json")))
		}
		if home != "" {
			groups = append(groups, readMCPServersJSON(filepath.Join(home, ".cursor", "mcp.json")))
		}
	case ProviderGemini:
		if cwd != "" {
			groups = append(groups,
				readMCPServersJSON(filepath.Join(cwd, ".gemini", "settings.json")),
				readGeminiExtensions(filepath.Join(cwd, ".gemini", "extensions")))
		}
		if home != "" {
			groups = append(groups,
				readMCPServersJSON(filepath.Join(home, ".gemini", "settings.json")),
				readGeminiExtensions(filepath.Join(home, ".gemini", "extensions")))
		}
	case ProviderKimi:
		// Kimi Code keeps mcp.json under .kimi-code (project and user scope,
		// user overridable via KIMI_CODE_HOME); ~/.kimi/mcp.json is the
		// legacy kimi-cli location, kept as a fallback.
		if cwd != "" {
			groups = append(groups, readMCPServersJSON(filepath.Join(cwd, ".kimi-code", "mcp.json")))
		}
		kimiHome := os.Getenv("KIMI_CODE_HOME")
		if kimiHome == "" && home != "" {
			kimiHome = filepath.Join(home, ".kimi-code")
		}
		if kimiHome != "" {
			groups = append(groups, readMCPServersJSON(filepath.Join(kimiHome, "mcp.json")))
		}
		if home != "" {
			groups = append(groups, readMCPServersJSON(filepath.Join(home, ".kimi", "mcp.json")))
		}
	case ProviderOpenCode:
		// opencode.json(c) at project root; global config under
		// $XDG_CONFIG_HOME/opencode (default ~/.config/opencode). The .jsonc
		// variants tolerate comments.
		if cwd != "" {
			groups = append(groups,
				readOpenCodeConfig(filepath.Join(cwd, "opencode.json")),
				readOpenCodeConfig(filepath.Join(cwd, "opencode.jsonc")))
		}
		cfgDir := os.Getenv("XDG_CONFIG_HOME")
		if cfgDir == "" && home != "" {
			cfgDir = filepath.Join(home, ".config")
		}
		if cfgDir != "" {
			groups = append(groups,
				readOpenCodeConfig(filepath.Join(cfgDir, "opencode", "opencode.json")),
				readOpenCodeConfig(filepath.Join(cfgDir, "opencode", "opencode.jsonc")))
		}
	}
	seen := map[string]bool{}
	var out []mcpConfigEntry
	for _, g := range groups {
		for _, e := range g {
			if e.Name == "" || seen[e.Name] {
				continue
			}
			seen[e.Name] = true
			out = append(out, e)
		}
	}
	return out
}

// mcpServerJSON is the per-server config shape shared by Claude's .mcp.json,
// Cursor's mcp.json, Gemini's settings.json and Kimi's mcp.json.
type mcpServerJSON struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	URL     string   `json:"url"`
	HTTPURL string   `json:"httpUrl"` // gemini's streamable-HTTP field
}

// readMCPServersJSON reads any config file whose MCP block is a top-level
// {"mcpServers": {name: {...}}} object.
func readMCPServersJSON(path string) []mcpConfigEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		MCPServers map[string]mcpServerJSON `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	return mcpEntriesFromJSON(doc.MCPServers)
}

// readClaudeUserConfig extracts the cwd's project-scoped ("local" scope)
// block and the top-level user-scoped mcpServers from ~/.claude.json.
func readClaudeUserConfig(path, cwd string) (local, user []mcpConfigEntry) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var doc struct {
		MCPServers map[string]mcpServerJSON `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]mcpServerJSON `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil
	}
	if cwd != "" {
		if proj, ok := doc.Projects[cwd]; ok {
			local = mcpEntriesFromJSON(proj.MCPServers)
		}
	}
	return local, mcpEntriesFromJSON(doc.MCPServers)
}

func mcpEntriesFromJSON(servers map[string]mcpServerJSON) []mcpConfigEntry {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]mcpConfigEntry, 0, len(names))
	for _, n := range names {
		s := servers[n]
		e := mcpConfigEntry{Name: n, URL: s.URL, Command: joinCommand(s.Command, s.Args)}
		if e.URL == "" {
			e.URL = s.HTTPURL
		}
		if e.URL == "" && e.Command == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func joinCommand(cmd string, args []string) string {
	if cmd == "" {
		return ""
	}
	return strings.Join(append([]string{cmd}, args...), " ")
}

// opencodeMCPJSON is one server in opencode.json's "mcp" block: type
// "local" (command array) or "remote" (url).
type opencodeMCPJSON struct {
	Command []string `json:"command"`
	URL     string   `json:"url"`
	Enabled *bool    `json:"enabled"`
}

// readOpenCodeConfig reads the "mcp" block of an opencode.json/.jsonc file.
func readOpenCodeConfig(path string) []mcpConfigEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		MCP map[string]opencodeMCPJSON `json:"mcp"`
	}
	if err := json.Unmarshal(stripJSONCComments(data), &doc); err != nil {
		return nil
	}
	names := make([]string, 0, len(doc.MCP))
	for n := range doc.MCP {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]mcpConfigEntry, 0, len(names))
	for _, n := range names {
		s := doc.MCP[n]
		if s.Enabled != nil && !*s.Enabled {
			continue
		}
		e := mcpConfigEntry{Name: n, URL: s.URL, Command: strings.Join(s.Command, " ")}
		if e.URL == "" && e.Command == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// stripJSONCComments blanks // and /* */ comments (string-aware) so the
// stdlib JSON decoder accepts .jsonc config files. Byte positions are
// preserved by replacing comment bytes with spaces.
func stripJSONCComments(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	inStr, inLine, inBlock := false, false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			} else {
				out[i] = ' '
			}
		case inBlock:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				inBlock = false
			} else if c != '\n' {
				out[i] = ' '
			}
		case inStr:
			switch c {
			case '\\':
				i++
			case '"':
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			out[i], out[i+1] = ' ', ' '
			i++
			inLine = true
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			i++
			inBlock = true
		}
	}
	return out
}

// readGeminiExtensions collects the MCP servers bundled by Gemini extensions
// (quirk #27): each extension ships a gemini-extension.json manifest whose
// optional mcpServers block never appears in settings.json — the Gemini
// analogue of Claude's plugin servers.
func readGeminiExtensions(dir string) []mcpConfigEntry {
	manifests, _ := filepath.Glob(filepath.Join(dir, "*", "gemini-extension.json"))
	var out []mcpConfigEntry
	for _, m := range manifests { // Glob returns sorted paths
		out = append(out, readMCPServersJSON(m)...)
	}
	return out
}

// readCodexConfigTOML extracts [mcp_servers.<name>] tables from Codex's
// config.toml.
func readCodexConfigTOML(path string) []mcpConfigEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseCodexMCPServers(data)
}

// codexMCPServerTOML is one [mcp_servers.<name>] table. Unknown keys (env,
// startup_timeout_ms, ...) are ignored by the decoder.
type codexMCPServerTOML struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	URL     string   `toml:"url"`
	Enabled *bool    `toml:"enabled"`
}

func parseCodexMCPServers(data []byte) []mcpConfigEntry {
	var doc struct {
		MCPServers map[string]codexMCPServerTOML `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	names := make([]string, 0, len(doc.MCPServers))
	for n := range doc.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]mcpConfigEntry, 0, len(names))
	for _, name := range names {
		s := doc.MCPServers[name]
		if s.Enabled != nil && !*s.Enabled {
			continue
		}
		e := mcpConfigEntry{Name: name, URL: s.URL, Command: joinCommand(s.Command, s.Args)}
		if e.URL == "" && e.Command == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// --- Claude slow path: `claude mcp list`, once per session (quirk #26) ---

// Plugin-provided and claude.ai-connector MCP servers appear in no config
// file on disk; only `claude mcp list` knows their transport. The CLI
// health-checks every server (seconds of wall time), so the runner shells
// out at most once per session — on the first MCP call the config fast path
// can't attribute — and caches the parsed inventory on disk keyed by session
// id. Failed and empty runs are cached too: one attempt per session, ever.

// claudeMCPListTimeout caps the health-checking CLI call, leaving most of
// the ~60s provider hook budget for the handler (the runner's own deadline
// is 55s, see defaultDeadline).
const claudeMCPListTimeout = 15 * time.Second

func runClaudeMCPList() []mcpConfigEntry {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeMCPListTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "mcp", "list").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	return parseClaudeMCPList(string(out))
}

// parseClaudeMCPList parses the textual output of `claude mcp list`. Lines
// that don't match the expected shape are skipped (the "Checking MCP server
// health..." preamble, blank lines). Expected shape:
//
//	<name>: <target>[ (<TRANSPORT>)] - <status>
//
// where <name> may itself contain colons ("plugin:slack:slack"), so parsing
// splits from the right: status first, then the optional transport, then the
// last ": " separates name from target.
func parseClaudeMCPList(out string) []mcpConfigEntry {
	var entries []mcpConfigEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if e, ok := parseClaudeMCPListLine(line); ok {
			entries = append(entries, e)
		}
	}
	return entries
}

func parseClaudeMCPListLine(line string) (mcpConfigEntry, bool) {
	sepIdx := strings.LastIndex(line, " - ")
	if sepIdx < 0 {
		return mcpConfigEntry{}, false
	}
	head := strings.TrimSpace(line[:sepIdx])
	if strings.HasSuffix(head, ")") {
		if open := strings.LastIndex(head, " ("); open > 0 && isUpperAlpha(head[open+2:len(head)-1]) {
			head = strings.TrimSpace(head[:open])
		}
	}
	colonIdx := strings.LastIndex(head, ": ")
	if colonIdx < 0 {
		return mcpConfigEntry{}, false
	}
	name := strings.TrimSpace(head[:colonIdx])
	target := strings.TrimSpace(head[colonIdx+2:])
	if name == "" || target == "" {
		return mcpConfigEntry{}, false
	}

	source, plugin, display := classifyClaudeMCPName(name)
	e := mcpConfigEntry{Name: display, Prefix: claudeMCPServerPrefix(source, plugin, display)}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		e.URL = target
	} else {
		e.Command = target
	}
	return e, true
}

// classifyClaudeMCPName splits a `claude mcp list` display name into its
// source ("claude.ai", "plugin", "local"), plugin name, and server name.
func classifyClaudeMCPName(raw string) (source, plugin, name string) {
	if after, ok := strings.CutPrefix(raw, "claude.ai "); ok {
		return "claude.ai", "", after
	}
	if after, ok := strings.CutPrefix(raw, "plugin:"); ok {
		if i := strings.Index(after, ":"); i > 0 {
			return "plugin", after[:i], after[i+1:]
		}
		return "plugin", "", after
	}
	return "local", "", raw
}

// claudeMCPServerPrefix derives the mcp__<prefix>__ identity Claude Code
// uses for an inventory entry. The rules are undocumented upstream and were
// inferred from observed tool names (e.g. "mcp__claude_ai_Linear_Acme__..."
// for the list entry "claude.ai Linear (Acme)").
func claudeMCPServerPrefix(source, plugin, name string) string {
	switch source {
	case "claude.ai":
		return "claude_ai_" + claudeSanitizeMCPName(name)
	case "plugin":
		return "plugin_" + claudeSanitizeMCPName(plugin) + "_" + claudeSanitizeMCPName(name)
	default:
		return claudeSanitizeMCPName(name)
	}
}

func isUpperAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
