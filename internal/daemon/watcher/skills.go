package watcher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/hooks"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/skills/pkg/service"
	"github.com/grovetools/skills/pkg/skills"
	"github.com/sirupsen/logrus"
)

// SkillHandler implements DomainHandler for skill file watching and sync.
// It replaces the old standalone SkillWatcher by plugging into the UnifiedWatcher.
type SkillHandler struct {
	store         *store.Store
	svc           *service.Service
	cfg           *config.Config
	hooksExecutor *hooks.Executor
	debounceMs    int

	log *logrus.Entry

	// Maps watched path -> associated WorkspaceNode (nil for global paths)
	watchedPaths map[string]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// Debounce timers per workspace path
	timers      map[string]*time.Timer
	timersMutex sync.Mutex

	// Cached skills config per workspace path for change detection
	cachedConfigs map[string]*skills.SkillsConfig
	configsMutex  sync.RWMutex

	// Cached global skills config for change detection on config reloads
	globalSkillsCfg *skills.SkillsConfig
}

// NewSkillHandler creates a new SkillHandler instance.
func NewSkillHandler(st *store.Store, cfg *config.Config, debounceMs int) (*SkillHandler, error) {
	if debounceMs <= 0 {
		debounceMs = 1000
	}

	svc := &service.Service{
		NotebookLocator: workspace.NewNotebookLocator(cfg),
		Config:          cfg,
	}

	return &SkillHandler{
		store:           st,
		svc:             svc,
		cfg:             cfg,
		hooksExecutor:   hooks.NewExecutor(cfg),
		debounceMs:      debounceMs,
		log:             logging.NewLogger("groved.skills.watcher"),
		watchedPaths:    make(map[string]*workspace.WorkspaceNode),
		timers:          make(map[string]*time.Timer),
		cachedConfigs:   make(map[string]*skills.SkillsConfig),
		globalSkillsCfg: skills.LoadGlobalSkillsConfig(cfg),
	}, nil
}

// Name returns the handler's domain name.
func (h *SkillHandler) Name() string {
	return "skills"
}

// ComputeWatchPaths returns all filesystem paths that need watching for skill changes.
// This includes global skill directories, workspace skill directories, and workspace
// roots (for grove.toml changes). It also updates the internal watchedPaths mapping
// used by MatchesEvent and HandleEvents.
func (h *SkillHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*workspace.WorkspaceNode)

	// 1. Global user skills directory
	home, _ := os.UserHomeDir()
	globalDir := filepath.Join(home, ".config", "grove", "skills")
	addWatchRecursive(globalDir, nil, newWatches)

	// 2. Workspace skill directories AND workspace config files
	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}
		if skillsDir, err := h.svc.NotebookLocator.GetSkillsDir(node); err == nil && skillsDir != "" {
			addWatchRecursive(skillsDir, node, newWatches)
		}

		// 3. Watch workspace directory for grove.toml changes
		if node.Path != "" {
			if _, err := os.Stat(node.Path); err == nil {
				newWatches[node.Path] = node
			}
		}
	}

	// Update internal path->node mapping
	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.pathsMutex.Unlock()

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}
	return paths
}

