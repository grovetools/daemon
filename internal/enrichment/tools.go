package enrichment

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	coreconfig "github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/delegation"
)

// groveListEntry represents a single tool from `grove list --json`
type groveListEntry struct {
	Name          string `json:"name"`
	RepoName      string `json:"repo_name"`
	Status        string `json:"status"`
	ActiveVersion string `json:"active_version"`
	LatestRelease string `json:"latest_release"`
}

// FetchToolInfoMap fetches release info and binary status using `grove list --json`.
// Returns maps indexed by workspace path.
func FetchToolInfoMap(nodes []*workspace.WorkspaceNode, fetchRelease, fetchBinary bool) (map[string]*models.ReleaseInfo, map[string]*models.BinaryStatus) {
	releases := make(map[string]*models.ReleaseInfo)
	binaries := make(map[string]*models.BinaryStatus)

	// Run `grove list --json` once to get all tool info
	cmd := delegation.Command("grove", "list", "--json")
	output, err := cmd.Output()
	if err != nil {
		return releases, binaries
	}

	var tools []groveListEntry
	if err := json.Unmarshal(output, &tools); err != nil {
		return releases, binaries
	}

	toolsByRepo := make(map[string]*groveListEntry)
	for i := range tools {
		toolsByRepo[tools[i].RepoName] = &tools[i]
	}

	for _, node := range nodes {
		// Resolve repo name (handle worktrees)
		repoName := filepath.Base(node.Path)
		if node.IsWorktree() && node.ParentProjectPath != "" {
			repoName = filepath.Base(node.ParentProjectPath)
		}

		tool, ok := toolsByRepo[repoName]
		if !ok {
			continue
		}

		if fetchRelease && tool.LatestRelease != "" {
			releases[node.Path] = &models.ReleaseInfo{
				LatestTag:    tool.LatestRelease,
				CommitsAhead: 0, // grove list doesn't provide this
			}
		}

		if fetchBinary {
			// Try to read binary name from project's grove config
			binaryName := tool.Name
			if configPath, err := coreconfig.FindConfigFile(node.Path); err == nil {
				if cfg, err := coreconfig.Load(configPath); err == nil {
					var binaryCfg struct {
						Name string `yaml:"name"`
					}
					if err := cfg.UnmarshalExtension("binary", &binaryCfg); err == nil && binaryCfg.Name != "" {
						binaryName = binaryCfg.Name
					}
				}
			}

			binaries[node.Path] = &models.BinaryStatus{
				ToolName:       binaryName,
				IsDevActive:    tool.Status == "dev",
				LinkName:       "",
				CurrentVersion: tool.ActiveVersion,
			}
		}
	}

	return releases, binaries
}

// cxPerLineStat matches the output of cx stats --per-line
type cxPerLineStat struct {
	Rule        string `json:"rule"`
	FileCount   int    `json:"fileCount"`
	TotalTokens int    `json:"totalTokens"`
	TotalSize   int64  `json:"totalSize"`
}

// FetchCxStatsMap fetches context stats by running cx stats --per-line.
// Returns a map indexed by workspace path.
func FetchCxStatsMap(nodes []*workspace.WorkspaceNode) map[string]*models.CxStats {
	stats := make(map[string]*models.CxStats)

	// Resolve the rules path using the notebook locator, falling back to legacy .grove/rules
	rulesPath := resolveRulesPath(nodes)
	if rulesPath == "" {
		return stats
	}

	// Run cx stats --per-line <resolved-rules-path> --json
	cmd := delegation.Command("cx", "stats", "--per-line", rulesPath, "--json")
	output, err := cmd.Output()
	if err != nil || len(output) == 0 {
		return stats
	}

	trimmed := strings.TrimSpace(string(output))
	if !strings.HasPrefix(trimmed, "[") {
		return stats
	}

	var perLineStats []cxPerLineStat
	if err := json.Unmarshal([]byte(trimmed), &perLineStats); err != nil {
		return stats
	}

	// Build a map of "ecosystem:workspace" -> node for matching
	projectByKey := make(map[string]*workspace.WorkspaceNode)
	for _, n := range nodes {
		ecosystemName := ""
		if n.RootEcosystemPath != "" {
			ecosystemName = filepath.Base(n.RootEcosystemPath)
		}
		key := n.Name
		if ecosystemName != "" {
			key = ecosystemName + ":" + n.Name
		}
		projectByKey[key] = n
	}

	// Match each rule to a project
	for _, lineStat := range perLineStats {
		rule := lineStat.Rule

		// Parse alias patterns like @a:ecosystem:workspace::ruleset
		if !strings.HasPrefix(rule, "@a:") && !strings.HasPrefix(rule, "@alias:") {
			continue
		}

		prefix := "@a:"
		if strings.HasPrefix(rule, "@alias:") {
			prefix = "@alias:"
		}
		rest := strings.TrimPrefix(rule, prefix)

		// Skip git aliases - they're external repos
		if strings.HasPrefix(rest, "git:") {
			continue
		}

		// Strip any modifiers like @grep:, @find:, etc.
		if idx := strings.Index(rest, " @"); idx != -1 {
			rest = rest[:idx]
		}

		// Parse ecosystem:workspace::ruleset
		parts := strings.SplitN(rest, "::", 2)
		if len(parts) < 1 {
			continue
		}

		// parts[0] is "ecosystem:workspace"
		matchKey := parts[0]

		// Find matching project by ecosystem:workspace key
		if node, ok := projectByKey[matchKey]; ok {
			stats[node.Path] = &models.CxStats{
				Files:  lineStat.FileCount,
				Tokens: lineStat.TotalTokens,
				Size:   lineStat.TotalSize,
			}
		}
	}

	return stats
}

// resolveRulesPath finds the rules file path by checking the notebook location first,
// then falling back to the legacy .grove/rules location.
func resolveRulesPath(nodes []*workspace.WorkspaceNode) string {
	coreCfg, err := coreconfig.LoadDefault()
	if err != nil {
		coreCfg = &coreconfig.Config{}
	}
	locator := workspace.NewNotebookLocator(coreCfg)

	for _, node := range nodes {
		// Check notebook location first
		if rulesFile, err := locator.GetContextRulesFile(node); err == nil {
			if _, statErr := os.Stat(rulesFile); statErr == nil {
				return rulesFile
			}
		}

		// Fall back to legacy .grove/rules
		legacyPath := filepath.Join(node.Path, ".grove", "rules")
		if _, err := os.Stat(legacyPath); err == nil {
			return legacyPath
		}
	}

	return ""
}

// GetRemoteURL fetches the git remote URL for a directory.
func GetRemoteURL(dir string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
