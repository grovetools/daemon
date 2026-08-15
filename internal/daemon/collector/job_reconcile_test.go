package collector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/core/pkg/sessions/health"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/flow/pkg/orchestration"
)

// writeJobFile lays down a plan job file with the given frontmatter
// status and back-dates its mtime by quiet.
func writeJobFile(t *testing.T, planDir, name, status string, quiet time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(planDir, name)
	body := "---\nid: " + name + "\nstatus: " + status + "\ntype: interactive_agent\n---\nbody\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-quiet)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

func jobFor(planDir, name, status string) *models.JobInfo {
	return &models.JobInfo{
		ID:          name,
		Title:       name,
		Type:        models.JobType("interactive_agent"),
		Status:      status,
		PlanDir:     planDir,
		PlanName:    "p",
		JobFile:     name,
		SubmittedAt: time.Now().Add(-time.Hour),
	}
}

// collectorWithRegistry builds a JobCollector whose classifier reads an
// isolated session registry rooted at stateDir.
func collectorWithRegistry(scope string, stateDir ...string) *JobCollector {
	c := &JobCollector{interval: time.Minute, scope: scope}
	if len(stateDir) > 0 {
		c.stateDir = stateDir[0]
	}
	return c
}

func settings(quiet time.Duration) reconcileSettings {
	return reconcileSettings{enabled: true, quietFor: quiet, maxPerRun: 25}
}

func TestSweepOnePassConvergesWithoutSuccessOrAbandonment(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plans", "p")
	path := writeJobFile(t, planDir, "one-pass.md", "running", time.Hour)
	job := jobFor(planDir, "one-pass.md", "running")
	c := collectorWithRegistry("", t.TempDir())

	changed := c.sweepStuckJobFiles(context.Background(), logging.NewUnifiedLogger("test.reconcile"), store.New(), []*models.JobInfo{job}, time.Now())
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	status, ok, err := orchestration.ReadJobFileStatus(path)
	if err != nil || !ok || status != "orphaned" {
		t.Fatalf("status = %q, ok=%v err=%v; want orphaned", status, ok, err)
	}
	if status == "completed" || status == "abandoned" {
		t.Fatalf("unsafe reconciliation status %q", status)
	}
}

// TestSweepConvictsAnAbandonedRunningJob is the ghost this exists for:
// a file claiming "running", no session anywhere, no live process, long
// since touched.
func TestSweepConvictsAnAbandonedRunningJob(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plans", "p")
	writeJobFile(t, planDir, "87-commit.md", "running", 23*time.Minute)

	c := collectorWithRegistry("")
	got := c.collectCandidates(
		[]*models.JobInfo{jobFor(planDir, "87-commit.md", "running")},
		map[string]*models.Session{}, map[string]*models.Session{},
		settings(10*time.Minute), time.Now(),
	)

	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1", len(got))
	}
	if got[0].to != "orphaned" {
		t.Errorf("to = %q, want orphaned for an agent job", got[0].to)
	}
	if got[0].verdict.State != health.Stale {
		t.Errorf("verdict = %v, want STALE", got[0].verdict.State)
	}
}

// TestSweepSkipsJobsWithAnActiveSessionWithoutAStaleVerdict: presence alone
// no longer vetoes reconciliation, but empty/unverified/alive rows do. Only
// Phase 2's authoritative stale verdict crosses the mutation gate.
func TestSweepSkipsJobsWithAnActiveSession(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plans", "p")
	path := writeJobFile(t, planDir, "01-job.md", "running", time.Hour)
	job := jobFor(planDir, "01-job.md", "running")

	c := collectorWithRegistry("")

	byFile := map[string]*models.Session{path: {ID: job.ID, Status: "running"}}
	if got := c.collectCandidates([]*models.JobInfo{job}, byFile, map[string]*models.Session{}, settings(time.Minute), time.Now()); len(got) != 0 {
		t.Errorf("candidates = %d, want 0 — a job with an active session is the reaper's", len(got))
	}

	byID := map[string]*models.Session{job.ID: {ID: job.ID, Status: "running"}}
	if got := c.collectCandidates([]*models.JobInfo{job}, map[string]*models.Session{}, byID, settings(time.Minute), time.Now()); len(got) != 0 {
		t.Errorf("candidates = %d, want 0 — matched by session ID", len(got))
	}
}

// TestSweepRespectsTheQuietThreshold: a file that was touched recently
// has a live writer, whatever the process table says.
func TestSweepAcceptsOnlyAuthoritativeStalePresentRow(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plans", "p")
	path := writeJobFile(t, planDir, "01-job.md", "running", time.Hour)
	job := jobFor(planDir, "01-job.md", "running")
	row := &models.Session{ID: job.ID, JobFilePath: path, Status: "running", Verified: "stale"}

	got := collectorWithRegistry("").collectCandidates([]*models.JobInfo{job},
		map[string]*models.Session{path: row}, map[string]*models.Session{job.ID: row},
		settings(time.Minute), time.Now())
	if len(got) != 1 || got[0].source != "stored_verdict" || got[0].to != "orphaned" {
		t.Fatalf("candidates = %+v, want one stored-stale orphan correction", got)
	}
}

func TestSweepRespectsTheQuietThreshold(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plans", "p")
	writeJobFile(t, planDir, "01-job.md", "running", time.Minute)
	job := jobFor(planDir, "01-job.md", "running")

	c := collectorWithRegistry("")
	if got := c.collectCandidates([]*models.JobInfo{job}, map[string]*models.Session{}, map[string]*models.Session{},
		settings(10*time.Minute), time.Now()); len(got) != 0 {
		t.Errorf("candidates = %d, want 0 — the file was touched a minute ago", len(got))
	}
}

