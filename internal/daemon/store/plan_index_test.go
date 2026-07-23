package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/flow/pkg/orchestration"
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

// TestSetHoldPersistencePublishesNavWorkspaceLifecycle is the production-path
// integration seam: Flow persists hold/unhold through orchestration.SetHold,
// the daemon loads that same file into its qualified plan-index snapshot, and
// the store publishes the exact workspace delta/snapshot consumed by Nav.
func TestSetHoldPersistencePublishesNavWorkspaceLifecycle(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	root := t.TempDir()
	planDir := filepath.Join(root, "notebook", "plans", "hold-plan")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, ".grove-plan.yml"), []byte("worktree: hold-plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boundRoot := filepath.Join(root, "fixture-repo", ".grove-worktrees", "hold-plan")
	boundMember := filepath.Join(boundRoot, "fixture-repo")
	twinRoot := filepath.Join(root, "twin-repo", ".grove-worktrees", "hold-plan")
	twinMember := filepath.Join(twinRoot, "twin-repo")
	parent := filepath.Join(root, "fixture-repo")
	unrelated := filepath.Join(root, "fixture-repo", ".grove-worktrees", "unrelated", "fixture-repo")
	s.state.Workspaces = map[string]*models.EnrichedWorkspace{
		boundMember: {WorkspaceNode: &workspace.WorkspaceNode{Path: boundMember, Kind: workspace.KindEcosystemSubProjectWorktree}, PlanStats: &models.PlanStats{AssociatedPlan: "hold-plan", AssociatedPlanDir: planDir}},
		twinMember:  {WorkspaceNode: &workspace.WorkspaceNode{Path: twinMember, Kind: workspace.KindEcosystemSubProjectWorktree}},
		parent:      {WorkspaceNode: &workspace.WorkspaceNode{Path: parent, Kind: workspace.KindEcosystemRoot}},
		unrelated:   {WorkspaceNode: &workspace.WorkspaceNode{Path: unrelated, Kind: workspace.KindEcosystemSubProjectWorktree}},
	}

	publishPersistedLifecycle := func() {
		t.Helper()
		plan, err := orchestration.LoadPlan(planDir)
		if err != nil {
			t.Fatal(err)
		}
		lifecycle := "live"
		if plan.Config != nil && plan.Config.Status == "hold" {
			lifecycle = "hold"
		}
		s.ApplyUpdate(Update{Type: UpdatePlanIndexSnapshot, Source: "flow_watcher", Payload: &models.PlanIndexSnapshot{Plans: []models.PlanSummary{{
			PlanDir: planDir, PlanName: "hold-plan", WorktreePath: boundRoot, Lifecycle: lifecycle,
		}}}})
	}
	assertDelta := func(want string) {
		t.Helper()
		if update := <-ch; update.Type != UpdatePlanIndexDelta {
			t.Fatalf("first update type=%q, want plan index delta", update.Type)
		}
		update := <-ch
		if update.Type != UpdateWorkspacesDelta {
			t.Fatalf("second update type=%q, want Nav-facing workspace delta", update.Type)
		}
		deltas := update.Payload.([]*models.WorkspaceDelta)
		if len(deltas) != 1 || deltas[0].Path != boundMember || deltas[0].PlanStats.PlanStatus != want {
			t.Fatalf("workspace deltas=%+v, want exact bound member status %q", deltas, want)
		}
		byPath := map[string]*models.EnrichedWorkspace{}
		for _, ws := range s.GetWorkspaces() {
			byPath[ws.Path] = ws
		}
		if byPath[boundMember].PlanStats.PlanStatus != want {
			t.Fatalf("snapshot bound status=%q want %q", byPath[boundMember].PlanStats.PlanStatus, want)
		}
		for _, path := range []string{twinMember, parent, unrelated} {
			if byPath[path].PlanStats != nil && byPath[path].PlanStats.PlanStatus == "hold" {
				t.Fatalf("snapshot cross-targeted preserved path %q", path)
			}
		}
	}

	if err := orchestration.SetHold(planDir, true); err != nil {
		t.Fatal(err)
	}
	publishPersistedLifecycle()
	assertDelta("hold")
	if err := orchestration.SetHold(planDir, false); err != nil {
		t.Fatal(err)
	}
	publishPersistedLifecycle()
	assertDelta("")
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
