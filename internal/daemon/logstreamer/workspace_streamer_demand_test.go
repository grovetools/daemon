package logstreamer

import (
	"context"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// newDemandFixture builds a streamer over n workspaces without starting any
// goroutines, so the tests below exercise the demand computation itself rather
// than racing real tailers against real log directories.
func newDemandFixture(t *testing.T, paths ...string) (*WorkspaceStreamer, *store.Store) {
	t.Helper()
	st := store.New()
	wss := make(map[string]*models.EnrichedWorkspace, len(paths))
	for _, p := range paths {
		wss[p] = &models.EnrichedWorkspace{
			WorkspaceNode: &workspace.WorkspaceNode{Path: p, Name: p},
		}
	}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: wss})
	return NewWorkspaceStreamer(st, 100), st
}

// demandFor runs the locked demand computation the way syncTailers does.
func demandFor(ws *WorkspaceStreamer) map[string]bool {
	workspaces := ws.store.GetWorkspaces()
	jobs := ws.store.GetJobs()
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.refreshEcoMapLocked()
	return ws.demandedPathsLocked(workspaces, jobs)
}

// TestNoSubscribersNoJobsDemandsNothing is the whole point of the change: a
// daemon that has discovered 600 workspaces and is being asked for none of
// their logs should tail none of them. Before this, every discovered workspace
// got a permanent tailer goroutine — 650 of them on the measured machine,
// against a 2000-goroutine budget that was already breached.
func TestNoSubscribersNoJobsDemandsNothing(t *testing.T) {
	ws, _ := newDemandFixture(t, "/ws/a", "/ws/b", "/ws/c")

	if got := demandFor(ws); len(got) != 0 {
		t.Fatalf("idle streamer demanded %d workspaces (%v), want 0", len(got), got)
	}
}

// TestWorkspaceScopedSubscriberDemandsOnlyItsOwn pins the narrow case a log
// viewer actually opens: one workspace on screen must not resurrect the other
// 600 tailers.
func TestWorkspaceScopedSubscriberDemandsOnlyItsOwn(t *testing.T) {
	ws, _ := newDemandFixture(t, "/ws/a", "/ws/b", "/ws/c")

	_, ch := ws.Subscribe(models.LogStreamOptions{Scope: "workspace", Workspace: "/ws/b"})
	defer ws.Unsubscribe(ch)

	got := demandFor(ws)
	if len(got) != 1 || !got["/ws/b"] {
		t.Fatalf("workspace-scoped subscriber demanded %v, want exactly {/ws/b}", got)
	}
}

// TestAllScopeSubscriberDemandsEverything is the escape hatch. The aggregate
// `core logs` view must be unchanged — it just no longer costs 650 goroutines
// when nobody has it open.
func TestAllScopeSubscriberDemandsEverything(t *testing.T) {
	ws, _ := newDemandFixture(t, "/ws/a", "/ws/b", "/ws/c")

	for _, scope := range []string{"all", ""} {
		_, ch := ws.Subscribe(models.LogStreamOptions{Scope: scope})
		got := demandFor(ws)
		if len(got) != 3 {
			t.Errorf("scope %q demanded %d workspaces (%v), want all 3", scope, len(got), got)
		}
		ws.Unsubscribe(ch)
	}
}

// TestSystemScopeSubscriberDemandsNoWorkspaces: the system tailer is
// unconditional (started outside the demand set), so a system-only viewer must
// not pull in workspace tailers.
func TestSystemScopeSubscriberDemandsNoWorkspaces(t *testing.T) {
	ws, _ := newDemandFixture(t, "/ws/a", "/ws/b")

	_, ch := ws.Subscribe(models.LogStreamOptions{Scope: "system"})
	defer ws.Unsubscribe(ch)

	if got := demandFor(ws); len(got) != 0 {
		t.Fatalf("system-scoped subscriber demanded %v, want none", got)
	}
}

// TestRunningJobDemandsItsWorkspace covers the half of the contract that is not
// about readers: work happening in a workspace must keep its log flowing into
// the ring, so the client that subscribes a moment later has history. Terminal
// jobs must NOT hold a tailer open — that is what would turn this into the same
// unbounded set by another route.
func TestRunningJobDemandsItsWorkspace(t *testing.T) {
	ws, st := newDemandFixture(t, "/ws/a", "/ws/b")

	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobSubmitted,
		Source:  "test",
		Payload: &models.JobInfo{ID: "j1", WorkDir: "/ws/a", Status: "running"},
	})
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobSubmitted,
		Source:  "test",
		Payload: &models.JobInfo{ID: "j2", WorkDir: "/ws/b", Status: "completed"},
	})

	got := demandFor(ws)
	if !got[store.NormalizePathKey("/ws/a")] {
		t.Errorf("running job's workspace /ws/a not demanded; got %v", got)
	}
	if got[store.NormalizePathKey("/ws/b")] {
		t.Errorf("completed job's workspace /ws/b is still demanded; got %v", got)
	}
}

// TestUnsubscribeReleasesDemand pins teardown. Without it the set only ever
// grows and demand-driven tailing degrades to the old behavior after a client
// has visited enough workspaces.
func TestUnsubscribeReleasesDemand(t *testing.T) {
	ws, _ := newDemandFixture(t, "/ws/a", "/ws/b")

	_, ch := ws.Subscribe(models.LogStreamOptions{Scope: "workspace", Workspace: "/ws/a"})
	if got := demandFor(ws); !got["/ws/a"] {
		t.Fatalf("subscriber did not create demand; got %v", got)
	}
	ws.Unsubscribe(ch)
	if got := demandFor(ws); len(got) != 0 {
		t.Fatalf("demand survived unsubscribe: %v", got)
	}
}

// TestSubscribeKicksResync: a client opening a workspace log stream must not
// wait out the 5s sync tick before its tailer exists. Subscribe rings the
// doorbell watchWorkspaces selects on.
func TestSubscribeKicksResync(t *testing.T) {
	ws, _ := newDemandFixture(t, "/ws/a")

	// Drain anything left from construction.
	select {
	case <-ws.resync:
	default:
	}

	_, ch := ws.Subscribe(models.LogStreamOptions{Scope: "workspace", Workspace: "/ws/a"})
	select {
	case <-ws.resync:
	case <-time.After(time.Second):
		t.Fatal("Subscribe did not kick a resync; the tailer would not start until the next tick")
	}

	ws.Unsubscribe(ch)
	select {
	case <-ws.resync:
	case <-time.After(time.Second):
		t.Fatal("Unsubscribe did not kick a resync; the idle tailer would linger until the next tick")
	}
}

// TestSyncTailersHonorsDemand drives the real syncTailers path (not just the
// demand helper) to prove the tailer map itself stays empty while idle and that
// the unconditional system tailer is the only survivor.
func TestSyncTailersHonorsDemand(t *testing.T) {
	ws, _ := newDemandFixture(t, "/ws/a", "/ws/b", "/ws/c")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.syncTailers(ctx)

	ws.mu.RLock()
	n := len(ws.activeTailers)
	_, hasSystem := ws.activeTailers["system"]
	ws.mu.RUnlock()

	if !hasSystem {
		t.Error("system tailer should always run")
	}
	if n != 1 {
		t.Errorf("idle daemon has %d tailers, want 1 (system only)", n)
	}
	if got := ws.ActiveTailers(); got != 1 {
		t.Errorf("ActiveTailers()=%d, want 1", got)
	}
}
