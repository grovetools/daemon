package collector

import (
	"context"
	"testing"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func ws(path string) *models.EnrichedWorkspace {
	return &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{Path: path}}
}

func keySet(paths ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		out[store.NormalizePathKey(p)] = struct{}{}
	}
	return out
}

func tiersByPath(items []sweepItem) map[string]sweepTier {
	out := make(map[string]sweepTier, len(items))
	for _, it := range items {
		out[it.ws.Path] = it.tier
	}
	return out
}

// The tiering must read only evidence the store already holds, and a workspace
// lands in the FIRST tier that matches it.
func TestClassifySweepItemsTiersByStoreEvidence(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	focused := ws("/repos/focused")
	job := ws("/repos/job")
	planning := ws("/repos/planning")
	planning.PlanStats = &models.PlanStats{Running: 1}
	dirty := ws("/repos/dirty")
	dirty.GitStatus = &git.ExtendedGitStatus{StatusInfo: &git.StatusInfo{IsDirty: true}}
	committed := ws("/repos/committed")
	committed.GitLanding = &git.LandingState{Computed: true, LastCommitAt: now.Add(-2 * time.Hour)}
	stale := ws("/repos/stale")
	stale.GitLanding = &git.LandingState{Computed: true, LastCommitAt: now.Add(-90 * 24 * time.Hour)}
	fresh := ws("/repos/new")

	sig := sweepSignals{
		focused:         keySet(focused.Path),
		active:          keySet(job.Path),
		newlyDiscovered: keySet(fresh.Path),
	}
	items := classifySweepItems(
		[]*models.EnrichedWorkspace{stale, committed, dirty, planning, job, focused, fresh}, sig, now)

	got := tiersByPath(items)
	want := map[string]sweepTier{
		focused.Path:   tierHot,
		job.Path:       tierActive,
		fresh.Path:     tierActive,
		planning.Path:  tierActive,
		dirty.Path:     tierWarm,
		committed.Path: tierWarm,
		stale.Path:     tierCold,
	}
	for path, wantTier := range want {
		if got[path] != wantTier {
			t.Errorf("%s tier = %s, want %s", path, got[path], wantTier)
		}
	}
	// The plan is emitted in tier order, which is the sweep order.
	for i := 1; i < len(items); i++ {
		if items[i-1].tier > items[i].tier {
			t.Fatalf("sweep plan out of tier order at %d: %+v", i, tiersByPath(items))
		}
	}
}

// A boot sweep has no previous scan to read, so the warm tier is empty rather
// than guessed at — everything falls to cold and the trickle covers it.
func TestClassifySweepItemsOnColdBootHasNoWarmTier(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	items := classifySweepItems(
		[]*models.EnrichedWorkspace{ws("/a"), ws("/b")},
		sweepSignals{focused: map[string]struct{}{}, active: map[string]struct{}{}}, now)
	for _, it := range items {
		if it.tier != tierCold {
			t.Fatalf("%s tier = %s, want cold on a store with no git data", it.ws.Path, it.tier)
		}
	}
}

// Job and session working directories promote their WORKSPACE, including when
// the work happens in a subdirectory; long-finished work does not.
func TestBuildSweepSignalsResolvesWorkingDirectories(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-time.Minute)
	state := store.State{
		Workspaces: map[string]*models.EnrichedWorkspace{
			"/repos/a": ws("/repos/a"),
			"/repos/b": ws("/repos/b"),
			"/repos/c": ws("/repos/c"),
			"/repos/d": ws("/repos/d"),
		},
		Jobs: map[string]*models.JobInfo{
			"running":   {WorkDir: "/repos/a/sub/dir", Status: "running"},
			"done-old":  {WorkDir: "/repos/d", Status: "completed", CompletedAt: &old, SubmittedAt: old},
			"done-just": {WorkDir: "/repos/b", Status: "completed", CompletedAt: &recent, SubmittedAt: old},
		},
		Sessions: map[string]*models.Session{
			"live": {WorkingDirectory: "/repos/c", Status: "running"},
			"gone": {WorkingDirectory: "/repos/d", Status: "completed", LastActivity: old},
		},
	}

	sig := buildSweepSignals(state, keySet(), now)
	for _, path := range []string{"/repos/a", "/repos/b", "/repos/c"} {
		if _, ok := sig.active[store.NormalizePathKey(path)]; !ok {
			t.Errorf("%s missing from the active set: %v", path, sig.active)
		}
	}
	if _, ok := sig.active[store.NormalizePathKey("/repos/d")]; ok {
		t.Error("a workspace whose only job and session finished hours ago must not be active")
	}
}

