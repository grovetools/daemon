package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	coregit "github.com/grovetools/core/git"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	corestate "github.com/grovetools/core/state"
	"github.com/grovetools/core/util/frontmatter"
	"github.com/grovetools/daemon/internal/daemon/jobattr"
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

	// Maps watched path -> the workspace node that OWNS that path's plans.
	// Many workspaces of one ecosystem resolve to a single centralized plans
	// directory, so the value is chosen by sortPlanOwners rather than by
	// whichever workspace happened to be registered last (see
	// ComputeWatchPaths). watchedNodes is the node set the same computation
	// saw, indexed for frontmatter `worktree:` resolution; it is replaced
	// atomically with watchedPaths so a lookup can never mix generations.
	watchedPaths map[string]*workspace.WorkspaceNode
	watchedNodes *jobattr.Index
	pathsMutex   sync.RWMutex

	// Debounce timer + accumulated scope for the next refresh (guarded by
	// refreshMu). pendingAll forces a full disk rescan; pendingDirs holds the
	// plans directories fsnotify implicated since the last run. A trigger with
	// neither set is an overlay-only pass: it re-projects cached rows through
	// the current bindings/git/session state without touching plan files.
	refreshTimer    *time.Timer
	refreshDeadline time.Time
	refreshMu       sync.Mutex
	pendingAll      bool
	pendingDirs     map[string]struct{}
	refreshRunMu    sync.Mutex
	debounceMs      int

	// Per-plansDir scan results, so event-scoped refreshes only re-read the
	// affected directory instead of the whole portfolio. Guarded by
	// refreshRunMu (only touched inside runRefresh).
	dirCache map[string]*dirScanResult

	// Aggregated-PlanStats pass bookkeeping. The stats leg re-reads every
	// plans directory on disk, so it executes on its own goroutine with
	// trailing-run coalescing — never under refreshRunMu, where it would
	// delay synchronous lifecycle publishes behind that disk work.
	statsMu      sync.Mutex
	statsRunning bool
	statsQueued  bool
	// scanSeq increments on every index publish; the async stats pass uses it
	// as a fence so results read from disk before a newer publish are
	// discarded instead of clobbering fresher lifecycle state.
	scanSeq atomic.Uint64
}

