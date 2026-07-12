package store

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
)

// applySatJobsSnapshot pushes an origin-scoped federation snapshot carrying
// only jobs, mirroring what the SatelliteCollector emits — in production the
// snapshot is the ONLY federated change signal (B1).
func applySatJobsSnapshot(s *Store, origin string, jobs ...*models.JobInfo) {
	s.ApplyUpdate(Update{
		Type:    UpdateSatelliteSnapshot,
		Source:  "satellite",
		Origin:  origin,
		Payload: &SatelliteSnapshotPayload{Origin: origin, Jobs: jobs},
	})
}

// drainTerminalJobUpdates empties a subscriber channel and returns only the
// terminal per-job updates. ApplyUpdate broadcasts synchronously under the
// store lock, so everything is buffered by the time it returns.
func drainTerminalJobUpdates(ch chan Update) []Update {
	var out []Update
	for {
		select {
		case u := <-ch:
			switch u.Type {
			case UpdateJobCompleted, UpdateJobFailed, UpdateJobCancelled:
				out = append(out, u)
			}
		default:
			return out
		}
	}
}

// TestSatelliteSnapshotSynthesizesTerminalUpdates drives the real federation
// pipeline (B1): a snapshot where a previously-running origin job is now
// completed must broadcast a synthesized UpdateJobCompleted to Store
// subscribers, while unchanged rows (running or already terminal) synthesize
// nothing, and the baseline snapshot is pure state transfer.
func TestSatelliteSnapshotSynthesizesTerminalUpdates(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	// Baseline snapshot: A/B running, C already completed (history — a
	// satellite full of old terminal jobs must not fire a burst on boot).
	applySatJobsSnapshot(s, "sat",
		&models.JobInfo{ID: "A", Status: "running", Origin: "sat"},
		&models.JobInfo{ID: "B", Status: "running", Origin: "sat"},
		&models.JobInfo{ID: "C", Status: "completed", Origin: "sat"},
	)
	if got := drainTerminalJobUpdates(ch); len(got) != 0 {
		t.Fatalf("baseline snapshot synthesized %d terminal updates, want 0: %+v", len(got), got)
	}

	// Re-snapshot: A transitioned to completed; B and C are unchanged.
	applySatJobsSnapshot(s, "sat",
		&models.JobInfo{ID: "A", Status: "completed", Origin: "sat"},
		&models.JobInfo{ID: "B", Status: "running", Origin: "sat"},
		&models.JobInfo{ID: "C", Status: "completed", Origin: "sat"},
	)
	got := drainTerminalJobUpdates(ch)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 synthesized terminal update, got %d: %+v", len(got), got)
	}
	if got[0].Type != UpdateJobCompleted || got[0].Origin != "sat" {
		t.Fatalf("unexpected synthesized update: %+v", got[0])
	}
	job, ok := got[0].Payload.(*models.JobInfo)
	if !ok || job.ID != "A" || job.Origin != "sat" {
		t.Fatalf("synthesized payload must be job A's origin-stamped JobInfo, got %+v", got[0].Payload)
	}

	// The synthesis is broadcast-only: rows were written exactly once by the
	// snapshot reconcile, never a second time by the synthetic event.
	if n := len(s.GetJobs()); n != 3 {
		t.Fatalf("expected 3 job rows after re-snapshot, got %d", n)
	}

	// An identical third snapshot transitions nothing — no re-fire.
	applySatJobsSnapshot(s, "sat",
		&models.JobInfo{ID: "A", Status: "completed", Origin: "sat"},
		&models.JobInfo{ID: "B", Status: "running", Origin: "sat"},
		&models.JobInfo{ID: "C", Status: "completed", Origin: "sat"},
	)
	if got := drainTerminalJobUpdates(ch); len(got) != 0 {
		t.Fatalf("unchanged re-snapshot synthesized %d terminal updates, want 0: %+v", len(got), got)
	}
}

// TestSatelliteSnapshotSynthesizesForNewlyAppearedTerminal covers the
// disconnected-finish case: after the baseline, a job the laptop never saw
// running appears already failed/cancelled in a re-snapshot (dispatch happened
// while the collector was down) and must still fire — the lease releaser
// depends on it. It also pins the failed/cancelled status→type mapping.
func TestSatelliteSnapshotSynthesizesForNewlyAppearedTerminal(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	// Baseline establishes the origin; no rows yet.
	applySatJobsSnapshot(s, "sat")
	drainTerminalJobUpdates(ch)

	applySatJobsSnapshot(s, "sat",
		&models.JobInfo{ID: "F", Status: "failed", Origin: "sat"},
		&models.JobInfo{ID: "X", Status: "cancelled", Origin: "sat"},
	)
	got := drainTerminalJobUpdates(ch)
	if len(got) != 2 {
		t.Fatalf("expected 2 synthesized terminal updates, got %d: %+v", len(got), got)
	}
	types := map[string]UpdateType{}
	for _, u := range got {
		job := u.Payload.(*models.JobInfo)
		types[job.ID] = u.Type
	}
	if types["F"] != UpdateJobFailed {
		t.Errorf("job F: got %v, want UpdateJobFailed", types["F"])
	}
	if types["X"] != UpdateJobCancelled {
		t.Errorf("job X: got %v, want UpdateJobCancelled", types["X"])
	}
}
