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
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/hooks"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/skills/pkg/service"
	"github.com/grovetools/skills/pkg/skills"
	"github.com/sirupsen/logrus"
)

// SkillWatcher monitors skill directories for changes and triggers sync operations.
type SkillWatcher struct {
	store         *store.Store
	svc           *service.Service
	cfg           *config.Config
	hooksExecutor *hooks.Executor
	debounceMs    int

	watcher *fsnotify.Watcher
	log     *logrus.Entry

	// Maps watched path string -> associated WorkspaceNode (nil for global)
	watchedPaths map[string]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// Debounce timers per workspace path
	timers      map[string]*time.Timer
	timersMutex sync.Mutex
}

// NewSkillWatcher creates a new SkillWatcher instance.
func NewSkillWatcher(st *store.Store, cfg *config.Config, debounceMs int) (*SkillWatcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if debounceMs <= 0 {
		debounceMs = 1000
	}

	svc := &service.Service{
		NotebookLocator: workspace.NewNotebookLocator(cfg),
		Config:          cfg,
	}

	return &SkillWatcher{
		store:         st,
		svc:           svc,
		cfg:           cfg,
		hooksExecutor: hooks.NewExecutor(cfg),
		debounceMs:    debounceMs,
		watcher:       fw,
		log:           logging.NewLogger("groved.skills.watcher"),
		watchedPaths:  make(map[string]*workspace.WorkspaceNode),
		timers:        make(map[string]*time.Timer),
	}, nil
}

// Start begins watching skill directories. It runs until the context is canceled.
func (w *SkillWatcher) Start(ctx context.Context) {
	defer w.watcher.Close()

	// Subscribe to store updates to catch config reloads
	updates := w.store.Subscribe()
	defer w.store.Unsubscribe(updates)

	// Handle file system events
	go w.handleEvents(ctx)

	// Handle store updates (config reloads)
	go w.handleStoreUpdates(ctx, updates)

	// Refresh watches periodically to catch new workspaces/directories
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	w.refreshWatches()

	// Perform initial skill sync for all workspaces on startup
	w.log.Info("Performing initial skill sync for all workspaces")
	w.syncAllWorkspaces()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.refreshWatches()
		}
	}
}

// handleStoreUpdates listens for config reload events and triggers skill syncs
func (w *SkillWatcher) handleStoreUpdates(ctx context.Context, updates chan store.Update) {
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Type == store.UpdateConfigReload {
				w.log.Info("Config reload detected, reloading config and syncing all skills")

				// Reload config to pick up changes
				newCfg, err := config.LoadDefault()
				if err != nil {
					w.log.WithError(err).Error("Failed to reload config")
					continue
				}
				w.cfg = newCfg
				w.svc.Config = newCfg
				w.svc.NotebookLocator = workspace.NewNotebookLocator(newCfg)
				w.hooksExecutor.UpdateConfig(newCfg)

				w.syncAllWorkspaces()
			}
		}
	}
}

// syncAllWorkspaces syncs skills for all workspaces.
// This includes workspaces with no skills configured, since they may have
// previously had skills that need to be cleaned up.
func (w *SkillWatcher) syncAllWorkspaces() {
	workspaces := w.store.GetWorkspaces()
	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		// Sync all workspaces - SyncForNode handles cleanup for workspaces
		// that no longer have skills configured
		w.syncWorkspace(node)
	}
}

func (w *SkillWatcher) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			// Ignore chmod
			if event.Op&fsnotify.Chmod == fsnotify.Chmod {
				continue
			}

			// Check if this is a grove.toml change in a workspace
			if filepath.Base(event.Name) == "grove.toml" {
				w.handleConfigChange(ctx, event.Name)
				continue
			}

			w.triggerSync(ctx, event.Name)

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.log.WithError(err).Error("fsnotify watcher error")
		}
	}
}

// handleConfigChange handles grove.toml changes in workspaces
func (w *SkillWatcher) handleConfigChange(ctx context.Context, configPath string) {
	configDir := filepath.Dir(configPath)

	w.pathsMutex.RLock()
	node, found := w.watchedPaths[configDir]
	w.pathsMutex.RUnlock()

	if !found || node == nil {
		return
	}

	w.log.WithFields(logrus.Fields{
		"workspace": node.Name,
		"config":    configPath,
	}).Info("Workspace config changed, syncing skills")

	w.syncWorkspace(node)
}