// dirScanResult is the disk-derived portion of one plans directory's rows.
// Selected/RunningSessions/bindings/git are overlays recomputed on every
// publish from live store state, so cached entries never pin them stale.
type dirScanResult struct {
	plans     []*orchestration.Plan
	summaries []models.PlanSummary
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

	nodes := make([]*workspace.WorkspaceNode, 0, len(workspaces))
	for _, ew := range workspaces {
		if ew != nil && ew.WorkspaceNode != nil {
			nodes = append(nodes, ew.WorkspaceNode)
		}
	}
	// Registration order decides who owns a plans directory, and the caller's
	// order is not an order at all: store.GetWorkspaces() materializes its
	// slice by ranging a map, so it is a fresh permutation on every refresh.
	// Every member repo and worktree of one ecosystem resolves to the SAME
	// centralized plans directory (NotebookLocator.getContextNodeForPath maps
	// them all onto the origin ecosystem's notebook workspace), so an
	// unordered registration attributed that directory — and therefore every
	// job discovered under it — to whichever workspace happened to land last.
	// That is how a grovetools plan job was persisted against a `tuimux`
	// checkout inside an unrelated worktree container.
	sortPlanOwners(nodes)

	claimed := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		plansDir, err := h.locator.GetPlansDir(node)
		if err != nil || plansDir == "" {
			continue
		}

		// Centralized notebook workspaces can be reached through aliases. Register
		// the resolved root because fsnotify reports target-path events on Darwin.
		resolved := resolveFlowWatchPath(plansDir)
		// First claim wins, and sortPlanOwners has already put the workspace
		// that should win first. Claiming explicitly (rather than relying on
		// addWatchRecursive's overwrite) keeps the rule readable and stops a
		// later node from silently re-owning the directory's subpaths.
		if _, dup := claimed[resolved]; dup {
			continue
		}
		claimed[resolved] = struct{}{}
		addWatchRecursive(resolved, node, newWatches)
	}

	h.pathsMutex.Lock()
	previous := h.watchedPaths
	h.watchedPaths = newWatches
	h.watchedNodes = jobattr.NewIndex(nodes)
	h.pathsMutex.Unlock()

	// Watch-registration boundary: a live daemon log must be able to prove
	// which plan directories the flow handler asked to cover. The set only
	// changes when plans/workspaces appear or disappear, so info is quiet.
	var added, removed []string
	for p := range newWatches {
		if _, ok := previous[p]; !ok {
			added = append(added, p)
		}
	}
	for p := range previous {
		if _, ok := newWatches[p]; !ok {
			removed = append(removed, p)
		}
	}
	if len(added) > 0 || len(removed) > 0 {
		sort.Strings(added)
		sort.Strings(removed)
		h.ulog.Info("Flow watch set changed").
			Field("watched", len(newWatches)).
			Field("added", strings.Join(added, ",")).
			Field("removed", strings.Join(removed, ",")).
			Log(context.Background())
	}

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
	affectedDirs := h.affectedPlansDirs(events)

	for _, event := range events {
		if (filepath.Base(event.Name) == ".grove-plan.yml" || filepath.Base(event.Name) == "config.yml") &&
			(event.Op&fsnotify.Write != 0 || event.Op&fsnotify.Create != 0 || event.Op&fsnotify.Rename != 0 || event.Op&fsnotify.Remove != 0) {
			lifecycleChanged = true
			// Event-match boundary: plan-config mutations are the hold/unhold
			// delivery proof and are rare, so log each one at info.
			h.ulog.Info("Plan lifecycle event received").
				Field("path", event.Name).
				Field("op", event.Op.String()).
				Log(ctx)
		}
		// A directory created directly under a plans dir is a new plan being
		// born (`flow plan init`, plan copy). Its config write happens before
		// fsnotify can watch the new directory, so this bare dir-create is the
		// ONLY signal we get — treat it as a lifecycle edge rather than
		// letting the new row wait out the enrichment debounce.
		if event.Op&fsnotify.Create != 0 && !lifecycleChanged && h.isDirectPlanDirCreate(event.Name) {
			lifecycleChanged = true
			h.ulog.Info("Plan directory created").
				Field("path", event.Name).
				Log(ctx)
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

			// Attribute the job to a workspace. This is the same computation
			// the JobCollector performs on its periodic sweep, and both
			// publish under the same store key with last-write-wins, so it
			// MUST go through the shared jobattr rule: any divergence makes a
			// job's recorded workspace flip depending on which producer ran
			// most recently.
			//
			// The plans directory only identifies the plan's OWNER workspace
			// (one ecosystem, many members). The job's own frontmatter
			// `worktree:` is the higher authority for which checkout the job
			// actually runs in, resolved within the owner's ecosystem and
			// deliberately degrading to the owner — never to a stranger — when
			// the name is missing, unknown, or ambiguous.
			h.pathsMutex.RLock()
			owner := h.ownerForPath(resolveFlowWatchPath(event.Name))
			index := h.watchedNodes
			h.pathsMutex.RUnlock()
			if owner != nil {
				job.WorkDir, job.Repo, job.Branch, _ = jobattr.JobWorkspace(
					index, owner, meta.Worktree, owner.Path, owner.Name)
			}

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
		h.triggerLifecycleRefresh(affectedDirs)
	} else {
		h.scheduleRefresh(affectedDirs, false, time.Duration(h.debounceMs)*time.Millisecond)
	}
	return nil
}

