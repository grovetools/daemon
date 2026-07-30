package collector

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/gitlimits"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// gitWorkers is the number of parallel git status workers. The size is shared
// with the watcher's event-driven scan pool (see gitlimits) so the two bounds
// cannot drift apart.
var gitWorkers = gitlimits.Workers

const (
	// backgroundScanInterval is the correctness reconciler for the global
	// event-driven owner. Filesystem events provide freshness; this hourly pass
	// only repairs dropped/coalesced events.
	backgroundScanInterval = time.Hour
	// Focus must never invert into near-continuous polling for small sets.
	focusedScanFloor = 5 * time.Second
	// Repeated verify-on-reveal refreshes reuse the just-computed snapshot.
	pathRefreshCooldown = 5 * time.Second
)

// maxFocusedChangedFiles caps how many changed files a focused repo may have
// before the daemon skips computing per-file blob hashes for it. A huge change
// set (e.g. a checked-in node_modules) would make the batch hash-object too
// costly; above the cap we still cache the file list but leave BlobHashes nil so
// the consumer falls back to live hashing.
const maxFocusedChangedFiles = 500

// focusedFileData computes the per-file change list and (when the set is small
// enough) the per-file working-tree blob hashes for a focused workspace. It is
// best-effort: any git error yields nil for the affected field so the emitted
// delta still carries the coarse GitStatus. Above maxFocusedChangedFiles the
// file list is returned but blob hashes are skipped to bound cost.
func focusedFileData(repoPath string) ([]git.FileStatus, map[string]string) {
	files, err := git.GetChangedFiles(repoPath)
	if err != nil {
		return nil, nil
	}
	if len(files) > maxFocusedChangedFiles {
		// The cap bounds file COUNT, not bytes (doc 50's byte bomb). Record
		// the whole set as skipped so the counter shows the cap firing.
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

// shouldComputeFocusedFileData decides whether the timer collector pays for the
// changed-file + blob-hash pass on this tick. It is intentionally coarse (see
// the call site): the watcher, not the ticker, is what guarantees content-only
// edits are seen promptly.
func shouldComputeFocusedFileData(focused, alreadyComputed bool, oldStatus, newStatus *git.ExtendedGitStatus) bool {
	return focused && (!alreadyComputed || !store.GitStatusEqual(oldStatus, newStatus))
}

func pathRefreshDue(last map[string]time.Time, path string, now time.Time) bool {
	previous, ok := last[path]
	return !ok || now.Sub(previous) >= pathRefreshCooldown
}

// dynamicInterval returns a scan interval based on workspace count.
// Fewer workspaces = faster scanning since it's cheaper.
func dynamicInterval(count int, baseInterval time.Duration) time.Duration {
	switch {
	case count <= 5:
		return max(baseInterval/4, focusedScanFloor)
	case count <= 15:
		return max(baseInterval/2, focusedScanFloor)
	case count <= 30:
		return baseInterval // Normal speed
	default:
		return baseInterval * 2 // Slower for large sets
	}
}

// GitStatusCollector updates git status for all workspaces.
type GitStatusCollector struct {
	interval time.Duration
	// scope is this daemon's owning scope ("" == unscoped/global). A scoped
	// collector's boot, Refresh, and background full sweeps cover only
	// workspaces at or under scope — the global workspace list is populated on
	// every daemon (see the WorkspaceCollector registration), so without this
	// bound N alive scoped daemons each full-sweep every repo on the machine.
	// Focused scans and RefreshPaths are demand-driven and stay unscoped.
	scope        string
	refresh      chan chan struct{}
	refreshPaths chan pathRefreshRequest
}

// pathRefreshRequest asks the Run loop for a synchronous scoped scan of just
// the given workspace paths. The reply channel is buffered so the loop never
// blocks on a caller that gave up (ctx canceled).
type pathRefreshRequest struct {
	paths []string
	reply chan []*models.EnrichedWorkspace
}

// NewGitStatusCollector creates a new GitStatusCollector with the specified interval.
// If interval is 0, defaults to 10 seconds. scope is the owning daemon scope
// ("" == unscoped/global) and bounds which workspaces full sweeps cover.
func NewGitStatusCollector(interval time.Duration, scope string) *GitStatusCollector {
	if interval == 0 {
		interval = 10 * time.Second
	}
	return &GitStatusCollector{
		interval:     interval,
		scope:        scope,
		refresh:      make(chan chan struct{}),
		refreshPaths: make(chan pathRefreshRequest),
	}
}

// inScope reports whether a workspace path lies at or under the collector's
// scope; a global collector (scope == "") owns every workspace. Both sides are
// canonicalized with the store's focus-path normalization (case/symlink, see
// store.NormalizePathKey) so scoped selection matches store semantics — a raw
// string compare would silently drop workspaces whose discovered spelling
// differs from the scope's (the same macOS case-mismatch that once broke
// focused per-file attachment). The match is path-boundary aware: /a/bc is
// NOT under scope /a/b.
func (c *GitStatusCollector) inScope(path string) bool {
	if c.scope == "" {
		return true
	}
	scopeKey := store.NormalizePathKey(c.scope)
	key := store.NormalizePathKey(path)
	return key == scopeKey || strings.HasPrefix(key, scopeKey+"/")
}

// scopedWorkspaces returns the workspaces a full sweep covers: everything for
// a global collector, only in-scope workspaces for a scoped one.
func (c *GitStatusCollector) scopedWorkspaces(workspaces map[string]*models.EnrichedWorkspace) []*models.EnrichedWorkspace {
	var out []*models.EnrichedWorkspace
	for _, ws := range workspaces {
		if c.inScope(ws.Path) {
			out = append(out, ws)
		}
	}
	return out
}

// Refresh triggers an immediate full git status scan and blocks until it completes.
func (c *GitStatusCollector) Refresh(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case c.refresh <- reply:
		select {
		case <-reply:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RefreshPaths triggers a synchronous scoped scan of just the given workspace
// paths and returns their fresh enriched workspaces. Unknown paths are
// silently skipped. Per-file data is computed for every requested path
// regardless of focus registration — the request is proof the user is looking.
// The scan also emits a normal git-source delta so SSE subscribers converge.
func (c *GitStatusCollector) RefreshPaths(ctx context.Context, paths []string) ([]*models.EnrichedWorkspace, error) {
	req := pathRefreshRequest{paths: paths, reply: make(chan []*models.EnrichedWorkspace, 1)}
	select {
	case c.refreshPaths <- req:
		select {
		case fresh := <-req.reply:
			return fresh, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Name returns the collector's name.
func (c *GitStatusCollector) Name() string { return "git" }

// Run starts the git status collection loop.
func (c *GitStatusCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ulog := logging.NewUnifiedLogger("groved.collector.git")
	var lastFullScan time.Time
	lastPathScan := make(map[string]time.Time)
	currentInterval := c.interval
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()
	// Publish the EFFECTIVE interval, not the configured one: dynamicInterval
	// rescales it by workspace count below, and "configured 10s / actually
	// running at 20s" is exactly the kind of divergence the telemetry tab
	// exists to make visible.
	telemetry.SetCollectorInterval(c.Name(), currentInterval)

	// scanWorkspaces runs git status on the given workspaces and emits a delta update.
	scanWorkspaces := func(toScan []*models.EnrichedWorkspace) {
		if len(toScan) == 0 {
			return
		}

		start := time.Now()
		defer func() {
			d := time.Since(start)
			// Every sweep feeds the telemetry registry (last/mean/max duration
			// + workspaces scanned) so /api/system/stats can answer "why is
			// this daemon busy?" without anyone grepping logs — this incident
			// class was historically found in Activity Monitor, not here.
			telemetry.RecordGitSweep(c.scope, len(toScan), d)
			if d > 1*time.Second {
				// scope/pid disambiguate which daemon swept in the shared log;
				// scanned separates a broad background sweep from a focused one.
				ulog.Debug("Slow git status scan detected").
					Field("duration", d).
					Field("scope", c.scope).
					Field("scanned", len(toScan)).
					Field("pid", os.Getpid()).
					Log(ctx)
			}
		}()

		var wg sync.WaitGroup
		sem := make(chan struct{}, gitWorkers)
		var mu sync.Mutex
		var deltas []*models.WorkspaceDelta

		for _, ws := range toScan {
			wg.Add(1)
			go func(ws *models.EnrichedWorkspace) {
				defer wg.Done()
				sem <- struct{}{}        // Acquire
				defer func() { <-sem }() // Release

				status, err := git.GetExtendedStatus(ws.Path)
				if err != nil {
					return
				}
				// Landing state rides the same sweep: it is the preflight's
				// divergence contract (local-main ahead/behind, origin-branch
				// presence, behind-origin, last-commit time) that consumers
				// render a landing verdict from without shelling out. Warm, it
				// costs zero forks — every field is pinned to ref SHAs read
				// straight off disk (git.GetLandingState).
				landing := git.GetLandingState(ws.Path, status.Branch)
				gitChanged := !store.GitStatusEqual(ws.GitStatus, status) ||
					!store.LandingEqual(ws.GitLanding, landing)
				focused := st.IsFocused(ws.Path)
				// Backfill: a focused repo whose coarse status is unchanged still
				// needs its per-file data emitted the FIRST time it becomes
				// focused. The daemon boots / rescans with the focus set empty, so
				// ChangedFiles starts nil; without this, a stable focused repo
				// never gets per-file data (the coarse-changed gate alone never
				// fires) and the git-viewer cache-misses forever, falling back to
				// live git in the TUI. Gate on the computed flag, not
				// ChangedFiles == nil: a clean repo's file list is nil, so the nil
				// test would re-emit every tick.
				needsFileBackfill := focused && !ws.ChangedFilesComputed
				if !gitChanged && !needsFileBackfill {
					return
				}
				// On the TIMER path the coarse status is used as the focused-data
				// fingerprint: only pay the changed-file/blob pass when it moved
				// or the first focused snapshot needs backfilling, so a focus set
				// that is quiet costs one `git status` per tick instead of a
				// changed-file + batch-hash pass per repo per tick. This is a
				// deliberate freshness trade: content-only edits (untracked files,
				// re-edits of an already-modified line, binary files — all invisible
				// to GitStatusEqual) are NOT caught here. The event-driven watcher
				// owns that guarantee and recomputes per-file data unconditionally
				// (see watcher.scanAndEmit), as does the scoped /api/refresh path.
				var files []git.FileStatus
				var hashes map[string]string
				if shouldComputeFocusedFileData(focused, ws.ChangedFilesComputed, ws.GitStatus, status) {
					files, hashes = focusedFileData(ws.Path)
				}
				delta := &models.WorkspaceDelta{
					Path:       ws.Path,
					GitStatus:  status,
					GitLanding: landing,
				}
				if focused {
					delta.ChangedFiles, delta.BlobHashes = files, hashes
					computed := true
					delta.ChangedFilesComputed = &computed
				}
				mu.Lock()
				deltas = append(deltas, delta)
				mu.Unlock()
			}(ws)
		}
		wg.Wait()

		if len(deltas) > 0 {
			updates <- store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "git",
				Scanned: len(toScan),
				Payload: deltas,
			}
		}
	}

	// scan is the global hourly correctness reconciler. Focus no longer causes
	// timer-driven git forks: the recursive watcher and RefreshPaths are the
	// latency/demand paths. Scoped collectors are passive RPC helpers and never
	// run background scans.
	scan := func() {
		if c.scope != "" || time.Since(lastFullScan) < backgroundScanInterval {
			return
		}
		state := st.Get()
		toScan := c.scopedWorkspaces(state.Workspaces)
		if len(toScan) > 0 {
			lastFullScan = time.Now()
		}
		scanWorkspaces(toScan)

		newInterval := dynamicInterval(len(state.Workspaces), c.interval)
		if newInterval != currentInterval {
			currentInterval = newInterval
			ticker.Reset(currentInterval)
			telemetry.SetCollectorInterval(c.Name(), currentInterval)
		}
	}

	// fullScan forces a scan of all in-scope workspaces (used by Refresh and
	// the boot pass).
	fullScan := func() {
		state := st.Get()
		toScan := c.scopedWorkspaces(state.Workspaces)
		// See scan(): an empty pass (pre-discovery cold start) must not spend
		// the background budget, or the first real scan waits ~5 minutes.
		if len(toScan) > 0 {
			lastFullScan = time.Now()
		}
		scanWorkspaces(toScan)
	}

	// scanPaths synchronously scans just the requested workspace paths (the
	// scoped /api/refresh form) and returns their fresh enriched workspaces.
	// A path that was scanned less than pathRefreshCooldown ago is served from
	// the store's just-computed snapshot instead of being rescanned, so a burst
	// of verify-on-reveal refreshes costs one git pass; every path that IS
	// scanned computes per-file data unconditionally — the request is proof the
	// user is looking, regardless of focus registration. Scanned results are
	// ALSO emitted as a normal git-source delta so SSE subscribers converge, and
	// the return value is never suppressed via GitStatusEqual: the response
	// always carries current state.
	scanPaths := func(reqPaths []string) []*models.EnrichedWorkspace {
		resolved := st.ResolveWorkspacePaths(reqPaths)
		if len(resolved) == 0 {
			return nil
		}

		var wg sync.WaitGroup
		sem := make(chan struct{}, gitWorkers)
		var mu sync.Mutex
		var deltas []*models.WorkspaceDelta
		fresh := make([]*models.EnrichedWorkspace, 0, len(resolved))
		var completedPaths []string
		now := time.Now()

		for _, ws := range resolved {
			if !pathRefreshDue(lastPathScan, ws.Path, now) {
				out := *ws
				fresh = append(fresh, &out)
				continue
			}
			wg.Add(1)
			go func(ws *models.EnrichedWorkspace) {
				defer wg.Done()
				sem <- struct{}{}        // Acquire
				defer func() { <-sem }() // Release

				status, err := git.GetExtendedStatus(ws.Path)
				if err != nil {
					return
				}
				landing := git.GetLandingState(ws.Path, status.Branch)
				files, hashes := focusedFileData(ws.Path)
				computed := true
				delta := &models.WorkspaceDelta{
					Path:                 ws.Path,
					GitStatus:            status,
					GitLanding:           landing,
					ChangedFiles:         files,
					BlobHashes:           hashes,
					ChangedFilesComputed: &computed,
				}
				// Shallow copy for the response so the store's stored value (a
				// shared pointer) isn't mutated outside ApplyUpdate.
				out := *ws
				out.GitStatus = status
				out.GitLanding = landing
				out.ChangedFiles = files
				out.BlobHashes = hashes
				out.ChangedFilesComputed = true
				mu.Lock()
				deltas = append(deltas, delta)
				fresh = append(fresh, &out)
				completedPaths = append(completedPaths, ws.Path)
				mu.Unlock()
			}(ws)
		}
		wg.Wait()
		for _, path := range completedPaths {
			lastPathScan[path] = now
		}

		if len(deltas) > 0 {
			updates <- store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "git",
				Scanned: len(resolved),
				Payload: deltas,
			}
		}
		return fresh
	}

	// The global owner establishes the initial snapshot after workspace
	// discovery. Scoped collectors deliberately skip this boot scan; they exist
	// only to serve explicit RefreshPaths without duplicating global work.
	if c.scope == "" {
		time.Sleep(1 * time.Second)
		fullScan()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan()
		case replyCh := <-c.refresh:
			// Bodyless refresh cannot make every scoped daemon duplicate global
			// state. Explicit path refresh remains available below.
			if c.scope == "" {
				fullScan()
			}
			close(replyCh)
		case req := <-c.refreshPaths:
			req.reply <- scanPaths(req.paths)
		}
	}
}