// MatchesEvent returns true if the filesystem event belongs to this handler.
func (h *SkillHandler) MatchesEvent(event fsnotify.Event) bool {
	// Ignore chmod events
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

// HandleEvents processes a batch of filesystem events, routing them to the
// appropriate sync logic (grove.toml changes vs skill file changes).
func (h *SkillHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	for _, event := range events {
		if filepath.Base(event.Name) == "grove.toml" {
			h.handleConfigChange(event.Name)
		} else {
			h.triggerSync(event.Name)
		}
	}
	return nil
}

// HandleStoreUpdate responds to store-level updates like config reloads and workspace discovery.
func (h *SkillHandler) HandleStoreUpdate(update store.Update) {
	switch update.Type {
	case store.UpdateConfigReload:
		h.handleConfigReload()
	case store.UpdateWorkspaces:
		h.handleWorkspacesDiscovered()
	}
}

// handleConfigReload reloads the global config and syncs all workspaces if skills config changed.
func (h *SkillHandler) handleConfigReload() {
	newCfg, err := config.LoadDefault()
	if err != nil {
		h.log.WithError(err).Error("Failed to reload config")
		return
	}

	// Check if global skills configuration actually changed
	newGlobalSkillsCfg := skills.LoadGlobalSkillsConfig(newCfg)
	if reflect.DeepEqual(h.globalSkillsCfg, newGlobalSkillsCfg) {
		h.log.Debug("Global skills config unchanged, skipping sync")
		// Still update internal references
		h.cfg = newCfg
		h.svc.Config = newCfg
		h.svc.NotebookLocator = workspace.NewNotebookLocator(newCfg)
		h.hooksExecutor.UpdateConfig(newCfg)
		return
	}

	h.log.Info("Skills config reload detected, syncing all skills")
	h.globalSkillsCfg = newGlobalSkillsCfg
	h.cfg = newCfg
	h.svc.Config = newCfg
	h.svc.NotebookLocator = workspace.NewNotebookLocator(newCfg)
	h.hooksExecutor.UpdateConfig(newCfg)

	h.syncAllWorkspaces()
}

// handleWorkspacesDiscovered syncs skills for newly discovered workspaces.
// On first discovery (no cached configs yet), syncs all workspaces.
// On subsequent discoveries, only syncs workspaces not yet seen.
func (h *SkillHandler) handleWorkspacesDiscovered() {
	workspaces := h.store.GetWorkspaces()

	h.configsMutex.RLock()
	cachedCount := len(h.cachedConfigs)
	h.configsMutex.RUnlock()

	if cachedCount == 0 {
		h.log.WithField("count", len(workspaces)).Info("Workspaces discovered, performing initial skill sync")
		h.syncAllWorkspaces()
		return
	}

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		h.configsMutex.RLock()
		_, seen := h.cachedConfigs[node.Path]
		h.configsMutex.RUnlock()

		if !seen {
			h.log.WithField("workspace", node.Name).Info("New workspace discovered, syncing skills")
			h.syncWorkspace(node)
		}
	}
}

// OnStart performs the initial skill sync for all workspaces.
// Note: At startup, the workspace store may not be populated yet.
// If no workspaces are available, the actual initial sync will happen
// when the first UpdateWorkspaces event arrives via handleWorkspacesDiscovered.
func (h *SkillHandler) OnStart(ctx context.Context) {
	workspaces := h.store.GetWorkspaces()
	if len(workspaces) == 0 {
		h.log.Debug("No workspaces available yet at startup, deferring initial sync to workspace discovery")
		return
	}
	h.log.Info("Performing initial skill sync for all workspaces")
	h.syncAllWorkspaces()
}

// handleConfigChange handles grove.toml changes in workspaces.
// Only triggers a skill sync if the skills-related config actually changed.
func (h *SkillHandler) handleConfigChange(configPath string) {
	configDir := filepath.Dir(configPath)

	h.pathsMutex.RLock()
	node, found := h.watchedPaths[configDir]
	h.pathsMutex.RUnlock()

	if !found || node == nil {
		return
	}

	// Load the new skills config and compare with cached version
	newCfg, err := skills.LoadSkillsConfig(h.cfg, node)
	if err != nil {
		h.log.WithError(err).WithField("workspace", node.Name).Debug("Failed to load skills config for change detection")
		// Fall through to sync on error — safer than skipping
	} else {
		h.configsMutex.RLock()
		prev := h.cachedConfigs[node.Path]
		h.configsMutex.RUnlock()

		if reflect.DeepEqual(prev, newCfg) {
			h.log.WithField("workspace", node.Name).Debug("Skills config unchanged in grove.toml, skipping sync")
			return
		}
	}

	h.log.WithFields(logrus.Fields{
		"workspace": node.Name,
		"config":    configPath,
	}).Info("Skills config changed, syncing skills")

	h.syncWorkspace(node)
}

