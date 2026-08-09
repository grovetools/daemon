package watcher

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	coregit "github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// fenceHandler is hygieneHandler with the rate floor OFF and a primed dir
// cache. The floor is disabled deliberately: it would hide the fence's effect
// behind its own throttling, and what these tests are about is whether a
// publish that changed nothing the stats reader sees can owe a rerun at all.
// Priming matters too — the first refresh is a cache miss, so it rescans and
// kicks like any real lifecycle event would.
func fenceHandler(t *testing.T) (*FlowHandler, string, string) {
	t.Helper()
	// runRefresh reads the worktree registry and the plan-selection state on
	// every publish, and these tests publish in tight loops. Point both at an
	// empty temp state dir: this keeps the test off the developer's real
	// registry (600+ entries, seconds per publish) as well as deterministic.
	t.Setenv("GROVE_HOME", t.TempDir())

	h, planDir := hygieneHandler(t)
	h.SetPlanStatsMinInterval(0)
	h.statsPass = func(uint64) {}

	h.runRefresh(true, nil)
	waitStatsIdle(t, h)
	return h, filepath.Dir(planDir), filepath.Join(planDir, "01-job.md")
}

// gatedStats installs a pass that blocks until the returned channel is closed,
// so a test can hold one pass in flight and publish underneath it.
func gatedStats(h *FlowHandler) (*atomic.Int64, chan struct{}, chan struct{}) {
	var runs atomic.Int64
	started := make(chan struct{}, 64)
	release := make(chan struct{})
	h.statsPass = func(uint64) {
		runs.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	return &runs, started, release
}

func awaitStart(t *testing.T, started chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the stats pass never started")
	}
}

// TestOverlayOnlyPublishesDoNotRerunTheStatsPass is the treadmill regression.
//
// runRefresh publishes for reasons that never touch a plan file — a git delta
// landing in the store re-projects every cached row through the fresh status.
// Those publishes used to advance the fence, and at portfolio scale a pass
// takes seconds, so one almost always raced it: the pass's own duration was
// what guaranteed its rerun. With no lifecycle change on disk, a pass must run
// once and exit no matter how many such publishes land underneath it.
func TestOverlayOnlyPublishesDoNotRerunTheStatsPass(t *testing.T) {
	h, _, _ := fenceHandler(t)
	runs, started, release := gatedStats(h)
	beforeReruns := telemetry.PlanStatsRerun.Value()

	h.kickPlanStats(false)
	awaitStart(t, started)

	// Twenty overlay-only re-projections while the pass reads disk. Each one
	// publishes a fresh index snapshot; none of them scans a plans directory.
	for i := 0; i < 20; i++ {
		h.runRefresh(false, nil)
	}
	close(release)
	waitStatsIdle(t, h)

	if got := runs.Load(); got != 1 {
		t.Fatalf("20 overlay-only publishes during a pass ran %d passes, want 1", got)
	}
	if got := telemetry.PlanStatsRerun.Value() - beforeReruns; got != 0 {
		t.Fatalf("planstats.rerun.count advanced by %d on overlay-only publishes, want 0", got)
	}
}

// TestStatsSeqAdvancesOnlyForStatsRelevantPublishes pins the fence's input
// directly: which publishes move it, and which are provably invisible to the
// aggregated counts.
func TestStatsSeqAdvancesOnlyForStatsRelevantPublishes(t *testing.T) {
	h, plansDir, job := fenceHandler(t)
	scope := map[string]struct{}{plansDir: {}}

	seq := h.statsSeq.Load()
	h.runRefresh(false, nil)
	if got := h.statsSeq.Load(); got != seq {
		t.Fatalf("an overlay-only publish advanced statsSeq %d -> %d", seq, got)
	}

	// A rescan that re-reads the same bytes is an unchanged-snapshot publish:
	// it did disk work, but nothing the counts derive from moved.
	h.runRefresh(false, scope)
	if got := h.statsSeq.Load(); got != seq {
		t.Fatalf("an unchanged rescan advanced statsSeq %d -> %d", seq, got)
	}

	// A job status change is exactly what the counts report.
	writeJobStatus(t, job, "completed")
	h.runRefresh(false, scope)
	if got := h.statsSeq.Load(); got == seq {
		t.Fatalf("a job status change left statsSeq at %d", got)
	}
	seq = h.statsSeq.Load()

	// So is a plan appearing.
	writeIndexedPlan(t, filepath.Join(plansDir, "second-plan"))
	h.runRefresh(false, scope)
	if got := h.statsSeq.Load(); got == seq {
		t.Fatalf("a new plan left statsSeq at %d", got)
	}
}