// isDirectPlanDirCreate reports whether path is a just-created directory that
// sits immediately under a watched plans directory (or its .archive
// container) — i.e. a new plan row, not organizational churn deeper inside an
// existing plan.
func (h *FlowHandler) isDirectPlanDirCreate(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if strings.HasPrefix(filepath.Base(path), ".") {
		return false
	}
	parent := resolveFlowWatchPath(filepath.Dir(path))
	if filepath.Base(parent) == ".archive" {
		parent = filepath.Dir(parent)
	}

	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()
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
		if parent == resolveFlowWatchPath(plansDir) {
			return true
		}
	}
	return false
}

// affectedPlansDirs maps event paths back to the plans directories they touch,
// so the follow-up refresh only re-reads those directories from disk.
func (h *FlowHandler) affectedPlansDirs(events []fsnotify.Event) map[string]struct{} {
	dirs := make(map[string]struct{})
	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()
	for _, event := range events {
		// One event belongs to exactly one plans directory, so the enclosing
		// watch entry has to be the most specific one — first-match-over-a-map
		// could scope the follow-up rescan to a different directory than the
		// one that actually changed and leave the real edit unindexed.
		owner := h.ownerForPath(resolveFlowWatchPath(event.Name))
		if owner == nil {
			continue
		}
		if plansDir, err := h.locator.GetPlansDir(owner); err == nil && plansDir != "" {
			dirs[plansDir] = struct{}{}
		}
	}
	return dirs
}

func (h *FlowHandler) HandleStoreUpdate(update store.Update) {
	switch update.Type {
	case store.UpdateConfigReload:
		newCfg, err := config.LoadDefault()
		if err != nil {
			h.ulog.Error("Failed to reload config").Err(err).Log(context.Background())
			return
		}
		h.cfg = newCfg
		h.locator = workspace.NewNotebookLocator(newCfg)

	case store.UpdateWorkspaces:
		// Workspace discovery just (re)populated the watch set — this is the
		// cold-start edge. Without it the first populated index build waits for
		// the 5-minute reconciliation ticker or a coincidental plan-file event:
		// OnStart's refresh usually fires before discovery completes and finds
		// no plans directories at all. The UnifiedWatcher recomputes watch
		// paths before broadcasting this update to handlers, so a short
		// coalescing delay is all that's needed.
		h.scheduleRefresh(nil, true, workspaceRefreshDelay)

	case store.UpdateWorkspacesDelta:
		// Git enrichment landing in the store is what fills the cheap cached
		// git column on rows (applyCachedPlanGit). Re-project cached rows
		// through the fresh state; the store suppresses no-change broadcasts,
		// so quiet deltas cost one in-memory pass and no SSE traffic. The flow
		// watcher's own PlanStats deltas carry no GitStatus and cannot loop.
		if deltas, ok := update.Payload.([]*models.WorkspaceDelta); ok {
			for _, delta := range deltas {
				if delta != nil && delta.GitStatus != nil {
					h.scheduleRefresh(nil, false, time.Duration(h.debounceMs)*time.Millisecond)
					break
				}
			}
		}
	}
}

// workspaceRefreshDelay coalesces bursts of workspace-set changes while still
// making the first populated snapshot land promptly after discovery.
const workspaceRefreshDelay = 250 * time.Millisecond

