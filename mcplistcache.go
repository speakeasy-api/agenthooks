package agenthooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	mcpListProbeTimeout    = 15 * time.Second
	mcpListWaitTimeout     = mcpListProbeTimeout + time.Second
	mcpListRefreshInterval = 5 * time.Minute
	mcpListCacheRetention  = 24 * time.Hour
)

type mcpListCache struct {
	CheckedAt int64            `json:"checked_at"`
	Entries   []mcpConfigEntry `json:"entries"`
}

func (r *Runner) claudeMCPWarmContext(cwd string) (claudeLaunchContext, bool) {
	if r.mcpResolveOff || r.mcpListOff {
		return claudeLaunchContext{}, false
	}
	launch := currentClaudeLaunchContext(cwd)
	if launch.SafeMode || launch.StrictMCP || (launch.Bare && len(launch.PluginDirs) == 0) {
		return claudeLaunchContext{}, false
	}
	return launch, true
}

func (r *Runner) shouldWarmClaudeMCP(cwd string) bool {
	_, ok := r.claudeMCPWarmContext(cwd)
	return ok
}

func (r *Runner) warmClaudeMCP(cwd string) {
	if launch, ok := r.claudeMCPWarmContext(cwd); ok {
		_ = r.claudeMCPListEntries(launch)
	}
}

func (r *Runner) claudeMCPListEntries(launch claudeLaunchContext) []mcpConfigEntry {
	return r.cachedMCPListEntries(launch.cacheKey(), func() ([]mcpConfigEntry, bool) {
		return runClaudeMCPList(launch)
	})
}

func (r *Runner) cachedMCPListEntries(key string, probe func() ([]mcpConfigEntry, bool)) []mcpConfigEntry {
	dir := r.mcpListCacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	path := filepath.Join(dir, key+".json")
	now := r.mcpListNow()
	cached := readMCPListCache(path)
	if mcpListCacheFresh(cached, now) {
		return cached.Entries
	}
	cleanupMCPListCache(dir, time.Now())

	// Only one process runs the expensive health check for a context. Waiters
	// consume its replacement snapshot instead of starting a probe stampede.
	var unlock func()
	deadline := time.Now().Add(mcpListWaitTimeout)
	backoff := 25 * time.Millisecond
	for {
		release, ok, lockErr := tryMCPListLock(path + ".lock")
		if lockErr != nil {
			if cached.CheckedAt != 0 {
				return cached.Entries
			}
			if entries, success := probe(); success {
				return entries
			}
			return nil
		}
		if ok {
			unlock = release
			break
		}
		if latest := readMCPListCache(path); mcpListCacheFresh(latest, r.mcpListNow()) {
			return latest.Entries
		}
		if !time.Now().Before(deadline) {
			if latest := readMCPListCache(path); latest.CheckedAt > cached.CheckedAt {
				return latest.Entries
			}
			return cached.Entries
		}
		time.Sleep(backoff)
		backoff = min(2*backoff, 250*time.Millisecond)
	}
	defer unlock()

	// The waiter may have observed staleness immediately before another
	// process refreshed and released the lock.
	cached = readMCPListCache(path)
	now = r.mcpListNow()
	if mcpListCacheFresh(cached, now) {
		return cached.Entries
	}
	if entries, success := probe(); success {
		cached.Entries = entries // successful probes replace, so removals stick
	}
	cached.CheckedAt = now.Unix()
	writeMCPListCache(path, cached)
	return cached.Entries
}

func (r *Runner) mcpListCacheDir() string {
	if r.dedupDir != "" {
		return filepath.Join(r.dedupDir, "agenthooks-mcplist")
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "agenthooks", "mcp-list")
	}
	return filepath.Join(os.TempDir(), "agenthooks-mcplist")
}

func (r *Runner) currentCodexLaunchContext(cwd string) (codexLaunchContext, bool) {
	if r.codexLaunchContext != nil {
		launch := *r.codexLaunchContext
		if cwd != "" {
			launch.CWD = cwd
		}
		return launch, true
	}
	return currentCodexLaunchContext(cwd)
}

func (r *Runner) codexMCPWarmContext(cwd string) (codexLaunchContext, bool) {
	if r.mcpResolveOff || r.mcpListOff {
		return codexLaunchContext{}, false
	}
	launch, ok := r.currentCodexLaunchContext(cwd)
	if !ok || launch.Unreplayable {
		return codexLaunchContext{}, false
	}
	return launch, true
}

func (r *Runner) warmCodexMCP(launch codexLaunchContext) {
	if !r.mcpResolveOff && !r.mcpListOff && !launch.Unreplayable {
		_ = r.codexMCPListEntries(launch)
	}
}

func (r *Runner) codexMCPListEntries(launch codexLaunchContext) []mcpConfigEntry {
	return r.cachedMCPListEntries(launch.cacheKey(), func() ([]mcpConfigEntry, bool) {
		return runCodexMCPList(launch)
	})
}

func cleanupMCPListCache(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if info, err := entry.Info(); err == nil {
			age := now.Sub(info.ModTime())
			if filepath.Ext(entry.Name()) == ".json" && age > mcpListCacheRetention {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
}

func (r *Runner) mcpListNow() time.Time { return r.now() }

func readMCPListCache(path string) mcpListCache {
	data, err := os.ReadFile(path)
	if err != nil {
		return mcpListCache{}
	}
	var cached mcpListCache
	if json.Unmarshal(data, &cached) != nil {
		return mcpListCache{}
	}
	return cached
}

func mcpListCacheFresh(cached mcpListCache, now time.Time) bool {
	return cached.CheckedAt != 0 && now.Sub(time.Unix(cached.CheckedAt, 0)) < mcpListRefreshInterval
}

func writeMCPListCache(path string, cached mcpListCache) {
	data, err := json.Marshal(cached)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "inventory-*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	if err == nil {
		err = os.Rename(tmpPath, path)
	}
	if err != nil {
		_ = os.Remove(tmpPath)
	}
}

func runClaudeMCPList(launch claudeLaunchContext) ([]mcpConfigEntry, bool) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpListProbeTimeout)
	defer cancel()
	args := append([]string(nil), launch.ReplayArgs...)
	if launch.Bare {
		args = append([]string{"--bare"}, args...)
	}
	args = append(args, "mcp", "list")
	cmd := exec.CommandContext(ctx, bin, args...)
	if launch.ProjectDir != "" {
		cmd.Dir = launch.ProjectDir
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return parseClaudeMCPList(string(out)), true
}

func runCodexMCPList(launch codexLaunchContext) ([]mcpConfigEntry, bool) {
	bin := launch.Executable
	if bin == "" || !filepath.IsAbs(bin) {
		var err error
		bin, err = exec.LookPath(bin)
		if err != nil {
			return nil, false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpListProbeTimeout)
	defer cancel()
	args := append(launch.replayArgs(), "mcp", "list", "--json")
	cmd := exec.CommandContext(ctx, bin, args...)
	if launch.CWD != "" {
		cmd.Dir = launch.CWD
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return decodeCodexMCPList(out)
}
