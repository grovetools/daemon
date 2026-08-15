package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func TestSessionEndIsEventIdempotent(t *testing.T) {
	s := New()
	firstEnd := time.Unix(1_700_000_000, 0)
	now := firstEnd
	s.now = func() time.Time { return now }

	s.ApplyUpdate(Update{
		Type:   UpdateSessions,
		Source: "test",
		Payload: []*models.Session{{
			ID:     "job-1",
			Status: "running",
		}},
	})
	s.ApplyUpdate(Update{
		Type:   UpdateJobStarted,
		Source: "test",
		Payload: &models.JobInfo{
			ID:     "job-1",
			Status: "running",
		},
	})

	sub := s.Subscribe()
	defer s.Unsubscribe(sub)

	s.ApplyUpdate(Update{
		Type:   UpdateSessionEnd,
		Source: "supervisor",
		Payload: &SessionEndPayload{
			JobID:   "job-1",
			Outcome: "exited",
			Reason:  "provider_exit_0",
		},
	})

	select {
	case update := <-sub:
		if update.Type != UpdateSessionEnd {
			t.Fatalf("first update type = %q, want %q", update.Type, UpdateSessionEnd)
		}
	case <-time.After(time.Second):
		t.Fatal("first session end was not published")
	}

	ended := s.GetSession("job-1")
	if ended == nil || ended.EndedAt == nil {
		t.Fatalf("ended session = %+v, want EndedAt", ended)
	}
	if ended.Status != "exited" || !ended.EndedAt.Equal(firstEnd) {
		t.Fatalf("ended session = status %q at %v, want exited at %v", ended.Status, ended.EndedAt, firstEnd)
	}
	if job := s.GetJob("job-1"); job == nil || job.Status != "running" || job.CompletedAt != nil {
		t.Fatalf("neutral session exit changed Flow job result: %+v", job)
	}

	now = firstEnd.Add(time.Hour)
	s.ApplyUpdate(Update{
		Type:   UpdateSessionEnd,
		Source: "pane_fallback",
		Payload: &SessionEndPayload{
			JobID:   "job-1",
			Outcome: "failed",
			Reason:  "duplicate_observer",
		},
	})

	afterDuplicate := s.GetSession("job-1")
	if afterDuplicate.Status != "exited" || afterDuplicate.EndedAt == nil || !afterDuplicate.EndedAt.Equal(firstEnd) {
		t.Fatalf("duplicate end mutated terminal session: %+v", afterDuplicate)
	}
	if job := s.GetJob("job-1"); job == nil || job.Status != "running" || job.CompletedAt != nil {
		t.Fatalf("duplicate end mutated Flow job: %+v", job)
	}
	select {
	case update := <-sub:
		t.Fatalf("duplicate end published update: %+v", update)
	case <-time.After(50 * time.Millisecond):
	}
}
