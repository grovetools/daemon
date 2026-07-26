package collector

import (
	"github.com/grovetools/core/pkg/workspace"
)

// worktreeOutcome reports how a frontmatter `worktree:` name resolved.
type worktreeOutcome int

const (
	// worktreeResolved: exactly one node in the owner's ecosystem matched.
	worktreeResolved worktreeOutcome = iota
	// worktreeNotFound: no node in the owner's ecosystem carries that name.
	worktreeNotFound
	// worktreeAmbiguous: several did, so no answer is defensible.
	worktreeAmbiguous
)

// worktreeIndex resolves a job's frontmatter `worktree:` name to a workspace
// node, scoped to the ecosystem that owns the plan.
//
// The original lookup was a single flat map[name]*node built across
// provider.All(). Workspace names are NOT globally unique — every ecosystem
// tends to grow a worktree named after the same branch/feature — so later
// entries silently overwrote earlier ones and resolution became dependent on
// provider enumeration order. A plan owned by ecosystem A could be persisted
// with ecosystem B's path and repo name while keeping A's branch, and a
// re-scan in a different order would "fix" or re-break it.
//
// The replacement never does last-write-wins:
//
//   - candidates are constrained to the owner's root ecosystem;
//   - worktree-kind nodes are preferred over other kinds with the same name;
//   - a tie at either tier is reported as ambiguous rather than picked.
//
// Callers keep their owner-derived WorkDir/Repo on anything but a unique hit.
type worktreeIndex struct {
	// byRoot maps a root ecosystem path to that ecosystem's nodes by name.
	// Values are slices because a name can legitimately repeat within one
	// ecosystem across kinds (a sub-project and a worktree of it).
	byRoot map[string]map[string][]*workspace.WorkspaceNode
}

func newWorktreeIndex(nodes []*workspace.WorkspaceNode) *worktreeIndex {
	ix := &worktreeIndex{byRoot: make(map[string]map[string][]*workspace.WorkspaceNode)}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		root := nodeRoot(n)
		if root == "" {
			continue
		}
		byName := ix.byRoot[root]
		if byName == nil {
			byName = make(map[string][]*workspace.WorkspaceNode)
			ix.byRoot[root] = byName
		}
		byName[n.Name] = append(byName[n.Name], n)
	}
	return ix
}

// resolve returns the node named name within owner's ecosystem.
func (ix *worktreeIndex) resolve(owner *workspace.WorkspaceNode, name string) (*workspace.WorkspaceNode, worktreeOutcome) {
	if ix == nil || owner == nil || name == "" {
		return nil, worktreeNotFound
	}
	root := nodeRoot(owner)
	if root == "" {
		return nil, worktreeNotFound
	}
	candidates := ix.byRoot[root][name]
	if len(candidates) == 0 {
		return nil, worktreeNotFound
	}

	// Tier 1: a `worktree:` key names a worktree. Prefer those.
	var worktrees []*workspace.WorkspaceNode
	for _, n := range candidates {
		if n.IsWorktree() {
			worktrees = append(worktrees, n)
		}
	}
	if len(worktrees) == 1 {
		return worktrees[0], worktreeResolved
	}
	if len(worktrees) > 1 {
		return nil, worktreeAmbiguous
	}

	// Tier 2: no worktree-kind node by that name, but something else in the
	// same ecosystem matches (an ecosystem sub-project sharing the branch
	// name). Accept it only when unique — this preserves the pre-fix behavior
	// for the non-worktree hits it used to serve, without reintroducing
	// cross-ecosystem bleed.
	if len(candidates) == 1 {
		return candidates[0], worktreeResolved
	}
	return nil, worktreeAmbiguous
}

// jobWorkspace computes the WorkDir/Repo/Branch trio a discovered JobInfo
// carries for a job whose frontmatter names worktreeName, given the plan
// owner's own workspace. Branch always mirrors the frontmatter value; WorkDir
// and Repo move to the resolved worktree ONLY on a unique in-ecosystem hit and
// otherwise stay owner-derived, so an unresolvable or ambiguous name degrades
// to "the plan's own workspace" rather than to somebody else's.
func jobWorkspace(ix *worktreeIndex, owner *workspace.WorkspaceNode, worktreeName, ownerWorkDir, ownerRepo string) (workDir, repo, branch string, outcome worktreeOutcome) {
	if worktreeName == "" {
		return ownerWorkDir, ownerRepo, "", worktreeNotFound
	}
	node, outcome := ix.resolve(owner, worktreeName)
	if outcome == worktreeResolved {
		return node.Path, node.Name, worktreeName, outcome
	}
	return ownerWorkDir, ownerRepo, worktreeName, outcome
}

// nodeRoot returns the top-level ecosystem a node belongs to. Nodes that are
// themselves the root (or a standalone project outside any ecosystem) carry no
// RootEcosystemPath, so they are their own root.
func nodeRoot(n *workspace.WorkspaceNode) string {
	if n == nil {
		return ""
	}
	if n.RootEcosystemPath != "" {
		return n.RootEcosystemPath
	}
	return n.Path
}
