package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func TestPruneTerminalSessionsRetentionMatrix(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-15 * 24 * time.Hour)
	recent := now.Add(-13 * 24 * time.Hour)
	st := New()
	st.ApplyUpdate(Update{Type: UpdateSessions, Payload: []*models.Session{
		{ID: "old-terminal", Status: "completed", StartedAt: recent, LastActivity: recent, EndedAt: &old},
		{ID: "recent-terminal", Status: "failed", StartedAt: old, LastActivity: old, EndedAt: &recent},
		{ID: "active-old", Status: "running", StartedAt: old, LastActivity: old},
		{ID: "legacy-terminal", Status: "interrupted", StartedAt: old},
	}})

	dropped := st.PruneTerminalSessions(now.Add(-14*24*time.Hour), "test")
	if len(dropped) != 2 || dropped[0] != "legacy-terminal" || dropped[1] != "old-terminal" {
		t.Fatalf("dropped = %v, want legacy-terminal and old-terminal", dropped)
	}
	if st.GetSession("old-terminal") != nil || st.GetSession("legacy-terminal") != nil {
		t.Fatal("old terminal rows remain in store")
	}
	if st.GetSession("recent-terminal") == nil {
		t.Fatal("recent terminal row was pruned")
	}
	if st.GetSession("active-old") == nil {
		t.Fatal("active row was pruned")
	}
}

func TestPruneTerminalSessionsPublishesCount(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	st := New()
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)
	st.ApplyUpdate(Update{Type: UpdateSessions, Payload: []*models.Session{
		{ID: "old", Status: "error", StartedAt: old},
	}})
	<-ch // initial sessions update

	if got := st.PruneTerminalSessions(now.Add(-14*24*time.Hour), "test_reconcile"); len(got) != 1 {
		t.Fatalf("pruned = %v", got)
	}
	select {
	case update := <-ch:
		if update.Type != UpdateSessionsPruned || update.Scanned != 1 || update.Source != "test_reconcile" {
			t.Fatalf("update = %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("no prune update published")
	}
}
