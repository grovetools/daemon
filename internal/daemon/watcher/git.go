package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/git"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// GitHandler implements DomainHandler for subsecond-fresh git status. It watches
// the git internals (HEAD, index, refs/heads, refs/remotes) of the focused
// workspace set and, on a debounced filesystem event, re-runs GetExtendedStatus
// for just that one workspace, emitting a WorkspaceDelta through the store.
//
// It complements (does not replace) the timer-driven GitStatusCollector, which
// remains the background fallback for unfocused workspaces. Watching only the
// focused set keeps the fsnotify/kqueue watch count bounded.
type GitHandler struct {
	store *store.Store
	ulog  *logging.UnifiedLogger

	// watchedPaths maps each watched git-internal path to the WorkspaceNode it
	// belongs to, so an incoming event can be routed back to a workspace.
	watchedPaths map[string]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// knownPaths is the set of workspace paths seen in the last UpdateWorkspaces,
	// used by HandleStoreUpdate to detect newly created workspaces. It is owned
	// EXCLUSIVELY by HandleStoreUpdate and must NOT be written by ComputeWatchPaths:
	// the UnifiedWatcher calls refreshWatches() (→ ComputeWatchPaths) BEFORE
	// HandleStoreUpdate on an UpdateWorkspaces, so if ComputeWatchPaths advanced
	// knownPaths the new-worktree diff below would always come up empty. Access is
	// serialized because HandleStoreUpdate runs under the UnifiedWatcher lock.
	knownPaths map[string]bool

	debounceMs  int
	timers      map[string]*time.Timer
	timersMutex sync.Mutex
}

// NewGitHandler creates a GitHandler with a per-workspace trailing debounce.
func NewGitHandler(st *store.Store, debounceMs int) *GitHandler {
	if debounceMs <= 0 {
		debounceMs = 150
	}
	return &GitHandler{
		store:        st,
		ulog:         logging.NewUnifiedLogger("groved.watcher.git"),
		watchedPaths: make(map[string]*workspace.WorkspaceNode),
		knownPaths:   make(map[string]bool),
		timers:       make(map[string]*time.Timer),
		debounceMs:   debounceMs,
	}
}

func (h *GitHandler) Name() string {
	return "git"
}

// ComputeWatchPaths returns the git-internal paths to watch for the focused
// workspace set only. For each focused workspace it resolves the gitdir and
// commondir (handling linked-worktree indirection) and watches HEAD, index,
// refs/heads, and refs/remotes when present on disk.
func (h *GitHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	ctx := context.Background()
	newWatches := make(map[string]*workspace.WorkspaceNode)

	addIfExists := func(p string, node *workspace.WorkspaceNode) {
		if p == "" {
			return
		}
		if _, err := os.Stat(p); err == nil {
			newWatches[p] = node
		}
	}

	focusedCount := 0
	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		// Only watch the focused/visible set; unfocused workspaces fall back to
		// the timer-driven GitStatusCollector.
		if !h.store.IsFocused(node.Path) {
			continue
		}
		focusedCount++

		gitDir, commonDir, err := git.ResolveGitDirs(ctx, node.Path)
		if err != nil {
			// Expected for non-repo container/ecosystem roots (no .git); keep at
			// Debug so it doesn't flood the log on every refresh.
			h.ulog.Debug("git watcher: ResolveGitDirs failed (non-repo?)").Err(err).Field("path", node.Path).Log(ctx)
			continue
		}

		addIfExists(filepath.Join(gitDir, "HEAD"), node)
		addIfExists(filepath.Join(gitDir, "index"), node)
		addIfExists(filepath.Join(commonDir, "refs", "heads"), node)
		addIfExists(filepath.Join(commonDir, "refs", "remotes"), node)
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.pathsMutex.Unlock()

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}

	// Debug: fires on every refresh (15s tick + each focus/workspace change), so
	// keep it off the default level. Enable with GROVE_LOG_LEVEL=debug to confirm
	// the watch set when freshness looks wrong.
	h.ulog.Debug("git watcher: computed watch set").
		Field("total_workspaces", len(workspaces)).
		Field("focused", focusedCount).
		Field("watch_paths", len(paths)).
		Log(ctx)
	for _, p := range paths {
		h.ulog.Debug("git watcher: watching").Field("path", p).Log(ctx)
	}
	return paths
}