func (w *SkillWatcher) triggerSync(ctx context.Context, changedPath string) {
	w.pathsMutex.RLock()
	var matchedWatchPath string
	found := false

	// Check if this change is in a watched path
	for watchedPath := range w.watchedPaths {
		if changedPath == watchedPath || filepath.HasPrefix(changedPath, watchedPath+string(filepath.Separator)) {
			matchedWatchPath = watchedPath
			found = true
			break
		}
	}
	w.pathsMutex.RUnlock()

	if !found {
		return
	}

	// Extract skill name from the changed path
	// Path format: .../skills/<skill-name>/... or .../skills/<skill-name>
	changedSkillName := extractSkillName(changedPath, matchedWatchPath)

	w.log.WithFields(logrus.Fields{
		"changedPath": changedPath,
		"skillName":   changedSkillName,
	}).Debug("Skill file changed, checking affected workspaces")

	// Find ALL workspaces that use this skill
	var nodesToSync []*workspace.WorkspaceNode
	workspaces := w.store.GetWorkspaces()

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		// Check if this workspace uses the changed skill
		skillsCfg, err := skills.LoadSkillsConfig(w.cfg, node)
		if err != nil || skillsCfg == nil {
			continue
		}

		// Check if the skill is in the Use list or Dependencies
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

	w.log.WithField("count", len(nodesToSync)).Debug("Workspaces to sync")

	// Sync each affected workspace with debouncing
	for _, node := range nodesToSync {
		w.syncWorkspace(node)
	}
}

// extractSkillName extracts the skill name from a changed file path
// Given a path like /path/to/skills/my-skill/SKILL.md and watchPath /path/to/skills
// it returns "my-skill"
func extractSkillName(changedPath, watchPath string) string {
	// Get the relative path from the watch path
	rel, err := filepath.Rel(watchPath, changedPath)
	if err != nil {
		return ""
	}

	// The first component is the skill name
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func (w *SkillWatcher) syncWorkspace(node *workspace.WorkspaceNode) {
	debounceKey := node.Path

	w.timersMutex.Lock()
	defer w.timersMutex.Unlock()

	if timer, exists := w.timers[debounceKey]; exists {
		timer.Stop()
	}

	// Capture node and executor in closure
	targetNode := node
	executor := w.hooksExecutor
	w.timers[debounceKey] = time.AfterFunc(time.Duration(w.debounceMs)*time.Millisecond, func() {
		w.log.WithField("target", debounceKey).Info("Executing skill sync")

		opts := skills.SyncOptions{Prune: true, DryRun: false}
		result, err := skills.SyncWorkspace(w.svc, targetNode, opts, nil)

		payload := store.SkillSyncPayload{
			Workspace:    result.Workspace,
			SyncedSkills: result.SyncedSkills,
			DestPaths:    result.DestPaths,
		}

		if err != nil {
			w.log.WithError(err).Error("Skill sync failed")
			payload.Error = err.Error()
		}

		// Broadcast the sync result to all subscribers
		w.store.ApplyUpdate(store.Update{
			Type:    store.UpdateSkillSync,
			Source:  "skills",
			Payload: payload,
		})

		// Execute on_skill_sync hooks
		changed := len(result.SyncedSkills) > 0
		executor.ExecuteOnSkillSync(context.Background(), targetNode.Path, result.SyncedSkills, changed)
	})
}

func (w *SkillWatcher) refreshWatches() {
	newWatches := make(map[string]*workspace.WorkspaceNode)

	// 1. Global user skills directory
	home, _ := os.UserHomeDir()
	globalDir := filepath.Join(home, ".config", "grove", "skills")
	w.addWatchRecursive(globalDir, nil, newWatches)

	// 2. Workspace skill directories AND workspace config files
	workspaces := w.store.GetWorkspaces()
	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}
		if skillsDir, err := w.svc.NotebookLocator.GetSkillsDir(node); err == nil && skillsDir != "" {
			w.addWatchRecursive(skillsDir, node, newWatches)
		}

		// 3. Watch workspace directory for grove.toml changes
		// This allows us to detect when skills config changes at the workspace level
		if node.Path != "" {
			newWatches[node.Path] = node
		}
	}

	w.pathsMutex.Lock()
	defer w.pathsMutex.Unlock()

	// Add new watches
	for path, node := range newWatches {
		if _, exists := w.watchedPaths[path]; !exists {
			if err := w.watcher.Add(path); err == nil {
				w.watchedPaths[path] = node
				w.log.WithField("path", path).Debug("Added skill watch")
			}
		}
	}

	// Remove obsolete watches
	for path := range w.watchedPaths {
		if _, exists := newWatches[path]; !exists {
			_ = w.watcher.Remove(path)
			delete(w.watchedPaths, path)
			w.log.WithField("path", path).Debug("Removed skill watch")
		}
	}
}

// addWatchRecursive adds the base directory and immediate subdirectories (where SKILL.md lives)
func (w *SkillWatcher) addWatchRecursive(baseDir string, node *workspace.WorkspaceNode, tracker map[string]*workspace.WorkspaceNode) {
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
