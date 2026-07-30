package watcher

import "testing"

// relevantGitEvent is the only filter between the recursive event source and
// scheduleScan. The route, not a textual .git-shaped component, is the authority
// for whether object and lock churn is git-internal.
func TestRelevantGitEvent(t *testing.T) {
	const (
		repo   = "/Users/x/code/repo"
		gitDir = repo + "/.git"
		wt     = "/Users/x/.local/share/grove/worktrees/plan/repo"
	)
	working := &gitEventRoute{root: repo}
	internal := &gitEventRoute{root: gitDir, internal: true}
	linkedInternal := &gitEventRoute{root: gitDir + "/worktrees/plan", internal: true}
	bareInternal := &gitEventRoute{root: "/srv/git/repo.git", internal: true}

	tests := []struct {
		name  string
		route *gitEventRoute
		path  string
		want  bool
	}{
		{"unrouted event", nil, "/other/file", false},
		{"in-tree git object is churn", internal, gitDir + "/objects/ab/cdef0123456789", false},
		{"in-tree pack file is churn", internal, gitDir + "/objects/pack/pack-deadbeef.pack", false},
		{"linked worktree gitdir object is churn", linkedInternal, gitDir + "/worktrees/plan/objects/ab/cdef", false},
		{"commondir linked object fallback is churn", internal, gitDir + "/worktrees/unrouted/objects/ab/cdef", false},
		{"bare repository linked object is churn", bareInternal, "/srv/git/repo.git/worktrees/plan/objects/ab/cdef", false},
		{"ordinary working-tree lock is relevant", working, repo + "/Cargo.lock", true},
		{"nested working-tree lock is relevant", working, repo + "/vendor/cache.lock", true},
		{"git internal lock is transient", internal, gitDir + "/index.lock", false},
		{"linked git internal lock is transient", linkedInternal, gitDir + "/worktrees/plan/refs/main.lock", false},
		{"working-tree fake git fixture is relevant", working, repo + "/testdata/fixture.git/worktrees/demo/objects/state.json", true},
		{"working-tree object directory is relevant", working, repo + "/objects/model.go", true},
		{"vendored object directory in grove worktree is relevant", &gitEventRoute{root: wt}, wt + "/node_modules/pkg/objects/index.js", true},
		{"ordinary working-tree file is relevant", working, repo + "/internal/watcher/git.go", true},
		{"index write is relevant", internal, gitDir + "/index", true},
		{"ref write is relevant", internal, gitDir + "/refs/heads/main", true},
		{"objects directory entry itself is relevant", internal, gitDir + "/objects", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := relevantGitEvent(tc.route, tc.path); got != tc.want {
				t.Errorf("relevantGitEvent(%+v, %q) = %v, want %v", tc.route, tc.path, got, tc.want)
			}
		})
	}
}
