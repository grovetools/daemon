package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// WorkspaceHandler implements DomainHandler for watching workspace roots and worktree directories.
// When a directory is created or removed, it triggers an immediate workspace rescan.
type WorkspaceHandler struct {
	store        *store.Store
	cfg          *config.Config
	log          *logrus.Entry
	watchedPaths map[string]bool
	pathsMutex   sync.RWMutex
	refreshTimer *time.Timer
	refreshMu    sync.Mutex
	debounceMs   int
}

func NewWorkspaceHandler(st *store.Store, cfg *config.Config, debounceMs int) *WorkspaceHandler {
	if debounceMs <= 0 {
		debounceMs = 2000
	}
	return &WorkspaceHandler{
		store:        st,
		cfg:          cfg,
		log:          logging.NewLogger("groved.workspace.watcher"),
		watchedPaths: make(map[string]bool),
		debounceMs:   debounceMs,
	}
}

func (h *WorkspaceHandler) Name() string {
	return "workspace"
}

func (h *WorkspaceHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]bool)

	// 1. Watch root grove paths (catches new clones)
	if h.cfg != nil && h.cfg.Groves != nil {
		for _, g := range h.cfg.Groves {
			if g.Enabled == nil || *g.Enabled {
				expanded := os.ExpandEnv(g.Path)
				if strings.HasPrefix(expanded, "~/") {
					home, _ := os.UserHomeDir()
					expanded = filepath.Join(home, expanded[2:])
				}
				if abs, err := filepath.Abs(expanded); err == nil {
					newWatches[abs] = true
				}
			}
		}
	}

	// 2. Watch every project's root and .grove-worktrees directory
	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		newWatches[node.Path] = true
		wtDir := filepath.Join(node.Path, ".grove-worktrees")
		if _, err := os.Stat(wtDir); err == nil {
			newWatches[wtDir] = true
		}
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.pathsMutex.Unlock()

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}
	return paths
}

func (h *WorkspaceHandler) MatchesEvent(event fsnotify.Event) bool {
	// Ignore chmod events
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}

	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	// We only care about events directly inside the watched directories
	dir := filepath.Dir(event.Name)
	return h.watchedPaths[dir] || h.watchedPaths[event.Name]
}

func (h *WorkspaceHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	h.log.WithField("count", len(events)).Debug("Workspace filesystem changes detected")
	h.triggerRefresh()
	return nil
}

func (h *WorkspaceHandler) HandleStoreUpdate(update store.Update) {
	if update.Type == store.UpdateConfigReload {
		if newCfg, err := config.LoadDefault(); err == nil {
			h.cfg = newCfg
		}
	}
}

func (h *WorkspaceHandler) OnStart(ctx context.Context) {}

func (h *WorkspaceHandler) triggerRefresh() {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
	}

	h.refreshTimer = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		h.log.Info("Refreshing workspaces after filesystem change")

		nodes, err := workspace.GetProjects(h.log.Logger)
		if err != nil {
			h.log.WithError(err).Error("Failed to discover workspaces")
			return
		}

		currentState := h.store.Get()
		enrichedMap := make(map[string]*models.EnrichedWorkspace)

		for _, node := range nodes {
			ew := &models.EnrichedWorkspace{WorkspaceNode: node}
			// Preserve existing enrichments
			if existing, ok := currentState.Workspaces[node.Path]; ok {
				ew.GitStatus = existing.GitStatus
				ew.NoteCounts = existing.NoteCounts
				ew.PlanStats = existing.PlanStats
				ew.ReleaseInfo = existing.ReleaseInfo
				ew.ActiveBinary = existing.ActiveBinary
				ew.CxStats = existing.CxStats
				ew.GitRemoteURL = existing.GitRemoteURL
			}
			enrichedMap[node.Path] = ew
		}

		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdateWorkspaces,
			Source:  "workspace_watcher",
			Payload: enrichedMap,
		})
	})
}
