package agenthooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// The provider CLI inventory (`claude mcp list`, `codex mcp list --json`) is
// expensive: Claude health-checks every server and can take well over 15
// seconds. Hooks therefore never run it. They serve the on-device snapshot
// as it is and, when it is due, start one detached refresh worker that
// replaces the snapshot for the next hook.
//
// Main starts the worker with startDetachedSelf and releases it without
// waiting, so it can outlive the hook and the agent:
//   - macOS and Linux: the worker runs in its own session (setsid), so a
//     terminal hangup or the agent's exit does not signal it. Once the hook
//     that spawned it exits it is reparented to launchd/init (or the nearest
//     subreaper), which reaps it. The probe runs in its own process group
//     and is killed as a group on timeout (mcpprobe_unix.go).
//   - Windows: the worker is a DETACHED_PROCESS in a new process group, so
//     console close and Ctrl events do not reach it. An agent that runs hooks
//     in a kill-on-close job without breakaway ends it with the agent; the
//     lock dies with it and the next hook starts a new refresh. The probe
//     runs without a window and taskkill /T ends its tree on timeout
//     (mcpprobe_windows.go).
//
// Either way a worker lives at most mcpListProbeTimeout plus
// mcpListProbeWaitDelay, holds only the per-context lock (released by the OS
// if the worker dies), and writes its snapshot even after the agent exits,
// which serves the next session with the same launch context.
const (
	// mcpListRefreshInterval is the fresh window. A snapshot from a probe
	// that finished inside it is reported as complete; an older one is still
	// served, as a partial inventory. It is also the retry backoff after a
	// failed or timed-out probe.
	mcpListRefreshInterval = 5 * time.Minute
	// mcpListCacheRetention bounds how old a snapshot may be and still be
	// served. Files untouched for longer are deleted.
	mcpListCacheRetention = 14 * 24 * time.Hour
	// mcpListProbeTimeout bounds a background probe. Only the detached
	// refresh worker runs probes, so this never delays an agent.
	mcpListProbeTimeout = 2 * time.Minute
	// mcpListProbeWaitDelay bounds how long a finished or killed probe may
	// hold its output pipes open through descendants that inherited them.
	mcpListProbeWaitDelay = 5 * time.Second
)

type mcpListCache struct {
	// CheckedAt is when the last probe attempt ended, successful or not.
	CheckedAt int64 `json:"checked_at"`
	// SnapshotAt is when the probe that produced Entries finished. Caches
	// written before this field existed fall back to CheckedAt.
	SnapshotAt  int64            `json:"snapshot_at,omitempty"`
	Entries     []mcpConfigEntry `json:"entries"`
	HasSnapshot bool             `json:"has_snapshot,omitempty"`
}

func (c mcpListCache) snapshotTime() time.Time {
	if !c.HasSnapshot {
		return time.Time{}
	}
	if c.SnapshotAt != 0 {
		return time.Unix(c.SnapshotAt, 0)
	}
	return time.Unix(c.CheckedAt, 0)
}

// mcpListView is what a hook may use from a cached snapshot. Entries is the
// last successful inventory if it is within retention. Complete reports that
// the inventory came from a probe that finished inside the fresh window.
// RefreshDue reports that no probe attempt ended inside the fresh window.
type mcpListView struct {
	Entries    []mcpConfigEntry
	Complete   bool
	RefreshDue bool
}

