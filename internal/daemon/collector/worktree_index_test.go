package collector

import (
	"testing"

	"github.com/grovetools/core/pkg/workspace"
)

// Two ecosystems, each with a worktree named "markdown-toc" — the exact shape
// that made the old flat map[name]*node last-write-wins.
const (
	rootA = "/eco/grovetools"
	rootB = "/eco/treemux"
)

func ecoRoot(path, name string) *workspace.WorkspaceNode {
	return &workspace.WorkspaceNode{Name: name, Path: path, Kind: workspace.KindEcosystemRoot}
}

func ecoWorktree(root, path, name string) *workspace.WorkspaceNode {
	return &workspace.WorkspaceNode{
		Name:                name,
		Path:                path,
		Kind:                workspace.KindEcosystemWorktree,
		RootEcosystemPath:   root,
		ParentEcosystemPath: root,
	}
}

func ecoSubProject(root, path, name string) *workspace.WorkspaceNode {
	return &workspace.WorkspaceNode{
		Name:                name,
		Path:                path,
		Kind:                workspace.KindEcosystemSubProject,
		RootEcosystemPath:   root,
		ParentEcosystemPath: root,
	}
}

// collidingWorld returns the two ecosystems and their same-named worktrees.
func collidingWorld() (a, b *workspace.WorkspaceNode, wtA, wtB *workspace.WorkspaceNode) {
	a = ecoRoot(rootA, "grovetools")
	b = ecoRoot(rootB, "treemux")
	wtA = ecoWorktree(rootA, rootA+"/.grove-worktrees/markdown-toc", "markdown-toc")
	wtB = ecoWorktree(rootB, rootB+"/.grove-worktrees/markdown-toc", "markdown-toc")
	return
}

// TestWorktreeResolutionIsOwnerRelativeAndOrderIndependent is the collision
// regression. Whichever order the provider enumerates nodes in, a plan owned by
// ecosystem A must resolve A's worktree — never B's. The old lookup returned
// whichever node happened to be indexed last.
func TestWorktreeResolutionIsOwnerRelativeAndOrderIndependent(t *testing.T) {
	a, b, wtA, wtB := collidingWorld()

	orders := map[string][]*workspace.WorkspaceNode{
		"A first":     {a, wtA, b, wtB},
		"B first":     {b, wtB, a, wtA},
		"interleaved": {wtB, a, wtA, b},
	}

	for name, nodes := range orders {
		t.Run(name, func(t *testing.T) {
			ix := newWorktreeIndex(nodes)

			got, outcome := ix.resolve(a, "markdown-toc")
			if outcome != worktreeResolved || got != wtA {
				t.Fatalf("owner A resolved to %+v (outcome %v), want %+v", got, outcome, wtA)
			}
			got, outcome = ix.resolve(b, "markdown-toc")
			if outcome != worktreeResolved || got != wtB {
				t.Fatalf("owner B resolved to %+v (outcome %v), want %+v", got, outcome, wtB)
			}
		})
	}
}

// TestWorktreeResolutionFromAWorktreeOwner: plans are frequently owned by a
// worktree rather than the ecosystem root. The owner's ROOT ecosystem is what
// bounds the search, so a sibling worktree in the same ecosystem still resolves.
func TestWorktreeResolutionFromAWorktreeOwner(t *testing.T) {
	a, b, wtA, wtB := collidingWorld()
	siblingA := ecoWorktree(rootA, rootA+"/.grove-worktrees/perf-audit", "perf-audit")

	ix := newWorktreeIndex([]*workspace.WorkspaceNode{b, wtB, a, wtA, siblingA})

	got, outcome := ix.resolve(siblingA, "markdown-toc")
	if outcome != worktreeResolved || got != wtA {
		t.Fatalf("worktree-owned plan resolved to %+v (outcome %v), want A's worktree %+v", got, outcome, wtA)
	}
}

// TestWorktreeResolutionReportsAmbiguityInsteadOfGuessing: two worktree-kind
// nodes with the same name inside ONE ecosystem have no defensible answer.
// Reporting ambiguity is what keeps the caller on its owner-derived path
// instead of silently adopting one at random.
func TestWorktreeResolutionReportsAmbiguityInsteadOfGuessing(t *testing.T) {
	a := ecoRoot(rootA, "grovetools")
	dup1 := ecoWorktree(rootA, rootA+"/.grove-worktrees/markdown-toc", "markdown-toc")
	dup2 := ecoWorktree(rootA, rootA+"/nested/.grove-worktrees/markdown-toc", "markdown-toc")

	ix := newWorktreeIndex([]*workspace.WorkspaceNode{a, dup1, dup2})

	got, outcome := ix.resolve(a, "markdown-toc")
	if outcome != worktreeAmbiguous {
		t.Fatalf("outcome = %v, want worktreeAmbiguous", outcome)
	}
	if got != nil {
		t.Fatalf("ambiguous resolution returned a node: %+v", got)
	}
}

