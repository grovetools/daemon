package collector

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// This file owns the shape of a full git status sweep: which workspaces go
// first, and how fast the rest are allowed to go.
//
// The problem it solves is a measured one. A flat full-fleet sweep of 681
// workspaces took 48.4s of pegged CPU at daemon boot — the single worst moment
// to spend it, because boot is also when treemux reconnects, SSE snapshots go
// out and agents resume, and the sweep itself deepens that contention. The
// sweep was flat in two senses: every workspace was equally urgent (they are
// not — the demand set is 1-10% of the fleet), and every workspace was scanned
// as fast as the worker pool allowed (nobody asked for the other 90% to be
// fast).
//
// So: order by demand, then pace by tier. The hot tier runs at full
// concurrency and is meant to finish within a couple of seconds; the cold tail
// runs on two workers with a duty cycle, turning a 48s burst into a few
// minutes of low single-digit CPU. Wall time gets much worse on purpose. What
// gets better is the wall time of the part somebody is looking at, and the
// machine's responsiveness while the rest happens.

// sweepTier orders a sweep from "somebody is looking at this right now" to
// "nothing in the daemon's state suggests anyone cares".
//
// The tiers read only evidence the store already holds. There is deliberately
// no scoring model: each tier is a predicate over the focus registry, job and
// session rows, and the previous scan's git data, and a workspace lands in the
// first one that matches.
type sweepTier int

const (
	// tierHot: a client holds a focus lease on this workspace (treemux's
	// visible git surfaces, nav peeks, git-viewer). This is the per-file tier
	// and the one whose completion time users actually feel.
	tierHot sweepTier = iota
	// tierActive: a job is queued/running/pending-user or finished recently
	// here, or a live session's working directory resolves here, or the plan
	// stats say a plan is in flight. Something is happening even if no window
	// is showing it.
	tierActive
	// tierWarm: the last scan left this repo dirty or its last commit is
	// recent — a repo that was moving is the one most likely to have moved
	// again. Empty on a boot sweep, which has no previous scan to read (git
	// data is not persisted across restarts); that is honest rather than
	// unfortunate — boot genuinely has no evidence here.
	tierWarm
	// tierCold: everything else. Correctness, not latency: it must happen, it
	// must not be felt.
	tierCold
	sweepTierCount
)

func (t sweepTier) String() string {
	switch t {
	case tierHot:
		return "hot"
	case tierActive:
		return "active"
	case tierWarm:
		return "warm"
	case tierCold:
		return "cold"
	}
	return "unknown"
}

const (
	// activeJobWindow is how long after a job's last lifecycle stamp its
	// workspace still counts as active. Long enough to cover "the agent just
	// finished and I am about to look at the diff", short enough that a fleet
	// with months of job history does not promote itself wholesale.
	activeJobWindow = 30 * time.Minute
	// recentCommitWindow is the same idea for commits: a repo committed to
	// today gets swept before one last touched in March.
	recentCommitWindow = 24 * time.Hour
	// maxWorkspaceAncestorWalk bounds the parent walk that maps a job or
	// session working directory onto its workspace.
	maxWorkspaceAncestorWalk = 24
)

// sweepSignals is the store-held evidence the tiering reads, resolved once per
// sweep from a single store snapshot so every workspace is classified against
// the same instant.
type sweepSignals struct {
	// focused is the aggregated focus registry (normalized paths). It is the
	// only signal re-read during the sweep — see tieredSweep.run.
	focused map[string]struct{}
	// active holds normalized workspace paths with job/session demand.
	active map[string]struct{}
	// newlyDiscovered holds workspaces this daemon has never swept, and is
	// populated ONLY once a first sweep has completed — after that, "never
	// swept" means "appeared since boot": an import, a new worktree, a repo
	// someone just added. That is demand, and it is why `grove repo add`
	// followed by a refresh does not have to wait out a cold tail. On the boot
	// sweep itself every workspace is unswept, which is exactly when this
	// signal would say nothing, so it stays empty there.
	newlyDiscovered map[string]struct{}
}

// sweptSet records which workspaces have been git-scanned this daemon
// lifetime. It is shared between the sweep goroutine and the collector's
// RefreshPaths path, hence the lock.
type sweptSet struct {
	mu sync.RWMutex
	m  map[string]struct{}
}

func newSweptSet() *sweptSet { return &sweptSet{m: make(map[string]struct{})} }

