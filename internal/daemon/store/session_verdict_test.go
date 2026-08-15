package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func TestSessionVerdictAppliesWithoutMasqueradingAsActivity(t *testing.T) {
	st := New()
	activity := time.Unix(100, 0)
	st.ApplyUpdate(Update{Type: UpdateSessions, Payload: []*models.Session{{
		ID: "job-1", Status: "running", LastActivity: activity,
	}}})
	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	st.ApplyUpdate(Update{Type: UpdateSessionVerdict, Source: "collector", Payload: &SessionVerdictPayload{
		JobID: "job-1", Verified: "stale",
	}})

	got := st.GetSession("job-1")
	if got == nil || got.Verified != "stale" {
		t.Fatalf("verdict = %#v, want stale", got)
	}
	if !got.LastActivity.Equal(activity) {
		t.Fatalf("verdict changed LastActivity: got %v want %v", got.LastActivity, activity)
	}
	select {
	case u := <-sub:
		if u.Type != UpdateSessionVerdict {
			t.Fatalf("update type = %q, want %q", u.Type, UpdateSessionVerdict)
		}
	case <-time.After(time.Second):
		t.Fatal("verdict update was not published")
	}
}

func TestSessionVerdictRejectsTerminalAndInvalidWrites(t *testing.T) {
	for _, tc := range []struct {
		name, status, verdict string
	}{
		{"terminal", "interrupted", "stale"},
		{"invalid", "running", "certainly-dead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := New()
			st.ApplyUpdate(Update{Type: UpdateSessions, Payload: []*models.Session{{ID: "job", Status: tc.status}}})
			st.ApplyUpdate(Update{Type: UpdateSessionVerdict, Payload: &SessionVerdictPayload{JobID: "job", Verified: tc.verdict}})
			if got := st.GetSession("job").Verified; got != "" {
				t.Fatalf("Verified = %q, want empty", got)
			}
		})
	}
}

func TestTerminalTransitionsClearSessionVerdict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Store)
	}{
		{"status", func(st *Store) {
			st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{JobID: "job", Status: "interrupted"}})
		}},
		{"end", func(st *Store) {
			st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{JobID: "job", Outcome: "interrupted"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := New()
			st.ApplyUpdate(Update{Type: UpdateSessions, Payload: []*models.Session{{ID: "job", Status: "running", Verified: "stale"}}})
			tc.apply(st)
			got := st.GetSession("job")
			if got.Verified != "" {
				t.Fatalf("terminal row retained verdict %q", got.Verified)
			}
		})
	}
}
