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
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// SettingsHandler implements DomainHandler for Claude Code settings reconciliation.
// It is a sibling of SkillHandler (daemon/internal/daemon/watcher/skills.go): it
// shares the same trigger surface (grove.toml changes, config reloads, workspace
// discovery) and the same per-workspace debounce pattern, but it carries no
// skill-graph logic. Instead, on each debounced trigger it reconciles the
// affected worktrees' .claude/settings.local.json DIRECTLY against the
// filesystem via workspace.SeedClaudeSettingsForWorktree — the [claude] config
// is NOT routed through daemon/internal/daemon/store.
type SettingsHandler struct {
	store      *store.Store
	cfg        *config.Config
	debounceMs int

	ulog *logging.UnifiedLogger

	// Maps watched path -> associated WorkspaceNode (nil for global paths).
	// Used by MatchesEvent and HandleEvents to attribute grove.toml events.
	watchedPaths map[string]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// Debounce timers per workspace path (the empty-string key coalesces the
	// "reconcile all worktrees" passes triggered by store updates).
	timers      map[string]*time.Timer
	timersMutex sync.Mutex
}

// NewSettingsHandler creates a new SettingsHandler instance. It mirrors the
// constructor style of NewSkillHandler.
func NewSettingsHandler(st *store.Store, cfg *config.Config, debounceMs int) (*SettingsHandler, error) {
	if debounceMs <= 0 {
		debounceMs = 1000
	}

	return &SettingsHandler{
		store:        st,
		cfg:          cfg,
		debounceMs:   debounceMs,
		ulog:         logging.NewUnifiedLogger("groved.watcher.claude_settings"),
		watchedPaths: make(map[string]*workspace.WorkspaceNode),
		timers:       make(map[string]*time.Timer),
	}, nil
}

// Name returns the handler's domain name.
func (h *SettingsHandler) Name() string {
	return "claude_settings"
}

// ComputeWatchPaths returns the filesystem paths to watch for settings changes.
// We only care about workspace roots (for grove.toml changes); the store's
// workspace list already includes worktree nodes, so watching each node.Path
// covers per-worktree grove.toml edits as well.
func (h *SettingsHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*workspace.WorkspaceNode)

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil || node.Path == "" {
			continue
		}
		if _, err := os.Stat(node.Path); err == nil {
			newWatches[node.Path] = node
		}
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

// MatchesEvent returns true if the filesystem event belongs to this handler.
func (h *SettingsHandler) MatchesEvent(event fsnotify.Event) bool {
	// Ignore chmod events.
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}

	// We only react to grove.toml changes.
	if filepath.Base(event.Name) != "grove.toml" {
		return false
	}

	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watchedPath := range h.watchedPaths {
		if event.Name == watchedPath || strings.HasPrefix(event.Name, watchedPath+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// HandleEvents processes a batch of filesystem events. Only grove.toml changes
// are relevant: each one schedules a debounced reconcile of the owning
// workspace's worktrees.
func (h *SettingsHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	for _, event := range events {
		if filepath.Base(event.Name) != "grove.toml" {
			continue
		}
		h.handleConfigChange(event.Name)
	}
	return nil
}

// HandleStoreUpdate responds to store-level updates like config reloads and
// workspace discovery, scheduling a debounced reconcile of all worktrees.
func (h *SettingsHandler) HandleStoreUpdate(update store.Update) {
	switch update.Type {
	case store.UpdateConfigReload:
		h.handleConfigReload()
	case store.UpdateWorkspaces:
		h.scheduleReconcile("", "")
	}
}

// OnStart performs the initial settings reconcile for all worktrees. If the
// store is not populated yet, the first UpdateWorkspaces event will trigger it.
func (h *SettingsHandler) OnStart(ctx context.Context) {
	workspaces := h.store.GetWorkspaces()
	if len(workspaces) == 0 {
		h.ulog.Debug("No workspaces available yet at startup, deferring initial settings reconcile").Log(ctx)
		return
	}
	h.ulog.Info("Performing initial Claude settings reconcile for all worktrees").Log(ctx)
	h.scheduleReconcile("", "")
}

// handleConfigReload reloads the global config and re-reconciles all worktrees.
func (h *SettingsHandler) handleConfigReload() {
	ctx := context.Background()
	newCfg, err := config.LoadDefault()
	if err != nil {
		h.ulog.Error("Failed to reload config").Err(err).Log(ctx)
		return
	}
	h.cfg = newCfg
	h.ulog.Info("Config reload detected, reconciling all Claude settings").Log(ctx)
	h.scheduleReconcile("", "")
}

// handleConfigChange handles a grove.toml change by scheduling a debounced
// reconcile scoped to the owning workspace's worktrees.
func (h *SettingsHandler) handleConfigChange(configPath string) {
	configDir := filepath.Dir(configPath)

	h.pathsMutex.RLock()
	node, found := h.watchedPaths[configDir]
	h.pathsMutex.RUnlock()

	if !found || node == nil {
		return
	}

	h.scheduleReconcile(node.Path, node.Name)
}

// scheduleReconcile (re)arms the debounce timer for the given workspace path.
// An empty workspacePath reconciles every discovered worktree; a non-empty
// path scopes the reconcile to worktrees that belong to that workspace.
func (h *SettingsHandler) scheduleReconcile(workspacePath, workspaceName string) {
	debounceKey := workspacePath

	h.timersMutex.Lock()
	defer h.timersMutex.Unlock()

	if timer, exists := h.timers[debounceKey]; exists {
		timer.Stop()
	}

	h.timers[debounceKey] = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		ctx := context.Background()
		h.ulog.Debug("Executing Claude settings reconcile").
			Field("workspace", workspaceName).
			Field("scope", debounceKey).
			Log(ctx)
		h.reconcile(workspacePath)
	})
}

