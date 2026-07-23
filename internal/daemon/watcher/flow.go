package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	corestate "github.com/grovetools/core/state"
	"github.com/grovetools/core/util/frontmatter"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/enrichment"
	"github.com/grovetools/flow/pkg/orchestration"
)

// FlowHandler implements DomainHandler for watching plan directories.
// When plan files change, it triggers an immediate plan stats re-scan
// rather than waiting for the PlanCollector's polling interval.
type FlowHandler struct {
	store   *store.Store
	cfg     *config.Config
	locator *workspace.NotebookLocator
	ulog    *logging.UnifiedLogger

	// Maps watched path -> workspace node
	watchedPaths map[string]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// Debounce timer for plan stats refresh
	refreshTimer *time.Timer
	refreshMu    sync.Mutex
	refreshRunMu sync.Mutex
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

		plansDir, err := h.locator.GetPlansDir(node)
		if err != nil || plansDir == "" {
			continue
		}

		// Centralized notebook workspaces can be reached through aliases. Register
		// the resolved root because fsnotify reports target-path events on Darwin.
		addWatchRecursive(resolveFlowWatchPath(plansDir), node, newWatches)
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

	eventPath := resolveFlowWatchPath(event.Name)
	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watchedPath := range h.watchedPaths {
		if eventPath == watchedPath || strings.HasPrefix(eventPath, watchedPath+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolveFlowWatchPath returns the stable filesystem spelling fsnotify uses.
func resolveFlowWatchPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

// HandleEvents triggers a debounced plan stats refresh when plan files change.
// It also parses modified/created .md files to instantly discover new jobs.
func (h *FlowHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	h.ulog.Debug("Plan file changes detected").Field("count", len(events)).Log(ctx)

	var discoveredJobs []*models.JobInfo
	lifecycleChanged := false

	for _, event := range events {
		if (filepath.Base(event.Name) == ".grove-plan.yml" || filepath.Base(event.Name) == "config.yml") &&
			(event.Op&fsnotify.Write != 0 || event.Op&fsnotify.Create != 0 || event.Op&fsnotify.Rename != 0) {
			lifecycleChanged = true
		}
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
		_ = file.Close()

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

			if len(meta.Channels) > 0 {
				job.Channels = meta.Channels
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

	if lifecycleChanged {
		// Plan lifecycle edges are state transitions, not eventually-consistent
		// enrichment. A debounced rescan can collapse hold→unhold into one live
		// snapshot, so publish each observed config mutation synchronously.
		h.triggerLifecycleRefresh()
	} else {
		h.triggerRefresh()
	}
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
	// fsnotify is an acceleration path, not a completeness guarantee. Periodic
	// reconciliation repairs missed/coalesced events and advances freshness.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.triggerRefresh()
			}
		}
	}()
}

// triggerRefresh debounces plan stats re-scan to avoid excessive work.
func (h *FlowHandler) triggerRefresh() {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
	}

	h.refreshTimer = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, h.refresh)
}

// triggerLifecycleRefresh cancels a pending eventually-consistent refresh and
// publishes this observed plan-config transition before HandleEvents returns.
func (h *FlowHandler) triggerLifecycleRefresh() {
	h.refreshMu.Lock()
	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
		h.refreshTimer = nil
	}
	h.refreshMu.Unlock()
	h.refresh()
}

func (h *FlowHandler) refresh() {
	h.refreshRunMu.Lock()
	defer h.refreshRunMu.Unlock()

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
	scanAt := time.Now()
	var summaries []models.PlanSummary
	registryEntries, _ := worktreeregistry.ListAll()
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

		indexed := loadIndexedPlans(plansDir)
		plans := make([]*orchestration.Plan, 0, len(indexed))
		selectedPlan, _ := corestate.GetString(wsNode.Path, coreplan.StateKey)
		for _, indexedPlan := range indexed {
			p := indexedPlan.plan
			if !indexedPlan.archived {
				plans = append(plans, p)
			}
			summary := summarizePlan(p, plansDir, wsNode.Path, selectedPlan, state.Sessions, scanAt)
			summary.Archived = indexedPlan.archived
			if indexedPlan.archived {
				summary.Lifecycle = "finished"
				summary.Selected = false
			}
			summaries = append(summaries, summary)
		}
		plansByDir[plansDir] = plans
	}
	h.pathsMutex.RUnlock()
	summaries = applyQualifiedPlanBindings(summaries, registryEntries)

	if len(plansByDir) > 0 {
		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdatePlans,
			Source:  "flow_watcher",
			Scanned: len(plansByDir),
			Payload: plansByDir,
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].PlanDir < summaries[j].PlanDir })
	h.store.ApplyUpdate(store.Update{
		Type: store.UpdatePlanIndexSnapshot, Source: "flow_watcher", Scanned: len(summaries),
		Payload: &models.PlanIndexSnapshot{ScannedAt: scanAt, Plans: summaries},
	})
}

