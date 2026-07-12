package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestSatelliteNotifierFiresFromSnapshotDiff drives the REAL production
// pipeline (B1): federation snapshots — the only per-job event source for
// remote jobs — must reach the ntfy bridge. Only the outermost I/O sink
// (osascript/ntfy) is stubbed via satelliteNotifyFn; the Store diff, the
// subscriber filter, and the per-job dedupe all run for real.
func TestSatelliteNotifierFiresFromSnapshotDiff(t *testing.T) {
	st := store.New()
	s := New(false)
	s.SetEngine(engine.New(st))

	calls := make(chan string, 10)
	s.satelliteNotifyFn = func(_ context.Context, job *models.JobInfo, updType store.UpdateType, _, _ string) {
		calls <- fmt.Sprintf("%s:%s:%s", updType, job.Origin, job.ID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartSatelliteNotifier(ctx, "", "")

	// Baseline: the job is running — nothing terminal, no notification.
	applySatJobsSnapshot(st, "sat", &models.JobInfo{ID: "j1", Status: "running", Origin: "sat"})
	// Re-snapshot: the job failed — exactly one notification.
	applySatJobsSnapshot(st, "sat", &models.JobInfo{ID: "j1", Status: "failed", Origin: "sat"})

	select {
	case got := <-calls:
		if got != "job_failed:sat:j1" {
			t.Fatalf("unexpected notification %q, want job_failed:sat:j1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier never fired from the federated snapshot diff (B1)")
	}

	// Unchanged terminal row: the diff synthesizes nothing.
	applySatJobsSnapshot(st, "sat", &models.JobInfo{ID: "j1", Status: "failed", Origin: "sat"})
	// Drop + reappear terminal: the diff fires again (the lease releaser needs
	// that), but the notifier's per-job dedupe must swallow the repeat.
	applySatJobsSnapshot(st, "sat")
	applySatJobsSnapshot(st, "sat", &models.JobInfo{ID: "j1", Status: "failed", Origin: "sat"})

	select {
	case got := <-calls:
		t.Fatalf("duplicate notification %q — re-snapshot dedupe broken", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSatelliteNotifierIgnoresLocalTerminalJobs pins the Origin filter: a
// local jobrunner terminal event must never reach the cross-machine bridge.
func TestSatelliteNotifierIgnoresLocalTerminalJobs(t *testing.T) {
	st := store.New()
	s := New(false)
	s.SetEngine(engine.New(st))

	calls := make(chan string, 10)
	s.satelliteNotifyFn = func(_ context.Context, job *models.JobInfo, _ store.UpdateType, _, _ string) {
		calls <- job.ID
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartSatelliteNotifier(ctx, "", "")

	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobCompleted,
		Source:  "jobrunner",
		Payload: &models.JobInfo{ID: "local-1", Status: "completed"},
	})

	select {
	case got := <-calls:
		t.Fatalf("local job %q reached the satellite notifier", got)
	case <-time.After(300 * time.Millisecond):
	}
}