func (h *FlowHandler) OnStart(ctx context.Context) {
	// Kick off a first refresh so /api/plans has a snapshot to serve
	// before any filesystem event arrives. The PlanCollector still
	// handles aggregated PlanStats; this populates the deep cache.
	// When workspace discovery already ran (restart, late registration) the
	// watch set is populated and there is no reason to sit out the full
	// debounce; otherwise the UpdateWorkspaces edge in HandleStoreUpdate is
	// what delivers the first populated snapshot.
	if h.store != nil && len(h.store.GetWorkspaces()) > 0 {
		h.scheduleRefresh(nil, true, workspaceRefreshDelay)
	} else {
		h.triggerRefresh()
	}
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

// triggerRefresh schedules a full debounced rescan of every plans directory.
func (h *FlowHandler) triggerRefresh() {
	h.scheduleRefresh(nil, true, time.Duration(h.debounceMs)*time.Millisecond)
}

// scheduleRefresh merges the requested scope into the pending set and (re)arms
// the debounce timer. all=true forces every plans directory to rescan; dirs
// scopes the disk work to the named plans directories; neither means an
// overlay-only re-projection of cached rows.
func (h *FlowHandler) scheduleRefresh(dirs map[string]struct{}, all bool, delay time.Duration) {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	if all {
		h.pendingAll = true
	}
	for dir := range dirs {
		if h.pendingDirs == nil {
			h.pendingDirs = make(map[string]struct{})
		}
		h.pendingDirs[dir] = struct{}{}
	}

	// Earliest-deadline coalescing: the scope above is already merged, so a
	// slower trigger must never push out an armed faster one (e.g. a git
	// delta's 2s debounce arriving after the 250ms cold-start edge). This
	// also bounds storm latency — a steady event stream fires at most one
	// debounce interval after its first event instead of deferring forever.
	deadline := time.Now().Add(delay)
	if h.refreshTimer != nil {
		if !h.refreshDeadline.After(deadline) {
			return
		}
		h.refreshTimer.Stop()
	}
	h.refreshDeadline = deadline
	h.refreshTimer = time.AfterFunc(delay, h.refresh)
}

// takePendingScope drains the accumulated refresh scope and cancels any armed
// timer, merging extra scope from a synchronous (lifecycle) caller.
func (h *FlowHandler) takePendingScope(extra map[string]struct{}) (bool, map[string]struct{}) {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
		h.refreshTimer = nil
	}
	all := h.pendingAll
	dirs := h.pendingDirs
	h.pendingAll = false
	h.pendingDirs = nil
	for dir := range extra {
		if dirs == nil {
			dirs = make(map[string]struct{})
		}
		dirs[dir] = struct{}{}
	}
	return all, dirs
}

// triggerLifecycleRefresh cancels a pending eventually-consistent refresh and
// publishes this observed plan-config transition before HandleEvents returns.
func (h *FlowHandler) triggerLifecycleRefresh(dirs map[string]struct{}) {
	all, merged := h.takePendingScope(dirs)
	h.runRefresh(all, merged)
}

// refresh is the debounce-timer callback: drain the pending scope and run.
func (h *FlowHandler) refresh() {
	all, dirs := h.takePendingScope(nil)
	h.runRefresh(all, dirs)
}

