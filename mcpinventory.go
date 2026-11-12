package agenthooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func shouldReportMCPInventory(base *Event, tool *ToolCall) bool {
	return base.Kind == KindSessionStart || base.NativeName == "ConfigChange" ||
		((base.Kind == KindToolPre || base.Kind == KindPermission) && tool != nil && tool.MCP != nil)
}

// mcpInventoryReportWaitTimeout bounds how long a hook waits for another hook
// process that is delivering the same inventory version for the session.
const mcpInventoryReportWaitTimeout = 16 * time.Second

// mcpInventoryReportRetention bounds how long per-session delivery markers
// are kept.
const mcpInventoryReportRetention = 24 * time.Hour

// reportMCPInventory reports the inventory the hook can see right now: launch
// flags, config files, and the cached provider CLI snapshot. It never waits
// for a CLI probe. A stale or missing snapshot starts a background refresh;
// the first MCP hook after it lands reports the complete inventory, since
// delivery is deduplicated per content and completeness.
func (r *Runner) reportMCPInventory(ctx context.Context, base *Event) error {
	if r.mcpResolveOff || len(r.hMCPInventory) == 0 || base.Session.ID == "" {
		return nil
	}

	entries, complete := r.effectiveMCPInventory(base)
	return r.reportMCPInventorySnapshot(ctx, base, entries, complete)
}

func (r *Runner) reportMCPInventorySnapshot(ctx context.Context, base *Event, entries []mcpConfigEntry, complete bool) error {
	if r.mcpResolveOff || len(r.hMCPInventory) == 0 || base.Session.ID == "" {
		return nil
	}
	dir := filepath.Join(r.stateDir(), "agenthooks-mcp-reported")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create MCP inventory report directory: %w", err)
	}
	cleanupMCPListCache(dir, time.Now(), mcpInventoryReportRetention)
	keyParts := make([]string, 0, len(entries))
	for _, entry := range entries {
		keyParts = append(keyParts, entry.Name+"\x00"+entry.URL+"\x00"+entry.Command)
	}
	sort.Strings(keyParts)
	completeness := "partial"
	if complete {
		completeness = "complete"
	}
	sum := sha256.Sum256([]byte(string(base.Provider) + "\x00" + base.Session.ID + "\x00" + completeness + "\x00" + strings.Join(keyParts, "\x00")))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])[:32])

	deadline := time.Now().Add(mcpInventoryReportWaitTimeout)
	for {
		if _, err := os.Stat(path); err == nil { // #nosec G703 -- path is a SHA-256 filename under stateDir.
			return nil
		}
		unlock, ok, err := tryMCPListLock(path + ".lock")
		if err != nil {
			return fmt.Errorf("lock MCP inventory report: %w", err)
		}
		if !ok {
			if !time.Now().Before(deadline) {
				return fmt.Errorf("wait for MCP inventory report: %w", context.DeadlineExceeded)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}

		if _, err := os.Stat(path); err == nil { // #nosec G703 -- path is a SHA-256 filename under stateDir.
			unlock()
			return nil
		}
		servers := make([]MCPServer, 0, len(entries))
		for _, entry := range entries {
			servers = append(servers, MCPServer{Name: entry.Name, URL: entry.URL, Command: entry.Command})
		}
		event := &MCPInventoryEvent{
			Event: Event{
				Provider:            base.Provider,
				Variant:             base.Variant,
				NativeName:          "MCPInventory",
				Kind:                KindMCPInventory,
				Time:                r.now(),
				Session:             base.Session,
				Agent:               base.Agent,
				DetectionConfidence: base.DetectionConfidence,
				Backfilled:          true,
				Raw:                 nil,
			},
			Servers:  servers,
			Complete: complete,
		}
		_, dispatchErr := r.dispatch(ctx, event)
		if dispatchErr == nil {
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G703 -- path is a SHA-256 filename under stateDir.
			if err == nil {
				err = file.Close()
			}
			if err != nil && !os.IsExist(err) {
				dispatchErr = fmt.Errorf("mark MCP inventory reported: %w", err)
			}
		}
		unlock()
		return dispatchErr
	}
}

// effectiveMCPInventory merges the inventory sources for base's provider.
// The provider CLI snapshot is served at any age within retention; it counts
// as complete only when it is fresh, so a stale snapshot is merged with the
// config files and reported as partial.
func (r *Runner) effectiveMCPInventory(base *Event) ([]mcpConfigEntry, bool) {
	switch base.Provider {
	case ProviderClaudeCode:
		return r.effectiveClaudeMCPInventory(base.Session.CWD)
	case ProviderCodex:
		return r.effectiveCodexMCPInventory(base.Session.CWD)
	default:
		return loadMCPConfigEntries(base.Provider, base.Session.CWD), true
	}
}

func (r *Runner) effectiveClaudeMCPInventory(cwd string) ([]mcpConfigEntry, bool) {
	launch := currentClaudeLaunchContext(cwd)
	if launch.SafeMode {
		return nil, true
	}
	explicit := launch.explicitMCPEntries()
	if launch.StrictMCP {
		return explicit, true
	}
	if launch.Bare {
		if r.mcpListOff || len(launch.PluginDirs) == 0 {
			return explicit, true
		}
		list := r.claudeMCPList(launch, cwd)
		return firstMCPEntries(explicit, launch.barePluginEntries(list.Entries)), list.Complete
	}
	configured := loadMCPConfigEntries(ProviderClaudeCode, cwd)
	if r.mcpListOff {
		return firstMCPEntries(explicit, configured), true
	}
	list := r.claudeMCPList(launch, cwd)
	if len(launch.ReplayArgs) == 0 {
		return firstMCPEntries(explicit, configured, list.Entries), list.Complete
	}
	// Launch settings and plugins can remove or shadow ordinary config, so a
	// fresh replayed snapshot is authoritative. Until one exists, the cached
	// snapshot leads and config files fill in.
	if list.Complete {
		return firstMCPEntries(explicit, list.Entries), true
	}
	return firstMCPEntries(explicit, list.Entries, configured), false
}

func (r *Runner) effectiveCodexMCPInventory(cwd string) ([]mcpConfigEntry, bool) {
	configured := loadMCPConfigEntries(ProviderCodex, cwd)
	launch, ok := r.currentCodexLaunchContext(cwd)
	if !ok || launch.Unreplayable || r.mcpListOff {
		return configured, true
	}
	list := r.codexMCPList(launch)
	if list.Complete {
		return list.Entries, true
	}
	return firstMCPEntries(list.Entries, configured), false
}
