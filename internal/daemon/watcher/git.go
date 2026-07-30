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
	"github.com/grovetools/daemon/internal/daemon/gitlimits"
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

	debounceMs int

	// scans holds the per-workspace scan serialization state, keyed by
	// workspace path. It replaces the old bare timers map, which had no
	// in-flight tracking at all: time.Timer.Stop's return value was discarded,
	// so a timer that had ALREADY FIRED was silently rearmed on top of the scan
	// it started, and N events during one scan produced N concurrent scans of
	// the same repository. Entries are pruned in HandleStoreUpdate.
	scans      map[string]*scanState
	scansMutex sync.Mutex

	// scanSem bounds concurrent watcher scans across all workspaces, mirroring
	// the collector's sweep pool (both sized from gitlimits.Workers). Without
	// it, a fleet-wide burst of events fans out to one git process per touched
	// repository at once.
	scanSem chan struct{}

	// scanFn is the scan entry point, indirected ONLY so tests can count and
	// stall scans without forking real git. Production is always scanAndEmit.
	// The state identity and generation let publication reject work made stale
	// by a remove/re-add while preserving one serialization authority per path.
	scanFn func(*workspace.WorkspaceNode, *scanState, uint64)

	// beforeTimerScan is a deterministic test seam for the fired-timer eviction
	// window. Production leaves it nil.
	beforeTimerScan func()
}

// scanState is one workspace's scan serialization state. At most one scan runs
// at a time (inFlight); requests that arrive while it runs set rerun, which
// buys exactly one trailing catch-up scan after it finishes. That trailing scan
// is MANDATORY, not an optimization: events that arrived mid-scan may describe
// state the running scan did not observe, and a dropped scan on a repo whose
// events have stopped is permanent staleness — the collector's reconciler is
// hourly and scoped daemons never sweep at all.
type scanState struct {
	timer      *time.Timer
	timerToken uint64
	inFlight   bool
	rerun      bool
	live       bool
	generation uint64
	node       *workspace.WorkspaceNode
}

// NewGitHandler creates a GitHandler with a per-workspace trailing debounce.
func NewGitHandler(st *store.Store, debounceMs int) *GitHandler {
	if debounceMs <= 0 {
		debounceMs = 150
	}
	h := &GitHandler{
		store:        st,
		ulog:         logging.NewUnifiedLogger("groved.watcher.git"),
		watchedPaths: make(map[string][]*workspace.WorkspaceNode),
		knownPaths:   make(map[string]bool),
		failedPaths:  make(map[string]bool),
		scans:        make(map[string]*scanState),
		scanSem:      make(chan struct{}, gitlimits.Workers),
		debounceMs:   debounceMs,
	}
	h.scanFn = h.scanAndEmitCurrent
	return h
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
// the workspace is rescanned and a delta emitted if changed. If a scan for this
// workspace is already running, no timer is armed and no second scan is
// started: the request is folded into that scan's single trailing rerun.
func (h *GitHandler) scheduleScan(node *workspace.WorkspaceNode) {
	path := node.Path
	h.scansMutex.Lock()
	defer h.scansMutex.Unlock()

	st := h.scanStateLocked(path)
	// A retained tombstone is an in-flight scan for a workspace that is no
	// longer in the store. Stale routes must not revive it; HandleStoreUpdate's
	// first-sight path is the only operation that can make it live again.
	if !st.live {
		return
	}
	st.node = node
	if st.inFlight {
		// Coalesce. The rerun bit is what keeps this from dropping the event:
		// the running scan re-runs exactly once after it finishes.
		h.requestRerunLocked(st)
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timerToken++
	token := st.timerToken
	st.timer = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		if h.beforeTimerScan != nil {
			h.beforeTimerScan()
		}
		h.beginTimerScan(node, st, token)
	})
}

// scanStateLocked returns (creating if needed) the scan state for path. Callers
// must hold scansMutex.
func (h *GitHandler) scanStateLocked(path string) *scanState {
	st, ok := h.scans[path]
	if !ok {
		st = &scanState{live: true}
		h.scans[path] = st
	}
	return st
}

// requestRerunLocked sets the trailing-rerun bit, counting only the transition
// so the counter reads as "scan requests that were coalesced away". Callers
// must hold scansMutex.
func (h *GitHandler) requestRerunLocked(st *scanState) {
	st.rerun = true
	telemetry.GitWatcherCoalesced.Inc()
}

// beginScan claims the workspace's in-flight slot and runs the scan loop in the
// background, or — if a scan is already running — records a trailing rerun and
// returns. It is the single entry point for actually scanning: both the
// debounce timer and HandleStoreUpdate's first-sight path go through it, so no
// caller can start a scan that bypasses the per-workspace guard or the global
// semaphore. It never blocks the caller.
func (h *GitHandler) beginScan(node *workspace.WorkspaceNode) {
	h.scansMutex.Lock()
	st := h.scanStateLocked(node.Path)
	if !st.live {
		// Re-add the path without replacing its state: an old loop remains the
		// serialization authority until it exits. Advancing the generation also
		// prevents that old scan from publishing after the re-add.
		st.live = true
		st.generation++
	}
	st.node = node
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	h.beginScanLocked(st)
}

