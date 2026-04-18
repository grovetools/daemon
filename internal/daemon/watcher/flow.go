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
	"github.com/grovetools/core/util/frontmatter"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/enrichment"
	"github.com/grovetools/flow/pkg/orchestration"
)

// FlowHandler implements DomainHandler for watching plan directories.
// When plan files change, it triggers an immediate plan stats re-scan
// rather than waiting for the PlanCollector's polling interval.
//
// On a scoped daemon, only plan directories inside the configured scope
// are watched — out-of-scope plans fall through to the periodic full
// scan from PlanCollector instead of fsnotify.
type FlowHandler struct {
	store   *store.Store
	cfg     *config.Config
	scope   string
	locator *workspace.NotebookLocator
	ulog    *logging.UnifiedLogger

	// Maps watched path -> workspace node
	watchedPaths map[string]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// Debounce timer for plan stats refresh
	refreshTimer *time.Timer
	refreshMu    sync.Mutex
	debounceMs   int
}

// NewFlowHandler creates a new FlowHandler instance. An empty scope watches
// every workspace's plans directory.
func NewFlowHandler(st *store.Store, cfg *config.Config, debounceMs int, scope string) *FlowHandler {
	if debounceMs <= 0 {
		debounceMs = 2000
	}

	return &FlowHandler{
		store:        st,
		cfg:          cfg,
		scope:        scope,
		locator:      workspace.NewNotebookLocator(cfg),
		ulog:         logging.NewUnifiedLogger("groved.watcher.flow"),
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
		if !store.IsInScope(node.Path, h.scope) {
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
// It also parses modified/created .md files to instantly discover new jobs.
func (h *FlowHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	h.ulog.Debug("Plan file changes detected").Field("count", len(events)).Log(ctx)

	var discoveredJobs []*models.JobInfo

	for _, event := range events {
		if !strings.HasSuffix(event.Name, ".md") {
			continue
		}
		if event.Op&fsnotify.Write == 0 && event.Op&fsnotify.Create == 0 {
			continue
		}

		base := filepath.Base(event.Name)
		if base == "spec.md" || base == "README.md" {
			continue
		}

		file, err := os.Open(event.Name)
		if err != nil {
			continue
		}

		meta, err := frontmatter.Parse(file)
		file.Close()

		if err == nil && meta.ID != "" {
			submittedAt := meta.StartedAt
			if submittedAt.IsZero() {
				submittedAt = meta.UpdatedAt
			}
			if submittedAt.IsZero() {
				submittedAt = time.Now()
			}

			planDir := filepath.Dir(event.Name)
			job := &models.JobInfo{
				ID:          meta.ID,
				Title:       meta.Title,
				Type:        models.JobType(meta.Type),
				Status:      meta.Status,
				PlanDir:     planDir,
				PlanName:    filepath.Base(planDir),
				JobFile:     base,
				SubmittedAt: submittedAt,
			}

			// Look up workspace from watched paths
			h.pathsMutex.RLock()
			for watchedPath, wsNode := range h.watchedPaths {
				if strings.HasPrefix(event.Name, watchedPath+string(filepath.Separator)) || event.Name == watchedPath {
					job.WorkDir = wsNode.Path
					job.Repo = wsNode.Name
					if meta.Worktree != "" {
						job.Branch = meta.Worktree
					} else if wsNode.IsWorktree() {
						job.Branch = wsNode.Name
					}
					break
				}
			}
			h.pathsMutex.RUnlock()

			discoveredJobs = append(discoveredJobs, job)
		}
	}

	if len(discoveredJobs) > 0 {
		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdateJobsDiscovered,
			Source:  "flow_watcher",
			Payload: discoveredJobs,
		})
	}

	h.triggerRefresh()
	return nil
}

func (h *FlowHandler) HandleStoreUpdate(update store.Update) {
	if update.Type == store.UpdateConfigReload {
		newCfg, err := config.LoadDefault()
		if err != nil {
			h.ulog.Error("Failed to reload config").Err(err).Log(context.Background())
			return
		}
		h.cfg = newCfg
		h.locator = workspace.NewNotebookLocator(newCfg)
	}
}

func (h *FlowHandler) OnStart(ctx context.Context) {
	// Kick off a first refresh so /api/plans has a snapshot to serve
	// before any filesystem event arrives. The PlanCollector still
	// handles aggregated PlanStats; this populates the deep cache.
	h.triggerRefresh()
}

// triggerRefresh debounces plan stats re-scan to avoid excessive work.
func (h *FlowHandler) triggerRefresh() {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
	}

	h.refreshTimer = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		ctx := context.Background()
		h.ulog.Debug("Refreshing plan stats after file change").Log(ctx)

		planStats, err := enrichment.FetchPlanStatsMap()
		if err != nil {
			h.ulog.Error("Failed to fetch plan stats").Err(err).Log(ctx)
			return
		}

		state := h.store.Get()
		var deltas []*models.WorkspaceDelta
		for k, v := range state.Workspaces {
			if stats, ok := planStats[k]; ok {
				if !store.PlanStatsEqual(v.PlanStats, stats) {
					deltas = append(deltas, &models.WorkspaceDelta{
						Path:      k,
						PlanStats: stats,
					})
				}
			}
		}

		if len(deltas) > 0 {
			h.store.ApplyUpdate(store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "flow_watcher",
				Scanned: len(deltas),
				Payload: deltas,
			})
		}

		// Refresh the deep plan cache the browser reads from.
		// Walking every watched plansDir keeps this work out of TUI
		// clients, and the debounce above limits how often we do it.
		plansByDir := make(map[string][]*orchestration.Plan)
		h.pathsMutex.RLock()
		seen := make(map[string]struct{})
		for _, wsNode := range h.watchedPaths {
			plansDir, err := h.locator.GetPlansDir(wsNode)
			if err != nil || plansDir == "" {
				continue
			}
			if _, dup := seen[plansDir]; dup {
				continue
			}
			seen[plansDir] = struct{}{}

			entries, err := os.ReadDir(plansDir)
			if err != nil {
				continue
			}
			plans := make([]*orchestration.Plan, 0, len(entries))
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				planPath := filepath.Join(plansDir, entry.Name())
				if p, err := orchestration.LoadPlan(planPath); err == nil {
					plans = append(plans, p)
				}
			}
			plansByDir[plansDir] = plans
		}
		h.pathsMutex.RUnlock()

		if len(plansByDir) > 0 {
			h.store.ApplyUpdate(store.Update{
				Type:    store.UpdatePlans,
				Source:  "flow_watcher",
				Scanned: len(plansByDir),
				Payload: plansByDir,
			})
		}
	})
}
