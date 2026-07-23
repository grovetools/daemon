package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/flow/pkg/orchestration"
)

func writeIndexedPlan(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".grove-plan.yml"), []byte("status: live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01-job.md"), []byte("---\nid: job\ntitle: job\ntype: oneshot\nstatus: pending\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFlowHandlerResolvesNotebookAliasForRuntimeEvents(t *testing.T) {
	notebookRoot := t.TempDir()
	realWorkspace := filepath.Join(notebookRoot, "workspaces", "fixture-repo")
	plansDir := filepath.Join(realWorkspace, "plans")
	planDir := filepath.Join(plansDir, "hold-plan")
	writeIndexedPlan(t, planDir)

	aliasWorkspace := filepath.Join(notebookRoot, "workspaces", "hold-plan")
	if err := os.Symlink(realWorkspace, aliasWorkspace); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Notebooks: &config.NotebooksConfig{
		Definitions: map[string]*config.Notebook{"test": {
			RootDir: notebookRoot, PlansPathTemplate: "workspaces/{{ .Workspace.Name }}/plans",
		}},
		Rules: &config.NotebookRules{Default: "test"},
	}}
	h := NewFlowHandler(nil, cfg, 1)
	node := &workspace.WorkspaceNode{Name: "hold-plan", Path: t.TempDir(), Kind: workspace.KindEcosystemRoot, NotebookName: "test"}

	paths := h.ComputeWatchPaths([]*models.EnrichedWorkspace{{WorkspaceNode: node}})
	realPlansDir := resolveFlowWatchPath(plansDir)
	if !containsPath(paths, realPlansDir) {
		t.Fatalf("watch paths do not contain resolved plans dir %q: %v", realPlansDir, paths)
	}
	realConfig := filepath.Join(planDir, ".grove-plan.yml")
	if !h.MatchesEvent(fsnotify.Event{Name: realConfig, Op: fsnotify.Write}) {
		t.Fatalf("resolved target event %q did not match alias-derived watch set", realConfig)
	}
	aliasConfig := filepath.Join(aliasWorkspace, "plans", "hold-plan", ".grove-plan.yml")
	if !h.MatchesEvent(fsnotify.Event{Name: aliasConfig, Op: fsnotify.Write}) {
		t.Fatalf("alias event %q did not match resolved watch set", aliasConfig)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func TestSummarizePlanCarriesHoldAndUnholdLifecycle(t *testing.T) {
	plan := &orchestration.Plan{Name: "same", Directory: "/notebook/a/plans/same", Config: &orchestration.PlanConfig{Status: "hold"}}
	held := summarizePlan(plan, "/notebook/a/plans", "/workspace/a", "", nil, time.Now())
	if held.Lifecycle != "hold" {
		t.Fatalf("held lifecycle=%q", held.Lifecycle)
	}

	plan.Config.Status = ""
	live := summarizePlan(plan, "/notebook/a/plans", "/workspace/a", "", nil, time.Now())
	if live.Lifecycle != "live" {
		t.Fatalf("unheld lifecycle=%q", live.Lifecycle)
	}
	if live.PlanDir != plan.Directory {
		t.Fatalf("qualified plan identity=%q", live.PlanDir)
	}
}

func TestApplyResolvedPlanBindingsKeepsDuplicateSlugsQualified(t *testing.T) {
	planA := "/notebooks/a/plans/same"
	planB := "/notebooks/b/plans/same"
	rootA := "/worktrees/a/same"
	rootB := "/worktrees/b/same"
	summaries := []models.PlanSummary{
		{PlanDir: planA, PlanName: "same", Worktree: "same"},
		{PlanDir: planB, PlanName: "same", Worktree: "same"},
	}
	entries := []*worktreeregistry.Entry{
		{AbsPath: rootA, Owner: "/repos/a", Repos: []string{"repo-a"}},
		{AbsPath: rootB, Owner: "/repos/b", Repos: []string{"repo-b"}},
	}
	bindings := map[string]coreplan.PlanBinding{
		planA: {Key: coreplan.NewPlanKey(planA), Health: coreplan.BindingValid, ContainerPath: rootA},
		planB: {Key: coreplan.NewPlanKey(planB), Health: coreplan.BindingValid, ContainerPath: rootB},
	}

	got := applyResolvedPlanBindings(summaries, entries, bindings)
	if got[0].WorktreePath != rootA || got[0].Anchor != "/repos/a" || len(got[0].Repositories) != 1 || got[0].Repositories[0] != "repo-a" {
		t.Fatalf("plan A binding=%+v", got[0])
	}
	if got[1].WorktreePath != rootB || got[1].Anchor != "/repos/b" || len(got[1].Repositories) != 1 || got[1].Repositories[0] != "repo-b" {
		t.Fatalf("plan B binding=%+v", got[1])
	}
}

func TestLoadIndexedPlansSeparatesArchiveContainerFromArchivedPlans(t *testing.T) {
	plansDir := t.TempDir()
	writeIndexedPlan(t, filepath.Join(plansDir, "live-plan"))
	writeIndexedPlan(t, filepath.Join(plansDir, ".archive", "old-plan"))
	writeIndexedPlan(t, filepath.Join(plansDir, ".artifacts", "not-a-plan"))

	got := loadIndexedPlans(plansDir)
	if len(got) != 2 {
		t.Fatalf("indexed %d entries, want live + archived: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, entry := range got {
		seen[entry.plan.Name] = entry.archived
		if entry.plan.Name == ".archive" {
			t.Fatal("archive container was indexed as a plan")
		}
	}
	if archived, ok := seen["live-plan"]; !ok || archived {
		t.Fatalf("live plan classification = %v, present=%v", archived, ok)
	}
	if archived, ok := seen["old-plan"]; !ok || !archived {
		t.Fatalf("archived plan classification = %v, present=%v", archived, ok)
	}
	if _, ok := seen["not-a-plan"]; ok {
		t.Fatal("hidden organizational directory was descended")
	}
}
