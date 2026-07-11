package sync

import (
	"io/fs"
	"path/filepath"

	"github.com/grovetools/core/pkg/syncproto"
)

// WalkTree walks the directory tree rooted at root, invoking onDir for every
// Included directory and onFile for every Included file. An excluded directory
// is pruned with fs.SkipDir before its subtree is read, so the walk costs
// O(included dirs): .git/, .artifacts/, .grove/rules/ etc. are never descended
// and never stat'd (fs.DirEntry carries the type from readdir — no os.Stat).
//
// Paths handed to the callbacks are slash-normalized workspace-relative
// (syncproto.NormalizePath), matching what DocSpace.Included, the watcher's
// lookupWatch, and the wire protocol expect. The root itself is passed with
// rel == "" (always Included; callers key on abs).
//
// This is the shared prune-aware walk reused by the watcher's recursive watch
// enumeration (Phase 2, computeWorkspaceWatches) and the anti-entropy reconcile
// pass (Phase 3, walkLocalTree). It lives on DocSpace in the sync package so
// antientropy.go can call it with the same DocSpace the watcher built — no
// import cycle — guaranteeing watch coverage and reconcile coverage can never
// disagree about what is in the doc space.
//
// onFile may be nil (Phase 2 registers directories only). Symlinked directories
// are not followed (filepath.WalkDir semantics — acceptable for the notebook
// tree). Transient per-entry errors (permission denied, race-deleted entries)
// are skipped; a failure on the root itself (a vanished workspace) is returned
// to the caller, who skips the workspace this tick like a failed os.Stat.
func (d *DocSpace) WalkTree(root string,
	onDir func(abs, rel string) error,
	onFile func(abs, rel string, de fs.DirEntry) error,
) error {
	return filepath.WalkDir(root, func(abs string, de fs.DirEntry, err error) error {
		if err != nil {
			if abs == root {
				return err // vanished/unreadable root: caller skips this tick
			}
			return nil // deeper race (permission, delete): skip and continue
		}

		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return nil
		}
		rel = syncproto.NormalizePath(rel)
		if rel == "." {
			rel = "" // the root is always Included; callers key on abs
		}

		if de.IsDir() {
			// Early prune: an excluded directory is never descended, keeping
			// the walk O(included dirs). The root (rel == "") always passes.
			if rel != "" && !d.Included(rel) {
				return fs.SkipDir
			}
			if onDir != nil {
				return onDir(abs, rel)
			}
			return nil
		}

		// Files: gate on the same manifest so walk coverage == doc space —
		// catches per-file rules (.DS_Store, *.lock, *.conflict.md) that live
		// inside an Included directory.
		if onFile == nil || !d.Included(rel) {
			return nil
		}
		return onFile(abs, rel, de)
	})
}
