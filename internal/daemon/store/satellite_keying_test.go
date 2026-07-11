package store

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
)

// applySessionsSnapshot pushes a wholesale UpdateSessions replacement for a
// given origin (empty == local), mirroring what a collector snapshot emits.
func applySessionsSnapshot(s *Store, origin string, sessions ...*models.Session) {
	s.ApplyUpdate(Update{Type: UpdateSessions, Origin: origin, Payload: sessions})
}

// TestOriginNamespacedJobKeys proves a same-ID local and remote job coexist
// under distinct composite keys (C7), and GetJob(bareID) resolves the local one.
func TestOriginNamespacedJobKeys(t *testing.T) {
	s := New()

	local := &models.JobInfo{ID: "job1", Status: "running", Title: "local"}
	remote := &models.JobInfo{ID: "job1", Status: "running", Title: "remote", Origin: "sat"}

	s.ApplyUpdate(Update{Type: UpdateJobSubmitted, Payload: local})
	s.ApplyUpdate(Update{Type: UpdateJobStarted, Payload: remote})

	if got := len(s.GetJobs()); got != 2 {
		t.Fatalf("expected 2 coexisting jobs, got %d", got)
	}
	// Bare-ID getter is local-only.
	gj := s.GetJob("job1")
	if gj == nil || gj.Origin != "" || gj.Title != "local" {
		t.Fatalf("GetJob(bareID) must resolve the local row, got %+v", gj)
	}
}

// TestOriginNamespacedSessionKeys is the session analogue of the job test.
func TestOriginNamespacedSessionKeys(t *testing.T) {
	s := New()

	local := &models.Session{ID: "s1", Status: "running"}
	remote := &models.Session{ID: "s1", Status: "running", Origin: "sat"}

	applySessionsSnapshot(s, "", local)
	applySessionsSnapshot(s, "sat", remote)

	if got := len(s.GetSessions()); got != 2 {
		t.Fatalf("expected 2 coexisting sessions, got %d", got)
	}
	gs := s.GetSession("s1")
	if gs == nil || gs.Origin != "" {
		t.Fatalf("GetSession(bareID) must resolve the local row, got %+v", gs)
	}
}

// TestLocalSessionsSnapshotDoesNotEvictRemote is the C7 wholesale-replacement
// guarantee: a local SessionCollector snapshot must not wipe federated rows, and
// a remote-origin snapshot must not wipe locals.
func TestLocalSessionsSnapshotDoesNotEvictRemote(t *testing.T) {
	s := New()

	remote := &models.Session{ID: "r1", Status: "running", Origin: "sat"}
	applySessionsSnapshot(s, "sat", remote)

	// A local snapshot arrives (no remote rows in it).
	local := &models.Session{ID: "l1", Status: "running"}
	applySessionsSnapshot(s, "", local)

	if s.GetSession("l1") == nil {
		t.Fatal("local session missing after local snapshot")
	}
	// The remote row must survive the local wholesale replace.
	found := false
	for _, sess := range s.GetSessions() {
		if sess.ID == "r1" && sess.Origin == "sat" {
			found = true
		}
	}
	if !found {
		t.Fatal("remote session was evicted by a local UpdateSessions snapshot (C7 violated)")
	}

	// Now a remote snapshot arrives (no local rows in it) — locals must survive.
	remote2 := &models.Session{ID: "r2", Status: "running", Origin: "sat"}
	applySessionsSnapshot(s, "sat", remote2)
	if s.GetSession("l1") == nil {
		t.Fatal("local session was evicted by a remote UpdateSessions snapshot (C7 violated)")
	}
	// The remote snapshot replaced its own origin: r1 gone, r2 present.
	var haveR1, haveR2 bool
	for _, sess := range s.GetSessions() {
		switch sess.ID {
		case "r1":
			haveR1 = true
		case "r2":
			haveR2 = true
		}
	}
	if haveR1 {
		t.Fatal("remote r1 should have been replaced by the new remote snapshot")
	}
	if !haveR2 {
		t.Fatal("remote r2 missing after remote snapshot")
	}
}