// beginTimerScan accepts only the exact timer and state that were armed. A
// fired callback queued behind eviction therefore cannot recreate an absent
// map entry or act on a later generation.
func (h *GitHandler) beginTimerScan(node *workspace.WorkspaceNode, st *scanState, token uint64) {
	h.scansMutex.Lock()
	if h.scans[node.Path] != st || !st.live || st.timer == nil || st.timerToken != token {
		h.scansMutex.Unlock()
		return
	}
	st.timer = nil
	st.node = node
	h.beginScanLocked(st)
}

// beginScanLocked claims st or coalesces onto its existing loop, then releases
// scansMutex. The caller must hold scansMutex.
func (h *GitHandler) beginScanLocked(st *scanState) {
	if st.inFlight {
		h.requestRerunLocked(st)
		h.scansMutex.Unlock()
		return
	}
	st.inFlight = true
	h.scansMutex.Unlock()
	go h.scanLoop(st)
}

// scanLoop runs the workspace's scan, then one more for each pending rerun
// request, until no request is outstanding. It owns st.inFlight for its whole
// lifetime, so exactly one of these loops exists per workspace at a time; N
// overlapping requests collapse into "one scan per scan-duration, plus one
// trailing catch-up".
func (h *GitHandler) scanLoop(st *scanState) {
	for {
		h.scansMutex.Lock()
		node, generation := st.node, st.generation
		h.scansMutex.Unlock()

		// The global bound is taken per scan rather than per loop so a repo
		// being rescanned in a tight event storm cannot hold a slot forever.
		// It is acquired OUTSIDE scanFn so the recorded scan duration measures
		// the git work, not the time spent queued behind other workspaces.
		h.scanSem <- struct{}{}
		h.scanFn(node, st, generation)
		<-h.scanSem

		h.scansMutex.Lock()
		if !st.live {
			st.inFlight = false
			if h.scans[node.Path] == st {
				delete(h.scans, node.Path)
			}
			h.scansMutex.Unlock()
			return
		}
		if !st.rerun {
			st.inFlight = false
			h.scansMutex.Unlock()
			return
		}
		st.rerun = false
		h.scansMutex.Unlock()
	}
}

// scanAndEmit re-runs GetExtendedStatus for one workspace and, if the status
// differs from what the store holds, emits a WorkspaceDelta.
// scanAndEmit is the synchronous test/helper entry point. Scheduled production
// scans use scanAndEmitCurrent so eviction generations gate publication.
func (h *GitHandler) scanAndEmit(node *workspace.WorkspaceNode) {
	h.scanAndEmitCurrent(node, nil, 0)
}

func (h *GitHandler) scanAndEmitCurrent(node *workspace.WorkspaceNode, scanState *scanState, generation uint64) {
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

	delta := &models.WorkspaceDelta{Path: node.Path, GitStatus: status, GitLanding: landing}
	if focused {
		delta.ChangedFiles, delta.BlobHashes = files, hashes
		computed := true
		delta.ChangedFilesComputed = &computed
	}

	h.publishIfCurrent(node.Path, scanState, generation, func() {
		emitted = true
		h.ulog.Debug("git watcher: emitting delta").
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
			Payload: []*models.WorkspaceDelta{delta},
		})
	})
}

// publishIfCurrent makes the generation check and publication atomic with
// eviction/re-add. Holding scansMutex across publish is intentional: otherwise
// removal could advance the generation after the check but before ApplyUpdate,
// allowing the old generation's stale delta to land last.
func (h *GitHandler) publishIfCurrent(path string, st *scanState, generation uint64, publish func()) bool {
	if st == nil { // synchronous helper/test scans have no eviction generation
		publish()
		return true
	}
	h.scansMutex.Lock()
	defer h.scansMutex.Unlock()
	if h.scans[path] != st || !st.live || st.generation != generation {
		return false
	}
	publish()
	return true
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
			// watcher's store-subscription loop. beginScan skips the debounce
			// entirely — the first sight of a workspace is still scanned
			// immediately — but it takes the same in-flight slot as an
			// event-driven scan, so discovery and an fs event landing together
			// cannot race two git passes over one repo.
			h.ulog.Debug("git watcher: new workspace, immediate scan").Field("path", path).Log(context.Background())
			h.beginScan(node)
		}
	}

	// Advance the known-set to the current snapshot. Owned solely here.
	known := make(map[string]bool, len(workspaces))
	for path := range workspaces {
		known[path] = true
	}
	h.knownPaths = known
	h.knownPathsInitialized = true

	// Prune scan state for workspaces that no longer exist. Pending states are
	// deleted immediately. In-flight states remain as tombstones until their
	// loop exits: retaining the same pointer is what serializes a remove/re-add
	// and its advanced generation suppresses stale old-generation publication.
	h.scansMutex.Lock()
	for path, st := range h.scans {
		if known[path] {
			continue
		}
		st.live = false
		st.generation++
		st.rerun = false
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
			st.timerToken++
		}
		if !st.inFlight {
			delete(h.scans, path)
		}
	}
	h.scansMutex.Unlock()
}

// OnStart logs handler startup; the initial watch set is established via
// ComputeWatchPaths during the UnifiedWatcher's first refreshWatches.
func (h *GitHandler) OnStart(ctx context.Context) {
	h.ulog.Info("git watcher: handler started").Field("debounce_ms", h.debounceMs).Log(ctx)
}
