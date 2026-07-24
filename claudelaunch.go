package agenthooks

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// claudeLaunchContext is the subset of Claude's argv that changes its MCP
// inventory. Hooks receive CLAUDE_PID but not the launch arguments themselves.
type claudeLaunchContext struct {
	ProjectDir string
	MCPConfigs []string
	ReplayArgs []string
	PluginDirs []string
	StrictMCP  bool
	Bare       bool
	SafeMode   bool
}

func currentClaudeLaunchContext(cwd string) claudeLaunchContext {
	projectDir := os.Getenv("CLAUDE_PROJECT_DIR")
	if projectDir == "" {
		projectDir = cwd
	}
	c := claudeLaunchContext{ProjectDir: projectDir}
	pid, err := strconv.Atoi(os.Getenv("CLAUDE_PID"))
	if err != nil || pid <= 1 {
		return c
	}
	args, err := procArgs(pid)
	if err != nil || len(args) == 0 {
		return c
	}
	c = parseClaudeLaunchArgs(args, projectDir)
	return c
}

func parseClaudeLaunchArgs(args []string, projectDir string) claudeLaunchContext {
	c := claudeLaunchContext{ProjectDir: projectDir}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		switch a {
		case "--strict-mcp-config":
			c.StrictMCP = true
		case "--bare":
			c.Bare = true
		case "--safe-mode":
			c.SafeMode = true
		case "--mcp-config":
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				c.MCPConfigs = append(c.MCPConfigs, args[i])
			}
		case "--settings", "--setting-sources", "--plugin-dir":
			if i+1 >= len(args) {
				continue
			}
			i++
			c.addReplayArg(a, args[i])
		case "--plugin-url":
			// Re-fetching a mutable remote plugin from a hook can execute code
			// that the running session never loaded. Leave it unattributed.
			i++
		default:
			name, value, ok := strings.Cut(a, "=")
			if !ok {
				continue
			}
			switch name {
			case "--mcp-config":
				c.MCPConfigs = append(c.MCPConfigs, value)
			case "--settings", "--setting-sources", "--plugin-dir":
				c.addReplayArg(name, value)
			}
		}
	}
	return c
}

func (c *claudeLaunchContext) addReplayArg(name, value string) {
	c.ReplayArgs = append(c.ReplayArgs, name, value)
	if name == "--plugin-dir" {
		c.PluginDirs = append(c.PluginDirs, value)
	}
}

func (c claudeLaunchContext) explicitMCPEntries() []mcpConfigEntry {
	// Later command-line configs win name collisions.
	var groups [][]mcpConfigEntry
	for i := len(c.MCPConfigs) - 1; i >= 0; i-- {
		if data, ok := c.readValue(c.MCPConfigs[i]); ok {
			groups = append(groups, parseClaudeLaunchMCPConfig(data))
		}
	}
	return firstMCPEntries(groups...)
}

func (c claudeLaunchContext) readValue(value string) ([]byte, bool) {
	if strings.HasPrefix(strings.TrimSpace(value), "{") {
		return []byte(value), true
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.ProjectDir, path)
	}
	data, err := os.ReadFile(path)
	return data, err == nil
}

// barePluginEntries keeps only servers belonging to explicitly supplied local
// plugins. Claude's management command currently ignores --bare for its normal
// inventory, so the result must be filtered back to the launch-only plugins.
func (c claudeLaunchContext) barePluginEntries(entries []mcpConfigEntry) []mcpConfigEntry {
	var prefixes []string
	for _, dir := range c.PluginDirs {
		if name := c.pluginName(dir); name != "" {
			prefixes = append(prefixes, claudeMCPServerPrefix("plugin", name, ""))
		}
	}
	var out []mcpConfigEntry
	for _, e := range entries {
		for _, prefix := range prefixes {
			if strings.HasPrefix(e.Prefix, prefix) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

func (c claudeLaunchContext) pluginName(value string) string {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.ProjectDir, path)
	}
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			return ""
		}
		defer func() { _ = zr.Close() }()
		for _, f := range zr.File {
			if strings.HasSuffix(filepath.ToSlash(f.Name), "/.claude-plugin/plugin.json") || f.Name == ".claude-plugin/plugin.json" {
				r, err := f.Open()
				if err != nil {
					return ""
				}
				data, _ := io.ReadAll(r)
				_ = r.Close()
				return pluginNameJSON(data)
			}
		}
		return ""
	}
	data, err := os.ReadFile(filepath.Join(path, ".claude-plugin", "plugin.json"))
	if err != nil {
		return filepath.Base(path)
	}
	return pluginNameJSON(data)
}

