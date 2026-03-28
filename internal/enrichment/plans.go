package enrichment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	coreconfig "github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/frontmatter"
	"github.com/sirupsen/logrus"
)

// FetchPlanStatsMap fetches plan statistics for all workspaces.
// It uses a lightweight frontmatter parser to avoid importing flow packages.
func FetchPlanStatsMap() (map[string]*models.PlanStats, error) {
	statsByPath := make(map[string]*models.PlanStats)
	seenDirs := make(map[string]*models.PlanStats)

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

	for _, node := range provider.All() {
		plansRootDir, err := locator.GetPlansDir(node)
		if err != nil {
			statsByPath[node.Path] = &models.PlanStats{}
			continue
		}

		if cachedStats, seen := seenDirs[plansRootDir]; seen {
			statsByPath[node.Path] = cachedStats
			continue
		}

		stats := &models.PlanStats{}
		statsByPath[node.Path] = stats
		seenDirs[plansRootDir] = stats

		entries, err := os.ReadDir(plansRootDir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}

			planPath := filepath.Join(plansRootDir, entry.Name())
			processPlanDir(planPath, stats, node)
		}

		if activePlan := getActivePlanForPath(node, locator); activePlan != "" {
			stats.ActivePlan = activePlan
		}
	}

	return statsByPath, nil
}

// processPlanDir scans a plan directory and aggregates stats
func processPlanDir(planPath string, stats *models.PlanStats, node *workspace.WorkspaceNode) {
	// Read plan config to check status
	configPath := filepath.Join(planPath, "config.yml")
	configData, err := os.ReadFile(configPath)
	planFinished := false
	var planWorktree string

	if err == nil {
		content := string(configData)
		if strings.Contains(content, "status: finished") {
			planFinished = true
		}
		// Extract worktree field
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(line, "worktree:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					planWorktree = strings.TrimSpace(parts[1])
				}
				break
			}
		}

		// If this is a worktree node and plan's worktree matches, set PlanStatus
		if node.IsWorktree() && planWorktree != "" {
			worktreeName := node.GetWorktreeName()
			if worktreeName != "" && planWorktree == worktreeName {
				// Extract status from config
				for _, line := range strings.Split(content, "\n") {
					if strings.HasPrefix(line, "status:") {
						parts := strings.SplitN(line, ":", 2)
						if len(parts) == 2 {
							stats.PlanStatus = strings.TrimSpace(parts[1])
						}
						break
					}
				}
			}
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
		file, err := os.Open(jobPath)
		if err != nil {
			continue
		}

		meta, _ := frontmatter.Parse(file)
		file.Close()

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
	stateFilePath := filepath.Join(node.Path, ".grove", "state.yml")

	// Try notebook location first: state.yml lives alongside plans/ in the notebook dir
	if plansDir, err := locator.GetPlansDir(node); err == nil {
		nbStatePath := filepath.Join(filepath.Dir(plansDir), "state.yml")
		if _, statErr := os.Stat(nbStatePath); statErr == nil {
			stateFilePath = nbStatePath
		}
	}

	data, err := os.ReadFile(stateFilePath)
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
	if val, ok := stateMap["flow.active_plan"].(string); ok {
		return val
	}
	if val, ok := stateMap["active_plan"].(string); ok {
		return val
	}
	return ""
}