// TestSatelliteSnapshotReplacesOnlyItsOrigin proves UpdateSatelliteSnapshot is
// the origin-scoped reconcile primitive: it adds/removes exactly its origin's
// jobs+sessions and leaves locals and other origins untouched (B7).
func TestSatelliteSnapshotReplacesOnlyItsOrigin(t *testing.T) {
	s := New()

	// Seed a local job/session, plus a row for a *different* origin.
	s.ApplyUpdate(Update{Type: UpdateJobSubmitted, Payload: &models.JobInfo{ID: "L", Status: "running"}})
	applySessionsSnapshot(s, "", &models.Session{ID: "L", Status: "running"})
	s.ApplyUpdate(Update{Type: UpdateSatelliteSnapshot, Origin: "other", Payload: &SatelliteSnapshotPayload{
		Origin:   "other",
		Jobs:     []*models.JobInfo{{ID: "O", Status: "running", Origin: "other"}},
		Sessions: []*models.Session{{ID: "O", Status: "running", Origin: "other"}},
	}})

	// First snapshot for origin "sat": adds job A, session A.
	s.ApplyUpdate(Update{Type: UpdateSatelliteSnapshot, Origin: "sat", Payload: &SatelliteSnapshotPayload{
		Origin:   "sat",
		Jobs:     []*models.JobInfo{{ID: "A", Status: "running", Origin: "sat"}},
		Sessions: []*models.Session{{ID: "A", Status: "running", Origin: "sat"}},
	}})

	// Second snapshot for "sat": A is gone, B appears. Only "sat" rows change.
	s.ApplyUpdate(Update{Type: UpdateSatelliteSnapshot, Origin: "sat", Payload: &SatelliteSnapshotPayload{
		Origin:   "sat",
		Jobs:     []*models.JobInfo{{ID: "B", Status: "running", Origin: "sat"}},
		Sessions: []*models.Session{{ID: "B", Status: "running", Origin: "sat"}},
	}})

	jobsByOrigin := map[string]map[string]bool{}
	for _, j := range s.GetJobs() {
		if jobsByOrigin[j.Origin] == nil {
			jobsByOrigin[j.Origin] = map[string]bool{}
		}
		jobsByOrigin[j.Origin][j.ID] = true
	}
	if !jobsByOrigin[""]["L"] {
		t.Fatal("local job L was disturbed by a satellite snapshot")
	}
	if !jobsByOrigin["other"]["O"] {
		t.Fatal("other-origin job O was disturbed by a sat snapshot")
	}
	if jobsByOrigin["sat"]["A"] {
		t.Fatal("sat job A should have been removed by the reconcile snapshot")
	}
	if !jobsByOrigin["sat"]["B"] {
		t.Fatal("sat job B missing after reconcile snapshot")
	}

	// Sessions mirror the jobs assertions.
	var haveLocalS, haveOtherS, haveSatA, haveSatB bool
	for _, sess := range s.GetSessions() {
		switch {
		case sess.Origin == "" && sess.ID == "L":
			haveLocalS = true
		case sess.Origin == "other" && sess.ID == "O":
			haveOtherS = true
		case sess.Origin == "sat" && sess.ID == "A":
			haveSatA = true
		case sess.Origin == "sat" && sess.ID == "B":
			haveSatB = true
		}
	}
	if !haveLocalS || !haveOtherS {
		t.Fatal("local/other session was disturbed by a sat snapshot")
	}
	if haveSatA {
		t.Fatal("sat session A should have been removed by the reconcile snapshot")
	}
	if !haveSatB {
		t.Fatal("sat session B missing after reconcile snapshot")
	}
}

// TestSyncSessionStatusSkippedForRemote proves the B9 guard: a remote session's
// foreign JobFilePath must never drive a local job-markdown write. We point a
// remote session at a real local job markdown and invoke the guarded writer
// directly — without the Origin!="" early-return it would rewrite the markdown
// to pending_user; with it, the markdown is untouched.
func TestSyncSessionStatusSkippedForRemote(t *testing.T) {
	dir := t.TempDir()
	jobPath := writeTestJob(t, dir, "running")

	s := New()
	remote := &models.Session{
		ID:          "test-job",
		Origin:      "sat",
		Status:      "running",
		JobFilePath: jobPath,
	}
	s.syncSessionStatusToJobMarkdown(remote, "running", "pending_user")

	if got := jobStatus(t, jobPath); got != "running" {
		t.Fatalf("remote session drove a local job-markdown write: status now %q, want running (B9 violated)", got)
	}
}
