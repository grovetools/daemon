package enrichment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	coreconfig "github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/core/util/frontmatter"
	"github.com/sirupsen/logrus"
)

// FetchPlanStatsMap fetches plan statistics for all workspaces.
// It uses a lightweight frontmatter parser to avoid importing flow packages.
func FetchPlanStatsMap() (map[string]*models.PlanStats, error) {
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	discoveryService := workspace.NewDiscoveryService(logger)
	discoveryResult, err := discoveryService.DiscoverAll()
	if err != nil {
		return nil, fmt.Errorf("workspace discovery failed: %w", err)
	}
	provider := workspace.NewProvider(discoveryResult)

	coreCfg, err := coreconfig.LoadDefault()
	if err != nil {
		coreCfg = &coreconfig.Config{}
	}
	locator := workspace.NewNotebookLocator(coreCfg)

	nodes := provider.All()
	statsByPath := resolvePerNodePlanStats(
		nodes,
		func(node *workspace.WorkspaceNode) (string, error) { return locator.GetPlansDir(node) },
		countPlanStats,
		func(node *workspace.WorkspaceNode) string { return getActivePlanForPath(node, locator) },
		planStatusForNode,
	)
	// PlanStatus is meaningful only with an explicit registry association.
	// Stamp that qualified identity separately from ActivePlan, whose legacy
	// state fallback is intentionally broader.
	for _, node := range nodes {
		if node == nil || !node.IsWorktree() {
			continue
		}
		planName, ok := worktreeregistry.PlanForPath(node.Path)
		if !ok {
			continue
		}
		if stats := statsByPath[node.Path]; stats != nil {
			stats.AssociatedPlan = planName
			if plansDir, err := locator.GetPlansDir(node); err == nil {
				stats.AssociatedPlanDir = filepath.Join(plansDir, planName)
			}
		}
	}

	return statsByPath, nil
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

// planStatusForNode reads only the plan explicitly associated with this exact
// worktree container in the registry. It never infers ownership from a shared
// worktree name or from the parent workspace's aggregate plan stats.
func planStatusForNode(plansRootDir string, node *workspace.WorkspaceNode) string {
	if node == nil || !node.IsWorktree() {
		return ""
	}
	planName, ok := worktreeregistry.PlanForPath(node.Path)
	if !ok {
		return ""
	}
	planDir := filepath.Join(plansRootDir, planName)
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

// getActivePlanForPath reads the active plan from a workspace's state file.
// It checks the notebook directory first (sibling of plans dir), then falls back
// to the legacy .grove/state.yml location.
func getActivePlanForPath(node *workspace.WorkspaceNode, locator *workspace.NotebookLocator) string {
	// Registry-first: for a worktree node the notebook locator redirects to the
	// main project's notebook, so reading flow.active_plan from the notebook-sibling
	// state.yml would return the main checkout's plan. The XDG worktree registry
	// holds the worktree's own plan keyed by its container path, so prefer it.
	if activePlan, ok := worktreeregistry.PlanForPath(node.Path); ok {
		return activePlan
	}

	stateFilePath := filepath.Join(node.Path, ".grove", "state.yml")

	// Try notebook location first: state.yml lives alongside plans/ in the notebook dir
	if plansDir, err := locator.GetPlansDir(node); err == nil {
		nbStatePath := filepath.Join(filepath.Dir(plansDir), "state.yml")
		if _, statErr := os.Stat(nbStatePath); statErr == nil {
			stateFilePath = nbStatePath
		}
	}

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