// TestPlanStatsLoopRerunsOnARelevantRace is the invariant the fence exists for,
// kept intact: when a publish DID move plan state while the pass was reading
// it, the pass's answer was thrown away by refreshPlanStats and the recount
// still has to happen. The rerun is attributed to the race, not to a kick.
func TestPlanStatsLoopRerunsOnARelevantRace(t *testing.T) {
	h := NewFlowHandler(nil, nil, 1).SetPlanStatsMinInterval(0)

	var runs atomic.Int64
	h.statsPass = func(uint64) {
		// Only the first pass is raced; a loop that reran forever would hang
		// the test rather than quietly pass it.
		if runs.Add(1) == 1 {
			h.statsSeq.Add(1)
		}
	}
	beforeRace := telemetry.PlanStatsRerunSeqRace.Value()
	beforeQueued := telemetry.PlanStatsRerunQueued.Value()

	h.kickPlanStats(false)
	awaitRuns(t, &runs, 2, 2*time.Second)
	waitStatsIdle(t, h)

	if got := runs.Load(); got != 2 {
		t.Fatalf("a single raced pass ran %d times, want 2", got)
	}
	if got := telemetry.PlanStatsRerunSeqRace.Value() - beforeRace; got != 1 {
		t.Fatalf("planstats.rerun.seq_race advanced by %d, want 1", got)
	}
	if got := telemetry.PlanStatsRerunQueued.Value() - beforeQueued; got != 0 {
		t.Fatalf("planstats.rerun.queued advanced by %d for an unqueued race, want 0", got)
	}
}

// TestStatsConvergeUnderLifecycleChurn is the other half of "narrower fence,
// same answer": real job-status churn arriving while a pass runs must still
// leave the LAST pass having observed the final on-disk state.
func TestStatsConvergeUnderLifecycleChurn(t *testing.T) {
	h, plansDir, job := fenceHandler(t)
	scope := map[string]struct{}{plansDir: {}}

	var runs atomic.Int64
	observed := make(chan string, 64)
	release := make(chan struct{})
	h.statsPass = func(uint64) {
		if runs.Add(1) == 1 {
			<-release
		}
		// Read what a real pass would read: the job's status, from disk.
		_, meta, _ := jobFrontmatter(job)
		observed <- meta.Status
	}

	h.kickPlanStats(false)
	for len(observed) == 0 && runs.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	// Churn underneath the in-flight pass, ending on the state that must win.
	for _, status := range []string{"running", "failed", "completed"} {
		writeJobStatus(t, job, status)
		h.runRefresh(false, scope)
	}
	close(release)

	deadline := time.Now().Add(3 * time.Second)
	last := ""
	for last != "completed" && time.Now().Before(deadline) {
		select {
		case last = <-observed:
		case <-time.After(50 * time.Millisecond):
		}
	}
	if last != "completed" {
		t.Fatalf("stats never converged on the final job status: last pass saw %q", last)
	}
	waitStatsIdle(t, h)
}

// TestGitDeltaStreamDoesNotRerunTheStatsPass drives the same claim through the
// wiring the daemon actually uses, rather than by calling runRefresh directly:
// git enrichment lands in the store, HandleStoreUpdate arms the debounce, and
// the timer publishes a re-projected index. That is the publish stream a busy
// host produces continuously, and none of it touches a plan file.
func TestGitDeltaStreamDoesNotRerunTheStatsPass(t *testing.T) {
	h, _, _ := fenceHandler(t)
	h.debounceMs = 20
	runs, started, release := gatedStats(h)
	beforeReruns := telemetry.PlanStatsRerun.Value()

	h.kickPlanStats(false)
	awaitStart(t, started)

	dirty := &coregit.ExtendedGitStatus{StatusInfo: &coregit.StatusInfo{IsDirty: true}}
	for i := 0; i < 15; i++ {
		h.HandleStoreUpdate(store.Update{
			Type:    store.UpdateWorkspacesDelta,
			Source:  "git_watcher",
			Payload: []*models.WorkspaceDelta{{Path: "/some/repo", GitStatus: dirty}},
		})
		time.Sleep(15 * time.Millisecond)
	}
	// Let the last armed debounce fire before the pass is allowed to finish.
	time.Sleep(60 * time.Millisecond)
	close(release)
	waitStatsIdle(t, h)

	if got := runs.Load(); got != 1 {
		t.Fatalf("a git-delta publish stream during a pass ran %d passes, want 1", got)
	}
	if got := telemetry.PlanStatsRerun.Value() - beforeReruns; got != 0 {
		t.Fatalf("planstats.rerun.count advanced by %d on a git-delta stream, want 0", got)
	}
}

func writeJobStatus(t *testing.T, path, status string) {
	t.Helper()
	body := "---\nid: job\ntitle: job\ntype: oneshot\nstatus: " + status + "\n---\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