func (s *sweptSet) has(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.m[key]
	return ok
}

func (s *sweptSet) add(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		s.m[k] = struct{}{}
	}
}

// unswept returns the normalized keys of workspaces this set has never seen.
func (s *sweptSet) unswept(workspaces []*models.EnrichedWorkspace) map[string]struct{} {
	out := make(map[string]struct{})
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ws := range workspaces {
		key := store.NormalizePathKey(ws.Path)
		if _, ok := s.m[key]; !ok {
			out[key] = struct{}{}
		}
	}
	return out
}

// jobIsActive reports whether a job row keeps its workspace in the active
// tier: non-terminal work, or terminal work that finished inside
// activeJobWindow.
func jobIsActive(job *models.JobInfo, now time.Time) bool {
	switch job.Status {
	case "queued", "running", "pending_user", "pending":
		return true
	}
	for _, ts := range []*time.Time{job.CompletedAt, job.StartedAt} {
		if ts != nil && now.Sub(*ts) < activeJobWindow {
			return true
		}
	}
	return !job.SubmittedAt.IsZero() && now.Sub(job.SubmittedAt) < activeJobWindow
}

// sessionIsActive reports whether a session row keeps its workspace in the
// active tier. Terminal sessions still count while their last activity is
// inside the window — a just-exited agent's repo is exactly what someone looks
// at next.
func sessionIsActive(sess *models.Session, now time.Time) bool {
	switch sess.Status {
	case "running", "idle", "pending_user", "waiting":
		return true
	}
	return !sess.LastActivity.IsZero() && now.Sub(sess.LastActivity) < activeJobWindow
}

// planStatsActive reports whether a workspace's plan enrichment says work is
// in flight there.
func planStatsActive(ps *models.PlanStats) bool {
	return ps != nil && (ps.Running > 0 || ps.Pending > 0)
}

// buildSweepSignals resolves the store snapshot into tier evidence. Job and
// session working directories are mapped onto workspaces by walking up from
// the directory until a known workspace matches, so a job running in a
// subdirectory still promotes its repo.
func buildSweepSignals(state store.State, focused map[string]struct{}, now time.Time) sweepSignals {
	byKey := make(map[string]struct{}, len(state.Workspaces))
	for _, ws := range state.Workspaces {
		byKey[store.NormalizePathKey(ws.Path)] = struct{}{}
	}
	sig := sweepSignals{focused: focused, active: make(map[string]struct{})}
	mark := func(dir string) {
		if dir == "" {
			return
		}
		for i := 0; i < maxWorkspaceAncestorWalk; i++ {
			key := store.NormalizePathKey(dir)
			if _, ok := byKey[key]; ok {
				sig.active[key] = struct{}{}
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return
			}
			dir = parent
		}
	}
	for _, job := range state.Jobs {
		if jobIsActive(job, now) {
			mark(job.WorkDir)
		}
	}
	for _, sess := range state.Sessions {
		if sessionIsActive(sess, now) {
			mark(sess.WorkingDirectory)
		}
	}
	return sig
}

// sweepItem is one workspace with its resolved tier and normalized key.
type sweepItem struct {
	ws   *models.EnrichedWorkspace
	key  string
	tier sweepTier
}

// classifySweepItems assigns each workspace its tier and returns the sweep
// order: tier first, then path, so a sweep is reproducible and a test can read
// it. Focus is re-evaluated per batch during the run, so this order is the
// starting plan rather than a fixed schedule.
func classifySweepItems(workspaces []*models.EnrichedWorkspace, sig sweepSignals, now time.Time) []sweepItem {
	items := make([]sweepItem, 0, len(workspaces))
	for _, ws := range workspaces {
		key := store.NormalizePathKey(ws.Path)
		items = append(items, sweepItem{ws: ws, key: key, tier: tierOf(ws, key, sig, now)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].tier != items[j].tier {
			return items[i].tier < items[j].tier
		}
		return items[i].ws.Path < items[j].ws.Path
	})
	return items
}

func tierOf(ws *models.EnrichedWorkspace, key string, sig sweepSignals, now time.Time) sweepTier {
	if _, ok := sig.focused[key]; ok {
		return tierHot
	}
	if _, ok := sig.active[key]; ok {
		return tierActive
	}
	if _, ok := sig.newlyDiscovered[key]; ok {
		return tierActive
	}
	if planStatsActive(ws.PlanStats) {
		return tierActive
	}
	if ws.GitStatus != nil && ws.GitStatus.StatusInfo != nil && ws.GitStatus.IsDirty {
		return tierWarm
	}
	if ws.GitLanding != nil && !ws.GitLanding.LastCommitAt.IsZero() &&
		now.Sub(ws.GitLanding.LastCommitAt) < recentCommitWindow {
		return tierWarm
	}
	return tierCold
}

