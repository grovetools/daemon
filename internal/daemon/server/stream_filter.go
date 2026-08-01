package server

import (
	coredaemon "github.com/grovetools/core/pkg/daemon"
)

// applyStreamFilter decides what a filtered /api/stream subscriber sees of one
// update, and is the only place the server interprets a StreamFilter.
//
// It returns the update to write and whether to write anything at all. When a
// path allow-list drops SOME but not all of an event's workspace rows, the
// event survives with only the matching rows: a subscriber that declared
// interest in one workspace should still learn that workspace changed even
// though the same sweep touched fifty others. The pruned value is a shallow
// copy — the rows themselves are shared with the store and must not be mutated.
func applyStreamFilter(f coredaemon.StreamFilter, u *apiStateUpdate) (*apiStateUpdate, bool) {
	if u == nil {
		return nil, false
	}
	if f.IsZero() {
		return u, true
	}
	if !f.AllowsType(u.UpdateType) {
		return nil, false
	}
	if len(f.Paths) == 0 {
		return u, true
	}

	// Only the workspace-bearing shapes can be judged by a path allow-list.
	// Everything else (sessions, jobs, notes, theme, ...) passes: see the
	// StreamFilter.Paths contract.
	hasWorkspaceRows := len(u.Workspaces) > 0 || len(u.WorkspaceDeltas) > 0
	if !hasWorkspaceRows {
		return u, true
	}

	pruned := *u
	if len(u.Workspaces) > 0 {
		kept := u.Workspaces[:0:0]
		for _, ws := range u.Workspaces {
			if ws == nil || ws.WorkspaceNode == nil {
				continue
			}
			if f.AllowsPath(ws.Path) {
				kept = append(kept, ws)
			}
		}
		pruned.Workspaces = kept
	}
	if len(u.WorkspaceDeltas) > 0 {
		kept := u.WorkspaceDeltas[:0:0]
		for _, d := range u.WorkspaceDeltas {
			if d == nil {
				continue
			}
			if f.AllowsPath(d.Path) {
				kept = append(kept, d)
			}
		}
		pruned.WorkspaceDeltas = kept
	}
	if len(pruned.Workspaces) == 0 && len(pruned.WorkspaceDeltas) == 0 {
		return nil, false
	}
	// Scanned describes the rows on the wire, not the rows the daemon looked
	// at; leaving the pre-prune count would tell a filtered subscriber it is
	// holding data it was never sent.
	pruned.Scanned = len(pruned.Workspaces) + len(pruned.WorkspaceDeltas)
	return &pruned, true
}