func pluginNameJSON(data []byte) string {
	var manifest struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	return manifest.Name
}

// cacheKey fingerprints launch inputs without persisting argv, inline configs,
// settings, or environment values that may contain credentials.
func (c claudeLaunchContext) cacheKey() string {
	h := sha256.New()
	writeHashPart := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	writeHashPart("agenthooks-claude-mcp-v2")
	writeHashPart(c.ProjectDir)
	writeHashPart(strconv.FormatBool(c.StrictMCP))
	writeHashPart(strconv.FormatBool(c.Bare))
	writeHashPart(strconv.FormatBool(c.SafeMode))
	for i := 0; i < len(c.ReplayArgs); i++ {
		writeHashPart(c.ReplayArgs[i])
		if c.ReplayArgs[i] == "--settings" && i+1 < len(c.ReplayArgs) {
			value := c.ReplayArgs[i+1]
			if data, ok := c.readValue(value); ok {
				sum := sha256.Sum256(data)
				writeHashPart(hex.EncodeToString(sum[:]))
			}
		}
		if c.ReplayArgs[i] == "--plugin-dir" && i+1 < len(c.ReplayArgs) {
			c.hashPluginDir(c.ReplayArgs[i+1], writeHashPart)
		}
	}
	env := append([]string(nil), os.Environ()...)
	sort.Strings(env)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "CLAUDE_PID", "CLAUDE_CODE_SESSION_ID", "CLAUDE_ENV_FILE", "PWD", "OLDPWD", "SHLVL", "_":
			continue
		}
		writeHashPart(kv)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (c claudeLaunchContext) hashPluginDir(value string, writeHashPart func(string)) {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.ProjectDir, path)
	}
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		if info, err := os.Stat(path); err == nil {
			writeHashPart(strconv.FormatInt(info.Size(), 10))
			writeHashPart(strconv.FormatInt(info.ModTime().UnixNano(), 10))
		}
		return
	}
	for _, name := range []string{
		filepath.Join(".claude-plugin", "plugin.json"),
		".mcp.json",
		"plugin.json",
	} {
		if data, err := os.ReadFile(filepath.Join(path, name)); err == nil {
			sum := sha256.Sum256(data)
			writeHashPart(name)
			writeHashPart(hex.EncodeToString(sum[:]))
		}
	}
}

func parseClaudeLaunchMCPConfig(data []byte) []mcpConfigEntry {
	var doc struct {
		MCPServers map[string]mcpServerJSON `json:"mcpServers"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	names := make([]string, 0, len(doc.MCPServers))
	for name := range doc.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []mcpConfigEntry
	for _, name := range names {
		server := doc.MCPServers[name]
		typ := strings.ToLower(server.Type)
		if server.URL != "" && typ != "http" && typ != "streamable-http" && typ != "sse" && typ != "ws" {
			continue
		}
		if server.Command != "" && typ != "" && typ != "stdio" {
			continue
		}
		args := make([]string, len(server.Args))
		for i, arg := range server.Args {
			args[i] = expandClaudeMCPEnv(arg)
		}
		entry := mcpConfigEntry{
			Name:    name,
			URL:     expandClaudeMCPEnv(server.URL),
			Command: joinCommand(expandClaudeMCPEnv(server.Command), args),
		}
		if entry.URL != "" || entry.Command != "" {
			out = append(out, entry)
		}
	}
	return out
}

func expandClaudeMCPEnv(value string) string {
	var out strings.Builder
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			out.WriteString(value)
			return out.String()
		}
		end := strings.IndexByte(value[start+2:], '}')
		if end < 0 {
			out.WriteString(value)
			return out.String()
		}
		end += start + 2
		out.WriteString(value[:start])
		expression := value[start+2 : end]
		name, fallback, hasFallback := strings.Cut(expression, ":-")
		replacement, found := os.LookupEnv(name)
		if hasFallback && (!found || replacement == "") {
			replacement = fallback
			found = true
		}
		if found {
			out.WriteString(replacement)
		} else {
			out.WriteString(value[start : end+1])
		}
		value = value[end+1:]
	}
}