// sweepPacing is the throttle. Two knobs per tier — how many git children may
// run at once, and what fraction of wall time the sweep is allowed to spend
// working — plus the batch size that decides how often progress is published
// and pacing is re-evaluated.
//
// Deliberately NOT here: renicing the cold tier's git children so the
// scheduler favors TUIs. Go gives no post-fork/pre-exec hook, and on Darwin
// setpriority(PRIO_PROCESS, 0, …) renices the calling PROCESS — the whole
// daemon, hot scans included — so the only correct implementations are
// wrapping every cold `git` in nice(1) or teaching core/git an exec seam.
// Both are a change to a package every repo forks git through, for a second
// helping of what the duty cycle already delivers: the cold tail's average
// CPU is bounded by coldWorkers × duty before the scheduler is consulted at
// all. Revisit only if measurement shows the trickle's BURSTS (not its
// average) are what a TUI feels.
type sweepPacing struct {
	// enabled=false restores the pre-tiering behavior: one flat full-concurrency
	// pass. The escape hatch for "the pacing is the problem".
	enabled     bool
	hotWorkers  int
	coldWorkers int
	hotBatch    int
	coldBatch   int
	// duty is the target work fraction per tier. 1.0 means "no pacing"; 0.1
	// means nine parts sleep to one part work, so ~2 cold workers average
	// ~0.2 of a core.
	duty [sweepTierCount]float64
	// maxPause caps a single inter-batch sleep so one pathologically slow
	// batch cannot stall the trickle for minutes.
	maxPause time.Duration
}

// defaultSweepPacing is the shipped policy, overridable from the environment.
//
// The knobs live in the environment rather than core's DaemonConfig on purpose
// (same reasoning as GROVE_PLANSTATS_MIN_INTERVAL_MS): a field there
// invalidates grove's generated config schema, which is a third repo, and this
// is a dial you turn while diagnosing rather than a setting you keep.
func defaultSweepPacing() sweepPacing {
	p := sweepPacing{
		enabled:     true,
		hotWorkers:  gitWorkers,
		coldWorkers: 2,
		hotBatch:    32,
		coldBatch:   8,
		maxPause:    10 * time.Second,
	}
	p.duty = [sweepTierCount]float64{
		tierHot:    1.0, // full speed: this is the ~2s budget
		tierActive: 0.5,
		tierWarm:   0.25,
		tierCold:   0.1, // the trickle
	}
	if v := os.Getenv("GROVE_SWEEP_TIERED"); v == "0" || strings.EqualFold(v, "false") {
		p.enabled = false
	}
	if n, err := strconv.Atoi(os.Getenv("GROVE_SWEEP_COLD_WORKERS")); err == nil && n > 0 {
		p.coldWorkers = n
	}
	if f, err := strconv.ParseFloat(os.Getenv("GROVE_SWEEP_COLD_DUTY"), 64); err == nil && f > 0 && f <= 1 {
		p.duty[tierCold] = f
	}
	return p
}

func (p sweepPacing) workersFor(t sweepTier) int {
	switch t {
	case tierHot, tierActive:
		return max(p.hotWorkers, 1)
	default:
		return max(p.coldWorkers, 1)
	}
}

func (p sweepPacing) batchFor(t sweepTier) int {
	switch t {
	case tierHot, tierActive:
		return max(p.hotBatch, 1)
	default:
		return max(p.coldBatch, 1)
	}
}

// pauseAfter returns how long to idle after a batch that took work to scan, so
// that the tier averages its duty cycle. Zero for an unpaced tier.
func (p sweepPacing) pauseAfter(t sweepTier, work time.Duration) time.Duration {
	duty := p.duty[t]
	if duty <= 0 || duty >= 1 || work <= 0 {
		return 0
	}
	pause := time.Duration(float64(work) * (1/duty - 1))
	if p.maxPause > 0 && pause > p.maxPause {
		return p.maxPause
	}
	return pause
}