// TestSweepIgnoresTerminalStatuses: a job that recorded how it ended is
// history and must never be rewritten.
func TestSweepIgnoresTerminalStatuses(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plans", "p")
	c := collectorWithRegistry("")

	for _, status := range []string{"completed", "failed", "interrupted", "abandoned", "pending", "todo"} {
		name := status + ".md"
		writeJobFile(t, planDir, name, status, time.Hour)
		got := c.collectCandidates([]*models.JobInfo{jobFor(planDir, name, status)},
			map[string]*models.Session{}, map[string]*models.Session{}, settings(time.Minute), time.Now())
		if len(got) != 0 {
			t.Errorf("status %q produced %d candidates, want 0", status, len(got))
		}
	}
}

// TestSweepSparesJobsWithALivePID: the registry's pid.lock is the
// signal that actually protects a running agent whose session the
// daemon lost.
func TestSweepSparesJobsWithALivePID(t *testing.T) {
	stateDir := t.TempDir()

	planDir := filepath.Join(t.TempDir(), "plans", "p")
	writeJobFile(t, planDir, "01-job.md", "running", time.Hour)
	job := jobFor(planDir, "01-job.md", "running")

	// A live agent leaves a pid.lock naming a real process.
	regDir := filepath.Join(stateDir, "hooks", "sessions", job.ID)
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "pid.lock"),
		[]byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	c := collectorWithRegistry("", stateDir)
	got := c.collectCandidates([]*models.JobInfo{job}, map[string]*models.Session{}, map[string]*models.Session{},
		settings(time.Minute), time.Now())
	if len(got) != 0 {
		t.Errorf("candidates = %d, want 0 — a live pid.lock must protect the job", len(got))
	}
}

// TestSweepIsScopeGated: a scoped daemon must never rewrite another
// scope's job files, exactly like the session reaper.
func TestSweepIsScopeGated(t *testing.T) {
	stateDir := t.TempDir()

	planDir := filepath.Join(t.TempDir(), "plans", "p")
	writeJobFile(t, planDir, "01-job.md", "running", time.Hour)
	job := jobFor(planDir, "01-job.md", "running")

	// A dead session owned by a DIFFERENT scope.
	regDir := filepath.Join(stateDir, "hooks", "sessions", job.ID)
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(sessions.SessionMetadata{
		SessionID: job.ID,
		JobID:     job.ID,
		Scope:     "/other/ecosystem",
	})
	if err := os.WriteFile(filepath.Join(regDir, "metadata.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}

	foreign := collectorWithRegistry("/my/ecosystem", stateDir)
	if got := foreign.collectCandidates([]*models.JobInfo{job}, map[string]*models.Session{}, map[string]*models.Session{},
		settings(time.Minute), time.Now()); len(got) != 0 {
		t.Errorf("candidates = %d, want 0 — another scope's job", len(got))
	}

	owner := collectorWithRegistry("/other/ecosystem", stateDir)
	if got := owner.collectCandidates([]*models.JobInfo{job}, map[string]*models.Session{}, map[string]*models.Session{},
		settings(time.Minute), time.Now()); len(got) != 1 {
		t.Errorf("candidates = %d, want 1 — the owning daemon may reconcile it", len(got))
	}
}

// TestReconcileSettingsDefaultOn pins the conservative anti-entropy default.
// The sweep can only retract unsupported active claims; false remains an off-switch.
func TestReconcileSettingsDefaultOn(t *testing.T) {
	s := resolveReconcileSettings(nil)
	if !s.enabled {
		t.Error("reconciliation defaulted off, want conservative loss correction enabled")
	}
	if s.quietFor != defaultReconcileQuietFor || s.maxPerRun != defaultReconcileMaxPerRun || s.sessionRetention != defaultSessionRetention {
		t.Errorf("defaults = %v/%d/%v, want %v/%d/%v", s.quietFor, s.maxPerRun, s.sessionRetention,
			defaultReconcileQuietFor, defaultReconcileMaxPerRun, defaultSessionRetention)
	}

	on := true
	s = resolveReconcileSettings(&config.Config{Daemon: &config.DaemonConfig{
		JobReconcile: &config.DaemonJobReconcileConfig{
			Enabled: &on, QuietFor: "30m", MaxPerRun: 5,
		},
	}})
	if !s.enabled || s.quietFor != 30*time.Minute || s.maxPerRun != 5 {
		t.Errorf("configured settings = %+v", s)
	}

	off := false
	s = resolveReconcileSettings(&config.Config{Daemon: &config.DaemonConfig{
		JobReconcile:     &config.DaemonJobReconcileConfig{Enabled: &off},
		SessionRetention: "48h",
	}})
	if s.enabled || s.sessionRetention != 48*time.Hour {
		t.Errorf("off-switch/retention settings = %+v", s)
	}

	// A malformed duration falls back rather than reconciling instantly.
	s = resolveReconcileSettings(&config.Config{Daemon: &config.DaemonConfig{
		JobReconcile: &config.DaemonJobReconcileConfig{QuietFor: "not-a-duration"},
	}})
	if s.quietFor != defaultReconcileQuietFor {
		t.Errorf("quietFor = %v on a bad value, want the default", s.quietFor)
	}
}
