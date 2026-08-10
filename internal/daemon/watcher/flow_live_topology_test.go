package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/flow/pkg/orchestration"
	"github.com/sirupsen/logrus"
)

// TestFlowHoldDeliveryFromDiscoveredLiveTopology mirrors the tui-pilot sandbox
// where Hold→Nav propagation failed three times: workspaces come from the real
// discovery pipeline (recorded roots/notebooks, no hand-built nodes), plans
// resolve through a centralized notebook, and the plan fixture is created only AFTER
// the unified watcher has started — exactly like `flow plan init` running
// against an already-running daemon. The production persistence edge
// (orchestration.SetHold) must reach the FlowHandler as a filesystem event and
// surface as a Nav-facing WorkspaceDelta that marks the bound member held.
func TestFlowHoldDeliveryFromDiscoveredLiveTopology(t *testing.T) {
	sb := t.TempDir()
	home := filepath.Join(sb, "home")
	t.Setenv("HOME", home)
	groveHome := filepath.Join(sb, "grove-home")
	t.Setenv("GROVE_HOME", groveHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(sb, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(sb, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(sb, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(sb, "cache"))
	// The live daemon runs from inside the sandbox; run config resolution from
	// there too so the ecosystem grove.toml above this test file can't leak in.
	t.Chdir(sb)

	codeRoot := filepath.Join(home, "code")
	repo := filepath.Join(codeRoot, "fixture-repo")
	twinRepo := filepath.Join(codeRoot, "twin-repo")
	notebookRoot := filepath.Join(home, "notebooks", "verifybook")
	plansRoot := filepath.Join(notebookRoot, "workspaces", "fixture-repo", "plans")
	twinPlansRoot := filepath.Join(notebookRoot, "workspaces", "twin-repo", "plans")

	configDir := filepath.Join(groveHome, "config", "grove")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "grove.toml"), []byte(
		"[onboarding]\ncompleted = true\n[worktree]\nlayout = \"legacy\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notebooksBody := "default = \"default\"\n[notebooks.default]\nroot = " + strconv.Quote(notebookRoot) + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "notebooks.toml"), []byte(notebooksBody), 0o600); err != nil {
		t.Fatal(err)
	}
	rootsBody := "[roots.code]\npath = " + strconv.Quote(codeRoot) + "\nscan = true\ndepth = 1\nnotebook = \"default\"\n" +
		"[roots.fixture_worktrees]\npath = " + strconv.Quote(filepath.Join(repo, ".grove-worktrees")) + "\nscan = true\ndepth = 1\nnotebook = \"default\"\n" +
		"[roots.twin_worktrees]\npath = " + strconv.Quote(filepath.Join(twinRepo, ".grove-worktrees")) + "\nscan = true\ndepth = 1\nnotebook = \"default\"\n"
	if err := os.WriteFile(filepath.Join(configDir, "roots.toml"), []byte(rootsBody), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetLoadCache()
	t.Cleanup(config.ResetLoadCache)

	// The parent repositories and the (still plan-free) notebook plans roots
	// exist before daemon start, mirroring setup-fixture.sh.
	for _, dir := range []string{plansRoot, twinPlansRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range []string{repo, twinRepo} {
		runGit(t, owner, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(owner, "README.md"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, owner, "add", "README.md")
		runGit(t, owner, "-c", "user.name=pilot", "-c", "user.email=pilot@example.invalid", "commit", "-qm", "init")
	}

	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatalf("loading sandbox grove config: %v", err)
	}

	st := store.New()
	discoveryLog := logrus.New()
	discoveryLog.SetLevel(logrus.WarnLevel)
	publishDiscovery := func() map[string]*models.EnrichedWorkspace {
		t.Helper()
		// Mirror WorkspaceCollector.scan: reconcile the registry, discover
		// globally, publish the enriched map wholesale.
		worktreeregistry.Reconcile(paths.WorktreesDir()) //nolint:errcheck // best-effort, as in the collector
		nodes, err := workspace.GetProjects(discoveryLog)
		if err != nil {
			t.Fatalf("workspace discovery: %v", err)
		}
		current := st.Get()
		enriched := make(map[string]*models.EnrichedWorkspace, len(nodes))
		for _, node := range nodes {
			ew := &models.EnrichedWorkspace{WorkspaceNode: node}
			if existing, ok := current.Workspaces[node.Path]; ok {
				ew.PlanStats = existing.PlanStats
			}
			enriched[node.Path] = ew
		}
		st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "workspace", Payload: enriched})
		return enriched
	}
	publishDiscovery()

	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	h := NewFlowHandler(st, cfg, 2000)
	uw, err := NewUnifiedWatcher(st, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	uw.refreshInterval = 25 * time.Millisecond
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

	// Only now — with the daemon-side watcher live — does the fixture get its
	// plans, worktrees, and registry entries, the way `flow plan init
	// --worktree --layout legacy` creates them from the repo directory.
	initPlan := func(owner, ownerName, plansDir, planName, registryPlan string) (planDir, member string) {
		t.Helper()
		planDir = filepath.Join(plansDir, planName)
		if err := os.MkdirAll(planDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(planDir, ".grove-plan.yml"), []byte("worktree: "+planName+"\nrepos:\n    - "+ownerName+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(planDir, "01-job.md"), []byte("---\nid: job\ntitle: job\ntype: oneshot\nstatus: pending\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, owner, "branch", planName)
		container := filepath.Join(owner, ".grove-worktrees", planName)
		member = filepath.Join(container, ownerName)
		runGit(t, owner, "worktree", "add", "-q", member, planName)
		resolvedContainer, err := filepath.EvalSymlinks(container)
		if err != nil {
			t.Fatal(err)
		}
		resolvedOwner, err := filepath.EvalSymlinks(owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := worktreeregistry.Save(&worktreeregistry.Entry{
			AbsPath: resolvedContainer, Owner: resolvedOwner,
			Repos: []string{ownerName}, Plan: registryPlan,
		}); err != nil {
			t.Fatal(err)
		}
		return planDir, member
	}
	planDir, member := initPlan(repo, "fixture-repo", plansRoot, "hold-plan", "hold-plan")
	_, twinMember := initPlan(twinRepo, "twin-repo", twinPlansRoot, "hold-plan", "other-plan")
	invalidDir := filepath.Join(plansRoot, "invalid-plan")
	if err := os.MkdirAll(invalidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidDir, ".grove-plan.yml"), []byte("repos:\n  - fixture-repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelatedMember := filepath.Join(repo, ".grove-worktrees", "unrelated", "fixture-repo")
	runGit(t, repo, "worktree", "add", "-q", "-b", "unrelated", unrelatedMember)

	// The live collector rescans within 10s of a fixture change; the watcher
	// then recomputes its watch set from the workspace update.
	workspaces := publishDiscovery()
	findWorkspace := func(target string) string {
		t.Helper()
		resolvedTarget, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Fatal(err)
		}
		for path := range workspaces {
			if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved == resolvedTarget {
				return path
			}
		}
		keys := make([]string, 0, len(workspaces))
		for path := range workspaces {
			keys = append(keys, path)
		}
		t.Fatalf("discovery never produced a workspace for %q; discovered=%v", target, keys)
		return ""
	}
	memberPath := findWorkspace(member)
	twinMemberPath := findWorkspace(twinMember)
	unrelatedPath := findWorkspace(unrelatedMember)
	ownerPath := findWorkspace(repo)

	// Give the watcher a bounded window to cover the just-created plan dir the
	// way the live daemon's periodic refresh would, then require coverage: a
	// production Hold is a single write with no retry, so if the watch set
	// still misses the plan config here, the live transition is lost.
	targetConfig := filepath.Join(planDir, ".grove-plan.yml")
	coverageDeadline := time.Now().Add(2 * time.Second)
	for !h.MatchesEvent(fsnotify.Event{Name: targetConfig, Op: fsnotify.Write}) {
		if time.Now().After(coverageDeadline) {
			t.Fatalf("flow watcher never covered plan config %q after plan creation; watch paths=%v",
				targetConfig, h.ComputeWatchPaths(st.GetWorkspaces()))
		}
		time.Sleep(10 * time.Millisecond)
	}

	awaitLifecycle := func(want string) {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
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
						(delta.Path == twinMemberPath || delta.Path == ownerPath || delta.Path == unrelatedPath) {
						t.Fatalf("lifecycle delta cross-targeted preserved path %q", delta.Path)
					}
					if delta.Path == memberPath && delta.PlanStats != nil && delta.PlanStats.PlanStatus == want {
						return
					}
				}
			case <-deadline.C:
				t.Fatalf("production hold write never reached Nav as lifecycle %q; plan_index=%+v",
					want, st.GetPlanIndexSnapshot())
			}
		}
	}

	if err := orchestration.SetHold(planDir, true); err != nil {
		t.Fatal(err)
	}
	awaitLifecycle("hold")

	// The plan index must carry the registry-resolved qualified binding —
	// applyPlanLifecycleToWorkspaces skips summaries with empty WorktreePath.
	// The hold delta can be published before the index snapshot lands, so
	// give the snapshot its own bounded window.
	bindingDeadline := time.Now().Add(2 * time.Second)
	for {
		foundBinding := false
		snapshot := st.GetPlanIndexSnapshot()
		if snapshot != nil {
			for _, summary := range snapshot.Plans {
				if summary.PlanDir == planDir && summary.WorktreePath != "" {
					foundBinding = true
				}
			}
		}
		if foundBinding {
			break
		}
		if time.Now().After(bindingDeadline) {
			t.Fatalf("held plan summary lost its qualified container binding: %+v", snapshot)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := orchestration.SetHold(planDir, false); err != nil {
		t.Fatal(err)
	}
	awaitLifecycle("")

	for _, preserved := range []string{twinMemberPath, ownerPath, unrelatedPath} {
		ws := workspaces[preserved]
		if ws != nil && ws.PlanStats != nil && ws.PlanStats.PlanStatus == "hold" {
			t.Fatalf("hold cross-targeted preserved path %q", preserved)
		}
	}
}
