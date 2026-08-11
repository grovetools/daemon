package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// TestClassifyPlanEventAllowlistsOnlyWhatTheReadersOpen pins the filter to the
// two readers it exists to protect: loadIndexedPlans/LoadPlanLenient for the
// index, and countPlanStats/processPlanCounts for the aggregated stats. Both
// enumerate a plans directory, then read each plan's config and its TOP-LEVEL
// .md files, and neither descends further — so everything an agent writes
// while it runs must classify as unreadable.
func TestClassifyPlanEventAllowlistsOnlyWhatTheReadersOpen(t *testing.T) {
	plansDir := filepath.Join("/nb", "notespaces", "grovetools", "plans")
	plan := filepath.Join(plansDir, "perf-audit")

	cases := []struct {
		name string
		path string
		want planEventClass
	}{
		// The churn this job exists to stop.
		{"artifact file", filepath.Join(plan, ".artifacts", "job-abc", "commands.jsonl"), planEventNone},
		{"artifact job log", filepath.Join(plan, ".artifacts", "job-abc", "job.log"), planEventNone},
		{"artifact dir entry", filepath.Join(plan, ".artifacts"), planEventNone},
		{"artifact markdown", filepath.Join(plan, ".artifacts", "job-abc", "notes.md"), planEventNone},
		{"claude settings", filepath.Join(plan, ".claude", "settings.local.json"), planEventNone},
		{"plan job lock", filepath.Join(plan, "01-job.md.lock"), planEventNone},
		{"plan hidden lock", filepath.Join(plan, ".flow-jobs.lock"), planEventNone},
		{"plan init journal", filepath.Join(plan, ".init-journal.json"), planEventNone},
		{"plans root output log", filepath.Join(plansDir, ".init-thing.output.log"), planEventNone},
		{"plans root DS_Store", filepath.Join(plansDir, ".DS_Store"), planEventNone},

		// Lifecycle: what the readers actually open.
		{"job markdown", filepath.Join(plan, "01-job.md"), planEventJob},
		{"spec markdown", filepath.Join(plan, "spec.md"), planEventJob},
		{"plan config", filepath.Join(plan, ".grove-plan.yml"), planEventConfig},
		{"legacy plan config", filepath.Join(plan, "config.yml"), planEventConfig},
		{"plan directory", plan, planEventMembership},
		{"plans root itself", plansDir, planEventMembership},
		{"rules dir", filepath.Join(plan, "rules"), planEventOther},
		{"rules file", filepath.Join(plan, "rules", "01-job.md.rules"), planEventOther},

		// .archive is a container, so archived plans classify identically.
		{"archive container", filepath.Join(plansDir, ".archive"), planEventMembership},
		{"archived plan dir", filepath.Join(plansDir, ".archive", "old"), planEventMembership},
		{"archived job", filepath.Join(plansDir, ".archive", "old", "01-job.md"), planEventJob},
		{"archived artifact", filepath.Join(plansDir, ".archive", "old", ".artifacts", "x.log"), planEventNone},

		// Unreasonable-to-classify paths must fail open, never closed.
		{"outside the plans root", filepath.Join("/nb", "elsewhere", "file.md"), planEventOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPlanEvent(plansDir, tc.path); got != tc.want {
				t.Fatalf("classifyPlanEvent(%q) = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}

// hygieneHandler builds a FlowHandler whose watch set covers one real plans
// directory, with a debounce long enough that no scheduled refresh can
// actually fire during the test (the assertion is on whether one was ARMED).
func hygieneHandler(t *testing.T) (*FlowHandler, string) {
	t.Helper()
	notebookRoot := t.TempDir()
	plansDir := filepath.Join(notebookRoot, "notespaces", "fixture-repo", "plans")
	planDir := filepath.Join(plansDir, "live-plan")
	writeIndexedPlan(t, planDir)
	if err := os.MkdirAll(filepath.Join(planDir, ".artifacts", "job-abc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Notebooks: &config.NotebooksConfig{
		Definitions: map[string]*config.Notebook{"test": {
			RootDir: notebookRoot, PlansPathTemplate: "notespaces/{{ .Workspace.Name }}/plans",
		}},
		Rules: &config.NotebookRules{Default: "test"},
	}}
	h := NewFlowHandler(store.New(), cfg, 600000)
	node := &workspace.WorkspaceNode{
		Name: "fixture-repo", Path: t.TempDir(),
		Kind: workspace.KindEcosystemRoot, NotebookName: "test",
	}
	h.ComputeWatchPaths([]*models.EnrichedWorkspace{{WorkspaceNode: node}})
	return h, planDir
}

func (h *FlowHandler) refreshArmed() bool {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	return h.refreshTimer != nil || h.pendingAll || len(h.pendingDirs) > 0
}

// TestHandleEventsDropsArtifactAndLogWritesBeforeTheDebounce is the regression
// for the measured treadmill: a running agent appending to `.artifacts/` and a
// job log inside a watched plan must not arm the refresh — which is what would
// go on to kick the portfolio-wide stats recount.
func TestHandleEventsDropsArtifactAndLogWritesBeforeTheDebounce(t *testing.T) {
	h, planDir := hygieneHandler(t)
	before := telemetry.PlanStatsEventsSuppressed.Value()

	noise := []fsnotify.Event{
		{Name: filepath.Join(planDir, ".artifacts", "job-abc", "commands.jsonl"), Op: fsnotify.Write},
		{Name: filepath.Join(planDir, ".artifacts", "job-abc", "job.log"), Op: fsnotify.Write},
		{Name: filepath.Join(planDir, ".artifacts", "job-abc", "briefing.xml"), Op: fsnotify.Create},
		{Name: filepath.Join(planDir, "01-job.md.lock"), Op: fsnotify.Create},
	}
	if err := h.HandleEvents(context.Background(), noise); err != nil {
		t.Fatal(err)
	}
	if h.refreshArmed() {
		t.Fatal("artifact/log/lock writes armed a plan index refresh")
	}
	if got := telemetry.PlanStatsEventsSuppressed.Value() - before; got != int64(len(noise)) {
		t.Fatalf("suppressed counter advanced by %d, want %d", got, len(noise))
	}

	// The same batch with one real job edit in it must still arm the refresh,
	// scoped to that plan's plans directory.
	if err := h.HandleEvents(context.Background(), append(noise,
		fsnotify.Event{Name: filepath.Join(planDir, "01-job.md"), Op: fsnotify.Write})); err != nil {
		t.Fatal(err)
	}
	if !h.refreshArmed() {
		t.Fatal("a job frontmatter edit did not arm a refresh")
	}
	h.refreshMu.Lock()
	_, scoped := h.pendingDirs[filepath.Dir(planDir)]
	h.refreshMu.Unlock()
	if !scoped {
		t.Fatalf("refresh scope %v does not name the plans dir %q", h.pendingDirs, filepath.Dir(planDir))
	}
}

// TestJobTranscriptAppendsDoNotTriggerARescan is the content half of the
// filter, and the one that carries the measured churn: an agent appending its
// chat transcript to a job `.md` writes the very file the index reads, but
// both readers stop at the closing frontmatter fence, so the append changes
// nothing they can report. A frontmatter edit in the same file must still get
// through immediately.
func TestJobTranscriptAppendsDoNotTriggerARescan(t *testing.T) {
	h, planDir := hygieneHandler(t)
	job := filepath.Join(planDir, "01-job.md")
	write := []fsnotify.Event{{Name: job, Op: fsnotify.Write}}

	// First sighting: the daemon cannot know what it missed while down.
	if err := h.HandleEvents(context.Background(), write); err != nil {
		t.Fatal(err)
	}
	if !h.refreshArmed() {
		t.Fatal("the first write of a job file did not arm a refresh")
	}

	for i := 0; i < 5; i++ {
		h.resetRefreshState()
		appendToFile(t, job, "\n## assistant\n\nanother transcript turn\n")
		if err := h.HandleEvents(context.Background(), write); err != nil {
			t.Fatal(err)
		}
		if h.refreshArmed() {
			t.Fatalf("transcript append %d armed a refresh", i+1)
		}
	}

	h.resetRefreshState()
	if err := os.WriteFile(job, []byte("---\nid: job\ntitle: job\ntype: oneshot\nstatus: completed\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleEvents(context.Background(), write); err != nil {
		t.Fatal(err)
	}
	if !h.refreshArmed() {
		t.Fatal("a status change in job frontmatter was suppressed as inert")
	}
}

// TestJobFrontmatterReadsOnlyTheBlock pins the two properties absorbJobFile
// depends on: a half-written file (no closing fence yet) must not memoize as
// "seen", and the block must be read without the body behind it.
func TestJobFrontmatterReadsOnlyTheBlock(t *testing.T) {
	dir := t.TempDir()

	half := filepath.Join(dir, "half.md")
	if err := os.WriteFile(half, []byte("---\nid: job\nstatus: pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := jobFrontmatter(half); ok {
		t.Fatal("a file with no closing fence parsed as complete frontmatter")
	}

	full := filepath.Join(dir, "full.md")
	body := strings.Repeat("transcript line\n", 200000)
	if err := os.WriteFile(full, []byte("---\nid: job\nstatus: running\n---\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, meta, ok := jobFrontmatter(full)
	if !ok || meta.ID != "job" || meta.Status != "running" {
		t.Fatalf("frontmatter parse: ok=%v meta=%+v", ok, meta)
	}
	if strings.Contains(raw, "transcript line") || len(raw) > 128 {
		t.Fatalf("frontmatter block ran into the body (%d bytes)", len(raw))
	}
}

func appendToFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func (h *FlowHandler) resetRefreshState() {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
		h.refreshTimer = nil
	}
	h.pendingAll = false
	h.pendingDirs = nil
}

// TestHandleEventsKeepsPlanMembershipAndConfigEdges guards the other side of
// the filter: the edges that MUST survive it.
func TestHandleEventsKeepsPlanMembershipAndConfigEdges(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
		op   fsnotify.Op
	}{
		{"plan config write", ".grove-plan.yml", fsnotify.Write},
		{"job file create", "02-new.md", fsnotify.Create},
		{"job file removal", "01-job.md", fsnotify.Remove},
		{"rules write", filepath.Join("rules", "01-job.md.rules"), fsnotify.Write},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, planDir := hygieneHandler(t)
			if err := h.HandleEvents(context.Background(), []fsnotify.Event{
				{Name: filepath.Join(planDir, tc.rel), Op: tc.op},
			}); err != nil {
				t.Fatal(err)
			}
			if !h.refreshArmed() && !h.statsWasKicked() {
				t.Fatalf("%s was suppressed", tc.name)
			}
		})
	}
}

// statsWasKicked reports whether the synchronous lifecycle path already ran
// (which drains the pending scope rather than leaving a timer armed).
func (h *FlowHandler) statsWasKicked() bool {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	return h.statsRunning || h.statsQueued || !h.statsLastRun.IsZero()
}

// countingStats swaps in a pass that records its runs instead of reading disk.
func countingStats(h *FlowHandler) (*atomic.Int64, chan struct{}) {
	var runs atomic.Int64
	ran := make(chan struct{}, 64)
	h.statsPass = func(uint64) {
		runs.Add(1)
		select {
		case ran <- struct{}{}:
		default:
		}
	}
	return &runs, ran
}

func awaitRuns(t *testing.T, runs *atomic.Int64, want int64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for runs.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("stats passes = %d, want %d within %s", runs.Load(), want, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPlanStatsFloorCollapsesAStreamIntoOnePass is the rate floor's core
// claim: a continuous stream of legitimate lifecycle kicks costs one pass per
// interval, not one per kick.
func TestPlanStatsFloorCollapsesAStreamIntoOnePass(t *testing.T) {
	h := NewFlowHandler(nil, nil, 1).SetPlanStatsMinInterval(10 * time.Second)
	runs, _ := countingStats(h)

	for i := 0; i < 25; i++ {
		h.kickPlanStats(false)
	}
	awaitRuns(t, runs, 1, 2*time.Second)
	time.Sleep(150 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("25 kicks under a 10s floor ran %d passes, want 1", got)
	}
	// The debt is remembered, not dropped.
	h.statsMu.Lock()
	queued, armed := h.statsQueued, h.statsFloorTimer != nil
	h.statsMu.Unlock()
	if !queued || !armed {
		t.Fatalf("suppressed kicks left no trailing run: queued=%v armed=%v", queued, armed)
	}
}

// TestPlanStatsFloorStillConvergesViaTrailingRun is the no-lost-updates half:
// the final state must be recomputed once the floor expires, with nothing
// further arriving to trigger it.
func TestPlanStatsFloorStillConvergesViaTrailingRun(t *testing.T) {
	h := NewFlowHandler(nil, nil, 1).SetPlanStatsMinInterval(120 * time.Millisecond)
	runs, _ := countingStats(h)

	h.kickPlanStats(false)
	awaitRuns(t, runs, 1, 2*time.Second)
	h.kickPlanStats(false) // inside the floor: deferred, not dropped
	awaitRuns(t, runs, 2, 3*time.Second)
}

// TestPlanStatsFloorExemptsFullRescans keeps the correctness paths — cold
// start, the UpdateWorkspaces edge and the 5-minute reconciliation ticker,
// all of which request a FULL rescan — running exactly as before.
func TestPlanStatsFloorExemptsFullRescans(t *testing.T) {
	h := NewFlowHandler(nil, nil, 1).SetPlanStatsMinInterval(10 * time.Second)
	runs, _ := countingStats(h)

	h.kickPlanStats(false)
	awaitRuns(t, runs, 1, 2*time.Second)
	waitStatsIdle(t, h)
	h.kickPlanStats(true)
	awaitRuns(t, runs, 2, 2*time.Second)
}

// TestPlanStatsFloorDisabledKeepsPreFloorBehaviour pins the configured-zero
// escape hatch to the behaviour it claims to restore.
func TestPlanStatsFloorDisabledKeepsPreFloorBehaviour(t *testing.T) {
	h := NewFlowHandler(nil, nil, 1).SetPlanStatsMinInterval(0)
	runs, _ := countingStats(h)

	h.kickPlanStats(false)
	awaitRuns(t, runs, 1, 2*time.Second)
	waitStatsIdle(t, h)
	h.kickPlanStats(false)
	awaitRuns(t, runs, 2, 2*time.Second)
}

func waitStatsIdle(t *testing.T, h *FlowHandler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.statsMu.Lock()
		running := h.statsRunning
		h.statsMu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stats pass never went idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
