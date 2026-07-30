// Package jobattr owns the single rule that maps a plan job to the workspace
// its JobInfo is filed under (WorkDir/Repo/Branch).
//
// Two independent producers publish the SAME job row into the store under the
// same key, and store.ApplyUpdate(UpdateJobsDiscovered) is last-write-wins:
//
//   - the JobCollector's periodic filesystem sweep, and
//   - the flow watcher's fsnotify fast path.
//
// If they disagree, a job's recorded workspace flips every time one of them
// runs — which is how a grovetools plan job came to be persisted against an
// unrelated member checkout of a different worktree container. Sharing this
// package is what makes the two producers incapable of disagreeing.
package jobattr

import (
	"github.com/grovetools/core/pkg/workspace"
)

// Outcome reports how a job's frontmatter `worktree:` name resolved.
type Outcome int

const (
	// Resolved: exactly one node in the owner's ecosystem matched.
	Resolved Outcome = iota
	// NotFound: no node in the owner's ecosystem carries that name.
	NotFound
	// Ambiguous: several did, so no answer is defensible.
	Ambiguous
)

// Index resolves a job's frontmatter `worktree:` name to a workspace node,
// scoped to the ecosystem that owns the plan.
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
type Index struct {
	// byRoot maps a root ecosystem path to that ecosystem's nodes by name.
	// Values are slices because a name can legitimately repeat within one
	// ecosystem across kinds (a sub-project and a worktree of it).
	byRoot map[string]map[string][]*workspace.WorkspaceNode
}

// NewIndex builds the index from a node set. Input order is irrelevant by
// construction: every same-named node in an ecosystem is retained, and the
// choice between them is made by Resolve from node properties alone.
func NewIndex(nodes []*workspace.WorkspaceNode) *Index {
	ix := &Index{byRoot: make(map[string]map[string][]*workspace.WorkspaceNode)}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		root := NodeRoot(n)
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

// Resolve returns the node named name within owner's ecosystem.
func (ix *Index) Resolve(owner *workspace.WorkspaceNode, name string) (*workspace.WorkspaceNode, Outcome) {
	if ix == nil || owner == nil || name == "" {
		return nil, NotFound
	}
	root := NodeRoot(owner)
	if root == "" {
		return nil, NotFound
	}
	candidates := ix.byRoot[root][name]
	if len(candidates) == 0 {
		return nil, NotFound
	}

	// Tier 1: a `worktree:` key names a worktree. Prefer those.
	var worktrees []*workspace.WorkspaceNode
	for _, n := range candidates {
		if n.IsWorktree() {
			worktrees = append(worktrees, n)
		}
	}
	if len(worktrees) == 1 {
		return worktrees[0], Resolved
	}
	if len(worktrees) > 1 {
		return nil, Ambiguous
	}

	// Tier 2: no worktree-kind node by that name, but something else in the
	// same ecosystem matches (an ecosystem sub-project sharing the branch
	// name). Accept it only when unique — this preserves the pre-fix behavior
	// for the non-worktree hits it used to serve, without reintroducing
	// cross-ecosystem bleed.
	if len(candidates) == 1 {
		return candidates[0], Resolved
	}
	return nil, Ambiguous
}

// JobWorkspace computes the WorkDir/Repo/Branch trio a discovered JobInfo
// carries for a job whose frontmatter names worktreeName, given the plan
// owner's own workspace. Branch always mirrors the frontmatter value; WorkDir
// and Repo move to the resolved worktree ONLY on a unique in-ecosystem hit and
// otherwise stay owner-derived, so an unresolvable or ambiguous name degrades
// to "the plan's own workspace" rather than to somebody else's.
func JobWorkspace(ix *Index, owner *workspace.WorkspaceNode, worktreeName, ownerWorkDir, ownerRepo string) (workDir, repo, branch string, outcome Outcome) {
	if worktreeName == "" {
		return ownerWorkDir, ownerRepo, "", NotFound
	}
	node, outcome := ix.Resolve(owner, worktreeName)
	if outcome == Resolved {
		return node.Path, node.Name, worktreeName, outcome
	}
	return ownerWorkDir, ownerRepo, worktreeName, outcome
}

// NodeRoot returns the top-level ecosystem a node belongs to. Nodes that are
// themselves the root (or a standalone project outside any ecosystem) carry no
// RootEcosystemPath, so they are their own root.
func NodeRoot(n *workspace.WorkspaceNode) string {
	if n == nil {
		return ""
	}
	if n.RootEcosystemPath != "" {
		return n.RootEcosystemPath
	}
	return n.Path
}
