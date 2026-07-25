package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sort"
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
// the git internals (HEAD, index, refs/heads, refs/remotes, the HEAD reflog,
// packed-refs) of the focused workspace set and, on a debounced filesystem
// event, re-runs GetExtendedStatus for just that one workspace, emitting a
// WorkspaceDelta through the store.
//
// It complements (does not replace) the timer-driven GitStatusCollector, which
// remains the background fallback for unfocused workspaces. Watching only the
// focused set keeps the fsnotify/kqueue watch count bounded.
type GitHandler struct {
	store *store.Store
	ulog  *logging.UnifiedLogger

	// watchedPaths maps each watched git-internal path to the WorkspaceNodes it
	// routes to, so an incoming event can be routed back to workspaces.
	// Commondir paths (refs, packed-refs, logs) are shared across all worktrees
	// of one repo, so one event may fan out to several focused worktrees —
	// each needs its own rescan (branch/ahead-behind state is per-worktree).
	watchedPaths map[string][]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// knownPaths is the set of workspace paths seen in the last UpdateWorkspaces,
	// used by HandleStoreUpdate to detect newly created workspaces. It is owned
	// EXCLUSIVELY by HandleStoreUpdate and must NOT be written by ComputeWatchPaths:
	// the UnifiedWatcher calls refreshWatches() (→ ComputeWatchPaths) BEFORE
	// HandleStoreUpdate on an UpdateWorkspaces, so if ComputeWatchPaths advanced
	// knownPaths the new-worktree diff below would always come up empty. Access is
	// serialized because HandleStoreUpdate runs under the UnifiedWatcher lock.
	knownPaths map[string]bool

	// lastWatchSetKey and failedPaths state-change-gate ComputeWatchPaths'
	// logging so a 15s refresh that changes nothing emits nothing:
	// lastWatchSetKey is a canonical key of the previous watch set (summary
	// logged only when it changes) and failedPaths is the set of workspace
	// paths whose ResolveGitDirs already failed (each non-repo path logged
	// once). Both are owned EXCLUSIVELY by ComputeWatchPaths, which runs under
	// the UnifiedWatcher lock, so no extra locking is needed.
	lastWatchSetKey string
	failedPaths     map[string]bool

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
		watchedPaths: make(map[string][]*workspace.WorkspaceNode),
		knownPaths:   make(map[string]bool),
		failedPaths:  make(map[string]bool),
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
// refs/heads, refs/remotes, the HEAD reflog, and packed-refs when present on
// disk.
func (h *GitHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	ctx := context.Background()
	newWatches := make(map[string][]*workspace.WorkspaceNode)

	addIfExists := func(p string, node *workspace.WorkspaceNode) {
		if p == "" {
			return
		}
		if _, err := os.Stat(p); err == nil {
			newWatches[p] = append(newWatches[p], node)
		}
	}

	focusedCount := 0
	newFailed := make(map[string]bool)
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
			// Expected for non-repo container/ecosystem roots (no .git). Log each
			// failing path ONCE (at Debug) rather than on every 15s refresh.
			newFailed[node.Path] = true
			if !h.failedPaths[node.Path] {
				h.ulog.Debug("git watcher: ResolveGitDirs failed (non-repo?)").Err(err).Field("path", node.Path).Log(ctx)
			}
			continue
		}

		addIfExists(filepath.Join(gitDir, "HEAD"), node)
		addIfExists(filepath.Join(gitDir, "index"), node)
		addIfExists(filepath.Join(commonDir, "refs", "heads"), node)
		addIfExists(filepath.Join(commonDir, "refs", "remotes"), node)
		// The HEAD reflog moves on every HEAD mutation (commit, reset, merge,
		// rebase, checkout) even when the refs are packed, so it covers
		// packed-ref and slash-named-branch updates the loose-ref watches miss.
		// Before the first reflog entry exists, watch the logs directory so its
		// creation is seen.
		logsHead := filepath.Join(commonDir, "logs", "HEAD")
		if _, err := os.Stat(logsHead); err == nil {
			newWatches[logsHead] = append(newWatches[logsHead], node)
		} else {
			addIfExists(filepath.Join(commonDir, "logs"), node)
		}
		// packed-refs covers git gc / pack-refs advancing branches with no
		// loose-ref event.
		addIfExists(filepath.Join(commonDir, "packed-refs"), node)
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.pathsMutex.Unlock()

	h.failedPaths = newFailed

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// Debug summary, state-change-gated: ComputeWatchPaths runs on every refresh
	// (15s tick + each focus/workspace change), so only log when the watch set
	// actually changed. Enable with GROVE_LOG_LEVEL=debug to confirm the watch
	// set when freshness looks wrong.
	watchSetKey := strings.Join(paths, "\x00")
	if watchSetKey != h.lastWatchSetKey {
		h.lastWatchSetKey = watchSetKey
		h.ulog.Debug("git watcher: computed watch set").
			Field("total_workspaces", len(workspaces)).
			Field("focused", focusedCount).
			Field("watch_paths", len(paths)).
			Log(ctx)
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
		// Lock files (e.g. packed-refs.lock, refs/heads/<branch>.lock) churn on
		// every git operation before the real ref moves; scanning on them would
		// feed back into git's own activity for no fresh state.
		if strings.HasSuffix(event.Name, ".lock") {
			continue
		}
		for watched, nodes := range h.watchedPaths {
			if event.Name != watched && !strings.HasPrefix(event.Name, watched+string(filepath.Separator)) {
				continue
			}
			for _, node := range nodes {
				if node == nil {
					continue
				}
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

	// Granular per-file data is computed ONLY for focused repos to bound git
	// cost; the watcher only ever fires for the focused watched set, but guard
	// explicitly to mirror the collector. Best-effort: a fetch error leaves the
	// file-level fields nil and the coarse status stands.
	focused := h.store.IsFocused(node.Path)
	var files []git.FileStatus
	var hashes map[string]string
	if focused {
		files, hashes = focusedFileData(node.Path)
	}

	// Compare against the currently stored status to suppress no-op updates —
	// but never suppress when a focused repo is still missing its per-file
	// cache (backfill), or when the per-file snapshot moved while the coarse
	// status stayed equal (an edit to an already-modified file changes
	// numstat/blob content but not the counts GitStatusEqual sees). Mirrors
	// the collector's gates.
	state := h.store.Get()
	if current, ok := state.Workspaces[node.Path]; ok {
		needsFileBackfill := focused && !current.ChangedFilesComputed
		fileDataChanged := focused && current.ChangedFilesComputed &&
			!store.FileDataEqual(current.ChangedFiles, current.BlobHashes, files, hashes)
		if store.GitStatusEqual(current.GitStatus, status) && !needsFileBackfill && !fileDataChanged {
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

	delta := &models.WorkspaceDelta{Path: node.Path, GitStatus: status}
	if focused {
		delta.ChangedFiles, delta.BlobHashes = files, hashes
		computed := true
		delta.ChangedFilesComputed = &computed
	}

	h.store.ApplyUpdate(store.Update{
		Type:    store.UpdateWorkspacesDelta,
		Source:  "git_watcher",
		Scanned: 1,
		Payload: []*models.WorkspaceDelta{delta},
	})
}

// maxFocusedChangedFiles caps how many changed files a focused repo may have
// before the watcher skips computing per-file blob hashes for it, mirroring the
// collector's bound so a huge change set can't make batch hashing too costly.
const maxFocusedChangedFiles = 500

// focusedFileData computes the per-file change list and (when the set is small
// enough) the per-file working-tree blob hashes for a focused workspace. It is
// best-effort: any git error yields nil for the affected field so the emitted
// delta still carries the coarse GitStatus. Above maxFocusedChangedFiles the
// file list is returned but blob hashes are skipped to bound cost. It mirrors
// the collector's helper of the same name.
func focusedFileData(repoPath string) ([]git.FileStatus, map[string]string) {
	files, err := git.GetChangedFiles(repoPath)
	if err != nil {
		return nil, nil
	}
	if len(files) > maxFocusedChangedFiles {
		return files, nil
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	hashes, err := git.GetBlobHashes(repoPath, paths)
	if err != nil {
		return files, nil
	}
	return files, hashes
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
