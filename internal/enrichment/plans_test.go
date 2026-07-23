package enrichment

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
)

// TestResolvePerNodePlanStats_SiblingsGetOwnActivePlan is the HUD wrong-plan
// regression: two sibling worktrees sharing ONE plans directory must each report
// their OWN active plan and plan status, while sharing the (node-independent)
// job counts. The prior implementation handed both siblings the same *PlanStats
// pointer and stamped ActivePlan only on the first-discovered sibling, so the
// first sibling's plan leaked onto all of them.
func TestPlanStatusForNodeUsesExactRegistryAssociation(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	plansDir := t.TempDir()
	planDir := filepath.Join(plansDir, "same")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, ".grove-plan.yml"), []byte("status: hold\nworktree: same\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	owner := t.TempDir()
	heldRoot := filepath.Join(owner, ".grove-worktrees", "same")
	heldPath := filepath.Join(heldRoot, "member-repo")
	unrelatedPath := filepath.Join(owner, ".grove-worktrees", "unrelated", "member-repo")
	if err := os.MkdirAll(heldPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unrelatedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := worktreeregistry.Save(&worktreeregistry.Entry{AbsPath: heldRoot, Plan: "same"}); err != nil {
		t.Fatal(err)
	}

	// Daemon discovery reports member checkouts as worktree nodes, but registry
	// ownership is attached to their synthetic container root.
	held := &workspace.WorkspaceNode{Path: heldPath, Kind: workspace.KindEcosystemSubProjectWorktree}
	unrelated := &workspace.WorkspaceNode{Path: unrelatedPath, Kind: workspace.KindEcosystemSubProjectWorktree}
	parent := &workspace.WorkspaceNode{Path: owner, Kind: workspace.KindEcosystemRoot}
	if got := planStatusForNode(plansDir, held); got != "hold" {
		t.Fatalf("held member status=%q", got)
	}
	if got := planStatusForNode(plansDir, unrelated); got != "" {
		t.Fatalf("unrelated same-name worktree inherited status %q", got)
	}
	if got := planStatusForNode(plansDir, parent); got != "" {
		t.Fatalf("parent inherited held status %q", got)
	}
}

func TestResolvePerNodePlanStats_SiblingsGetOwnActivePlan(t *testing.T) {
	const sharedPlansDir = "/nb/project/plans"

	nodeA := &workspace.WorkspaceNode{Name: "wt-a", Path: "/wt/a", Kind: workspace.KindEcosystemWorktree}
	nodeB := &workspace.WorkspaceNode{Name: "wt-b", Path: "/wt/b", Kind: workspace.KindEcosystemWorktree}

	activePlanByPath := map[string]string{
		"/wt/a": "plan-alpha",
		"/wt/b": "plan-beta",
	}
	planStatusByPath := map[string]string{
		"/wt/a": "running",
		"/wt/b": "hold",
	}

	countsCalls := 0
	statsByPath := resolvePerNodePlanStats(
		[]*workspace.WorkspaceNode{nodeA, nodeB},
		// Both siblings resolve to the SAME plans dir.
		func(_ *workspace.WorkspaceNode) (string, error) { return sharedPlansDir, nil },
		// Counts are node-independent — computed once, shared.
		func(_ string) *models.PlanStats {
			countsCalls++
			return &models.PlanStats{TotalPlans: 3, Running: 1, Completed: 2}
		},
		func(n *workspace.WorkspaceNode) string { return activePlanByPath[n.Path] },
		func(_ string, n *workspace.WorkspaceNode) string { return planStatusByPath[n.Path] },
	)

	if countsCalls != 1 {
		t.Errorf("countsFor called %d times, want 1 (shared per plans dir)", countsCalls)
	}

	a := statsByPath["/wt/a"]
	b := statsByPath["/wt/b"]
	if a == nil || b == nil {
		t.Fatalf("missing stats: a=%v b=%v", a, b)
	}

	// Per-node ActivePlan/PlanStatus — the core assertion.
	if a.ActivePlan != "plan-alpha" {
		t.Errorf("node A ActivePlan = %q, want plan-alpha", a.ActivePlan)
	}
	if b.ActivePlan != "plan-beta" {
		t.Errorf("node B ActivePlan = %q, want plan-beta (sibling leak regression)", b.ActivePlan)
	}
	if a.PlanStatus != "running" {
		t.Errorf("node A PlanStatus = %q, want running", a.PlanStatus)
	}
	if b.PlanStatus != "hold" {
		t.Errorf("node B PlanStatus = %q, want hold", b.PlanStatus)
	}

	// Shared counts copied to both (values equal, pointers distinct so a later
	// per-node stamp can't bleed across siblings).
	if a.TotalPlans != 3 || b.TotalPlans != 3 || a.Completed != 2 || b.Completed != 2 {
		t.Errorf("counts not shared: a=%+v b=%+v", a, b)
	}
	if a == b {
		t.Error("siblings share the SAME *PlanStats pointer — per-node stamps would collide")
	}
}
