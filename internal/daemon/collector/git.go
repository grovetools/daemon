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
	//
	// It is ONE HOUR, and it runs on the global daemon ONLY — scan() returns
	// immediately when c.scope != "", so a scoped daemon has no reconciler at
	// all. This is the safety net any change to event routing or scan
	// suppression is priced against: on the global daemon a wrongly-dropped
	// event costs up to an hour of staleness, and on a scoped daemon it costs
	// staleness until something explicitly refreshes that path. Do not read
	// "the sweep will catch it" as a five-minute promise; it is not one.
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
	// pacing is the tiered sweep's throttle (see git_sweep.go). Held on the
	// collector so a test can shrink the batches and duty cycles.
	pacing sweepPacing
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
		pacing:       defaultSweepPacing(),
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

// Refresh triggers an immediate full git status sweep and blocks until the
// DEMANDED part of it is done — the focused and active tiers — not until the
// whole fleet has been rescanned.
//
// That is a deliberate narrowing of the old contract ("blocks until the full
// scan completes"). The sweep is now tier-ordered and paced: its cold tail is
// minutes long on purpose, and no caller of a bodyless /api/refresh wants to
// block for that. What every caller does want — the workspaces someone is
// looking at, plus anything newly discovered — is what the demanded tiers
// cover, and a workspace still in the cold tail carries GitStatusPending so
// its row cannot be misread as fresh.
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
	// swept records which workspaces a sweep has covered THIS daemon lifetime.
	// It backs two things: the GitStatusPending marker (a row nobody has
	// scanned yet must not read as fresh) and the newly-discovered promotion
	// in buildSweepSignals. Written only by the sweep goroutine, of which
	// exactly one runs at a time.
	swept := newSweptSet()
	bootSweepDone := false
	currentInterval := c.interval
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()
	// Publish the EFFECTIVE interval, not the configured one: dynamicInterval
	// rescales it by workspace count below, and "configured 10s / actually
	// running at 20s" is exactly the kind of divergence the telemetry tab
	// exists to make visible.
	telemetry.SetCollectorInterval(c.Name(), currentInterval)

	// send publishes a store update, giving up on shutdown. A tiered sweep
	// publishes once per batch and runs for minutes, so an unguarded send into
	// a channel whose consumer has already exited would hold the collector
	// goroutine open forever and hang the engine's shutdown wait.
	send := func(u store.Update) bool {
		select {
		case updates <- u:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// scanBatch runs git status over one batch of workspaces with the given
	// worker bound and publishes their deltas as soon as the batch finishes.
	//
	// Publishing per batch is load-bearing: a sweep is now tier-ordered and
	// paced, so accumulating every delta and publishing once at the end would
	// hold the hot tier's data — the data someone is looking at — hostage to
	// the cold tail's several minutes.
	scanBatch := func(toScan []*models.EnrichedWorkspace, workers int) sweepBatchResult {
		res := sweepBatchResult{Scanned: len(toScan)}
		if len(toScan) == 0 {
			return res
		}
		if workers < 1 {
			workers = 1
		}

		var wg sync.WaitGroup
		sem := make(chan struct{}, workers)
		var mu sync.Mutex
		var deltas []*models.WorkspaceDelta

		for _, ws := range toScan {
			wg.Add(1)
			go func(ws *models.EnrichedWorkspace) {
				defer wg.Done()
				sem <- struct{}{}        // Acquire
				defer func() { <-sem }() // Release

				// Per-workspace cost is summed (not wall-timed) so the sweep's
				// health measure stays independent of worker count and of the
				// pacing sleeps — see telemetry.RecordGitSweepTrickle.
				workStart := time.Now()
				defer func() {
					cost := time.Since(workStart)
					mu.Lock()
					res.Cost += cost
					mu.Unlock()
				}()

				firstScan := !swept.has(store.NormalizePathKey(ws.Path))

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
				// A first scan always emits, even when it found nothing to
				// change: its delta is what clears GitStatusPending. Suppress
				// it and a clean, unchanged workspace would stay marked
				// pending for the daemon's whole lifetime.
				if !gitChanged && !needsFileBackfill && !firstScan {
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
				if firstScan {
					notPending := false
					delta.GitStatusPending = &notPending
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

		res.Emitted = len(deltas)
		if len(deltas) > 0 {
			send(store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "git",
				Scanned: len(toScan),
				Payload: deltas,
			})
		}
		return res
	}

	// markPending publishes the honest-staleness marker for every workspace
	// this daemon has not swept yet, and reports how many that is.
	//
	// It exists because a tiered sweep makes "the row is on screen" and "the
	// row's git data has been computed" separate facts for minutes at a time.
	// Without an explicit marker, a rail would render an empty status as
	// clean, and a fleet-wide aggregation ("N dirty repos") would quietly
	// count unscanned repos as clean ones.
	markPending := func(items []sweepItem) int {
		deltas := make([]*models.WorkspaceDelta, 0)
		for _, it := range items {
			if swept.has(it.key) {
				continue
			}
			pending := true
			deltas = append(deltas, &models.WorkspaceDelta{Path: it.ws.Path, GitStatusPending: &pending})
		}
		telemetry.RecordGitSweepPending(len(deltas))
		if len(deltas) > 0 {
			send(store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "git",
				Scanned: len(deltas),
				Payload: deltas,
			})
		}
		return len(deltas)
	}

	// runSweep executes one tier-ordered, paced sweep of the in-scope fleet.
	// reason is "boot", "refresh" or "reconcile" — every full sweep the daemon
	// runs goes through here, so a rebuild-triggered refresh gets the same
	// treatment as boot rather than re-introducing a flat 681-workspace burst.
	//
	// demandDone is closed once the tiers a caller could reasonably wait for
	// (hot + active) are complete; the cold tail keeps running afterwards.
	runSweep := func(reason string, demandDone chan struct{}) {
		demandReleased := false
		releaseDemand := func() {
			if !demandReleased {
				demandReleased = true
				close(demandDone)
			}
		}
		defer releaseDemand()
		defer telemetry.RecordGitSweepIdle()

		state := st.Get()
		toScan := c.scopedWorkspaces(state.Workspaces)
		if len(toScan) == 0 {
			return
		}
		now := time.Now()
		sig := buildSweepSignals(state, st.GetFocus(), now)
		if bootSweepDone {
			// After the first sweep, "never swept" means "discovered since the
			// daemon started" — an import, a new worktree, a repo someone just
			// added. That is demand, so those do not go to the back of a
			// minutes-long queue. On the boot sweep itself every workspace is
			// unswept, which is why this is gated.
			sig.newlyDiscovered = swept.unswept(toScan)
		}
		items := classifySweepItems(toScan, sig, now)

		pending := markPending(items)
		ulog.Info("Starting tiered git sweep").
			Field("reason", reason).
			Field("workspaces", len(items)).
			Field("pending", pending).
			Field("scope", c.scope).
			Field("pid", os.Getpid()).
			Log(ctx)
		send(store.Update{
			Type:    store.UpdateSweepStarted,
			Source:  "git",
			Scanned: len(items),
			Payload: sweepStartPayload(reason, c.scope, items),
		})

		sweep := &tieredSweep{
			pacing: c.pacing,
			items:  items,
			hotSet: st.GetFocus,
			scan: func(batch []*models.EnrichedWorkspace, workers int) sweepBatchResult {
				res := scanBatch(batch, workers)
				for _, ws := range batch {
					swept.add(store.NormalizePathKey(ws.Path))
				}
				return res
			},
			progress: func(p sweepProgress) {
				telemetry.RecordGitSweepProgress(int(p.Tier)+1, p.TierDone, p.TierTotal, p.Done, p.Total)
				// The demanded tiers are done the moment the sweep starts
				// working on a paced one — that is what Refresh waits for.
				if p.Tier >= tierWarm {
					releaseDemand()
				}
				send(store.Update{
					Type:    store.UpdateSweepProgress,
					Source:  "git",
					Scanned: p.Done,
					Payload: sweepProgressPayload(reason, c.scope, p),
				})
			},
			sleep: sweepSleep,
			now:   time.Now,
		}
		out := sweep.run(ctx)
		releaseDemand()
		if out.Completed {
			// Only a sweep that actually finished licenses the
			// newly-discovered promotion above: after a sweep cut short by
			// shutdown, "never swept" would still mean "most of the fleet",
			// and promoting all of it would rebuild the burst this replaced.
			bootSweepDone = true
		}
		telemetry.RecordGitSweepPending(len(swept.unswept(toScan)))

		// Three separate measurements, because they answer three different
		// questions: how long the whole sweep took (informational, and long on
		// purpose), how long the part users feel took (the alarm), and how
		// expensive git itself was in the trickle (the other alarm).
		telemetry.RecordGitSweep(c.scope, out.Scanned, out.Work, out.Elapsed)
		telemetry.RecordGitSweepHot(c.scope, out.TierScanned[tierHot], out.HotElapsed)
		telemetry.RecordGitSweepTrickle(c.scope, out.PacedScanned, out.PacedCost, out.PacedElapsed)
		send(store.Update{
			Type:    store.UpdateSweepCompleted,
			Source:  "git",
			Scanned: out.Scanned,
			Payload: sweepDonePayload(reason, c.scope, out),
		})
		ulog.Info("Tiered git sweep finished").
			Field("reason", reason).
			Field("scanned", out.Scanned).
			Field("emitted", out.Emitted).
			Field("elapsed", out.Elapsed).
			Field("work", out.Work).
			Field("hot", out.HotElapsed).
			Field("completed", out.Completed).
			Field("scope", c.scope).
			Log(ctx)
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
				// An explicit refresh is a real scan, so it clears the
				// pending marker too — otherwise a workspace the user just
				// verified would keep claiming "not swept yet" until the cold
				// tail reached it, minutes later.
				notPending := false
				delta := &models.WorkspaceDelta{
					Path:                 ws.Path,
					GitStatus:            status,
					GitLanding:           landing,
					ChangedFiles:         files,
					BlobHashes:           hashes,
					ChangedFilesComputed: &computed,
					GitStatusPending:     &notPending,
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
			swept.add(store.NormalizePathKey(path))
		}

		if len(deltas) > 0 {
			send(store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "git",
				Scanned: len(resolved),
				Payload: deltas,
			})
		}
		return fresh
	}

	// Exactly one sweep runs at a time, and it runs on its OWN goroutine while
	// this loop keeps serving RefreshPaths and ticks. That is new, and it is
	// required: a sweep used to block the loop for its duration, which was
	// tolerable at 48 seconds and would not be at the several minutes a paced
	// cold tail takes — verify-on-reveal (/api/refresh with paths) must stay
	// answerable while the fleet trickles.
	var (
		sweepFinished chan struct{} // non-nil while a sweep runs
		sweepDemand   chan struct{} // non-nil until its demanded tiers are done
		refreshWaits  []chan struct{}
		refreshOwed   bool
	)
	releaseRefreshWaiters := func() {
		for _, w := range refreshWaits {
			close(w)
		}
		refreshWaits = nil
	}
	// startSweep launches a sweep unless one is already running or there is
	// nothing in scope to sweep. An empty pass must not spend the background
	// budget: leaving lastFullScan alone is what makes the next tick retry
	// instead of waiting a full backgroundScanInterval (an hour) for the first
	// real sweep on a daemon that booted before workspace discovery finished.
	startSweep := func(reason string) bool {
		if sweepFinished != nil {
			return false
		}
		if len(c.scopedWorkspaces(st.Get().Workspaces)) == 0 {
			return false
		}
		lastFullScan = time.Now()
		demand, finished := make(chan struct{}), make(chan struct{})
		sweepDemand, sweepFinished = demand, finished
		go func() {
			defer close(finished)
			runSweep(reason, demand)
		}()
		return true
	}

	// The global owner establishes the initial snapshot after workspace
	// discovery. Scoped collectors deliberately skip this boot scan; they exist
	// only to serve explicit RefreshPaths without duplicating global work.
	if c.scope == "" && sweepSleep(ctx, 1*time.Second) {
		startSweep("boot")
	}

	for {
		select {
		case <-ctx.Done():
			// Give an in-flight sweep a moment to observe the cancellation and
			// unwind (it checks between batches and inside its pacing sleep)
			// rather than returning while it still has git children to reap.
			if sweepFinished != nil {
				select {
				case <-sweepFinished:
				case <-time.After(2 * time.Second):
				}
			}
			return nil
		case <-ticker.C:
			// The hourly correctness reconciler. Focus no longer causes
			// timer-driven git forks: the recursive watcher and RefreshPaths
			// are the latency/demand paths. Scoped collectors are passive RPC
			// helpers and never run background sweeps.
			if c.scope == "" && time.Since(lastFullScan) >= backgroundScanInterval {
				reason := "reconcile"
				if lastFullScan.IsZero() {
					reason = "boot"
				}
				startSweep(reason)
			}
			newInterval := dynamicInterval(len(st.Get().Workspaces), c.interval)
			if newInterval != currentInterval {
				currentInterval = newInterval
				ticker.Reset(currentInterval)
				telemetry.SetCollectorInterval(c.Name(), currentInterval)
			}
		case <-sweepDemand:
			sweepDemand = nil
			releaseRefreshWaiters()
		case <-sweepFinished:
			sweepFinished, sweepDemand = nil, nil
			releaseRefreshWaiters()
			if refreshOwed {
				refreshOwed = false
				startSweep("refresh")
			}
		case replyCh := <-c.refresh:
			// Bodyless refresh cannot make every scoped daemon duplicate global
			// state. Explicit path refresh remains available below.
			if c.scope != "" {
				close(replyCh)
				continue
			}
			switch {
			case sweepFinished == nil:
				refreshWaits = append(refreshWaits, replyCh)
				if !startSweep("refresh") {
					releaseRefreshWaiters()
				}
			case sweepDemand != nil:
				// A sweep is running and has not finished its demanded tiers;
				// fold into it, and owe a follow-up so workspaces discovered
				// after it was planned still get covered.
				refreshWaits = append(refreshWaits, replyCh)
				refreshOwed = true
			default:
				// The running sweep's demanded tiers are already done, so the
				// caller's answer is ready now; the owed follow-up picks up
				// anything discovered since.
				refreshOwed = true
				close(replyCh)
			}
		case req := <-c.refreshPaths:
			req.reply <- scanPaths(req.paths)
		}
	}
}