type indexedPlanEntry struct {
	plan     *orchestration.Plan
	archived bool
}

// loadIndexedPlans recognizes only direct live plan directories and direct
// children of the archive container. Hidden organizational directories are
// never themselves plans, and archived plans remain separately identifiable
// as read-only rows in daemon clients.
func loadIndexedPlans(plansDir string) []indexedPlanEntry {
	var indexed []indexedPlanEntry
	loadChildren := func(parent string, archived bool) {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if p, err := orchestration.LoadPlan(filepath.Join(parent, entry.Name())); err == nil {
				indexed = append(indexed, indexedPlanEntry{plan: p, archived: archived})
			}
		}
	}
	loadChildren(plansDir, false)
	loadChildren(filepath.Join(plansDir, ".archive"), true)
	return indexed
}

func summarizePlan(p *orchestration.Plan, plansDir, workspaceRoot, selectedPlan string, sessions map[string]*models.Session, scannedAt time.Time) models.PlanSummary {
	lifecycle := "live"
	worktree := ""
	var repos []string
	if p.Config != nil {
		worktree = p.Config.Worktree
		repos = append(repos, p.Config.Repos...)
		switch p.Config.Status {
		case "hold", "review", "finished":
			lifecycle = p.Config.Status
		}
	}
	counts := make(map[string]int)
	for _, job := range p.Jobs {
		counts[string(job.Status)]++
	}
	running := 0
	for _, session := range sessions {
		if session != nil && session.PlanName == p.Name && session.EndedAt == nil {
			running++
		}
	}
	updatedAt := scannedAt
	if info, err := os.Stat(p.Directory); err == nil {
		updatedAt = info.ModTime()
	}
	return models.PlanSummary{
		PlanDir: p.Directory, PlanName: p.Name, WorkspaceRoot: workspaceRoot,
		PlansDir: plansDir, Lifecycle: lifecycle, Selected: selectedPlan == p.Name,
		Worktree: worktree, Repositories: repos, JobCounts: counts,
		RunningSessions: running, UpdatedAt: updatedAt, ScannedAt: scannedAt,
	}
}

// applyQualifiedPlanBindings enriches summaries from the one canonical
// registry-backed resolver. Bare plan names are deliberately never used as a
// join key: duplicate slugs in different notebook workspaces must retain their
// own container association.
func applyQualifiedPlanBindings(summaries []models.PlanSummary, entries []*worktreeregistry.Entry) []models.PlanSummary {
	requests := make([]coreplan.BindingRequest, 0, len(summaries))
	for _, summary := range summaries {
		requests = append(requests, coreplan.BindingRequest{
			PlanDir:            summary.PlanDir,
			WorkspaceRoot:      summary.WorkspaceRoot,
			ConfiguredWorktree: summary.Worktree,
			Archived:           summary.Archived,
		})
	}
	return applyResolvedPlanBindings(summaries, entries, coreplan.ResolvePlanBindings(requests))
}

func applyResolvedPlanBindings(summaries []models.PlanSummary, entries []*worktreeregistry.Entry, bindings map[string]coreplan.PlanBinding) []models.PlanSummary {
	entriesByPath := make(map[string]*worktreeregistry.Entry, len(entries))
	for _, entry := range entries {
		if entry != nil {
			entriesByPath[filepath.Clean(entry.AbsPath)] = entry
		}
	}
	for i := range summaries {
		binding := bindings[coreplan.NewPlanKey(summaries[i].PlanDir).String()]
		if !binding.Valid() {
			continue
		}
		summaries[i].WorktreePath = binding.ContainerPath
		entry := entriesByPath[filepath.Clean(binding.ContainerPath)]
		if entry == nil {
			continue
		}
		if len(summaries[i].Repositories) == 0 {
			summaries[i].Repositories = append([]string(nil), entry.Repos...)
		}
		summaries[i].Anchor = entry.AnchorOverride
		if summaries[i].Anchor == "" {
			summaries[i].Anchor = entry.Owner
		}
	}
	return summaries
}
