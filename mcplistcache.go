package agenthooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The `claude mcp list` inventory is cached globally on disk (not per session)
// so concurrent Claude sessions share one warm copy: whichever session probed
// most recently keeps every other session's first MCP tool call instant. Writes
// merge additively by server name so a session that momentarily observes a
// smaller list (a server mid-restart, a health-check blip) cannot clobber
// another session's entry; a removed server simply lingers until overwritten.
//
// The old cache was keyed by session id, so every session re-ran the slow CLI
// once. The shared cache plus an out-of-band warmer (see RefreshClaudeMCPList /
// WarmClaudeMCPList) lets a session-start worker populate it before the first
// tool call, removing the probe from the interactive path entirely.

// mcpListRefreshInterval throttles CLI re-runs triggered by a resolve miss: a
// tool call for a server absent from the cache re-probes at most this often, so
// a genuinely-unknown tool name cannot stall every event on the CLI timeout.
const mcpListRefreshInterval = 60 * time.Second

// mcpListLockStale bounds how long the merge lock is honored before a crashed
// writer's lock is reclaimed. The critical section only reads, merges, and
// rewrites the small cache file (the slow CLI call happens outside it), so a
// live holder never needs anywhere near this long.
const mcpListLockStale = 10 * time.Second

// MCPListEntry is one server from the shared inventory, exported for callers
// that warm the cache out of band (e.g. a detached session-start worker) and
// want the parsed result back to relay elsewhere.
type MCPListEntry struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	Command string `json:"command,omitempty"`
}

// mcpListCache is the on-disk shared inventory: entries keyed by server name,
// plus the time of the last successful merge used to throttle re-probes.
type mcpListCache struct {
	RefreshedAt int64                     `json:"refreshed_at"`
	Entries     map[string]mcpConfigEntry `json:"entries"`
}

func (c mcpListCache) slice() []mcpConfigEntry {
	out := make([]mcpConfigEntry, 0, len(c.Entries))
	for _, e := range c.Entries {
		out = append(out, e)
	}
	// Deterministic order keeps longest-prefix matching stable regardless of
	// map iteration order.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Runner) mcpListDir() string  { return filepath.Join(r.stateDir(), "agenthooks-mcplist") }
func (r *Runner) mcpListPath() string { return filepath.Join(r.mcpListDir(), "inventory.json") }

func readMCPListCache(path string) mcpListCache {
	c := mcpListCache{RefreshedAt: 0, Entries: map[string]mcpConfigEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	if json.Unmarshal(data, &c) != nil || c.Entries == nil {
		c.Entries = map[string]mcpConfigEntry{}
	}
	return c
}

// mcpListEntries returns the shared inventory, probing the CLI once to seed an
// empty cache. A populated cache is returned as-is; a present-but-not-matching
// cache is handled by mcpListEntriesOnMiss so ordinary events never re-probe.
func (r *Runner) mcpListEntries() []mcpConfigEntry {
	c := readMCPListCache(r.mcpListPath())
	if c.RefreshedAt == 0 {
		c = r.refreshMCPList()
	}
	return c.slice()
}

// mcpListEntriesOnMiss re-probes when a resolve miss might be explained by a
// server installed since the last refresh, throttled to mcpListRefreshInterval
// so an unresolvable name cannot stall every event on the CLI timeout. The bool
// reports whether a refresh actually ran.
func (r *Runner) mcpListEntriesOnMiss() ([]mcpConfigEntry, bool) {
	c := readMCPListCache(r.mcpListPath())
	if c.RefreshedAt != 0 && time.Since(time.Unix(c.RefreshedAt, 0)) < mcpListRefreshInterval {
		return nil, false
	}
	return r.refreshMCPList().slice(), true
}

// refreshMCPList runs `claude mcp list` and merges the result into the shared
// cache. The CLI runs outside the merge lock so concurrent sessions do not
// serialize on the slow health check.
func (r *Runner) refreshMCPList() mcpListCache {
	if err := os.MkdirAll(r.mcpListDir(), 0o700); err != nil {
		return readMCPListCache(r.mcpListPath())
	}
	return r.mergeMCPList(runClaudeMCPList())
}

// mergeMCPList unions fresh entries into the on-disk cache under a best-effort
// lock. Additive by name: fresh entries add or update; entries absent from
// fresh are kept. RefreshedAt is stamped even for an empty result so a machine
// with no servers (or no `claude` CLI) is negative-cached against re-probing.
func (r *Runner) mergeMCPList(fresh []mcpConfigEntry) mcpListCache {
	path := r.mcpListPath()
	unlock := r.lockMCPList()
	defer unlock()
	c := readMCPListCache(path)
	for _, e := range fresh {
		if e.Name == "" {
			continue
		}
		c.Entries[e.Name] = e
	}
	c.RefreshedAt = time.Now().Unix()
	if data, err := json.Marshal(c); err == nil {
		if tmp, err := os.CreateTemp(r.mcpListDir(), "inventory-*"); err == nil {
			_, werr := tmp.Write(data)
			if cerr := tmp.Close(); werr == nil && cerr == nil {
				_ = os.Rename(tmp.Name(), path)
			} else {
				_ = os.Remove(tmp.Name())
			}
		}
	}
	return c
}

// lockMCPList serializes the read-merge-write critical section across processes
// with an O_EXCL marker, reclaiming a stale lock from a crashed writer. It is
// best-effort: if the lock cannot be acquired it proceeds anyway, since the
// additive union already bounds the damage of a lost update to one missed
// refresh that the next miss re-probes.
func (r *Runner) lockMCPList() func() {
	lockPath := r.mcpListPath() + ".lock"
	for i := 0; i < 40; i++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > mcpListLockStale {
			_ = os.Remove(lockPath)
			continue
		}
		time.Sleep(25 * time.Millisecond)
	}
	return func() {}
}

// RefreshClaudeMCPList runs `claude mcp list` and merges the result into the
// shared inventory cache that resolveMCP consults for MCP URL attribution. It
// is safe to call from a detached session-start worker to warm the cache before
// the first MCP tool call, and returns the merged inventory. The CLI inherits
// the calling process's working directory, so a caller wanting project-scoped
// servers should chdir first.
func (r *Runner) RefreshClaudeMCPList() []MCPListEntry {
	return exportMCPEntries(r.refreshMCPList().slice())
}

// WarmClaudeMCPList refreshes the default shared inventory cache (the one an
// unconfigured Runner reads). It is the package-level entry point for a
// detached warmer that does not hold the in-process Runner.
func WarmClaudeMCPList() []MCPListEntry {
	return (&Runner{}).RefreshClaudeMCPList()
}

func exportMCPEntries(in []mcpConfigEntry) []MCPListEntry {
	out := make([]MCPListEntry, 0, len(in))
	for _, e := range in {
		out = append(out, MCPListEntry{Name: e.Name, URL: e.URL, Command: e.Command})
	}
	return out
}
