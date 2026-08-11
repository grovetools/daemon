package enrichment

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	coreconfig "github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/core/util/pathutil"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
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
	pass := newTestPass()
	if got := pass.planStatusForNode(plansDir, held); got != "hold" {
		t.Fatalf("held member status=%q", got)
	}
	if got := pass.planStatusForNode(plansDir, unrelated); got != "" {
		t.Fatalf("unrelated same-name worktree inherited status %q", got)
	}
	if got := pass.planStatusForNode(plansDir, parent); got != "" {
		t.Fatalf("parent inherited held status %q", got)
	}
}

// newTestPass builds a pass the way FetchPlanStatsMap does. It must be created
// AFTER the test's registry entries are saved: a pass snapshots the registry
// once at construction, which is the whole point of it.
func newTestPass() *planStatsPass {
	return newPlanStatsPass(workspace.NewNotebookLocator(nil))
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

// TestAssociatedPlanDirUsesCanonicalRegistryResolver pins the enrichment-side
// derivation to core's canonical registry resolver: for a standalone-repo
// container the plan dir is qualified by the OWNER workspace (where flow plan
// init created it), not by joining this node's own plans dir with the plan
// name (which would qualify by the container basename — the plan name itself).
func TestAssociatedPlanDirUsesCanonicalRegistryResolver(t *testing.T) {
	groveHome := t.TempDir()
	t.Setenv("GROVE_HOME", groveHome)
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(groveHome, "config", "grove")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	owner := filepath.Join(root, "alpha-repo")
	if err := os.MkdirAll(filepath.Join(owner, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	container := filepath.Join(owner, ".grove-worktrees", "alpha-view")
	if err := os.MkdirAll(filepath.Join(container, ".grove"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "grove.toml"), []byte("workspaces = [\"*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := "branch: alpha-view\nplan: alpha-view\nowner: " + owner + "\necosystem: true\n"
	if err := os.WriteFile(filepath.Join(container, ".grove", "workspace"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := worktreeregistry.Save(&worktreeregistry.Entry{AbsPath: container, Owner: owner, Plan: "alpha-view"}); err != nil {
		t.Fatal(err)
	}

	// Plan routing follows the authoritative recorded code-root/notebook pair.
	// Merely creating a directory at the old ~/.grove/notebooks location must
	// not influence resolution.
	notebookRoot := filepath.Join(home, "notebooks", "nb")
	notebooks := "default = \"nb\"\n[notebooks.nb]\nroot = " + strconv.Quote(notebookRoot) + "\n"
	roots := "[roots.alpha-repo]\npath = " + strconv.Quote(owner) + "\nnotebook = \"nb\"\n"
	if err := os.WriteFile(filepath.Join(configDir, "notebooks.toml"), []byte(notebooks), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "roots.toml"), []byte(roots), 0o600); err != nil {
		t.Fatal(err)
	}
	coreconfig.ResetLoadCache()
	t.Cleanup(coreconfig.ResetLoadCache)

	planDir := filepath.Join(notebookRoot, "notespaces", "alpha-repo", "plans", "alpha-view")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, ".grove-plan.yml"), []byte("status: hold\nworktree: alpha-view\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	node := &workspace.WorkspaceNode{Path: filepath.Join(container, "alpha-repo"), Kind: workspace.KindEcosystemSubProjectWorktree}
	pass := newTestPass()
	if got := pass.associatedPlanDirForNode(node); got != planDir {
		t.Fatalf("associatedPlanDirForNode = %q, want %q", got, planDir)
	}
	// The status read must go through the canonical plan dir, not the
	// (deliberately bogus) plans root the caller derived for this node.
	if got := pass.planStatusForNode(filepath.Join(root, "not-a-plans-root"), node); got != "hold" {
		t.Fatalf("planStatusForNode = %q, want hold", got)
	}
}

// TestFetchPlanStatsMapUsesOnlyInjectedNodes is the discovery-removal
// regression. FetchPlanStatsMap used to run its own workspace.DiscoverAll on
// every call, so its output was keyed by whatever happened to be on the
// filesystem rather than by what the caller asked about. It must now answer
// for EXACTLY the node set it is handed — that is what lets the daemon reuse
// the store's already-discovered workspaces instead of re-walking disk on
// every plan-file event.
func TestFetchPlanStatsMapUsesOnlyInjectedNodes(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	wsPath := filepath.Join(t.TempDir(), "workspace")
	node := &workspace.WorkspaceNode{Name: "workspace", Path: wsPath, Kind: workspace.KindStandaloneProject}

	// Materialize the plan where THIS host's notebook layout puts it, rather
	// than assuming local mode: the assertion under test is about the node set,
	// and hard-coding .notebook/plans would make it fail on a centralized
	// notebook config for reasons that have nothing to do with discovery.
	cfg, err := coreconfig.LoadDefault()
	if err != nil {
		cfg = &coreconfig.Config{}
	}
	plansDir, err := workspace.NewNotebookLocator(cfg).GetPlansDir(node)
	if err != nil {
		t.Fatal(err)
	}
	planDir := filepath.Join(plansDir, "alpha")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "01-job.md"),
		[]byte("---\nid: j1\nstatus: running\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats := FetchPlanStatsMap([]*workspace.WorkspaceNode{node}, nil)
	if len(stats) != 1 {
		t.Fatalf("stats covered %d paths, want exactly the 1 injected node: %v", len(stats), stats)
	}
	got := stats[wsPath]
	if got == nil {
		t.Fatalf("no stats for injected node %q", wsPath)
	}
	if got.TotalPlans != 1 || got.Running != 1 {
		t.Fatalf("stats = %+v, want TotalPlans=1 Running=1", got)
	}

	// A nil node in the set (a store row restored before discovery re-saw it)
	// must be skipped, not panic the whole pass.
	if stats := FetchPlanStatsMap([]*workspace.WorkspaceNode{nil, node}, nil); len(stats) != 1 {
		t.Fatalf("nil node changed the result set: %v", stats)
	}
	// An empty node set is a legitimate pre-discovery state, not an error.
	if stats := FetchPlanStatsMap(nil, nil); len(stats) != 0 {
		t.Fatalf("empty node set produced %d entries", len(stats))
	}
}

// TestPlanStatsPassLoadsRegistryOnce pins the per-pass hoist: the worktree
// registry is read ONCE when the pass is built, not once per node (it was
// being loaded several times per node — via PlanForPath, planStatusForNode and
// plan.ResolveTarget — at 600 nodes a pass). Deleting the entry from disk
// after construction proves the answers come from the snapshot.
func TestPlanStatsPassLoadsRegistryOnce(t *testing.T) {
	groveHome := t.TempDir()
	t.Setenv("GROVE_HOME", groveHome)

	owner := t.TempDir()
	container := filepath.Join(owner, ".grove-worktrees", "alpha")
	memberA := filepath.Join(container, "repo-a")
	memberB := filepath.Join(container, "repo-b")
	for _, dir := range []string{memberA, memberB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := worktreeregistry.Save(&worktreeregistry.Entry{AbsPath: container, Plan: "alpha"}); err != nil {
		t.Fatal(err)
	}

	pass := newTestPass()

	entries, err := worktreeregistry.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := worktreeregistry.Delete(pathutil.WorktreeID(entry.AbsPath)); err != nil {
			t.Fatal(err)
		}
	}
	if remaining, _ := worktreeregistry.ListAll(); len(remaining) != 0 {
		t.Fatalf("registry not cleared: %d entries", len(remaining))
	}

	for _, node := range []*workspace.WorkspaceNode{
		{Path: memberA, Kind: workspace.KindEcosystemSubProjectWorktree},
		{Path: memberB, Kind: workspace.KindEcosystemSubProjectWorktree},
	} {
		plan, ok := pass.registeredPlanForNode(node)
		if !ok || plan != "alpha" {
			t.Fatalf("node %s: registeredPlanForNode = (%q, %v), want (alpha, true) from the pass snapshot",
				node.Path, plan, ok)
		}
	}
}

// TestPlanStatsPassMemoizesPerContainer pins the other half of the hoist: the
// plan-dir derivation reaches plan.ResolveTarget, which loads and JSON-schema
// validates a grove.toml. It is a function of the worktree container root
// alone, so sibling member checkouts of one container must resolve it once
// between them, not once each.
func TestPlanStatsPassMemoizesPerContainer(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())

	owner := t.TempDir()
	container := filepath.Join(owner, ".grove-worktrees", "alpha")
	siblings := []string{filepath.Join(container, "repo-a"), filepath.Join(container, "repo-b")}
	for _, dir := range siblings {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pass := newTestPass()
	for _, dir := range siblings {
		pass.associatedPlanDirForNode(&workspace.WorkspaceNode{Path: dir, Kind: workspace.KindEcosystemSubProjectWorktree})
	}

	if len(pass.planDirByRoot) != 1 {
		t.Fatalf("memoized %d plan dirs for one container, want 1: %v", len(pass.planDirByRoot), pass.planDirByRoot)
	}
	if _, ok := pass.planDirByRoot[container]; !ok {
		t.Fatalf("memo not keyed by the container root %q: %v", container, pass.planDirByRoot)
	}
}

// TestFetchPlanStatsMapRecordsTelemetry pins the counter that makes this pass
// visible on /api/system/stats. Before it, a pass that had grown to seconds
// and ran every few seconds left no number anywhere.
func TestFetchPlanStatsMapRecordsTelemetry(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())

	before, _, _, _ := telemetry.PlanStatsPass.Snapshot()
	FetchPlanStatsMap([]*workspace.WorkspaceNode{
		{Name: "ws", Path: t.TempDir(), Kind: workspace.KindStandaloneProject},
	}, nil)
	after, _, _, _ := telemetry.PlanStatsPass.Snapshot()
	if after != before+1 {
		t.Fatalf("planstats.pass.count went %d -> %d, want +1", before, after)
	}
}
