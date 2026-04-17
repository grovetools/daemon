package watcher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
)

// SkillHandler implements DomainHandler for skill file watching and sync.
// It replaces the old standalone SkillWatcher by plugging into the UnifiedWatcher.
type SkillHandler struct {
	store         *store.Store
	svc           *service.Service
	cfg           *config.Config
	hooksExecutor *hooks.Executor
	debounceMs    int

	ulog *logging.UnifiedLogger

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
		ulog:            logging.NewUnifiedLogger("groved.watcher.skills"),
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

		// Watch skill directories inside each playbook bundle, so SKILL.md
		// edits inside playbooks/<name>/skills/ trigger the same sync pipeline
		// as standalone skills.
		if playbooksDir, err := h.svc.NotebookLocator.GetPlaybooksDir(node); err == nil && playbooksDir != "" {
			if entries, err := os.ReadDir(playbooksDir); err == nil {
				for _, entry := range entries {
					if !entry.IsDir() {
						continue
					}
					pbSkills := filepath.Join(playbooksDir, entry.Name(), "skills")
					addWatchRecursive(pbSkills, node, newWatches)
				}
			}
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
//
// Skill file events are coalesced: rather than running the full workspace
// graph resolution once per event (which turned a 50-file git checkout into
// 50 × N_workspaces passes of ResolveConfiguredSkills), we extract the unique
// changed skill names from the batch and resolve each workspace a single
// time, checking all changed skills against its transitive graph in one pass.
func (h *SkillHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	changedSkills := make(map[string]struct{})
	for _, event := range events {
		if filepath.Base(event.Name) == "grove.toml" {
			h.handleConfigChange(event.Name)
			continue
		}

		name, ok := h.skillNameForPath(event.Name)
		if !ok || name == "" {
			continue
		}
		changedSkills[name] = struct{}{}
	}

	if len(changedSkills) > 0 {
		h.triggerSync(changedSkills)
	}
	return nil
}

// skillNameForPath maps a changed filesystem path to its owning skill name by
// locating the matching watch path and extracting the skill directory. Returns
// false if the path is not under any currently-watched skill directory.
func (h *SkillHandler) skillNameForPath(changedPath string) (string, bool) {
	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watchedPath := range h.watchedPaths {
		if changedPath == watchedPath || filepath.HasPrefix(changedPath, watchedPath+string(filepath.Separator)) {
			return extractSkillName(changedPath, watchedPath), true
		}
	}
	return "", false
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
	ctx := context.Background()
	newCfg, err := config.LoadDefault()
	if err != nil {
		h.ulog.Error("Failed to reload config").Err(err).Log(ctx)
		return
	}

	// Check if global skills configuration actually changed
	newGlobalSkillsCfg := skills.LoadGlobalSkillsConfig(newCfg)
	if reflect.DeepEqual(h.globalSkillsCfg, newGlobalSkillsCfg) {
		h.ulog.Debug("Global skills config unchanged, skipping sync").Log(ctx)
		// Still update internal references
		h.cfg = newCfg
		h.svc.Config = newCfg
		h.svc.NotebookLocator = workspace.NewNotebookLocator(newCfg)
		h.hooksExecutor.UpdateConfig(newCfg)
		return
	}

	h.ulog.Info("Skills config reload detected, syncing all skills").Log(ctx)
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
	ctx := context.Background()
	workspaces := h.store.GetWorkspaces()

	h.configsMutex.RLock()
	cachedCount := len(h.cachedConfigs)
	h.configsMutex.RUnlock()

	if cachedCount == 0 {
		h.ulog.Info("Workspaces discovered, performing initial skill sync").
			Field("count", len(workspaces)).
			Log(ctx)
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
			h.ulog.Debug("New workspace discovered, syncing skills").
				Field("workspace", node.Name).
				Log(ctx)
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
		h.ulog.Debug("No workspaces available yet at startup, deferring initial sync to workspace discovery").Log(ctx)
		return
	}
	h.ulog.Info("Performing initial skill sync for all workspaces").Log(ctx)
	h.syncAllWorkspaces()
}

// handleConfigChange handles grove.toml changes in workspaces.
// Only triggers a skill sync if the skills-related config actually changed.
func (h *SkillHandler) handleConfigChange(configPath string) {
	ctx := context.Background()
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
		h.ulog.Debug("Failed to load skills config for change detection").
			Err(err).
			Field("workspace", node.Name).
			Log(ctx)
		// Fall through to sync on error — safer than skipping
	} else {
		h.configsMutex.RLock()
		prev := h.cachedConfigs[node.Path]
		h.configsMutex.RUnlock()

		if reflect.DeepEqual(prev, newCfg) {
			h.ulog.Debug("Skills config unchanged in grove.toml, skipping sync").
				Field("workspace", node.Name).
				Log(ctx)
			return
		}
	}

	h.ulog.Info("Skills config changed, syncing skills").
		Field("workspace", node.Name).
		Field("config", configPath).
		Log(ctx)

	h.syncWorkspace(node)
}

// triggerSync finds affected workspaces for a batch of changed skill names
// and syncs them. Uses the full resolved dependency graph to detect changes
// in nested/transitive skills. The workspace loop runs a single graph
// resolution per workspace regardless of how many skills changed, so a batch
// of N events against W workspaces costs O(W) resolves instead of O(N × W).
func (h *SkillHandler) triggerSync(changedSkills map[string]struct{}) {
	if len(changedSkills) == 0 {
		return
	}

	ctx := context.Background()
	h.ulog.Debug("Skill files changed, checking affected workspaces").
		Field("changedSkills", len(changedSkills)).
		Log(ctx)

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

		// Use the resolver to check if any of the changed skills are anywhere
		// in the transitive graph. A single resolve covers the whole batch.
		resolved, err := skills.ResolveConfiguredSkills(h.svc, node, skillsCfg)
		if err != nil {
			h.ulog.Debug("Best effort resolving skills failed").Err(err).Log(ctx)
		}

		if resolved == nil {
			continue
		}
		for changedSkillName := range changedSkills {
			if _, exists := resolved[changedSkillName]; exists {
				nodesToSync = append(nodesToSync, node)
				break
			}
		}
	}

	h.ulog.Debug("Workspaces to sync").Field("count", len(nodesToSync)).Log(ctx)

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
		// Skip workspaces whose paths no longer exist (deleted worktrees, removed repos).
		// This is a transient, expected state during worktree teardown when fsnotify
		// is still flushing events — no need to log.
		if _, err := os.Stat(targetNode.Path); err != nil {
			return
		}

		ctx := context.Background()
		h.ulog.Info("Executing skill sync").Field("target", debounceKey).Log(ctx)

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
			payload.Error = err.Error()
		}

		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdateSkillSync,
			Source:  "skills",
			Payload: payload,
		})

		switch {
		case err != nil:
			h.ulog.Error("Skill sync failed").
				Err(err).
				Field("workspace", targetNode.Name).
				Log(ctx)
		case len(result.SyncedSkills) > 0:
			h.ulog.Info("Skill sync completed").
				Field("workspace", targetNode.Name).
				Field("synced", len(result.SyncedSkills)).
				Field("dest_paths", result.DestPaths).
				Log(ctx)
		}

		changed := len(result.SyncedSkills) > 0
		executor.ExecuteOnSkillSync(context.Background(), targetNode.Path, result.SyncedSkills, changed)
	})
}

// extractSkillName extracts the skill name from a changed file path.
// It ascends the directory tree from the changed file until it finds a
// directory containing SKILL.md, and returns that directory's base name.
// This correctly handles nested resources like `references/` or `scripts/`
// inside a skill directory, which the previous implementation mis-attributed
// to the closest parent instead of the skill root.
//
// For /path/to/skills/kitchen/prep/SKILL.md, returns "prep".
// For /path/to/skills/kitchen/prep/references/example.md, returns "prep".
// For /path/to/skills/my-skill/SKILL.md, returns "my-skill".
func extractSkillName(changedPath, watchPath string) string {
	rel, err := filepath.Rel(watchPath, changedPath)
	if err != nil || rel == "." {
		return ""
	}

	// Ascend from the changed file's parent directory until we find a
	// directory containing SKILL.md, or we hit the watch root.
	dir := filepath.Dir(changedPath)
	for dir != watchPath && dir != "." && dir != "/" {
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
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