// Focus asserted AFTER the sweep was planned still preempts the tail. This is
// what makes tiering work at boot at all: the focus registry is in-memory and
// empty when the daemon starts, so every lease arrives mid-sweep.
func TestTakeSweepBatchPromotesLateFocus(t *testing.T) {
	p := defaultSweepPacing()
	p.coldBatch = 2
	pending := []sweepItem{
		{ws: ws("/a"), key: store.NormalizePathKey("/a"), tier: tierCold},
		{ws: ws("/b"), key: store.NormalizePathKey("/b"), tier: tierCold},
		{ws: ws("/c"), key: store.NormalizePathKey("/c"), tier: tierCold},
	}

	batch, tier := takeSweepBatch(&pending, keySet("/c"), p)
	if tier != tierHot {
		t.Fatalf("tier = %s, want hot", tier)
	}
	if len(batch) != 1 || batch[0].ws.Path != "/c" {
		t.Fatalf("batch = %+v, want just the newly focused workspace", batch)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want the other two still queued", len(pending))
	}

	batch, tier = takeSweepBatch(&pending, keySet(), p)
	if tier != tierCold || len(batch) != 2 {
		t.Fatalf("second batch = (%s, %d items), want (cold, 2)", tier, len(batch))
	}
}

func TestSweepPacingPausesOnlyPacedTiers(t *testing.T) {
	p := defaultSweepPacing()
	if got := p.pauseAfter(tierHot, time.Second); got != 0 {
		t.Errorf("hot pause = %s, want none — the hot tier is the latency budget", got)
	}
	// duty 0.1 means nine parts sleep per part work.
	if got := p.pauseAfter(tierCold, 100*time.Millisecond); got != 900*time.Millisecond {
		t.Errorf("cold pause = %s, want 900ms", got)
	}
	p.maxPause = 2 * time.Second
	if got := p.pauseAfter(tierCold, 10*time.Second); got != 2*time.Second {
		t.Errorf("pause = %s, want the cap: one slow batch must not stall the trickle", got)
	}
}

// fakeSweep builds a runner over a fixed clock, recording the order batches
// were scanned in and how long the runner asked to sleep.
type fakeSweep struct {
	clock   time.Time
	scanned [][]string
	tiers   []sweepTier
	pauses  []time.Duration
	events  []sweepProgress
	perWS   time.Duration
}

func (f *fakeSweep) runner(items []sweepItem, p sweepPacing, focus func() map[string]struct{}) *tieredSweep {
	if f.perWS == 0 {
		f.perWS = 10 * time.Millisecond
	}
	return &tieredSweep{
		pacing: p,
		items:  items,
		hotSet: focus,
		scan: func(batch []*models.EnrichedWorkspace, workers int) sweepBatchResult {
			paths := make([]string, 0, len(batch))
			for _, w := range batch {
				paths = append(paths, w.Path)
			}
			f.scanned = append(f.scanned, paths)
			f.clock = f.clock.Add(f.perWS * time.Duration(len(batch)))
			return sweepBatchResult{
				Scanned: len(batch),
				Emitted: len(batch),
				Cost:    f.perWS * time.Duration(len(batch)),
			}
		},
		progress: func(pr sweepProgress) {
			f.tiers = append(f.tiers, pr.Tier)
			f.events = append(f.events, pr)
		},
		sleep: func(ctx context.Context, d time.Duration) bool {
			f.pauses = append(f.pauses, d)
			f.clock = f.clock.Add(d)
			return ctx.Err() == nil
		},
		now: func() time.Time { return f.clock },
	}
}

// The whole point, end to end: hot first and unpaced, cold last and paced,
// with progress published per batch rather than once at the end.
func TestTieredSweepRunsHotFirstAndPacesTheTail(t *testing.T) {
	p := defaultSweepPacing()
	p.hotBatch, p.coldBatch = 4, 2
	items := []sweepItem{
		{ws: ws("/hot1"), key: store.NormalizePathKey("/hot1"), tier: tierHot},
		{ws: ws("/hot2"), key: store.NormalizePathKey("/hot2"), tier: tierHot},
		{ws: ws("/cold1"), key: store.NormalizePathKey("/cold1"), tier: tierCold},
		{ws: ws("/cold2"), key: store.NormalizePathKey("/cold2"), tier: tierCold},
		{ws: ws("/cold3"), key: store.NormalizePathKey("/cold3"), tier: tierCold},
	}
	f := &fakeSweep{clock: time.Unix(1_700_000_000, 0)}
	out := f.runner(items, p, func() map[string]struct{} { return keySet() }).run(context.Background())

	if len(f.scanned) != 3 {
		t.Fatalf("batches = %v, want hot(2) + cold(2) + cold(1)", f.scanned)
	}
	if f.scanned[0][0] != "/hot1" || f.scanned[0][1] != "/hot2" {
		t.Errorf("first batch = %v, want both hot workspaces", f.scanned[0])
	}
	if f.tiers[0] != tierHot || f.tiers[1] != tierCold {
		t.Errorf("tier order = %v, want hot then cold", f.tiers)
	}
	// One pause, after the first cold batch: none after hot, and none after
	// the final batch (nothing is left to pace).
	if len(f.pauses) != 1 {
		t.Fatalf("pauses = %v, want exactly one (after the first cold batch)", f.pauses)
	}
	if f.pauses[0] != 180*time.Millisecond { // 2 ws × 10ms work at duty 0.1
		t.Errorf("cold pause = %s, want 180ms", f.pauses[0])
	}
	if len(f.events) != 3 {
		t.Errorf("progress events = %d, want one per batch", len(f.events))
	}

	if !out.Completed || out.Scanned != 5 {
		t.Fatalf("outcome = %+v, want a completed 5-workspace sweep", out)
	}
	if out.TierScanned[tierHot] != 2 || out.TierScanned[tierCold] != 3 {
		t.Errorf("tier counts = %v, want 2 hot / 3 cold", out.TierScanned)
	}
	if out.HotElapsed != 20*time.Millisecond {
		t.Errorf("HotElapsed = %s, want the 20ms the hot tier actually took", out.HotElapsed)
	}
	if out.PacedScanned != 3 {
		t.Errorf("PacedScanned = %d, want the 3 cold workspaces", out.PacedScanned)
	}
	if out.Elapsed <= out.Work {
		t.Errorf("Elapsed %s must exceed Work %s — the difference IS the pacing", out.Elapsed, out.Work)
	}
}

