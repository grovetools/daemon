package watcher

import "testing"

// relevantGitEvent is the ONLY filter between the recursive event source and
// scheduleScan, so a false negative here is a repository that silently stops
// refreshing. The `objects` cases pin the operator-precedence bug described on
// inGitObjectDB: under the old A || (B && C) grouping every path below a grove
// worktree containing `/objects/` was dropped, working tree included.
func TestRelevantGitEvent(t *testing.T) {
	const wt = "/Users/x/.local/share/grove/worktrees/plan/repo"

	tests := []struct {
		name string
		path string
		want bool
	}{{
		name: "in-tree git object is churn",
		path: "/Users/x/code/repo/.git/objects/ab/cdef0123456789",
		want: false,
	}, {
		name: "in-tree pack file is churn",
		path: "/Users/x/code/repo/.git/objects/pack/pack-deadbeef.pack",
		want: false,
	}, {
		name: "linked worktree gitdir object is churn",
		path: "/Users/x/code/repo/.git/worktrees/plan/objects/ab/cdef0123456789",
		want: false,
	}, {
		name: "submodule linked worktree gitdir object is churn",
		path: "/Users/x/code/eco/.git/modules/daemon/worktrees/daemon12/objects/ab/cdef",
		want: false,
	}, {
		name: "bare repository linked worktree gitdir object is churn",
		path: "/srv/git/repo.git/worktrees/plan/objects/ab/cdef",
		want: false,
	}, {
		// worktrees/<id>/objects is only an object DB under a git dir; a
		// repository that happens to be NAMED objects is a working tree.
		name: "grove worktree of a repository named objects",
		path: "/Users/x/.local/share/grove/worktrees/plan/objects/main.go",
		want: true,
	}, {
		// The live false-drop that motivated the fix.
		name: "vendored directory named objects inside a grove worktree",
		path: wt + "-website/node_modules/axobject-query/lib/etc/objects/index.js",
		want: true,
	}, {
		name: "working-tree directory literally named objects",
		path: wt + "/objects/model.go",
		want: true,
	}, {
		name: "working-tree file under a nested objects directory",
		path: wt + "/internal/objects/store/store.go",
		want: true,
	}, {
		name: "the objects directory entry itself is not containment",
		path: wt + "/pkg/objects",
		want: true,
	}, {
		name: "ordinary working-tree file in a grove worktree",
		path: wt + "/internal/daemon/watcher/git.go",
		want: true,
	}, {
		name: "index write is relevant",
		path: wt + "/.git/index",
		want: true,
	}, {
		name: "ref write is relevant",
		path: "/Users/x/code/repo/.git/refs/heads/main",
		want: true,
	}, {
		name: "lock files are transient",
		path: "/Users/x/code/repo/.git/index.lock",
		want: false,
	}, {
		name: "worktree lock files are transient",
		path: wt + "/.git/refs/heads/main.lock",
		want: false,
	}, {
		name: "a worktrees path component alone drops nothing",
		path: "/Users/x/.local/share/grove/worktrees/plan/repo/README.md",
		want: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := relevantGitEvent(tc.path); got != tc.want {
				t.Errorf("relevantGitEvent(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
