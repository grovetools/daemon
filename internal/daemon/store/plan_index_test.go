package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
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

func TestPlanIndexLifecycleEmitsQualifiedWorkspaceDeltas(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	root := t.TempDir()
	planA := root + "/workspace-a/plans/same"
	planB := root + "/workspace-b/plans/same"
	boundRoot := root + "/repo/.grove-worktrees/a"
	unrelatedRoot := root + "/repo/.grove-worktrees/b"
	boundPath := boundRoot + "/member"
	unrelatedPath := unrelatedRoot + "/member"
	parentPath := root + "/repo"
	s.state.Workspaces = map[string]*models.EnrichedWorkspace{
		boundPath: {WorkspaceNode: &workspace.WorkspaceNode{Path: boundPath, Kind: workspace.KindEcosystemSubProjectWorktree}},
		unrelatedPath: {WorkspaceNode: &workspace.WorkspaceNode{Path: unrelatedPath, Kind: workspace.KindEcosystemSubProjectWorktree}, PlanStats: &models.PlanStats{
			AssociatedPlan: "same", AssociatedPlanDir: planB,
		}},
		parentPath: {WorkspaceNode: &workspace.WorkspaceNode{Path: parentPath, Kind: workspace.KindEcosystemRoot}, PlanStats: &models.PlanStats{PlanStatus: "hold"}},
	}

	s.ApplyUpdate(Update{Type: UpdatePlanIndexSnapshot, Source: "flow_watcher", Payload: &models.PlanIndexSnapshot{
		Plans: []models.PlanSummary{
			{PlanDir: planA, PlanName: "same", WorktreePath: boundRoot, Lifecycle: "hold"},
			{PlanDir: planB, PlanName: "same", WorktreePath: unrelatedRoot, Lifecycle: "live"},
		},
	}})
	if update := <-ch; update.Type != UpdatePlanIndexDelta {
		t.Fatalf("first update type=%q, want plan index", update.Type)
	}
	holdUpdate := <-ch
	if holdUpdate.Type != UpdateWorkspacesDelta {
		t.Fatalf("second update type=%q, want workspace delta", holdUpdate.Type)
	}
	deltas := holdUpdate.Payload.([]*models.WorkspaceDelta)
	if len(deltas) != 1 || deltas[0].Path != boundPath || deltas[0].PlanStats.PlanStatus != "hold" {
		t.Fatalf("qualified hold deltas=%+v", deltas)
	}
	if got := s.state.Workspaces[unrelatedPath].PlanStats.PlanStatus; got != "" {
		t.Fatalf("same-named unrelated workspace status=%q", got)
	}
	if got := s.state.Workspaces[parentPath].PlanStats.PlanStatus; got != "hold" {
		t.Fatalf("unassociated parent status changed to %q", got)
	}

	s.ApplyUpdate(Update{Type: UpdatePlanIndexSnapshot, Source: "flow_watcher", Payload: &models.PlanIndexSnapshot{
		Plans: []models.PlanSummary{
			{PlanDir: planA, PlanName: "same", WorktreePath: boundRoot, Lifecycle: "live"},
			{PlanDir: planB, PlanName: "same", WorktreePath: unrelatedRoot, Lifecycle: "live"},
		},
	}})
	<-ch // plan index delta
	unholdUpdate := <-ch
	unhold := unholdUpdate.Payload.([]*models.WorkspaceDelta)
	if len(unhold) != 1 || unhold[0].Path != boundPath || unhold[0].PlanStats.PlanStatus != "" {
		t.Fatalf("qualified unhold deltas=%+v", unhold)
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
