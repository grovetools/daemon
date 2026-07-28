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
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// GitHandler implements per-repository debouncing and status publication. In
// the global daemon it owns every workspace and is fed both recursive FSEvents
// and git-internal UnifiedWatcher events. The collector is only an hourly
// correctness reconciler. Its focused-only mode remains a portable fallback.
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
	knownPaths            map[string]bool
	knownPathsInitialized bool

	// lastWatchSetKey and failedPaths state-change-gate ComputeWatchPaths'
	// logging so a 15s refresh that changes nothing emits nothing:
	// lastWatchSetKey is a canonical key of the previous watch set (summary
	// logged only when it changes) and failedPaths is the set of workspace
	// paths whose ResolveGitDirs already failed (each non-repo path logged
	// once). Both are owned EXCLUSIVELY by ComputeWatchPaths, which runs under
	// the UnifiedWatcher lock, so no extra locking is needed.
	lastWatchSetKey string
	failedPaths     map[string]bool

	// broadCoverage marks the global event owner. Recursive working-tree and
	// git-internal events arrive through RunGlobalGitEvents; the unified
	// fsnotify fallback remains focused-only so broad coverage does not create
	// thousands of per-repository kqueue watches.
	broadCoverage bool

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

// SetBroadCoverage makes this handler the global owner for every repository.
// It must be called before the handler is registered with UnifiedWatcher.
func (h *GitHandler) SetBroadCoverage(enabled bool) *GitHandler {
	h.broadCoverage = enabled
	return h
}

// ComputeWatchPaths returns fallback git-internal watches for the focused
// workspace set only. Broad global coverage is provided by one recursive
// FSEvents stream, not by expanding this set per repository. For each focused
// workspace it resolves gitdir and commondir and watches HEAD, index, refs,
// reflog, and packed-refs.
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

		// Keep the unified fsnotify fallback bounded to the focused set. The
		// global owner's recursive FSEvents stream covers all repositories.
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
	started := time.Now()
	// emitted is flipped on the one path that publishes a delta; every other
	// return is a no-op scan. The ratio (emitted vs noop) is the watcher's
	// signal-to-noise, and a collapsing ratio means the debounce is losing to
	// something writing continuously in a watched tree.
	emitted := false
	defer func() { telemetry.RecordGitWatcherScan(node.Path, time.Since(started), emitted) }()

	status, err := git.GetExtendedStatus(node.Path)
	if err != nil {
		telemetry.GitWatcherFailed.Inc()
		h.ulog.Warn("git watcher: scan failed").Err(err).Field("path", node.Path).Log(ctx)
		return
	}

	// A watcher scan only runs because a filesystem event already fired for this
	// repo, so per-file data is ALWAYS recomputed for focused repos — the coarse
	// status is NOT a valid fingerprint for it here. GitStatusEqual sees only
	// branch / ahead-behind / file counts / numstat totals: editing an untracked
	// file, re-editing an already-modified line, or touching any binary file
	// (numstat reports 0 for both) leaves it equal while the file contents — and
	// the blob hashes git-viewer's review state keys on — moved. Gating the
	// changed-file/blob pass on the coarse status here is exactly the bug 291d3fb
	// fixed ("edits to already-modified files were previously invisible"): the
	// consumer would keep stale blob hashes and keep rendering an edited file as
	// reviewed. The timer-driven collector keeps that fingerprint gate because it
	// must survive tick storms across the whole focus set; the watcher is already
	// debounced per workspace, so this pass runs at most once per event burst.
	// Best-effort: a git error leaves the file-level fields nil and the coarse
	// status stands.
	focused := h.store.IsFocused(node.Path)
	var files []git.FileStatus
	var hashes map[string]string
	if focused {
		files, hashes = focusedFileData(node.Path)
	}

	// Landing state (the preflight divergence contract consumers render landing
	// verdicts from) is recomputed for every repo, focused or not: the events
	// that reach here include refs/heads, refs/remotes and packed-refs writes —
	// exactly the pushes and fetches that move it while leaving the coarse
	// status untouched. Warm and unmoved it costs zero forks.
	landing := git.GetLandingState(node.Path, status.Branch)

	// Suppress only genuine no-ops — an fs event that moved nothing observable.
	// Safe because the per-file comparison below sees content (blob hashes), so
	// no content change can be suppressed by it. Never suppress when a focused
	// repo is still missing its per-file cache (backfill).
	state := h.store.Get()
	if current, ok := state.Workspaces[node.Path]; ok {
		needsFileBackfill := focused && !current.ChangedFilesComputed
		fileDataChanged := focused && current.ChangedFilesComputed &&
			!store.FileDataEqual(current.ChangedFiles, current.BlobHashes, files, hashes)
		if store.GitStatusEqual(current.GitStatus, status) && store.LandingEqual(current.GitLanding, landing) &&
			!needsFileBackfill && !fileDataChanged {
			h.ulog.Debug("git watcher: scan no-op (status unchanged)").Field("path", node.Path).Log(ctx)
			return
		}
	}

	emitted = true
	h.ulog.Debug("git watcher: emitting delta").
		Field("path", node.Path).
		Field("branch", status.Branch).
		Field("dirty", status.IsDirty).
		Field("ahead_main", status.AheadMainCount).
		Field("behind_main", status.BehindMainCount).
		Log(ctx)

	delta := &models.WorkspaceDelta{Path: node.Path, GitStatus: status, GitLanding: landing}
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
		telemetry.RecordBlobHash(telemetry.BlobHashObservation{Repo: repoPath, Skipped: len(files)})
		return files, nil
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	started := time.Now()
	hashes, batch, err := git.GetBlobHashesObserved(repoPath, paths)
	telemetry.RecordBlobHash(telemetry.BlobHashObservation{
		Repo:         repoPath,
		Files:        batch.Hashed,
		Skipped:      batch.Skipped,
		NonRegular:   batch.NonRegular,
		LargestBytes: batch.LargestBytes,
		LargestPath:  batch.LargestPath,
		Duration:     time.Since(started),
	})
	if err != nil {
		return files, nil
	}
	return files, hashes
}

// HandleStoreUpdate reacts to store changes. On UpdateWorkspaces it diffs the
// incoming workspace set against the known set and, for any NEW workspace the
// handler owns (all repos globally, focused repos in fallback mode), bypasses
// the debounce to do an immediate first scan.
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
		// The collector owns the global boot snapshot. Avoid duplicating that
		// whole-machine scan when the watcher's first workspace update arrives;
		// broad coverage applies immediately only to workspaces discovered later.
		if (h.knownPathsInitialized && h.broadCoverage) || h.store.IsFocused(path) {
			// Scan asynchronously so workspace discovery never blocks the unified
			// watcher's store-subscription loop.
			h.ulog.Debug("git watcher: new workspace, immediate scan").Field("path", path).Log(context.Background())
			go h.scanAndEmit(node)
		}
	}

	// Advance the known-set to the current snapshot. Owned solely here.
	known := make(map[string]bool, len(workspaces))
	for path := range workspaces {
		known[path] = true
	}
	h.knownPaths = known
	h.knownPathsInitialized = true
}

// OnStart logs handler startup; the initial watch set is established via
// ComputeWatchPaths during the UnifiedWatcher's first refreshWatches.
func (h *GitHandler) OnStart(ctx context.Context) {
	h.ulog.Info("git watcher: handler started").Field("debounce_ms", h.debounceMs).Log(ctx)
}
