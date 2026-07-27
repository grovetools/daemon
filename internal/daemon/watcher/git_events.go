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
}

// buildGitEventRoutes builds the path-to-repository index used by the global
// recursive event source. Both worktree contents and git internals are covered.
func buildGitEventRoutes(ctx context.Context, workspaces []*models.EnrichedWorkspace) []gitEventRoute {
	byRoot := make(map[string]map[string]*workspace.WorkspaceNode)
	add := func(root string, node *workspace.WorkspaceNode) {
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
		add(node.Path, node)
		add(gitDir, node)
		add(commonDir, node)
	}
	routes := make([]gitEventRoute, 0, len(byRoot))
	for root, nodesByPath := range byRoot {
		nodes := make([]*workspace.WorkspaceNode, 0, len(nodesByPath))
		for _, node := range nodesByPath {
			nodes = append(nodes, node)
		}
		routes = append(routes, gitEventRoute{root: root, nodes: nodes})
	}
	// Deepest root first makes nested repositories route to their actual owner,
	// rather than also dirtying every containing ecosystem repository.
	sort.Slice(routes, func(i, j int) bool { return len(routes[i].root) > len(routes[j].root) })
	return routes
}

func routeGitEvent(path string, routes []gitEventRoute) []*workspace.WorkspaceNode {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	path = filepath.Clean(path)
	for _, route := range routes {
		if path == route.root || strings.HasPrefix(path, route.root+string(filepath.Separator)) {
			return route.nodes
		}
	}
	return nil
}

// relevantGitEvent filters high-volume git object churn that cannot change the
// status surface. Working-tree events are always relevant; inside a git dir we
// retain refs, HEAD, index, logs and packed-refs changes.
func relevantGitEvent(path string) bool {
	p := filepath.ToSlash(path)
	if strings.Contains(p, "/.git/objects/") || strings.Contains(p, "/objects/") && strings.Contains(p, "/worktrees/") {
		return false
	}
	return !strings.HasSuffix(p, ".lock")
}

// RunGlobalGitEvents starts the platform recursive event source. It is called
// only by the global daemon. Platform implementations must return on ctx cancel.
func RunGlobalGitEvents(ctx context.Context, st *store.Store, handler *GitHandler) {
	ulog := logging.NewUnifiedLogger("groved.watcher.git.events")
	if err := runGlobalGitEvents(ctx, st, handler); err != nil && ctx.Err() == nil {
		ulog.Warn("global recursive git event source stopped").Err(err).Log(ctx)
	}
}