// sweepBatchResult is what scanning one batch reports back.
type sweepBatchResult struct {
	// Scanned counts workspaces the batch attempted (including failures);
	// Emitted counts the ones whose state actually changed.
	Scanned int
	Emitted int
	// Cost is the SUMMED per-workspace git time, not the batch's wall time.
	// Summing makes it independent of worker count and of pacing, which is
	// what lets the re-tuned slow-sweep warning tell "git is slow" apart from
	// "we are deliberately going slowly".
	Cost time.Duration
}

// sweepProgress is one observation handed to the runner's progress callback.
type sweepProgress struct {
	Tier      sweepTier
	TierDone  int
	TierTotal int
	Done      int
	Total     int
	Emitted   int
	Elapsed   time.Duration
	Work      time.Duration
	// Cost is the summed per-workspace git time so far.
	Cost time.Duration
}

// sweepOutcome is the whole sweep, as recorded when it ends (or is cut short
// by shutdown).
type sweepOutcome struct {
	Total   int
	Scanned int
	Emitted int
	Elapsed time.Duration
	Work    time.Duration
	Cost    time.Duration
	// TierTotals is the initial plan; TierScanned what actually happened. They
	// differ when focus promotes workspaces mid-sweep.
	TierTotals  [sweepTierCount]int
	TierScanned [sweepTierCount]int
	// HotElapsed is wall time from sweep start until the last hot-tier
	// workspace was scanned — the number a user feels, and the one the
	// re-tuned warning fires on. Zero when nothing was hot.
	HotElapsed time.Duration
	// PacedCost/PacedScanned/PacedElapsed cover the paced tiers only (warm +
	// cold): the trickle's own cost and throughput, kept apart from the hot
	// burst so neither measurement contaminates the other.
	PacedCost    time.Duration
	PacedScanned int
	PacedElapsed time.Duration
	// Completed is false when the context was canceled mid-sweep.
	Completed bool
}

// tieredSweep runs one full sweep. Its dependencies are injected so the
// ordering and pacing logic is testable without forking git or a real store.
type tieredSweep struct {
	pacing sweepPacing
	items  []sweepItem
	// hotSet re-reads the focus registry. It is called once per batch, which
	// is what makes a late-arriving client's focus preempt the trickle: at
	// boot the registry is empty (leases are in-memory and clients re-assert
	// on stream-ready), so without re-reading, the tiering would be a plan
	// made before any of the evidence arrived.
	hotSet func() map[string]struct{}
	// scan scans one batch with the given worker bound and publishes its
	// deltas. Publishing per batch — rather than accumulating the whole sweep
	// and publishing once — is what makes hot data appear in ~2s instead of
	// after the trickle.
	scan func(batch []*models.EnrichedWorkspace, workers int) sweepBatchResult
	// progress is called after every batch.
	progress func(sweepProgress)
	// sleep paces between batches; returns false when the context ended.
	sleep func(ctx context.Context, d time.Duration) bool
	now   func() time.Time
}

// run executes the sweep and returns what happened.
func (s *tieredSweep) run(ctx context.Context) sweepOutcome {
	start := s.now()
	out := sweepOutcome{Total: len(s.items)}
	for _, it := range s.items {
		out.TierTotals[it.tier]++
	}
	if len(s.items) == 0 {
		out.Completed = true
		return out
	}

	pending := make([]sweepItem, len(s.items))
	copy(pending, s.items)
	// Tier decomposition moves as focus promotes workspaces, so per-tier
	// totals are recomputed per batch rather than pinned to the plan.
	for len(pending) > 0 {
		if ctx.Err() != nil {
			return s.finish(out, start, false)
		}
		focused := s.hotSet()
		batch, tier := takeSweepBatch(&pending, focused, s.pacing)
		if len(batch) == 0 {
			return s.finish(out, start, true)
		}

		workspaces := make([]*models.EnrichedWorkspace, 0, len(batch))
		for _, it := range batch {
			workspaces = append(workspaces, it.ws)
		}
		batchStart := s.now()
		res := s.scan(workspaces, s.pacing.workersFor(tier))
		work := s.now().Sub(batchStart)

		out.Scanned += res.Scanned
		out.Emitted += res.Emitted
		out.Work += work
		out.Cost += res.Cost
		out.TierScanned[tier] += len(batch)
		if tier == tierHot {
			out.HotElapsed = s.now().Sub(start)
		}
		paced := tier >= tierWarm
		if paced {
			out.PacedCost += res.Cost
			out.PacedScanned += res.Scanned
			out.PacedElapsed += work
		}

		tierDone, tierTotal := tierCounts(pending, focused, tier, out.TierScanned[tier])
		if s.progress != nil {
			s.progress(sweepProgress{
				Tier:      tier,
				TierDone:  tierDone,
				TierTotal: tierTotal,
				Done:      out.Scanned,
				Total:     out.Total,
				Emitted:   out.Emitted,
				Elapsed:   s.now().Sub(start),
				Work:      out.Work,
				Cost:      out.Cost,
			})
		}

		if pause := s.pacing.pauseAfter(tier, work); pause > 0 && len(pending) > 0 {
			if paced {
				out.PacedElapsed += pause
			}
			if !s.sleep(ctx, pause) {
				return s.finish(out, start, false)
			}
		}
	}
	return s.finish(out, start, true)
}

