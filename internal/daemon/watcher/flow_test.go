package watcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/daemon/internal/daemon/store"
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

// TestFlowHoldLifecycleIsNotLostToDebounce exercises the production mutation,
// fsnotify dispatch, watcher recomputation, and store publication path. Each
// lifecycle edge must publish immediately rather than wait behind the ordinary
// enrichment debounce.
func TestFlowHoldLifecycleIsNotLostToDebounce(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

	notebookRoot := filepath.Join(root, "notebook")
	codeRoot := filepath.Join(root, "code")
	configPath := filepath.Join(root, "config", "grove", "grove.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	configBody := "[groves.code]\npath = \"" + codeRoot + "\"\nenabled = true\ndepth = 1\nnotebook = \"default\"\n" +
		"[groves.worktrees]\npath = \"" + codeRoot + "\"\nenabled = true\ndepth = 5\nnotebook = \"default\"\n" +
		"[notebooks.definitions.default]\nroot_dir = \"" + notebookRoot + "\"\n" +
		"[notebooks.rules]\ndefault = \"default\"\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Notebooks: &config.NotebooksConfig{
		Definitions: map[string]*config.Notebook{"default": {RootDir: notebookRoot}},
		Rules:       &config.NotebookRules{Default: "default"},
	}}

	type fixture struct {
		owner, planDir, container, member string
		parent, workspace                 *models.EnrichedWorkspace
	}
	makeFixture := func(name string) fixture {
		t.Helper()
		owner := filepath.Join(codeRoot, name)
		runGit(t, owner, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(owner, "grove.toml"), []byte("name = \""+name+"\"\nversion = \"1.0\"\nmanaged = true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(owner, "README.md"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, owner, "add", ".")
		runGit(t, owner, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "init")
		runGit(t, owner, "branch", "hold-plan")
		container := filepath.Join(owner, ".grove-worktrees", "hold-plan")
		member := filepath.Join(container, name)
		runGit(t, owner, "worktree", "add", "-q", member, "hold-plan")
		planDir := filepath.Join(notebookRoot, "workspaces", name, "plans", "hold-plan")
		writeIndexedPlan(t, planDir)
		if err := os.WriteFile(filepath.Join(planDir, ".grove-plan.yml"), []byte("worktree: hold-plan\nrepos:\n  - "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := worktreeregistry.Save(&worktreeregistry.Entry{AbsPath: container, Owner: owner, Repos: []string{name}, Plan: "hold-plan"}); err != nil {
			t.Fatal(err)
		}
		return fixture{
			owner: owner, planDir: planDir, container: container, member: member,
			parent:    &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{Name: name, Path: owner, Kind: workspace.KindStandaloneProject, NotebookName: "default"}},
			workspace: &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{Name: name, Path: member, Kind: workspace.KindStandaloneProjectWorktree, ParentProjectPath: owner, NotebookName: "default"}},
		}
	}
	target := makeFixture("fixture-repo")
	twin := makeFixture("twin-repo")
	unrelatedPath := filepath.Join(target.owner, ".grove-worktrees", "unrelated", "fixture-repo")
	unrelated := &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{Name: "fixture-repo", Path: unrelatedPath, Kind: workspace.KindStandaloneProjectWorktree, ParentProjectPath: target.owner, NotebookName: "default"}}

	st := store.New()
	workspaces := map[string]*models.EnrichedWorkspace{
		target.owner: target.parent, target.member: target.workspace,
		twin.owner: twin.parent, twin.member: twin.workspace,
		unrelatedPath: unrelated,
	}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: workspaces})
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)
	h := NewFlowHandler(st, cfg, 2000)
	paths := h.ComputeWatchPaths(st.GetWorkspaces())
	targetConfig := filepath.Join(target.planDir, ".grove-plan.yml")
	if !containsPath(paths, resolveFlowWatchPath(target.planDir)) || !h.MatchesEvent(fsnotify.Event{Name: targetConfig, Op: fsnotify.Write}) {
		targetPlans, targetErr := h.locator.GetPlansDir(target.workspace.WorkspaceNode)
		t.Fatalf("production watcher does not cover target config; paths=%v resolved=%q err=%v workspaces=%d", paths, targetPlans, targetErr, len(st.GetWorkspaces()))
	}
	uw, err := NewUnifiedWatcher(st, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	uw.Register(h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uw.Start(ctx)
	for {
		update := <-ch
		if update.Type == store.UpdateWatcherStatus {
			if payload, ok := update.Payload.(map[string]interface{}); ok && payload["event"] == "started" {
				break
			}
		}
	}

	awaitLifecycle := func(want string) {
		t.Helper()
		deadline := time.NewTimer(time.Second)
		defer deadline.Stop()
		for {
			select {
			case update := <-ch:
				if update.Type != store.UpdateWorkspacesDelta || update.Source != "flow_watcher" {
					continue
				}
				deltas := update.Payload.([]*models.WorkspaceDelta)
				for _, delta := range deltas {
					if delta.PlanStats != nil && delta.PlanStats.PlanStatus == "hold" &&
						(delta.Path == twin.member || delta.Path == target.owner || delta.Path == unrelatedPath) {
						t.Fatalf("lifecycle delta cross-targeted preserved path %q", delta.Path)
					}
					if delta.Path == target.member && delta.PlanStats != nil && delta.PlanStats.PlanStatus == want {
						return
					}
				}
			case <-deadline.C:
				t.Fatalf("filesystem watcher did not publish lifecycle %q before debounce window; plan_index=%+v", want, st.GetPlanIndexSnapshot())
			}
		}
	}
	if err := orchestration.SetHold(target.planDir, true); err != nil {
		t.Fatal(err)
	}
	awaitLifecycle("hold")
	if err := orchestration.SetHold(target.planDir, false); err != nil {
		t.Fatal(err)
	}
	awaitLifecycle("")
	if got := st.GetPlanIndexSnapshot().Revision; got < 2 {
		t.Fatalf("hold/unhold did not advance plan index twice: revision=%d", got)
	}
	for path, ws := range map[string]*models.EnrichedWorkspace{twin.member: twin.workspace, target.owner: target.parent, unrelatedPath: unrelated} {
		if ws.PlanStats != nil && ws.PlanStats.PlanStatus == "hold" {
			t.Fatalf("hold cross-targeted preserved path %q", path)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
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
