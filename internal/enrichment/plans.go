package enrichment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	coreconfig "github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/core/util/frontmatter"
	"github.com/grovetools/core/util/pathutil"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// FetchPlanStatsMap computes per-workspace plan statistics for the ALREADY
// DISCOVERED workspace nodes it is handed. It uses a lightweight frontmatter
// parser to avoid importing flow packages.
//
// Taking the node set as a parameter is the point of this function's shape.
// It used to call workspace.NewDiscoveryService(...).DiscoverAll() itself, so
// every pass re-walked and re-classified every workspace on the filesystem —
// TOML-parsing and JSON-schema-validating each grove.toml, plus a registry
// load per node — for a set the daemon's store was already holding. The pass
// is kicked by plan-file events on a 2s debounce, so that rediscovery ran
// essentially continuously and its allocation churn was the daemon's dominant
// GC (and therefore CPU) load. Discovery from disk belongs to the workspace
// collector; every other producer reads its result.
//
// locator resolves each node's plans directory. Callers that already own one
// (the flow watcher builds its watch set from exactly this mapping) MUST pass
// it: a pass that re-derived its own from config.LoadDefault could disagree
// with the caller about where a workspace's plans live, and since these stats
// overwrite the same store field the plan index projects onto, the two
// producers would then fight over every row. nil falls back to the default
// config, for callers with no locator of their own.
func FetchPlanStatsMap(nodes []*workspace.WorkspaceNode, locator *workspace.NotebookLocator) map[string]*models.PlanStats {
	start := time.Now()

	if locator == nil {
		coreCfg, err := coreconfig.LoadDefault()
		if err != nil {
			coreCfg = &coreconfig.Config{}
		}
		locator = workspace.NewNotebookLocator(coreCfg)
	}
	pass := newPlanStatsPass(locator)

	statsByPath := resolvePerNodePlanStats(
		nodes,
		pass.plansDirFor,
		countPlanStats,
		pass.activePlanForNode,
		pass.planStatusForNode,
	)
	// PlanStatus is meaningful only with an explicit registry association.
	// Stamp that qualified identity separately from ActivePlan, whose legacy
	// state fallback is intentionally broader.
	for _, node := range nodes {
		if node == nil {
			continue
		}
		planName, ok := pass.registeredPlanForNode(node)
		if !ok {
			continue
		}
		if stats := statsByPath[node.Path]; stats != nil {
			stats.AssociatedPlan = planName
			if planDir := pass.associatedPlanDirForNode(node); planDir != "" {
				stats.AssociatedPlanDir = planDir
			} else if plansDir, err := pass.plansDirFor(node); err == nil {
				stats.AssociatedPlanDir = filepath.Join(plansDir, planName)
			}
		}
	}

	telemetry.RecordPlanStatsPass(len(nodes), time.Since(start))
	return statsByPath
}

// planStatsPass carries one FetchPlanStatsMap run's invariants and memos.
//
// Every derivation below is a function of something strictly COARSER than the
// node — the worktree container root, the plan directory, the state file —
// while the node set is the fleet (600+ at the scale that motivated this).
// Computing them per node meant the same few dozen registry files, plan
// configs and workspace configs were re-read thousands of times per pass. The
// memos key on the coarse thing, so each is paid once.
//
// A pass is short-lived by design: it is created per FetchPlanStatsMap call
// and thrown away, so nothing here can serve a stale answer across passes.
type planStatsPass struct {
	locator *workspace.NotebookLocator

	// entries is the whole ACTIVE worktree registry, loaded once per pass and
	// keyed exactly the way worktreeregistry.Save keys its files
	// (pathutil.WorktreeID of the entry's AbsPath), so a lookup here is
	// equivalent to the per-path Load it replaces.
	entries map[string]*worktreeregistry.Entry

	planDirByRoot map[string]string
	statusByDir   map[string]string
	activeByFile  map[string]string
	existsByPath  map[string]bool
}

func newPlanStatsPass(locator *workspace.NotebookLocator) *planStatsPass {
	p := &planStatsPass{
		locator:       locator,
		entries:       make(map[string]*worktreeregistry.Entry),
		planDirByRoot: make(map[string]string),
		statusByDir:   make(map[string]string),
		activeByFile:  make(map[string]string),
		existsByPath:  make(map[string]bool),
	}
	// A registry read failure degrades to "no node has a registered plan",
	// which is exactly what the per-node Loads it replaces produced when the
	// registry directory was unreadable.
	all, _ := worktreeregistry.ListAll()
	for _, entry := range all {
		if entry != nil && entry.AbsPath != "" {
			p.entries[pathutil.WorktreeID(entry.AbsPath)] = entry
		}
	}
	return p
}

func (p *planStatsPass) plansDirFor(node *workspace.WorkspaceNode) (string, error) {
	return p.locator.GetPlansDir(node)
}

// planForPath is worktreeregistry.PlanForPath served from the pass snapshot.
func (p *planStatsPass) planForPath(absPath string) (string, bool) {
	entry, ok := p.entries[pathutil.WorktreeID(absPath)]
	if !ok || entry == nil || entry.IsFinished() {
		return "", false
	}
	return entry.Plan, entry.Plan != ""
}