func (s *tieredSweep) finish(out sweepOutcome, start time.Time, completed bool) sweepOutcome {
	out.Elapsed = s.now().Sub(start)
	out.Completed = completed
	return out
}

// takeSweepBatch removes and returns the next batch: the highest-priority tier
// still pending, capped at that tier's batch size. Priority is the item's
// planned tier, except that a workspace focused RIGHT NOW is hot regardless of
// what it was when the sweep was planned — that promotion is the whole point
// of re-reading focus per batch.
func takeSweepBatch(pending *[]sweepItem, focused map[string]struct{}, p sweepPacing) ([]sweepItem, sweepTier) {
	items := *pending
	if len(items) == 0 {
		return nil, tierCold
	}
	effective := func(it sweepItem) sweepTier {
		if _, ok := focused[it.key]; ok {
			return tierHot
		}
		return it.tier
	}
	if !p.enabled {
		// Escape hatch: one flat pass at full concurrency, as before tiering.
		*pending = nil
		return items, tierHot
	}

	best := tierCold
	for _, it := range items {
		if t := effective(it); t < best {
			best = t
		}
		if best == tierHot {
			break
		}
	}
	limit := p.batchFor(best)
	batch := make([]sweepItem, 0, limit)
	rest := make([]sweepItem, 0, len(items))
	for _, it := range items {
		if len(batch) < limit && effective(it) == best {
			batch = append(batch, it)
			continue
		}
		rest = append(rest, it)
	}
	*pending = rest
	return batch, best
}

// tierCounts reports progress within the tier just scanned: how many of it are
// done and how many it now holds (done + still pending at that effective
// tier).
func tierCounts(pending []sweepItem, focused map[string]struct{}, tier sweepTier, done int) (int, int) {
	remaining := 0
	for _, it := range pending {
		t := it.tier
		if _, ok := focused[it.key]; ok {
			t = tierHot
		}
		if t == tier {
			remaining++
		}
	}
	return done, done + remaining
}

// sweepSleep is the production pacing sleep: interruptible by shutdown.
func sweepSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// sweepProgressPayload renders a progress observation onto the wire shape.
func sweepProgressPayload(reason, scope string, p sweepProgress) *models.GitSweepProgress {
	return &models.GitSweepProgress{
		Reason:    reason,
		Scope:     scope,
		Tier:      p.Tier.String(),
		TierDone:  p.TierDone,
		TierTotal: p.TierTotal,
		Done:      p.Done,
		Total:     p.Total,
		Emitted:   p.Emitted,
		ElapsedMS: p.Elapsed.Milliseconds(),
		WorkMS:    p.Work.Milliseconds(),
	}
}

// sweepStartPayload announces a sweep and the shape of the work it plans.
func sweepStartPayload(reason, scope string, items []sweepItem) *models.GitSweepProgress {
	totals := make(map[string]int, sweepTierCount)
	for _, it := range items {
		totals[it.tier.String()]++
	}
	return &models.GitSweepProgress{
		Reason:     reason,
		Scope:      scope,
		Total:      len(items),
		TierTotals: totals,
	}
}

// sweepDonePayload closes a sweep out.
func sweepDonePayload(reason, scope string, out sweepOutcome) *models.GitSweepProgress {
	return &models.GitSweepProgress{
		Reason:    reason,
		Scope:     scope,
		Done:      out.Scanned,
		Total:     out.Total,
		Emitted:   out.Emitted,
		ElapsedMS: out.Elapsed.Milliseconds(),
		WorkMS:    out.Work.Milliseconds(),
	}
}