// MatchesEvent reports whether an event touches a watched git-internal path.
// refs/heads and refs/remotes are watched as directories, so child events
// (e.g. refs/heads/<branch>) must match by prefix.
func (h *GitHandler) MatchesEvent(event fsnotify.Event) bool {
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}

	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watched := range h.watchedPaths {
		if event.Name == watched || strings.HasPrefix(event.Name, watched+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// HandleEvents debounces events per workspace and, when the timer fires,
// rescans that one workspace's git status and emits a delta if it changed.
func (h *GitHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	// Collect the distinct workspaces touched by this batch.
	touched := make(map[string]*workspace.WorkspaceNode)
	h.pathsMutex.RLock()
	for _, event := range events {
		for watched, node := range h.watchedPaths {
			if node == nil {
				continue
			}
			if event.Name == watched || strings.HasPrefix(event.Name, watched+string(filepath.Separator)) {
				touched[node.Path] = node
			}
		}
	}
	h.pathsMutex.RUnlock()

	if len(touched) > 0 {
		paths := make([]string, 0, len(touched))
		for p := range touched {
			paths = append(paths, p)
		}
		h.ulog.Debug("git watcher: events received").
			Field("raw_events", len(events)).
			Field("touched_workspaces", paths).
			Log(ctx)
	}

	for _, node := range touched {
		h.scheduleScan(node)
	}
	return nil
}

// scheduleScan resets the per-workspace trailing debounce timer; when it fires
// the workspace is rescanned and a delta emitted if changed.
func (h *GitHandler) scheduleScan(node *workspace.WorkspaceNode) {
	path := node.Path
	h.timersMutex.Lock()
	defer h.timersMutex.Unlock()

	if timer, ok := h.timers[path]; ok {
		timer.Stop()
	}
	h.timers[path] = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		h.scanAndEmit(node)
	})
}

// scanAndEmit re-runs GetExtendedStatus for one workspace and, if the status
// differs from what the store holds, emits a WorkspaceDelta.
func (h *GitHandler) scanAndEmit(node *workspace.WorkspaceNode) {
	ctx := context.Background()
	status, err := git.GetExtendedStatus(node.Path)
	if err != nil {
		h.ulog.Warn("git watcher: scan failed").Err(err).Field("path", node.Path).Log(ctx)
		return
	}

	// Compare against the currently stored status to suppress no-op updates.
	state := h.store.Get()
	if current, ok := state.Workspaces[node.Path]; ok {
		if store.GitStatusEqual(current.GitStatus, status) {
			h.ulog.Debug("git watcher: scan no-op (status unchanged)").Field("path", node.Path).Log(ctx)
			return
		}
	}

	h.ulog.Info("git watcher: emitting delta").
		Field("path", node.Path).
		Field("branch", status.Branch).
		Field("dirty", status.IsDirty).
		Field("ahead_main", status.AheadMainCount).
		Field("behind_main", status.BehindMainCount).
		Log(ctx)

	h.store.ApplyUpdate(store.Update{
		Type:    store.UpdateWorkspacesDelta,
		Source:  "git_watcher",
		Scanned: 1,
		Payload: []*models.WorkspaceDelta{{Path: node.Path, GitStatus: status}},
	})
}

// HandleStoreUpdate reacts to store changes. On UpdateWorkspaces it diffs the
// incoming workspace set against the known set and, for any NEW focused
// workspace, bypasses the debounce to do an immediate first scan — so freshly
// created XDG worktrees go subsecond without waiting for the timer collector.
func (h *GitHandler) HandleStoreUpdate(update store.Update) {
	if update.Type != store.UpdateWorkspaces {
		return
	}
	workspaces, ok := update.Payload.(map[string]*models.EnrichedWorkspace)
	if !ok {
		return
	}

	for path, ew := range workspaces {
		if h.knownPaths[path] {
			continue
		}
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}
		if h.store.IsFocused(path) {
			// New, focused workspace: scan immediately in a goroutine so we never
			// block the watcher's store-subscription loop.
			h.ulog.Info("git watcher: new focused workspace, immediate scan").Field("path", path).Log(context.Background())
			go h.scanAndEmit(node)
		}
	}

	// Advance the known-set to the current snapshot. Owned solely here.
	known := make(map[string]bool, len(workspaces))
	for path := range workspaces {
		known[path] = true
	}
	h.knownPaths = known
}

// OnStart logs handler startup; the initial watch set is established via
// ComputeWatchPaths during the UnifiedWatcher's first refreshWatches.
func (h *GitHandler) OnStart(ctx context.Context) {
	h.ulog.Info("git watcher: handler started").Field("debounce_ms", h.debounceMs).Log(ctx)
}
