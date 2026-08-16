package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func TestSessionActivityIsMonotonicAndAttemptGuarded(t *testing.T) {
	started := time.Unix(1_000, 0)
	last := started.Add(time.Minute)
	now := started.Add(10 * time.Minute)
	st := New()
	st.now = func() time.Time { return now }
	st.ApplyUpdate(Update{Type: UpdateSessions, Payload: []*models.Session{{
		ID: "job", Status: "running", StartedAt: started, LastActivity: last, Verified: "unverified",
	}}})

	apply := func(at time.Time, source string, expected time.Time) {
		st.ApplyUpdate(Update{Type: UpdateSessionActivity, Payload: &SessionActivityPayload{
			JobID: "job", ObservedAt: at, Source: source, ExpectedStartedAt: expected,
		}})
	}
	apply(last.Add(time.Minute), "transcript", started)
	got := st.GetSession("job")
	if !got.LastActivity.Equal(last.Add(time.Minute)) || got.Verified != "unverified" || got.Status != "running" {
		t.Fatalf("new activity malformed row: %+v", got)
	}
	apply(last, "pty", started)
	if got = st.GetSession("job"); !got.LastActivity.Equal(last.Add(time.Minute)) {
		t.Fatalf("older activity moved clock: %v", got.LastActivity)
	}
	apply(now.Add(time.Hour), "pty", started)
	if got = st.GetSession("job"); !got.LastActivity.Equal(now) {
		t.Fatalf("future activity was not capped: %v want %v", got.LastActivity, now)
	}
	apply(now.Add(time.Hour), "hook", started.Add(time.Second))
	if got = st.GetSession("job"); !got.LastActivity.Equal(now) {
		t.Fatalf("mismatched attempt renewed row: %v", got.LastActivity)
	}
	apply(now.Add(time.Hour), "ping", started)
	if got = st.GetSession("job"); !got.LastActivity.Equal(now) {
		t.Fatalf("invalid source renewed row: %v", got.LastActivity)
	}
}

func TestHookStatusRenewsMonotonicallyButPingDoesNot(t *testing.T) {
	started := time.Unix(1_500, 0)
	now := started.Add(time.Minute)
	st := New()
	st.now = func() time.Time { return now }
	st.ApplyUpdate(Update{Type: UpdateSessions, Payload: []*models.Session{{ID: "job", Status: "running", StartedAt: started, LastActivity: started}}})
	st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{JobID: "job", Status: "running"}})
	if got := st.GetSession("job"); !got.LastActivity.Equal(now) {
		t.Fatalf("unchanged hook status did not renew: %v", got.LastActivity)
	}
	now = now.Add(time.Minute)
	st.ApplyUpdate(Update{Type: UpdateSessionPing, Payload: &SessionPingPayload{JobID: "job"}})
	if got := st.GetSession("job"); !got.LastActivity.Equal(started.Add(time.Minute)) {
		t.Fatalf("idle ping renewed activity: %v", got.LastActivity)
	}
}

func TestSessionActivitySkipsTerminalAndRemote(t *testing.T) {
	now := time.Now().Round(0)
	st := New()
	st.now = func() time.Time { return now }
	st.ApplyUpdate(Update{Type: UpdateSessions, Payload: []*models.Session{
		{ID: "terminal", Status: "completed", StartedAt: now.Add(-time.Hour), LastActivity: now.Add(-time.Hour)},
	}})
	// Seed the origin-bearing row under the local lookup key to exercise the
	// apply guard directly; federated snapshots normally namespace this key.
	st.mu.Lock()
	st.state.Sessions["remote"] = &models.Session{ID: "remote", Status: "running", Origin: "sat", StartedAt: now.Add(-time.Hour), LastActivity: now.Add(-time.Hour)}
	st.mu.Unlock()
	for _, id := range []string{"terminal", "remote", "missing"} {
		st.ApplyUpdate(Update{Type: UpdateSessionActivity, Payload: &SessionActivityPayload{JobID: id, ObservedAt: now, Source: "pty"}})
	}
	for _, id := range []string{"terminal", "remote"} {
		if got := st.GetSession(id); !got.LastActivity.Equal(now.Add(-time.Hour)) {
			t.Fatalf("%s was renewed: %v", id, got.LastActivity)
		}
	}
}
