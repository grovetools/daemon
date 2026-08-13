package store

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

// The pending marker is the honest-staleness half of the tiered sweep: it must
// set, clear, and — critically — survive deltas from producers that know
// nothing about git freshness.
func TestGitStatusPendingSetsClearsAndSurvivesUnrelatedDeltas(t *testing.T) {
	s := New()
	s.ApplyUpdate(Update{
		Type: UpdateWorkspaces,
		Payload: map[string]*models.EnrichedWorkspace{
			"/ws/demo": {WorkspaceNode: &workspace.WorkspaceNode{Path: "/ws/demo"}},
		},
	})
	pending, notPending := true, false

	s.ApplyUpdate(Update{
		Type:    UpdateWorkspacesDelta,
		Payload: []*models.WorkspaceDelta{{Path: "/ws/demo", GitStatusPending: &pending}},
	})
	if !s.Get().Workspaces["/ws/demo"].GitStatusPending {
		t.Fatal("pending marker did not land")
	}

	// A note count arriving mid-sweep must not make an unswept workspace look
	// fresh: only the git producers speak about git freshness.
	s.ApplyUpdate(Update{
		Type:    UpdateWorkspacesDelta,
		Payload: []*models.WorkspaceDelta{{Path: "/ws/demo", NoteCounts: &models.NoteCounts{Inbox: 1}}},
	})
	if !s.Get().Workspaces["/ws/demo"].GitStatusPending {
		t.Fatal("an unrelated delta cleared the pending marker")
	}

	s.ApplyUpdate(Update{
		Type:    UpdateWorkspacesDelta,
		Payload: []*models.WorkspaceDelta{{Path: "/ws/demo", GitStatusPending: &notPending}},
	})
	if s.Get().Workspaces["/ws/demo"].GitStatusPending {
		t.Fatal("the sweep's first scan did not clear the pending marker")
	}
}

// The sweep event types must be in the vocabulary, or the on_event hook
// matcher and the config reference cannot name them.
func TestSweepUpdateTypesAreDeclared(t *testing.T) {
	for _, typ := range []UpdateType{UpdateSweepStarted, UpdateSweepProgress, UpdateSweepCompleted} {
		if !IsKnownUpdateType(typ) {
			t.Errorf("%q is not in the update-type vocabulary", typ)
		}
	}
}