// A workspace that becomes focused while the tail trickles is swept next,
// not minutes later.
func TestTieredSweepPreemptsForLateFocus(t *testing.T) {
	p := defaultSweepPacing()
	p.coldBatch = 1
	items := []sweepItem{
		{ws: ws("/a"), key: store.NormalizePathKey("/a"), tier: tierCold},
		{ws: ws("/b"), key: store.NormalizePathKey("/b"), tier: tierCold},
		{ws: ws("/c"), key: store.NormalizePathKey("/c"), tier: tierCold},
	}
	f := &fakeSweep{clock: time.Unix(1_700_000_000, 0)}
	calls := 0
	focus := func() map[string]struct{} {
		calls++
		if calls >= 2 {
			return keySet("/c")
		}
		return keySet()
	}
	f.runner(items, p, focus).run(context.Background())

	if len(f.scanned) != 3 {
		t.Fatalf("batches = %v", f.scanned)
	}
	if f.scanned[1][0] != "/c" {
		t.Errorf("second batch = %v, want the newly focused /c", f.scanned[1])
	}
	if f.tiers[1] != tierHot {
		t.Errorf("promoted batch ran as %s, want hot (full concurrency, no pacing)", f.tiers[1])
	}
}

// The escape hatch restores the pre-tiering behavior exactly: one flat pass,
// no pacing.
func TestTieredSweepDisabledRunsOneFlatPass(t *testing.T) {
	p := defaultSweepPacing()
	p.enabled = false
	items := []sweepItem{
		{ws: ws("/a"), key: store.NormalizePathKey("/a"), tier: tierCold},
		{ws: ws("/b"), key: store.NormalizePathKey("/b"), tier: tierCold},
	}
	f := &fakeSweep{clock: time.Unix(1_700_000_000, 0)}
	out := f.runner(items, p, func() map[string]struct{} { return keySet() }).run(context.Background())

	if len(f.scanned) != 1 || len(f.scanned[0]) != 2 {
		t.Fatalf("batches = %v, want a single flat pass", f.scanned)
	}
	if len(f.pauses) != 0 {
		t.Errorf("pauses = %v, want none when tiering is off", f.pauses)
	}
	if out.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", out.Scanned)
	}
}

// Shutdown must not have to wait out a minutes-long trickle.
func TestTieredSweepStopsOnCancel(t *testing.T) {
	p := defaultSweepPacing()
	p.coldBatch = 1
	items := []sweepItem{
		{ws: ws("/a"), key: store.NormalizePathKey("/a"), tier: tierCold},
		{ws: ws("/b"), key: store.NormalizePathKey("/b"), tier: tierCold},
		{ws: ws("/c"), key: store.NormalizePathKey("/c"), tier: tierCold},
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeSweep{clock: time.Unix(1_700_000_000, 0)}
	sweep := f.runner(items, p, func() map[string]struct{} { return keySet() })
	sweep.sleep = func(context.Context, time.Duration) bool {
		cancel()
		return false
	}
	out := sweep.run(ctx)

	if out.Completed {
		t.Error("a canceled sweep reported completion")
	}
	if out.Scanned != 1 {
		t.Errorf("scanned = %d, want to stop after the first batch", out.Scanned)
	}
}

// The started event describes the plan, so a progress bar can be drawn before
// any work happens.
func TestSweepStartPayloadCarriesThePlan(t *testing.T) {
	items := []sweepItem{
		{ws: ws("/a"), tier: tierHot},
		{ws: ws("/b"), tier: tierCold},
		{ws: ws("/c"), tier: tierCold},
	}
	p := sweepStartPayload("boot", "", items)
	if p.Total != 3 || p.TierTotals["hot"] != 1 || p.TierTotals["cold"] != 2 {
		t.Fatalf("start payload = %+v", p)
	}
	if p.Reason != "boot" {
		t.Errorf("reason = %q", p.Reason)
	}
}
