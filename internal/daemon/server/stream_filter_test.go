package server

import (
	"testing"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

func ws(path string) *models.EnrichedWorkspace {
	return &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{Path: path}}
}

// An unfiltered subscriber must get the byte-identical value the handler
// produced — same pointer, no pruning, no rewritten Scanned. This is the
// old-client compatibility guarantee.
func TestApplyStreamFilterZeroPassesThroughUntouched(t *testing.T) {
	u := &apiStateUpdate{UpdateType: "workspaces_delta", Scanned: 3, WorkspaceDeltas: []*models.WorkspaceDelta{
		{Path: "/ws/a"}, {Path: "/ws/b"}, {Path: "/ws/c"},
	}}
	got, ok := applyStreamFilter(coredaemon.StreamFilter{}, u)
	if !ok {
		t.Fatal("zero filter dropped an event")
	}
	if got != u {
		t.Fatal("zero filter copied the update instead of passing it through")
	}
}

func TestApplyStreamFilterByType(t *testing.T) {
	f := coredaemon.StreamFilter{Types: []string{"session", "job_started"}}

	if _, ok := applyStreamFilter(f, &apiStateUpdate{UpdateType: "session"}); !ok {
		t.Error("declared type was dropped")
	}
	if _, ok := applyStreamFilter(f, &apiStateUpdate{UpdateType: "workspaces_delta"}); ok {
		t.Error("undeclared type survived")
	}
	// The snapshot is subject to the same allow-list — that is the whole
	// saving, since it is the largest frame the endpoint ever writes.
	if _, ok := applyStreamFilter(f, &apiStateUpdate{UpdateType: coredaemon.StreamTypeInitial}); ok {
		t.Error("snapshot survived a filter that did not name it")
	}
}

func TestApplyStreamFilterNilUpdate(t *testing.T) {
	if _, ok := applyStreamFilter(coredaemon.StreamFilter{}, nil); ok {
		t.Error("nil update reported as deliverable")
	}
}

// A path allow-list narrows a fleet-wide delta to the rows the subscriber asked
// about rather than dropping the event: a workspace the subscriber cares about
// still changed, even though the same sweep touched fifty others.
func TestApplyStreamFilterPrunesWorkspaceDeltas(t *testing.T) {
	f := coredaemon.StreamFilter{Paths: []string{"/ws/keep"}}
	u := &apiStateUpdate{UpdateType: "workspaces_delta", Scanned: 3, WorkspaceDeltas: []*models.WorkspaceDelta{
		{Path: "/ws/drop"}, {Path: "/ws/keep"}, {Path: "/ws/keep/nested"},
	}}
	got, ok := applyStreamFilter(f, u)
	if !ok {
		t.Fatal("event with matching rows was dropped")
	}
	if len(got.WorkspaceDeltas) != 2 {
		t.Fatalf("kept %d deltas, want 2: %+v", len(got.WorkspaceDeltas), got.WorkspaceDeltas)
	}
	for _, d := range got.WorkspaceDeltas {
		if d.Path == "/ws/drop" {
			t.Error("non-matching delta survived")
		}
	}
	if got.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2 (it must describe the rows on the wire)", got.Scanned)
	}
	// The prune must not mutate the store-owned slice the other subscribers
	// are about to be handed.
	if len(u.WorkspaceDeltas) != 3 || u.WorkspaceDeltas[0].Path != "/ws/drop" || u.Scanned != 3 {
		t.Fatalf("applyStreamFilter mutated the caller's update: %+v", u)
	}
}

