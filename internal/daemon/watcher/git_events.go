package watcher

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// gitEventRoute maps one recursively watched root to every workspace whose git
// state can be affected below it. Worktree roots normally map one-to-one;
// common git dirs can map to every linked worktree in a repository.
type gitEventRoute struct {
	root  string
	nodes []*workspace.WorkspaceNode
	// internal marks a root that is a git directory (the gitdir or the
	// commondir) rather than a working tree. It exists so dead-subtree
	// suppression can be confined to working-tree paths: HEAD, index and refs
	// writes are how grove learns about commits and branch switches, and a
	// .gitignore is free to contain patterns like `index` or `HEAD`.
	//
	// It LATCHES true. A root registered as BOTH a workspace path and a git dir
	// (a bare layout where node.Path IS the git dir) counts as internal. That
	// disables working-tree dead-subtree suppression while still allowing the
	// narrow object/lock filters defined from the proven gitdir identity.
	internal bool
}

// buildGitEventRoutes builds the path-to-repository index used by the global
// recursive event source. Both worktree contents and git internals are covered.
func buildGitEventRoutes(ctx context.Context, workspaces []*models.EnrichedWorkspace) []gitEventRoute {
	byRoot := make(map[string]map[string]*workspace.WorkspaceNode)
	internalRoots := make(map[string]bool)
	add := func(root string, node *workspace.WorkspaceNode, internal bool) {
		if root == "" || node == nil {
			return
		}
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		root = filepath.Clean(root)
		if byRoot[root] == nil {
			byRoot[root] = make(map[string]*workspace.WorkspaceNode)
		}
		byRoot[root][node.Path] = node
		if internal {
			internalRoots[root] = true
		}
	}
	for _, ew := range workspaces {
		if ew == nil || ew.WorkspaceNode == nil {
			continue
		}
		node := ew.WorkspaceNode
		gitDir, commonDir, err := git.ResolveGitDirs(ctx, node.Path)
		if err != nil { // container/ecosystem rows are not repositories
			continue
		}
		add(node.Path, node, false)
		add(gitDir, node, true)
		add(commonDir, node, true)
	}
	routes := make([]gitEventRoute, 0, len(byRoot))
	for root, nodesByPath := range byRoot {
		nodes := make([]*workspace.WorkspaceNode, 0, len(nodesByPath))
		for _, node := range nodesByPath {
			nodes = append(nodes, node)
		}
		routes = append(routes, gitEventRoute{root: root, nodes: nodes, internal: internalRoots[root]})
	}
	// Deepest root first makes nested repositories route to their actual owner,
	// rather than also dirtying every containing ecosystem repository.
	sort.Slice(routes, func(i, j int) bool { return len(routes[i].root) > len(routes[j].root) })
	return routes
}

// resolveEventPath canonicalizes one raw event path against the same
// symlink-resolved, cleaned form buildGitEventRoutes stores its roots in.
// Callers resolve ONCE and pass the result to routing, invalidation and
// suppression alike: EvalSymlinks walks every path component, so doing it per
// consumer would multiply a syscall on the daemon's highest-volume hot path,
// and mixing resolved roots with unresolved paths breaks filepath.Rel.
func resolveEventPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return filepath.Clean(path)
}

// routeGitEvent returns the deepest route containing path together with the
// workspaces it affects. path must already be canonical (see resolveEventPath).
// The route itself is returned because the caller needs its metadata — notably
// route.internal, which gates suppression.
func routeGitEvent(path string, routes []gitEventRoute) (*gitEventRoute, []*workspace.WorkspaceNode) {
	for i := range routes {
		route := &routes[i]
		if path == route.root || strings.HasPrefix(path, route.root+string(filepath.Separator)) {
			return route, route.nodes
		}
	}
	return nil, nil
}

// relevantGitEvent filters high-volume git-internal churn that cannot change
// the status surface. Classification is based on the route that was built from
// git's resolved gitdir/commondir identities, never on path spellings: ordinary
// working-tree Cargo.lock files and directories named fixture.git are valid
// tracked content and must always schedule a scan.
func relevantGitEvent(route *gitEventRoute, path string) bool {
	if route == nil {
		return false
	}
	if !route.internal {
		return true
	}
	if strings.HasSuffix(filepath.ToSlash(path), ".lock") {
		return false
	}
	return !inGitObjectDB(route, path)
}

// inGitObjectDB reports whether path is below an object database identified
// relative to an actual gitdir/commondir route. A linked worktree gitdir routes
// its objects directory directly; a commondir can also observe the equivalent
// worktrees/<id>/objects layout when no deeper linked-worktree route exists.
func inGitObjectDB(route *gitEventRoute, path string) bool {
	if route == nil || !route.internal {
		return false
	}
	rel, err := filepath.Rel(route.root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 2 && parts[0] == "objects" {
		return true
	}
	return len(parts) >= 4 && parts[0] == "worktrees" && parts[1] != "" && parts[2] == "objects"
}

// RunGlobalGitEvents starts the platform recursive event source. It is called
// only by the global daemon. Platform implementations must return on ctx cancel.
func RunGlobalGitEvents(ctx context.Context, st *store.Store, handler *GitHandler) {
	ulog := logging.NewUnifiedLogger("groved.watcher.git.events")
	if err := runGlobalGitEvents(ctx, st, handler); err != nil && ctx.Err() == nil {
		ulog.Warn("global recursive git event source stopped").Err(err).Log(ctx)
	}
}
