package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// applySatJobsSnapshot pushes an origin-scoped federation snapshot, exactly
// what the SatelliteCollector emits — in production the snapshot diff is the
// ONLY source of per-job terminal events for federated jobs (B1).
func applySatJobsSnapshot(st *store.Store, origin string, jobs ...*models.JobInfo) {
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSatelliteSnapshot,
		Source:  "satellite",
		Origin:  origin,
		Payload: &store.SatelliteSnapshotPayload{Origin: origin, Jobs: jobs},
	})
}

// waitForLeaseRelease polls until the lease file and the jobID→planDir mapping
// are both gone (the releaser runs on its own subscriber goroutine).
func waitForLeaseRelease(t *testing.T, s *Server, planDir, jobID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.satelliteLeasesMu.Lock()
		_, tracked := s.satelliteLeases[jobID]
		s.satelliteLeasesMu.Unlock()
		if _, err := os.Stat(filepath.Join(planDir, coreplan.LeaseFileName)); os.IsNotExist(err) && !tracked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lease for %s was never released", jobID)
}

// TestSatelliteLeaseWriteAndRelease covers the lease lifecycle the laptop daemon
// owns (M2 C14): writeSatelliteLease creates .grove-lease.yml and tracks the
// job; releaseSatelliteLease removes it when the terminal event arrives.
func TestSatelliteLeaseWriteAndRelease(t *testing.T) {
	s := New(false)
	ctx := context.Background()
	planDir := t.TempDir()

	s.writeSatelliteLease(ctx, planDir, "grove-satellite", &models.JobInfo{ID: "job-1"})

	lease, err := coreplan.ReadLease(planDir)
	if err != nil {
		t.Fatalf("ReadLease: %v", err)
	}
	if lease == nil {
		t.Fatal("expected a lease file after writeSatelliteLease")
	}
	if lease.HolderOrigin != "grove-satellite" || lease.JobID != "job-1" {
		t.Errorf("unexpected lease contents: %+v", lease)
	}
	s.satelliteLeasesMu.Lock()
	tracked := s.satelliteLeases["job-1"]
	s.satelliteLeasesMu.Unlock()
	if tracked != planDir {
		t.Errorf("lease not tracked for job-1: %q", tracked)
	}

	// Release removes the file and forgets the mapping.
	s.releaseSatelliteLease(ctx, "job-1")
	if _, err := os.Stat(filepath.Join(planDir, coreplan.LeaseFileName)); !os.IsNotExist(err) {
		t.Errorf("lease file still present after release: %v", err)
	}
	s.satelliteLeasesMu.Lock()
	_, still := s.satelliteLeases["job-1"]
	s.satelliteLeasesMu.Unlock()
	if still {
		t.Error("lease mapping still tracked after release")
	}
}

// TestSatelliteLeaseReleaseUnknownJobIsNoop guards against removing a lease we
// never wrote (e.g. a local job's terminal event).
func TestSatelliteLeaseReleaseUnknownJobIsNoop(t *testing.T) {
	s := New(false)
	s.releaseSatelliteLease(context.Background(), "never-tracked") // must not panic
}

// TestSatelliteLeaseReleasedByFederatedSnapshot drives the REAL production
// pipeline end-to-end (B1): dispatch writes a lease, a baseline snapshot shows
// the job running on the satellite, and the next re-snapshot's terminal row —
// via the Store's synthesized per-job event, not a hand-injected update — makes
// the lease releaser remove .grove-lease.yml.
func TestSatelliteLeaseReleasedByFederatedSnapshot(t *testing.T) {
	st := store.New()
	s := New(false)
	s.SetEngine(engine.New(st))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	planDir := t.TempDir()
	s.writeSatelliteLease(ctx, planDir, "sat", &models.JobInfo{ID: "job-1"})
	s.StartSatelliteLeaseReleaser(ctx)

	// Baseline: the dispatched job is running on the satellite.
	applySatJobsSnapshot(st, "sat", &models.JobInfo{ID: "job-1", Status: "running", Origin: "sat"})
	// The job finishes; the debounced re-snapshot carries the terminal row.
	applySatJobsSnapshot(st, "sat", &models.JobInfo{ID: "job-1", Status: "completed", Origin: "sat"})

	waitForLeaseRelease(t, s, planDir, "job-1")
}

// TestSatelliteLeaseReleasedWhenJobAppearsAlreadyTerminal covers the
// disconnected-finish case: the laptop never saw the job running (it was
// dispatched, then the collector dropped), and a post-baseline re-snapshot
// shows it already completed. The lease must still release — TTL expiry is
// only the restart fallback.
func TestSatelliteLeaseReleasedWhenJobAppearsAlreadyTerminal(t *testing.T) {
	st := store.New()
	s := New(false)
	s.SetEngine(engine.New(st))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	planDir := t.TempDir()
	s.writeSatelliteLease(ctx, planDir, "sat", &models.JobInfo{ID: "job-2"})
	s.StartSatelliteLeaseReleaser(ctx)

	// Baseline snapshot does not include the job yet.
	applySatJobsSnapshot(st, "sat")
	// Reconnect re-snapshot: the job appears already terminal.
	applySatJobsSnapshot(st, "sat", &models.JobInfo{ID: "job-2", Status: "completed", Origin: "sat"})

	waitForLeaseRelease(t, s, planDir, "job-2")
}

func TestIsTerminalJobUpdate(t *testing.T) {
	terminal := []store.UpdateType{store.UpdateJobCompleted, store.UpdateJobFailed, store.UpdateJobCancelled}
	for _, ty := range terminal {
		if !isTerminalJobUpdate(ty) {
			t.Errorf("%v should be terminal", ty)
		}
	}
	nonTerminal := []store.UpdateType{store.UpdateJobSubmitted, store.UpdateJobStarted, store.UpdateJobPendingUser}
	for _, ty := range nonTerminal {
		if isTerminalJobUpdate(ty) {
			t.Errorf("%v should not be terminal", ty)
		}
	}
}
