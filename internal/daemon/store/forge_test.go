package store

import (
	"testing"

	"github.com/grovetools/core/pkg/forge"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

// TestReviewStatsDeltaApplies pins the poller's projection onto the same
// UpdateWorkspacesDelta path NoteCounts uses: the delta must land on the
// workspace, and a delta that does not mention ReviewStats must leave an
// existing value alone (the pointer convention every other field here follows).
func TestReviewStatsDeltaApplies(t *testing.T) {
	s := New()
	s.ApplyUpdate(Update{
		Type: UpdateWorkspaces,
		Payload: map[string]*models.EnrichedWorkspace{
			"/ws/demo": {WorkspaceNode: &workspace.WorkspaceNode{Path: "/ws/demo", Name: "demo"}},
		},
	})

	stats := &models.ReviewStats{
		SchemaVersion: models.ReviewStatsSchemaVersion,
		Freshness:     models.ReviewFreshnessFresh,
		Repo:          "github.com/grovetools/demo",
		PRs:           &models.PRCounts{Open: 2, Draft: 1},
		Checks:        forge.CheckStatePending,
	}
	s.ApplyUpdate(Update{
		Type:    UpdateWorkspacesDelta,
		Source:  "forge_poller",
		Payload: []*models.WorkspaceDelta{{Path: "/ws/demo", ReviewStats: stats}},
	})

	got := s.Get().Workspaces["/ws/demo"].ReviewStats
	if got == nil {
		t.Fatal("ReviewStats delta did not land on the workspace")
	}
	if got.Freshness != models.ReviewFreshnessFresh || got.PRs.Open != 2 {
		t.Errorf("ReviewStats = %+v, want the applied value", got)
	}

	// An unrelated delta must not clobber it.
	s.ApplyUpdate(Update{
		Type:    UpdateWorkspacesDelta,
		Source:  "note_watcher",
		Payload: []*models.WorkspaceDelta{{Path: "/ws/demo", NoteCounts: &models.NoteCounts{Inbox: 3}}},
	})
	if after := s.Get().Workspaces["/ws/demo"].ReviewStats; after == nil || after.PRs.Open != 2 {
		t.Errorf("ReviewStats = %+v after an unrelated delta, want it preserved", after)
	}
}

// TestForgeStateIsBroadcastNotStored: the poller owns its cache. The store
// relays forge_state to subscribers and keeps nothing of its own — a
// subscriber that missed a frame reconciles through the workspace enrichment,
// not through daemon-held forge state.
func TestForgeStateIsBroadcastNotStored(t *testing.T) {
	s := New()
	sub := s.Subscribe()
	defer s.Unsubscribe(sub)

	payload := &ForgeStatePayload{Repos: []ForgeRepoState{{
		Provider: "github",
		Repo:     "github.com/grovetools/demo",
		State:    ForgeStateUnknown,
	}}}
	s.ApplyUpdate(Update{Type: UpdateForgeState, Source: "forge_poller", Payload: payload})

	select {
	case u := <-sub:
		if u.Type != UpdateForgeState {
			t.Fatalf("subscriber got %q, want forge_state", u.Type)
		}
		got, ok := u.Payload.(*ForgeStatePayload)
		if !ok || len(got.Repos) != 1 {
			t.Fatalf("payload did not survive the broadcast: %+v", u.Payload)
		}
		if got.Repos[0].State != ForgeStateUnknown {
			t.Errorf("state = %q, want unknown", got.Repos[0].State)
		}
	default:
		t.Fatal("forge_state was not broadcast to subscribers")
	}
}