// TestWorktreeResolutionPrefersWorktreeKind: a `worktree:` key names a
// worktree, so a same-named sub-project in the same ecosystem must not shadow
// it. Kind is the tiebreak, not enumeration order.
func TestWorktreeResolutionPrefersWorktreeKind(t *testing.T) {
	a := ecoRoot(rootA, "grovetools")
	sub := ecoSubProject(rootA, rootA+"/markdown-toc", "markdown-toc")
	wt := ecoWorktree(rootA, rootA+"/.grove-worktrees/markdown-toc", "markdown-toc")

	for _, nodes := range [][]*workspace.WorkspaceNode{{a, sub, wt}, {a, wt, sub}} {
		ix := newWorktreeIndex(nodes)
		got, outcome := ix.resolve(a, "markdown-toc")
		if outcome != worktreeResolved || got != wt {
			t.Fatalf("resolved to %+v (outcome %v), want the worktree node", got, outcome)
		}
	}
}

// TestWorktreeResolutionMissNeverCrossesEcosystems: a name that exists ONLY in
// another ecosystem must be a miss, not a cross-ecosystem hit. This is the
// precise failure that persisted a grovetools plan pointing at a treemux path.
func TestWorktreeResolutionMissNeverCrossesEcosystems(t *testing.T) {
	a, b, _, wtB := collidingWorld()
	ix := newWorktreeIndex([]*workspace.WorkspaceNode{a, b, wtB})

	got, outcome := ix.resolve(a, "markdown-toc")
	if outcome != worktreeNotFound || got != nil {
		t.Fatalf("resolved to %+v (outcome %v), want a miss — %s belongs to another ecosystem", got, outcome, wtB.Path)
	}
}

// TestJobWorkspaceFieldsPersistOwnerPathOnCollision asserts the three JobInfo
// fields the daemon persists (WorkDir, Repo, Branch) for each outcome. Branch
// always mirrors the frontmatter; WorkDir/Repo move only on a unique hit.
func TestJobWorkspaceFieldsPersistOwnerPathOnCollision(t *testing.T) {
	a, b, wtA, wtB := collidingWorld()
	dupB := ecoWorktree(rootB, rootB+"/nested/.grove-worktrees/markdown-toc", "markdown-toc")

	t.Run("unique hit adopts the worktree", func(t *testing.T) {
		ix := newWorktreeIndex([]*workspace.WorkspaceNode{b, wtB, a, wtA})
		wd, repo, branch, outcome := jobWorkspace(ix, a, "markdown-toc", rootA, "grovetools")
		if outcome != worktreeResolved {
			t.Fatalf("outcome = %v, want resolved", outcome)
		}
		if wd != wtA.Path || repo != "markdown-toc" || branch != "markdown-toc" {
			t.Fatalf("WorkDir=%q Repo=%q Branch=%q, want %q/markdown-toc/markdown-toc", wd, repo, branch, wtA.Path)
		}
	})

	t.Run("ambiguous keeps the owner path", func(t *testing.T) {
		ix := newWorktreeIndex([]*workspace.WorkspaceNode{b, wtB, dupB})
		wd, repo, branch, outcome := jobWorkspace(ix, b, "markdown-toc", rootB, "treemux")
		if outcome != worktreeAmbiguous {
			t.Fatalf("outcome = %v, want ambiguous", outcome)
		}
		if wd != rootB || repo != "treemux" {
			t.Fatalf("WorkDir=%q Repo=%q, want the owner's %q/treemux", wd, repo, rootB)
		}
		if branch != "markdown-toc" {
			t.Fatalf("Branch=%q, want the frontmatter value even when unresolvable", branch)
		}
	})

	t.Run("foreign-only name keeps the owner path", func(t *testing.T) {
		ix := newWorktreeIndex([]*workspace.WorkspaceNode{a, b, wtB})
		wd, repo, branch, outcome := jobWorkspace(ix, a, "markdown-toc", rootA, "grovetools")
		if outcome != worktreeNotFound {
			t.Fatalf("outcome = %v, want not-found", outcome)
		}
		if wd != rootA || repo != "grovetools" || branch != "markdown-toc" {
			t.Fatalf("WorkDir=%q Repo=%q Branch=%q, want %q/grovetools/markdown-toc", wd, repo, branch, rootA)
		}
	})

	t.Run("no worktree key leaves branch empty", func(t *testing.T) {
		ix := newWorktreeIndex([]*workspace.WorkspaceNode{a, wtA})
		wd, repo, branch, _ := jobWorkspace(ix, a, "", rootA, "grovetools")
		if wd != rootA || repo != "grovetools" || branch != "" {
			t.Fatalf("WorkDir=%q Repo=%q Branch=%q, want %q/grovetools/<empty>", wd, repo, branch, rootA)
		}
	})
}
