package watcher

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/enrichment"
	"github.com/sirupsen/logrus"
)

// FlowHandler implements DomainHandler for watching plan directories.
// When plan files change, it triggers an immediate plan stats re-scan
// rather than waiting for the PlanCollector's polling interval.
type FlowHandler struct {
	store   *store.Store
	cfg     *config.Config
	locator *workspace.NotebookLocator
	log     *logrus.Entry

	// Maps watched path -> workspace node
	watchedPaths map[string]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// Debounce timer for plan stats refresh
	refreshTimer *time.Timer
	refreshMu    sync.Mutex
	debounceMs   int
}

// NewFlowHandler creates a new FlowHandler instance.
func NewFlowHandler(st *store.Store, cfg *config.Config, debounceMs int) *FlowHandler {
	if debounceMs <= 0 {
		debounceMs = 2000
	}

	return &FlowHandler{
		store:        st,
		cfg:          cfg,
		locator:      workspace.NewNotebookLocator(cfg),
		log:          logging.NewLogger("groved.flow.watcher"),
		watchedPaths: make(map[string]*workspace.WorkspaceNode),
		debounceMs:   debounceMs,
	}
}

func (h *FlowHandler) Name() string {
	return "flow"
}

// ComputeWatchPaths returns plan directories for all workspaces.
func (h *FlowHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*workspace.WorkspaceNode)

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		plansDir, err := h.locator.GetPlansDir(node)
		if err != nil || plansDir == "" {
			continue
		}

		addWatchRecursive(plansDir, node, newWatches)
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

func (h *FlowHandler) MatchesEvent(event fsnotify.Event) bool {
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}

	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watchedPath := range h.watchedPaths {
		if event.Name == watchedPath || filepath.HasPrefix(event.Name, watchedPath+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// HandleEvents triggers a debounced plan stats refresh when plan files change.
func (h *FlowHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	h.log.WithField("count", len(events)).Debug("Plan file changes detected")
	h.triggerRefresh()
	return nil
}

func (h *FlowHandler) HandleStoreUpdate(update store.Update) {
	if update.Type == store.UpdateConfigReload {
		newCfg, err := config.LoadDefault()
		if err != nil {
			h.log.WithError(err).Error("Failed to reload config")
			return
		}
		h.cfg = newCfg
		h.locator = workspace.NewNotebookLocator(newCfg)
	}
}

func (h *FlowHandler) OnStart(ctx context.Context) {
	// No initial sync needed — the PlanCollector handles the first scan.
}

// triggerRefresh debounces plan stats re-scan to avoid excessive work.
func (h *FlowHandler) triggerRefresh() {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
	}

	h.refreshTimer = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		h.log.Debug("Refreshing plan stats after file change")

		planStats, err := enrichment.FetchPlanStatsMap()
		if err != nil {
			h.log.WithError(err).Error("Failed to fetch plan stats")
			return
		}

		state := h.store.Get()
		newWorkspaces := make(map[string]*models.EnrichedWorkspace)
		for k, v := range state.Workspaces {
			cpy := *v
			if stats, ok := planStats[k]; ok {
				cpy.PlanStats = stats
			}
			newWorkspaces[k] = &cpy
		}

		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdateWorkspaces,
			Source:  "flow_watcher",
			Scanned: len(newWorkspaces),
			Payload: newWorkspaces,
		})
	})
}
