package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/daemon/internal/daemon/store"
)

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