// registeredPlanForNode resolves registry ownership at the canonical worktree
// container root. Discovery nodes inside a synthetic container point at member
// repo checkouts, while the registry intentionally keys the container itself.
// Membership is structural, not kind-based: member checkouts without a
// grove.yml discover as NonGroveRepo (IsWorktree() == false) but still belong
// to their registered container; anything outside a container fails
// WorktreeRootForPath and resolves to no plan, exactly as before.
func (p *planStatsPass) registeredPlanForNode(node *workspace.WorkspaceNode) (string, bool) {
	if node == nil {
		return "", false
	}
	root, ok := workspace.WorktreeRootForPath(node.Path)
	if !ok {
		return "", false
	}
	return p.planForPath(root)
}

// associatedPlanDirForNode derives the qualified plan directory for a node's
// registered plan through the canonical registry resolver (plan.ResolveTarget
// on the worktree container root), NOT by joining this node's own plans dir
// with the plan name: a node resolved from a container path can carry a plans
// dir qualified by the container basename, which is wrong for standalone-repo
// containers (the plan name would masquerade as the workspace name). Returns
// "" when the node is not inside a registered container or the resolver
// cannot derive a plan dir.
//
// The result depends only on the container root, so it is memoized there.
// ResolveTarget is the expensive call in this file — it reaches
// workspace.GetProjectByPath and config.Load, i.e. a TOML parse and a JSON
// schema validation — and every member checkout and worktree of one container
// asks the same question.
func (p *planStatsPass) associatedPlanDirForNode(node *workspace.WorkspaceNode) string {
	if node == nil {
		return ""
	}
	root, ok := workspace.WorktreeRootForPath(node.Path)
	if !ok {
		return ""
	}
	if planDir, memoized := p.planDirByRoot[root]; memoized {
		return planDir
	}
	planDir := resolveAssociatedPlanDir(root)
	p.planDirByRoot[root] = planDir
	return planDir
}

func resolveAssociatedPlanDir(root string) string {
	target, err := plan.ResolveTarget(root)
	if err != nil || target == nil {
		return ""
	}
	return target.PlanDir
}

// planStatusForNode reads only the plan explicitly associated with this exact
// worktree container in the registry. It never infers ownership from a shared
// worktree name or from the parent workspace's aggregate plan stats.
func (p *planStatsPass) planStatusForNode(plansRootDir string, node *workspace.WorkspaceNode) string {
	if node == nil {
		return ""
	}
	planName, ok := p.registeredPlanForNode(node)
	if !ok {
		return ""
	}
	planDir := p.associatedPlanDirForNode(node)
	if planDir == "" {
		planDir = filepath.Join(plansRootDir, planName)
	}
	if status, memoized := p.statusByDir[planDir]; memoized {
		return status
	}
	status := readPlanStatus(planDir)
	p.statusByDir[planDir] = status
	return status
}

// readPlanStatus reads the `status:` field of a plan directory's config.
func readPlanStatus(planDir string) string {
	for _, filename := range []string{".grove-plan.yml", "config.yml"} {
		configData, err := os.ReadFile(filepath.Join(planDir, filename)) //nolint:gosec // qualified registry plan under known plans root
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(configData), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "status:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1])
				}
			}
		}
	}
	return ""
}

// activePlanForNode reads the active plan from a workspace's state file.
// It checks the notebook directory first (sibling of plans dir), then falls
// back to the legacy .grove/state.yml location.
func (p *planStatsPass) activePlanForNode(node *workspace.WorkspaceNode) string {
	if node == nil {
		return ""
	}
	// Registry-first: for a worktree node the notebook locator redirects to the
	// main project's notebook, so reading flow.active_plan from the notebook-sibling
	// state.yml would return the main checkout's plan. The XDG worktree registry
	// holds the worktree's own plan keyed by its container path, so prefer it.
	if activePlan, ok := p.planForPath(node.Path); ok {
		return activePlan
	}

	stateFilePath := filepath.Join(node.Path, ".grove", "state.yml")

	// Try notebook location first: state.yml lives alongside plans/ in the
	// notebook dir. Sibling nodes of one ecosystem share that path, so both the
	// existence probe and the parse are memoized on it.
	if plansDir, err := p.plansDirFor(node); err == nil {
		nbStatePath := filepath.Join(filepath.Dir(plansDir), "state.yml")
		if p.fileExists(nbStatePath) {
			stateFilePath = nbStatePath
		}
	}

	if activePlan, memoized := p.activeByFile[stateFilePath]; memoized {
		return activePlan
	}
	activePlan := readActivePlan(stateFilePath)
	p.activeByFile[stateFilePath] = activePlan
	return activePlan
}

func (p *planStatsPass) fileExists(path string) bool {
	if exists, memoized := p.existsByPath[path]; memoized {
		return exists
	}
	_, err := os.Stat(path)
	exists := err == nil
	p.existsByPath[path] = exists
	return exists
}