// triggerSync finds affected workspaces for a changed skill file and syncs them.
func (h *SkillHandler) triggerSync(changedPath string) {
	h.pathsMutex.RLock()
	var matchedWatchPath string
	found := false

	for watchedPath := range h.watchedPaths {
		if changedPath == watchedPath || filepath.HasPrefix(changedPath, watchedPath+string(filepath.Separator)) {
			matchedWatchPath = watchedPath
			found = true
			break
		}
	}
	h.pathsMutex.RUnlock()

	if !found {
		return
	}

	changedSkillName := extractSkillName(changedPath, matchedWatchPath)

	h.log.WithFields(logrus.Fields{
		"changedPath": changedPath,
		"skillName":   changedSkillName,
	}).Debug("Skill file changed, checking affected workspaces")

	var nodesToSync []*workspace.WorkspaceNode
	workspaces := h.store.GetWorkspaces()

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		skillsCfg, err := skills.LoadSkillsConfig(h.cfg, node)
		if err != nil || skillsCfg == nil {
			continue
		}

		usesSkill := false
		for _, s := range skillsCfg.Use {
			if s == changedSkillName {
				usesSkill = true
				break
			}
		}
		if !usesSkill {
			if _, exists := skillsCfg.Dependencies[changedSkillName]; exists {
				usesSkill = true
			}
		}

		if usesSkill {
			nodesToSync = append(nodesToSync, node)
		}
	}

	h.log.WithField("count", len(nodesToSync)).Debug("Workspaces to sync")

	for _, node := range nodesToSync {
		h.syncWorkspace(node)
	}
}

// syncAllWorkspaces syncs skills for all workspaces.
func (h *SkillHandler) syncAllWorkspaces() {
	workspaces := h.store.GetWorkspaces()
	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}
		h.syncWorkspace(node)
	}
}

// syncWorkspace triggers a debounced skill sync for a single workspace.
func (h *SkillHandler) syncWorkspace(node *workspace.WorkspaceNode) {
	debounceKey := node.Path

	h.timersMutex.Lock()
	defer h.timersMutex.Unlock()

	if timer, exists := h.timers[debounceKey]; exists {
		timer.Stop()
	}

	targetNode := node
	executor := h.hooksExecutor
	h.timers[debounceKey] = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		// Skip workspaces whose paths no longer exist (deleted worktrees, removed repos)
		if _, err := os.Stat(targetNode.Path); err != nil {
			h.log.WithFields(logrus.Fields{
				"workspace": targetNode.Name,
				"path":      targetNode.Path,
			}).Debug("Skipping skill sync for workspace with missing path")
			return
		}

		h.log.WithField("target", debounceKey).Info("Executing skill sync")

		// Cache the current skills config for change detection on future grove.toml saves
		if cfg, loadErr := skills.LoadSkillsConfig(h.cfg, targetNode); loadErr == nil {
			h.configsMutex.Lock()
			h.cachedConfigs[targetNode.Path] = cfg
			h.configsMutex.Unlock()
		}

		opts := skills.SyncOptions{Prune: true, DryRun: false}
		result, err := skills.SyncWorkspace(h.svc, targetNode, opts, nil)

		payload := store.SkillSyncPayload{
			Workspace:    result.Workspace,
			SyncedSkills: result.SyncedSkills,
			DestPaths:    result.DestPaths,
		}

		if err != nil {
			h.log.WithError(err).Error("Skill sync failed")
			payload.Error = err.Error()
		}

		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdateSkillSync,
			Source:  "skills",
			Payload: payload,
		})

		h.log.WithFields(logrus.Fields{
			"workspace": targetNode.Name,
			"synced":    len(result.SyncedSkills),
		}).Debug("Skill sync completed")

		changed := len(result.SyncedSkills) > 0
		executor.ExecuteOnSkillSync(context.Background(), targetNode.Path, result.SyncedSkills, changed)
	})
}

// extractSkillName extracts the skill name from a changed file path.
// Given a path like /path/to/skills/my-skill/SKILL.md and watchPath /path/to/skills
// it returns "my-skill".
func extractSkillName(changedPath, watchPath string) string {
	rel, err := filepath.Rel(watchPath, changedPath)
	if err != nil {
		return ""
	}

	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// addWatchRecursive adds the base directory and immediate subdirectories to the tracker.
func addWatchRecursive(baseDir string, node *workspace.WorkspaceNode, tracker map[string]*workspace.WorkspaceNode) {
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		return
	}

	tracker[baseDir] = node
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			tracker[filepath.Join(baseDir, entry.Name())] = node
		}
	}
}
