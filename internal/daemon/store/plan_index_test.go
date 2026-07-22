package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func TestPlanIndexSnapshotMaterializesRevisionedDelta(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)
	now := time.Now()

	s.ApplyUpdate(Update{Type: UpdatePlanIndexSnapshot, Source: "test", Payload: &models.PlanIndexSnapshot{
		ScannedAt: now,
		Plans:     []models.PlanSummary{{PlanDir: "/plans/a", PlanName: "a"}, {PlanDir: "/plans/b", PlanName: "b"}},
	}})
	first := <-ch
	if first.Type != UpdatePlanIndexDelta {
		t.Fatalf("broadcast type=%q want plan index delta", first.Type)
	}
	delta := first.Payload.(*models.PlanIndexDelta)
	if delta.Revision != 1 || len(delta.Upserts) != 2 {
		t.Fatalf("first delta=%+v", delta)
	}
	if snap := s.GetPlanIndexSnapshot(); snap.Revision != 1 || len(snap.Plans) != 2 {
		t.Fatalf("snapshot=%+v", snap)
	}

	s.ApplyUpdate(Update{Type: UpdatePlanIndexSnapshot, Source: "test", Payload: &models.PlanIndexSnapshot{
		ScannedAt: now.Add(time.Minute), Plans: []models.PlanSummary{{PlanDir: "/plans/b", PlanName: "b"}},
	}})
	second := (<-ch).Payload.(*models.PlanIndexDelta)
	if second.Revision != 2 || len(second.Removed) != 1 || second.Removed[0] != "/plans/a" {
		t.Fatalf("second delta=%+v", second)
	}
}

func TestGetPlanIndexSnapshotIsDetached(t *testing.T) {
	s := New()
	s.ApplyUpdate(Update{Type: UpdatePlanIndexSnapshot, Payload: &models.PlanIndexSnapshot{Plans: []models.PlanSummary{{PlanDir: "/a"}}}})
	copy := s.GetPlanIndexSnapshot()
	copy.Plans[0].PlanDir = "/mutated"
	if got := s.GetPlanIndexSnapshot().Plans[0].PlanDir; got != "/a" {
		t.Fatalf("store snapshot mutated through getter: %q", got)
	}
}
