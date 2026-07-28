package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

// Landing state moves independently of the coarse status, so every field must
// participate in the equality check that gates delta emission.
func TestLandingEqual(t *testing.T) {
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	base := func() *git.LandingState {
		return &git.LandingState{
			Onto: "main", Ahead: 2, Behind: 1,
			HasRemote: true, BehindOrigin: 3,
			LastCommitAt: at, Computed: true,
		}
	}
	if !LandingEqual(nil, nil) {
		t.Error("two absent landing states compared unequal")
	}
	if LandingEqual(base(), nil) || LandingEqual(nil, base()) {
		t.Error("an absent landing state compared equal to a present one")
	}
	if !LandingEqual(base(), base()) {
		t.Error("identical landing states compared unequal")
	}

	mutations := map[string]func(*git.LandingState){
		"onto":          func(l *git.LandingState) { l.Onto = "master" },
		"ahead":         func(l *git.LandingState) { l.Ahead = 3 },
		"behind":        func(l *git.LandingState) { l.Behind = 0 },
		"has remote":    func(l *git.LandingState) { l.HasRemote = false },
		"behind origin": func(l *git.LandingState) { l.BehindOrigin = 0 },
		"last commit":   func(l *git.LandingState) { l.LastCommitAt = at.Add(time.Hour) },
		"computed":      func(l *git.LandingState) { l.Computed = false },
	}
	for name, mutate := range mutations {
		other := base()
		mutate(other)
		if LandingEqual(base(), other) {
			t.Errorf("a change to %s was swallowed; its delta would never be emitted", name)
		}
	}

	// The same instant carried by a differently-constructed time.Time must
	// compare equal: it is re-parsed from `git log` on every computation, and a
	// pointer-sensitive == would emit a delta on every sweep.
	reparsed := base()
	reparsed.LastCommitAt = at.In(time.FixedZone("UTC", 0))
	if !LandingEqual(base(), reparsed) {
		t.Error("the same instant in an equivalent zone compared unequal")
	}
}

// The delta convention: nil means unchanged, so a metadata-only delta must not
// clear a repo's cached landing state.
func TestWorkspaceDeltaLandingFollowsPointerConvention(t *testing.T) {
	const path = "/repo"
	land := &git.LandingState{Onto: "main", Behind: 2, Computed: true}
	st := New()
	st.ApplyUpdate(Update{Type: UpdateWorkspaces, Source: "test", Payload: enrichedFixture(path)})
	st.ApplyUpdate(Update{Type: UpdateWorkspacesDelta, Source: "git", Payload: []*models.WorkspaceDelta{{
		Path: path, GitLanding: land,
	}}})
	if got := st.Get().Workspaces[path].GitLanding; got == nil || got.Behind != 2 {
		t.Fatalf("landing state = %+v, want the delta's", got)
	}

	st.ApplyUpdate(Update{Type: UpdateWorkspacesDelta, Source: "flow_watcher", Payload: []*models.WorkspaceDelta{{
		Path: path, PlanStats: &models.PlanStats{TotalPlans: 1},
	}}})
	if got := st.Get().Workspaces[path].GitLanding; got == nil || got.Behind != 2 {
		t.Fatalf("a metadata-only delta cleared the cached landing state: %+v", got)
	}
}

func enrichedFixture(path string) map[string]*models.EnrichedWorkspace {
	return map[string]*models.EnrichedWorkspace{
		path: {WorkspaceNode: &workspace.WorkspaceNode{Path: path}},
	}
}