// runRefresh rebuilds and publishes the plan index, then updates the
// aggregated PlanStats enrichment. Ordering is deliberate: the row projection
// is built from cheap per-directory scans and already-collected daemon state,
// and is published BEFORE the PlanStats pass, which recounts every plans
// directory in the portfolio and must never gate first-row availability.
func (h *FlowHandler) runRefresh(all bool, scopeDirs map[string]struct{}) {
	h.refreshRunMu.Lock()
	defer h.refreshRunMu.Unlock()

	ctx := context.Background()
	start := time.Now()

	state := h.store.Get()

	// Snapshot the watch set into unique plansDir -> workspace node targets.
	// The first entry wins per directory, so the keys are visited in sorted
	// order: the representative decides each row's WorkspaceRoot and selected
	// plan, and those must not differ between two refreshes of an unchanged
	// watch set.
	h.pathsMutex.RLock()
	watched := make([]string, 0, len(h.watchedPaths))
	for path := range h.watchedPaths {
		watched = append(watched, path)
	}
	sort.Strings(watched)
	targets := make(map[string]*workspace.WorkspaceNode)
	for _, path := range watched {
		wsNode := h.watchedPaths[path]
		plansDir, err := h.locator.GetPlansDir(wsNode)
		if err != nil || plansDir == "" {
			continue
		}
		if _, dup := targets[plansDir]; !dup {
			targets[plansDir] = wsNode
		}
	}
	h.pathsMutex.RUnlock()

	// Boot ordering: before workspace discovery has populated the store there
	// is nothing to index yet. Publishing the empty snapshot here would be a
	// lie ("scanned, zero plans") that a genuinely empty portfolio later can't
	// be distinguished from; the UpdateWorkspaces edge re-triggers us.
	if len(targets) == 0 && len(state.Workspaces) == 0 {
		h.ulog.Debug("Skipping plan index refresh before workspace discovery").Log(ctx)
		return
	}

	if h.dirCache == nil {
		h.dirCache = make(map[string]*dirScanResult)
	}

	// Rescan only what the scope implicates (plus cache misses); reuse the
	// cached disk scan for everything else.
	scanAt := time.Now()
	rescanned := 0
	for plansDir := range targets {
		_, affected := scopeDirs[plansDir]
		if _, cached := h.dirCache[plansDir]; cached && !all && !affected {
			continue
		}
		h.dirCache[plansDir] = scanPlansDir(plansDir, planWorkspaceRoot(targets[plansDir]), scanAt)
		rescanned++
	}
	for dir := range h.dirCache {
		if _, ok := targets[dir]; !ok {
			delete(h.dirCache, dir)
		}
	}
	scanDone := time.Now()

	// Merge cached scans and re-apply the live overlays (selection, running
	// sessions, registry bindings, cached git) from current store state.
	plansByDir := make(map[string][]*orchestration.Plan, len(targets))
	var summaries []models.PlanSummary
	registryEntries, _ := worktreeregistry.ListAll()
	for plansDir, wsNode := range targets {
		result := h.dirCache[plansDir]
		if result == nil {
			continue
		}
		selectedPlan, _ := corestate.GetString(planWorkspaceRoot(wsNode), coreplan.StateKey)
		for _, base := range result.summaries {
			row := base
			row.RunningSessions = countRunningSessions(state.Sessions, row.PlanName)
			if !row.Archived {
				row.Selected = selectedPlan == row.PlanName
			}
			summaries = append(summaries, row)
		}
		plansByDir[plansDir] = result.plans
	}
	summaries = applyQualifiedPlanBindings(summaries, registryEntries)
	summaries = applyCachedPlanGit(summaries, state.Workspaces)

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
	publishDone := time.Now()
	h.scanSeq.Add(1)

	// Aggregated PlanStats enrichment. Overlay-only passes changed nothing on
	// disk, so the recount is skipped for them. The pass runs asynchronously:
	// holding refreshRunMu across its disk reads would queue the next
	// synchronous lifecycle publish behind them.
	if all || rescanned > 0 || len(scopeDirs) > 0 {
		h.kickPlanStats()
	}

	elapsed := time.Since(start)
	entry := h.ulog.Debug("Plan index refresh")
	if elapsed > time.Second {
		entry = h.ulog.Info("Slow plan index refresh")
	}
	entry.Field("rows", len(summaries)).
		Field("dirs", len(targets)).
		Field("rescanned", rescanned).
		Field("full", all).
		Field("scan_ms", scanDone.Sub(start).Milliseconds()).
		Field("publish_ms", publishDone.Sub(scanDone).Milliseconds()).
		Field("total_ms", elapsed.Milliseconds()).
		Log(ctx)
}

// kickPlanStats starts (or queues onto) the async aggregated-PlanStats pass.
// At most one pass runs at a time; kicks during a run coalesce into exactly
// one trailing run, which re-reads disk and so converges on the final state.
func (h *FlowHandler) kickPlanStats() {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	if h.statsRunning {
		h.statsQueued = true
		return
	}
	h.statsRunning = true
	go h.planStatsLoop()
}

func (h *FlowHandler) planStatsLoop() {
	for {
		seq := h.scanSeq.Load()
		h.refreshPlanStats(seq)
		h.statsMu.Lock()
		if h.statsQueued {
			h.statsQueued = false
			h.statsMu.Unlock()
			continue
		}
		if h.scanSeq.Load() != seq {
			// An index publish raced this pass; rerun so the emitted stats
			// can never lag the last published lifecycle state.
			h.statsMu.Unlock()
			continue
		}
		h.statsRunning = false
		h.statsMu.Unlock()
		return
	}
}

