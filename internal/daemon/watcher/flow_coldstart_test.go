package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func coldStartConfig(notebookRoot string) *config.Config {
	return &config.Config{Notebooks: &config.NotebooksConfig{
		Definitions: map[string]*config.Notebook{"test": {
			RootDir: notebookRoot, PlansPathTemplate: "notespaces/{{ .Workspace.Name }}/plans",
		}},
		Rules: &config.NotebookRules{Default: "test"},
	}}
}

func coldStartWorkspace(t *testing.T, root, name string) *models.EnrichedWorkspace {
	t.Helper()
	wsPath := filepath.Join(root, "code", name)
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	return &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{
		Name: name, Path: wsPath, Kind: workspace.KindEcosystemRoot, NotebookName: "test",
	}}
}

// TestFlowColdStartPublishesAfterLateWorkspaceDiscovery is the cold-start
// regression for the 72-plan-portfolio latency finding: when workspace
// discovery completes AFTER the FlowHandler's OnStart refresh has already
// fired (the common cold-boot interleaving), the UpdateWorkspaces edge must
// produce the first populated plan index promptly — not wait for the
// 5-minute reconciliation ticker or a coincidental plan-file event.
//
// The 30-second ceiling is a deliberately generous CI-safe bound; the
// expected latency is well under one second.
func TestFlowColdStartPublishesAfterLateWorkspaceDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

	notebookRoot := filepath.Join(root, "notebook")
	cfg := coldStartConfig(notebookRoot)

	workspaces := make(map[string]*models.EnrichedWorkspace)
	total := 0
	for _, name := range []string{"alpha-repo", "beta-repo", "gamma-repo"} {
		ws := coldStartWorkspace(t, root, name)
		workspaces[ws.Path] = ws
		for i := 0; i < 24; i++ {
			writeIndexedPlan(t, filepath.Join(notebookRoot, "notespaces", name, "plans", name+"-live-"+string(rune('a'+i))))
			total++
		}
	}

	st := store.New()
	h := NewFlowHandler(st, cfg, 50)
	uw, err := NewUnifiedWatcher(st, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	uw.Register(h)
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)
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

	// Let the OnStart refresh fire against the empty store: it must not
	// publish a bogus "scanned, zero plans" snapshot before discovery.
	time.Sleep(300 * time.Millisecond)
	if snap := st.GetPlanIndexSnapshot(); snap.Revision != 0 || len(snap.Plans) != 0 {
		t.Fatalf("pre-discovery snapshot published: revision=%d plans=%d", snap.Revision, len(snap.Plans))
	}

	// Late workspace discovery: this is the edge that must now trigger the
	// first populated index build.
	discoveredAt := time.Now()
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "workspace", Payload: workspaces})

	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			snap := st.GetPlanIndexSnapshot()
			t.Fatalf("populated plan index did not appear within 30s of workspace discovery: revision=%d plans=%d", snap.Revision, len(snap.Plans))
		case <-ch:
		case <-time.After(50 * time.Millisecond):
		}
		if snap := st.GetPlanIndexSnapshot(); len(snap.Plans) == total {
			t.Logf("first populated snapshot (%d plans) %.0fms after discovery", total, time.Since(discoveredAt).Seconds()*1000)
			return
		}
	}
}

// TestFlowScopedRefreshServesUnaffectedDirsFromCache verifies the incremental
// delta path: an event scoped to one plans directory re-reads only that
// directory from disk, while rows from other directories are carried forward
// from the per-directory cache — and a later full refresh reconciles anything
// the scoped pass deliberately skipped.
func TestFlowScopedRefreshServesUnaffectedDirsFromCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

	notebookRoot := filepath.Join(root, "notebook")
	cfg := coldStartConfig(notebookRoot)

	wsA := coldStartWorkspace(t, root, "repo-a")
	wsB := coldStartWorkspace(t, root, "repo-b")
	plansA := filepath.Join(notebookRoot, "notespaces", "repo-a", "plans")
	plansB := filepath.Join(notebookRoot, "notespaces", "repo-b", "plans")
	writeIndexedPlan(t, filepath.Join(plansA, "a-one"))
	writeIndexedPlan(t, filepath.Join(plansA, "a-two"))
	writeIndexedPlan(t, filepath.Join(plansB, "b-one"))

	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: map[string]*models.EnrichedWorkspace{
		wsA.Path: wsA, wsB.Path: wsB,
	}})

	// Huge debounce so only the explicit synchronous refreshes below run.
	h := NewFlowHandler(st, cfg, 600000)
	h.ComputeWatchPaths(st.GetWorkspaces())

	h.runRefresh(true, nil)
	if snap := st.GetPlanIndexSnapshot(); len(snap.Plans) != 3 {
		t.Fatalf("full refresh rows=%d want 3: %+v", len(snap.Plans), snap.Plans)
	}

	// Mutate BOTH directories on disk, but deliver an event only for dir A.
	// The scoped pass must pick up A's change and keep serving B from cache.
	writeIndexedPlan(t, filepath.Join(plansB, "b-two")) // no event: must stay invisible
	holdConfig := filepath.Join(plansA, "a-one", ".grove-plan.yml")
	if err := os.WriteFile(holdConfig, []byte("status: hold\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleEvents(context.Background(), []fsnotify.Event{{Name: holdConfig, Op: fsnotify.Write}}); err != nil {
		t.Fatal(err)
	}

	snap := st.GetPlanIndexSnapshot()
	byName := map[string]models.PlanSummary{}
	for _, row := range snap.Plans {
		byName[row.PlanName] = row
	}
	if row, ok := byName["a-one"]; !ok || row.Lifecycle != "hold" {
		t.Fatalf("scoped refresh missed a-one hold: %+v", byName)
	}
	if _, ok := byName["b-one"]; !ok {
		t.Fatalf("scoped refresh dropped cached dir B rows: %+v", byName)
	}
	if _, ok := byName["b-two"]; ok {
		t.Fatalf("scoped refresh for dir A re-read dir B from disk (cache not used): %+v", byName)
	}
	if len(snap.Plans) != 3 {
		t.Fatalf("scoped refresh rows=%d want 3", len(snap.Plans))
	}

	// Removal through the scoped path: deleting a plan in dir A must drop
	// exactly that row. fsnotify reports deletions under the watched (already
	// symlink-resolved) spelling, so the synthetic event uses it too.
	resolvedPlansA := resolveFlowWatchPath(plansA)
	if err := os.RemoveAll(filepath.Join(plansA, "a-two")); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleEvents(context.Background(), []fsnotify.Event{{Name: filepath.Join(resolvedPlansA, "a-two", ".grove-plan.yml"), Op: fsnotify.Rename}}); err != nil {
		t.Fatal(err)
	}
	snap = st.GetPlanIndexSnapshot()
	names := map[string]bool{}
	for _, row := range snap.Plans {
		names[row.PlanName] = true
	}
	if names["a-two"] || !names["a-one"] || !names["b-one"] || len(snap.Plans) != 2 {
		t.Fatalf("scoped removal produced %v", names)
	}

	// A full refresh reconciles what the scoped passes skipped.
	h.runRefresh(true, nil)
	snap = st.GetPlanIndexSnapshot()
	names = map[string]bool{}
	for _, row := range snap.Plans {
		names[row.PlanName] = true
	}
	if !names["b-two"] || len(snap.Plans) != 3 {
		t.Fatalf("full refresh did not reconcile: %v", names)
	}
}