// readActivePlan parses flow.active_plan out of one state file, accepting both
// the JSON and the legacy YAML spellings.
func readActivePlan(stateFilePath string) string {
	data, err := os.ReadFile(stateFilePath) //nolint:gosec // G304: path from known plan directory
	if err != nil {
		return ""
	}

	var stateMap map[string]interface{}
	if err := json.Unmarshal(data, &stateMap); err != nil {
		// Try YAML format - look for "flow.active_plan:" line
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "flow.active_plan:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1])
				}
			}
		}
		return ""
	}

	// Try both keys for backward compatibility
	if val, ok := stateMap[plan.StateKey].(string); ok {
		return val
	}
	if val, ok := stateMap[plan.LegacyStateKey].(string); ok {
		return val
	}
	return ""
}

// resolvePerNodePlanStats builds the per-node PlanStats map. The job-count
// aggregation over a plans directory is node-independent, so it is computed once
// per plansRootDir and cached. ActivePlan and PlanStatus, however, are
// PER-NODE: sibling worktrees under one project share a plans dir but each has
// its own active plan / plan status. The prior implementation handed every
// sibling the SAME *PlanStats pointer and stamped ActivePlan only on the
// cache-miss (first-discovered) node, so the first sibling's plan leaked onto
// all of them. Here each node gets a shallow copy of the shared counts with its
// own ActivePlan/PlanStatus stamped, fixing every consumer of the map (HUD and
// otherwise). Resolvers are injected so the logic is unit-testable without
// filesystem discovery.
func resolvePerNodePlanStats(
	nodes []*workspace.WorkspaceNode,
	plansDirFor func(*workspace.WorkspaceNode) (string, error),
	countsFor func(plansRootDir string) *models.PlanStats,
	activePlanFor func(*workspace.WorkspaceNode) string,
	planStatusFor func(plansRootDir string, node *workspace.WorkspaceNode) string,
) map[string]*models.PlanStats {
	statsByPath := make(map[string]*models.PlanStats)
	seenDirs := make(map[string]*models.PlanStats)

	for _, node := range nodes {
		// The node set now comes from the daemon store rather than a fresh
		// discovery, so a nil entry is a caller's bug rather than an
		// impossibility; skipping beats panicking the whole enrichment pass.
		if node == nil {
			continue
		}
		plansRootDir, err := plansDirFor(node)
		if err != nil {
			statsByPath[node.Path] = &models.PlanStats{}
			continue
		}

		cached, seen := seenDirs[plansRootDir]
		if !seen {
			cached = countsFor(plansRootDir)
			seenDirs[plansRootDir] = cached
		}

		// Shallow-copy the shared counts, then stamp the per-node fields.
		nodeStats := *cached
		nodeStats.ActivePlan = activePlanFor(node)
		nodeStats.PlanStatus = planStatusFor(plansRootDir, node)
		statsByPath[node.Path] = &nodeStats
	}

	return statsByPath
}

// countPlanStats aggregates the node-independent job counts across every plan
// directory under plansRootDir. PlanStatus/ActivePlan are intentionally NOT set
// here — they are per-node (see resolvePerNodePlanStats).
func countPlanStats(plansRootDir string) *models.PlanStats {
	stats := &models.PlanStats{}
	entries, err := os.ReadDir(plansRootDir)
	if err != nil {
		return stats
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		planPath := filepath.Join(plansRootDir, entry.Name())
		processPlanCounts(planPath, stats)
	}
	return stats
}

// processPlanCounts scans a single plan directory and aggregates node-independent
// job counts into stats. Node-specific fields (PlanStatus/ActivePlan) are handled
// separately per node (see resolvePerNodePlanStats), so this takes no node.
func processPlanCounts(planPath string, stats *models.PlanStats) {
	// Read plan config to check whether the plan is finished (finished plans are
	// not counted as active).
	configPath := filepath.Join(planPath, "config.yml")
	configData, err := os.ReadFile(configPath) //nolint:gosec // G304: path from known plan directory
	planFinished := false

	if err == nil {
		if strings.Contains(string(configData), "status: finished") {
			planFinished = true
		}
	}

	if planFinished {
		return
	}

	// Count this as an active plan
	stats.TotalPlans++

	// Read job files
	entries, err := os.ReadDir(planPath)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if entry.Name() == "spec.md" || entry.Name() == "README.md" {
			continue
		}

		jobPath := filepath.Join(planPath, entry.Name())
		file, err := os.Open(jobPath) //nolint:gosec // G304: path from known plan directory
		if err != nil {
			continue
		}

		meta, _ := frontmatter.Parse(file)
		_ = file.Close()

		switch meta.Status {
		case "completed":
			stats.Completed++
		case "running":
			stats.Running++
		case "pending", "pending_user":
			stats.Pending++
		case "failed":
			stats.Failed++
		case "todo":
			stats.Todo++
		case "hold":
			stats.Hold++
		case "abandoned":
			stats.Abandoned++
		}
	}
}