// refreshPlanStats recomputes the aggregated per-workspace PlanStats. It runs
// off the refresh mutex so its cost never delays row publishes. seq fences
// staleness: results computed from disk state older than the latest index
// publish are discarded (the trailing loop run recomputes them).
//
// The workspace set comes from the store, never from a fresh
// workspace.DiscoverAll: this pass fires on a 2s debounce behind plan-file
// events, and re-walking/re-classifying every workspace on disk each time was
// the daemon's dominant allocation (and therefore GC, and therefore CPU) load.
func (h *FlowHandler) refreshPlanStats(seq uint64) {
	planStats := enrichment.FetchPlanStatsMap(h.store.WorkspaceNodes(), h.locator)
	if h.scanSeq.Load() != seq {
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
}

// sortPlanOwners orders workspaces so the first node claiming a shared plans
// directory is the one that should own it.
//
// The preference is the same one NotebookLocator.ScanForAllPlans applies for
// the JobCollector ("prefer main projects over worktrees"), expressed through
// planWorkspaceRoot: a node that IS its own plan-workspace root is the
// workspace the centralized plans directory is named after, while members and
// worktrees merely inherit it. Path is the tiebreak purely so the result is
// reproducible across daemon restarts — never a coin flip that files the same
// job under a different workspace on the next scan.
func sortPlanOwners(nodes []*workspace.WorkspaceNode) {
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		ownsA := planWorkspaceRoot(a) == a.Path
		ownsB := planWorkspaceRoot(b) == b.Path
		if ownsA != ownsB {
			return ownsA
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})
}

// ownerForPath returns the workspace node owning the watched path that most
// specifically contains eventPath, or nil when the path is unwatched.
//
// Most-specific wins: the watch set holds a plans directory AND each of its
// plan subdirectories, so any job file matches several entries at once. Taking
// whichever entry map iteration yielded first was arbitrary twice over — it
// ignored prefix length, and it re-rolled on every event. Callers must hold
// pathsMutex.
func (h *FlowHandler) ownerForPath(eventPath string) *workspace.WorkspaceNode {
	var best string
	var owner *workspace.WorkspaceNode
	for watchedPath, wsNode := range h.watchedPaths {
		if eventPath != watchedPath && !strings.HasPrefix(eventPath, watchedPath+string(filepath.Separator)) {
			continue
		}
		if owner != nil && len(watchedPath) <= len(best) {
			continue
		}
		best, owner = watchedPath, wsNode
	}
	return owner
}

// planWorkspaceRoot returns the canonical owner identity for plan rows. Many
// ecosystem members resolve to the same centralized plans directory; the
// representative node is picked deterministically by sortPlanOwners, and this
// still normalizes it, so row identity is the parent ecosystem even when the
// representative is a child checkout.
func planWorkspaceRoot(node *workspace.WorkspaceNode) string {
	if node == nil {
		return ""
	}
	if node.RootEcosystemPath != "" {
		return node.RootEcosystemPath
	}
	if node.Kind == workspace.KindEcosystemRoot {
		return node.Path
	}
	if node.IsWorktree() && node.ParentProjectPath != "" {
		return node.ParentProjectPath
	}
	return node.Path
}

// scanPlansDir reads one plans directory from disk into its cacheable base
// rows. Live overlays (Selected, RunningSessions, bindings, git) are
// deliberately NOT stamped here — runRefresh re-applies them on every publish
// so cached entries can never pin them stale.
func scanPlansDir(plansDir, workspaceRoot string, scanAt time.Time) *dirScanResult {
	indexed := loadIndexedPlans(plansDir)
	result := &dirScanResult{plans: make([]*orchestration.Plan, 0, len(indexed))}
	for _, indexedPlan := range indexed {
		p := indexedPlan.plan
		if !indexedPlan.archived {
			result.plans = append(result.plans, p)
		}
		summary := summarizePlan(p, plansDir, workspaceRoot, "", nil, scanAt)
		summary.Archived = indexedPlan.archived
		if indexedPlan.archived {
			summary.Lifecycle = "finished"
			summary.Selected = false
		}
		result.summaries = append(result.summaries, summary)
	}
	return result
}