func TestApplyStreamFilterPrunesWorkspaceSnapshot(t *testing.T) {
	f := coredaemon.StreamFilter{
		Types: []string{coredaemon.StreamTypeInitial},
		Paths: []string{"/ws/keep"},
	}
	u := &apiStateUpdate{
		UpdateType: coredaemon.StreamTypeInitial,
		Workspaces: []*models.EnrichedWorkspace{ws("/ws/drop"), ws("/ws/keep")},
	}
	got, ok := applyStreamFilter(f, u)
	if !ok {
		t.Fatal("snapshot with a matching workspace was dropped")
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].Path != "/ws/keep" {
		t.Fatalf("snapshot pruned to %+v, want only /ws/keep", got.Workspaces)
	}
}

func TestApplyStreamFilterDropsEventWhenNoRowMatches(t *testing.T) {
	f := coredaemon.StreamFilter{Paths: []string{"/ws/keep"}}
	u := &apiStateUpdate{UpdateType: "workspaces_delta", WorkspaceDeltas: []*models.WorkspaceDelta{
		{Path: "/ws/drop"}, {Path: "/ws/other"},
	}}
	if _, ok := applyStreamFilter(f, u); ok {
		t.Error("event whose every row was filtered out still reached the wire")
	}
}

// A path allow-list must not silently starve the event types it cannot judge.
// Job and session frames carry no workspace path; dropping them would make
// ?paths= a stealth type filter.
func TestApplyStreamFilterPathsDoNotTouchPathlessEvents(t *testing.T) {
	f := coredaemon.StreamFilter{Paths: []string{"/ws/keep"}}
	for _, typ := range []string{"session", "job_started", "note_index", "theme_changed"} {
		if _, ok := applyStreamFilter(f, &apiStateUpdate{UpdateType: typ}); !ok {
			t.Errorf("path filter dropped pathless event %q", typ)
		}
	}
}

// The combination is an AND: the type must be declared and at least one row
// must be in scope.
func TestApplyStreamFilterTypeAndPathCombine(t *testing.T) {
	f := coredaemon.StreamFilter{Types: []string{"workspaces_delta"}, Paths: []string{"/ws/keep"}}

	if _, ok := applyStreamFilter(f, &apiStateUpdate{
		UpdateType:      "workspaces",
		WorkspaceDeltas: []*models.WorkspaceDelta{{Path: "/ws/keep"}},
	}); ok {
		t.Error("in-scope path let an undeclared type through")
	}
	if _, ok := applyStreamFilter(f, &apiStateUpdate{
		UpdateType:      "workspaces_delta",
		WorkspaceDeltas: []*models.WorkspaceDelta{{Path: "/ws/elsewhere"}},
	}); ok {
		t.Error("declared type let an out-of-scope path through")
	}
	if _, ok := applyStreamFilter(f, &apiStateUpdate{
		UpdateType:      "workspaces_delta",
		WorkspaceDeltas: []*models.WorkspaceDelta{{Path: "/ws/keep"}},
	}); !ok {
		t.Error("event matching both halves was dropped")
	}
}

// flow's status view is the first converted client; its declaration must admit
// exactly the frames it acts on and nothing else — above all not the snapshot.
func TestFlowStatusStyleFilterAdmitsOnlyJobAndSessionFrames(t *testing.T) {
	f := coredaemon.StreamFilter{Types: coredaemon.StreamFilterTypes(map[string]bool{
		"session": true, "job_submitted": true, "job_started": true,
		"job_completed": true, "job_failed": true, "job_cancelled": true,
		"job_pending_user": true,
	})}
	for _, typ := range []string{"session", "job_started", "job_pending_user"} {
		if _, ok := applyStreamFilter(f, &apiStateUpdate{UpdateType: typ}); !ok {
			t.Errorf("actionable type %q was dropped", typ)
		}
	}
	for _, typ := range []string{
		coredaemon.StreamTypeInitial, "workspaces", "workspaces_delta", "note_index",
		"memory_index", "skill_sync", "focus", "plan_index", "satellite_status",
	} {
		if _, ok := applyStreamFilter(f, &apiStateUpdate{UpdateType: typ}); ok {
			t.Errorf("non-actionable type %q reached the wire", typ)
		}
	}
}
