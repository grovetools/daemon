package watcher

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// HUDWatcher emits per-workspace HUD snapshots over a channel.
//
// It subscribes to the central daemon store and, whenever an update could
// affect the targeted workspace, builds a fresh models.WorkspaceHUD from
// the current EnrichedWorkspace and forwards it. Emits are debounced so
// expensive operations downstream don't see more than one snapshot per
// debounceWindow.
type HUDWatcher struct {
	store         *store.Store
	path          string
	debounceWindow time.Duration
}

// NewHUDWatcher creates a HUD watcher targeting the given workspace path.
func NewHUDWatcher(st *store.Store, path string) *HUDWatcher {
	return &HUDWatcher{
		store:          st,
		path:           path,
		debounceWindow: 5 * time.Second,
	}
}

// Watch subscribes to the store and forwards HUD snapshots until ctx is
// cancelled. The returned channel is closed when the goroutine exits.
// Callers must drain the channel or risk dropped events (the watcher uses
// non-blocking sends so a slow consumer is silently degraded).
func (h *HUDWatcher) Watch(ctx context.Context) <-chan models.WorkspaceHUD {
	out := make(chan models.WorkspaceHUD, 4)
	sub := h.store.Subscribe()

	go func() {
		defer close(out)
		defer h.store.Unsubscribe(sub)

		// Emit an initial snapshot immediately so the consumer has data
		// without waiting for the next store update.
		if snap, ok := h.snapshot(); ok {
			select {
			case out <- snap:
			case <-ctx.Done():
				return
			}
		}

		var (
			lastEmit  time.Time
			pending   bool
			timer     *time.Timer
			timerC    <-chan time.Time
		)

		emit := func() {
			if snap, ok := h.snapshot(); ok {
				select {
				case out <- snap:
					lastEmit = time.Now()
					pending = false
				case <-ctx.Done():
				}
			} else {
				pending = false
			}
		}

		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case _, ok := <-sub:
				if !ok {
					// Store closed our subscription.
					return
				}
				// Debounce: emit immediately if we haven't emitted within
				// the debounce window, otherwise schedule a trailing emit.
				since := time.Since(lastEmit)
				if since >= h.debounceWindow {
					emit()
				} else if !pending {
					pending = true
					wait := h.debounceWindow - since
					if timer == nil {
						timer = time.NewTimer(wait)
					} else {
						timer.Reset(wait)
					}
					timerC = timer.C
				}
			case <-timerC:
				timerC = nil
				if pending {
					emit()
				}
			}
		}
	}()

	// Lifecycle contract: ctx cancellation drains the goroutine, which
	// closes out. Callers wait by receiving until out is closed.
	return out
}

// snapshot builds a WorkspaceHUD for h.path from the current store state.
// Returns false if the workspace is not currently known to the daemon.
func (h *HUDWatcher) snapshot() (models.WorkspaceHUD, bool) {
	state := h.store.Get()
	ws, ok := state.Workspaces[h.path]
	if !ok || ws == nil || ws.WorkspaceNode == nil {
		return models.WorkspaceHUD{}, false
	}

	hud := models.WorkspaceHUD{
		WorkspacePath: ws.Path,
		WorkspaceName: ws.Name,
		ShortPath:     shortPath(ws.Path),
	}

	if gs := ws.GitStatus; gs != nil && gs.StatusInfo != nil {
		hud.GitBranch = gs.Branch
		hud.GitDirty = gs.IsDirty
		hud.GitAhead = gs.AheadCount
		hud.GitBehind = gs.BehindCount
	}

	if ps := ws.PlanStats; ps != nil {
		hud.ActivePlan = ps.ActivePlan
		hud.PlanStatus = ps.PlanStatus
		hud.PlanJobCounts = models.HUDPlanJobCounts{
			Running: ps.Running,
			Pending: ps.Pending + ps.Todo,
			Done:    ps.Completed,
		}
	}

	if cs := ws.CxStats; cs != nil {
		hud.CxFiles = cs.Files
		hud.CxTokens = cs.Tokens
	}

	if nc := ws.NoteCounts; nc != nil {
		hud.NotebookCount = nc.Current + nc.Inbox + nc.Issues + nc.Docs +
			nc.Review + nc.InProgress + nc.Other
	}

	// HooksActive: not surfaced via the store yet; left at zero so the
	// HUD renders gracefully and a future commit can wire it in.
	hud.HooksActive = 0

	return hud, true
}

// shortPath returns "parent/leaf" for display purposes, falling back to
// the leaf alone (or the full path if it has no separators).
func shortPath(p string) string {
	clean := filepath.Clean(p)
	if clean == "" || clean == "." {
		return p
	}
	parent, leaf := filepath.Split(clean)
	parent = strings.TrimSuffix(parent, string(filepath.Separator))
	if parent == "" {
		return leaf
	}
	return filepath.Base(parent) + "/" + leaf
}