// countRunningSessions mirrors summarizePlan's live-session overlay for rows
// merged from the per-directory cache.
func countRunningSessions(sessions map[string]*models.Session, planName string) int {
	running := 0
	for _, session := range sessions {
		if session != nil && session.PlanName == planName && session.EndedAt == nil {
			running++
		}
	}
	return running
}

type indexedPlanEntry struct {
	plan     *orchestration.Plan
	archived bool
}

// loadIndexedPlans recognizes only direct live plan directories and direct
// children of the archive container. Hidden organizational directories are
// never themselves plans, and archived plans remain separately identifiable
// as read-only rows in daemon clients.
//
// Loading is lenient: one malformed or half-written job file must degrade to
// a row with fewer jobs, never to the plan silently vanishing from the index
// (the pilot's burst-insert plans were dropped exactly that way).
func loadIndexedPlans(plansDir string) []indexedPlanEntry {
	ulog := logging.NewUnifiedLogger("groved.watcher.flow")
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
			p, problems := orchestration.LoadPlanLenient(filepath.Join(parent, entry.Name()))
			for _, problem := range problems {
				ulog.Debug("Plan indexed with degraded jobs").
					Field("plan_dir", filepath.Join(parent, entry.Name())).
					Err(problem).
					Log(context.Background())
			}
			if p != nil {
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
	notes := ""
	var repos []string
	if p.Config != nil {
		worktree = p.Config.Worktree
		notes = p.Config.Notes
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
		Worktree: worktree, Repositories: repos, Notes: notes, JobCounts: counts,
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

// applyCachedPlanGit projects only already-collected daemon status into cheap
// rows. It never invokes Git. Ecosystem rows aggregate their declared member
// checkouts; selected-row detail remains the only live Git path in Flow.
func applyCachedPlanGit(summaries []models.PlanSummary, workspaces map[string]*models.EnrichedWorkspace) []models.PlanSummary {
	byPath := make(map[string]*coregit.StatusInfo, len(workspaces))
	for path, workspace := range workspaces {
		if workspace == nil || workspace.GitStatus == nil || workspace.GitStatus.StatusInfo == nil {
			continue
		}
		byPath[filepath.Clean(path)] = workspace.GitStatus.StatusInfo
		if workspace.WorkspaceNode != nil {
			byPath[filepath.Clean(workspace.Path)] = workspace.GitStatus.StatusInfo
		}
	}
	for i := range summaries {
		if summaries[i].WorktreePath == "" {
			continue
		}
		paths := []string{summaries[i].WorktreePath}
		if len(summaries[i].Repositories) > 0 {
			paths = paths[:0]
			for _, repo := range summaries[i].Repositories {
				paths = append(paths, filepath.Join(summaries[i].WorktreePath, repo))
			}
		}
		aggregate := &coregit.StatusInfo{}
		found := false
		for _, path := range paths {
			status := byPath[filepath.Clean(path)]
			if status == nil {
				continue
			}
			found = true
			aggregate.IsDirty = aggregate.IsDirty || status.IsDirty
			aggregate.ModifiedCount += status.ModifiedCount
			aggregate.UntrackedCount += status.UntrackedCount
			aggregate.StagedCount += status.StagedCount
			aggregate.AheadCount += status.AheadMainCount
			aggregate.BehindCount += status.BehindMainCount
		}
		if found {
			summaries[i].GitStatus = aggregate
		}
	}
	return summaries
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
		summaries[i].BindingHealth = string(binding.Health)
		summaries[i].BindingReason = binding.Reason
		summaries[i].RegistryID = binding.RegistryID
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