func viewMCPListCache(cached mcpListCache, now time.Time) mcpListView {
	view := mcpListView{
		RefreshDue: cached.CheckedAt == 0 || now.Sub(time.Unix(cached.CheckedAt, 0)) >= mcpListRefreshInterval,
	}
	if !cached.HasSnapshot {
		return view
	}
	age := now.Sub(cached.snapshotTime())
	if age > mcpListCacheRetention {
		return view
	}
	view.Entries = cached.Entries
	view.Complete = age < mcpListRefreshInterval
	return view
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

// refreshClaudeMCPListIfDue starts a background `claude mcp list` for the
// session's launch context when its snapshot is due. It never waits.
func (r *Runner) refreshClaudeMCPListIfDue(cwd string) {
	if launch, ok := r.claudeMCPWarmContext(cwd); ok {
		_ = r.claudeMCPList(launch, cwd)
	}
}

// warmClaudeMCP is the detached refresh worker's entry point.
func (r *Runner) warmClaudeMCP(cwd string) {
	launch, ok := r.claudeMCPWarmContext(cwd)
	if !ok {
		return
	}
	r.refreshMCPList(context.Background(), launch.cacheKey(), func(ctx context.Context) ([]mcpConfigEntry, bool) {
		return runClaudeMCPList(ctx, launch)
	})
}

// claudeMCPList serves the cached `claude mcp list` snapshot for launch and
// starts a background refresh when it is due. It never runs the probe.
func (r *Runner) claudeMCPList(launch claudeLaunchContext, cwd string) mcpListView {
	var start func()
	if r.mcpWarmStart != nil {
		start = func() { r.mcpWarmStart(cwd) }
	}
	return r.cachedMCPList(launch.cacheKey(), start)
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

// warmCodexMCP is the detached refresh worker's entry point.
func (r *Runner) warmCodexMCP(launch codexLaunchContext) {
	if r.mcpResolveOff || r.mcpListOff || launch.Unreplayable {
		return
	}
	r.refreshMCPList(context.Background(), launch.cacheKey(), func(ctx context.Context) ([]mcpConfigEntry, bool) {
		return runCodexMCPList(ctx, launch)
	})
}

// codexMCPList serves the cached `codex mcp list --json` snapshot for launch
// and starts a background refresh when it is due. It never runs the probe.
func (r *Runner) codexMCPList(launch codexLaunchContext) mcpListView {
	var start func()
	if r.codexMCPWarmStart != nil {
		start = func() { r.codexMCPWarmStart(launch) }
	}
	return r.cachedMCPList(launch.cacheKey(), start)
}

// cachedMCPList reads the snapshot for key and, when a refresh is due and no
// worker holds the context's lock, calls start once. start comes from
// Runner.mcpWarmStart or codexMCPWarmStart, which Main sets to spawn a
// detached worker and return at once; without Main they are nil and hooks
// serve the cache without refreshing it. The snapshot is read again after
// start so a synchronous start is visible to the caller.
func (r *Runner) cachedMCPList(key string, start func()) mcpListView {
	path := filepath.Join(r.mcpListCacheDir(), key+".json")
	view := viewMCPListCache(readMCPListCache(path), r.mcpListNow())
	if !view.RefreshDue || start == nil || !r.claimMCPListRefresh(key, path) {
		return view
	}
	start()
	return viewMCPListCache(readMCPListCache(path), r.mcpListNow())
}

// claimMCPListRefresh reports whether this process should start a refresh
// worker for key: not already started by this process inside the fresh
// window, and no worker currently holding the context's lock. The lock is
// only probed here; the worker takes it for itself.
func (r *Runner) claimMCPListRefresh(key, path string) bool {
	now := r.mcpListNow()
	r.mcpRefreshMu.Lock()
	defer r.mcpRefreshMu.Unlock()
	if started, ok := r.mcpRefreshStarted[key]; ok && now.Sub(started) < mcpListRefreshInterval {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false
	}
	release, ok, err := tryMCPListLock(path + ".lock")
	if err != nil || !ok {
		return false
	}
	release()
	if r.mcpRefreshStarted == nil {
		r.mcpRefreshStarted = map[string]time.Time{}
	}
	r.mcpRefreshStarted[key] = now
	return true
}

// refreshMCPList runs one probe for key under the context's lock and replaces
// the snapshot. A worker that finds the lock held exits at once: another
// worker already owns the refresh. A failed or timed-out probe records the
// attempt, which keeps the last snapshot and backs off for the fresh window.
func (r *Runner) refreshMCPList(parent context.Context, key string, probe func(context.Context) ([]mcpConfigEntry, bool)) {
	dir := r.mcpListCacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	cleanupMCPListCache(dir, time.Now(), mcpListCacheRetention)
	path := filepath.Join(dir, key+".json")
	unlock, ok, err := tryMCPListLock(path + ".lock")
	if err != nil || !ok {
		return
	}
	defer unlock()

	// Another worker may have refreshed between the caller's read and this
	// lock.
	cached := readMCPListCache(path)
	if !viewMCPListCache(cached, r.mcpListNow()).RefreshDue {
		return
	}
	ctx, cancel := context.WithTimeout(parent, mcpListProbeTimeout)
	defer cancel()
	entries, success := probe(ctx)
	if parent.Err() != nil {
		return
	}
	finished := r.mcpListNow().Unix()
	cached.CheckedAt = finished
	if success {
		cached.Entries = entries // successful probes replace, so removals stick
		cached.HasSnapshot = true
		cached.SnapshotAt = finished
	}
	writeMCPListCache(path, cached)
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

func cleanupMCPListCache(dir string, now time.Time, retention time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > retention {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
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

func runClaudeMCPList(parent context.Context, launch claudeLaunchContext) ([]mcpConfigEntry, bool) {
	// Prefer the binary that launched this session over a PATH search: hooks
	// inherit the session's environment, and a desktop- or MDM-launched
	// Claude frequently passes one without the CLI on it. Same order as
	// runCodexMCPList.
	bin := launch.Executable
	if bin == "" {
		bin = "claude"
	}
	if !filepath.IsAbs(bin) {
		var err error
		bin, err = exec.LookPath(bin)
		if err != nil {
			return nil, false
		}
	}
	ctx, cancel := context.WithTimeout(parent, mcpListProbeTimeout)
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
	configureMCPListProbe(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return parseClaudeMCPList(string(out)), true
}

func runCodexMCPList(parent context.Context, launch codexLaunchContext) ([]mcpConfigEntry, bool) {
	bin := launch.Executable
	if bin == "" {
		bin = "codex"
	}
	if !filepath.IsAbs(bin) {
		var err error
		bin, err = exec.LookPath(bin)
		if err != nil {
			return nil, false
		}
	}
	ctx, cancel := context.WithTimeout(parent, mcpListProbeTimeout)
	defer cancel()
	args := append(launch.replayArgs(), "mcp", "list", "--json")
	cmd := exec.CommandContext(ctx, bin, args...)
	if launch.CWD != "" {
		cmd.Dir = launch.CWD
	}
	configureMCPListProbe(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return decodeCodexMCPList(out)
}