// reconcile enumerates worktrees and reconciles each one's
// .claude/settings.local.json directly. It uses DiscoveryService.DiscoverAll as
// the authoritative worktree list (it walks XDG ecosystem worktrees that the
// skills enumeration misses), after reconciling the worktree registry so newly
// created XDG dirs are adopted. When filterWorkspacePath is non-empty, only
// worktrees that belong to that workspace are reconciled.
func (h *SettingsHandler) reconcile(filterWorkspacePath string) {
	ctx := context.Background()

	// Reconcile the worktree registry before discovery so stale entries are
	// pruned and newly-created XDG dirs are adopted (mirrors collector/workspace.go).
	if wtd := paths.WorktreesDir(); wtd != "" {
		_ = worktreeregistry.Reconcile(wtd)
	}

	discoveryLogger := logrus.New()
	discoveryLogger.SetLevel(logrus.WarnLevel)
	discoveryService := workspace.NewDiscoveryService(discoveryLogger)
	discoveryResult, err := discoveryService.DiscoverAll()
	if err != nil {
		h.ulog.Error("Workspace discovery failed during Claude settings reconcile").Err(err).Log(ctx)
		return
	}
	provider := workspace.NewProvider(discoveryResult)

	// Authoritative member-repo list per worktree, keyed by absolute path.
	reposByPath := make(map[string][]string)
	if entries, lerr := worktreeregistry.ListAll(); lerr == nil {
		for _, entry := range entries {
			if entry != nil && entry.AbsPath != "" {
				reposByPath[entry.AbsPath] = entry.Repos
			}
		}
	}

	for _, node := range provider.All() {
		if node == nil || !node.IsWorktree() {
			continue
		}
		if filterWorkspacePath != "" && !worktreeBelongsTo(node, filterWorkspacePath) {
			continue
		}
		// Skip worktrees whose paths no longer exist (transient teardown state).
		if _, statErr := os.Stat(node.Path); statErr != nil {
			continue
		}

		repos := reposByPath[node.Path]
		if seedErr := workspace.SeedClaudeSettingsForWorktree(node.Path, repos, provider); seedErr != nil {
			h.ulog.Warn("Failed to reconcile Claude settings for worktree").
				Err(seedErr).
				Field("worktree", node.Path).
				Log(ctx)
			continue
		}
		h.ulog.Debug("Reconciled Claude settings for worktree").
			Field("worktree", node.Path).
			Log(ctx)
	}
}

// worktreeBelongsTo reports whether a worktree node is owned by (or is) the
// workspace at workspacePath, matching either the worktree itself, its managing
// project, or its root ecosystem.
func worktreeBelongsTo(node *workspace.WorkspaceNode, workspacePath string) bool {
	return node.Path == workspacePath ||
		node.ParentProjectPath == workspacePath ||
		node.ParentEcosystemPath == workspacePath ||
		node.RootEcosystemPath == workspacePath
}
