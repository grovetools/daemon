package sync

import (
	"path"
	"strings"

	"github.com/grovetools/core/config"
)

// RouteDecision classifies how a document should be transmitted — or that it
// should not be transmitted at all.
type RouteDecision int

const (
	RouteInline RouteDecision = iota // small enough to ride inline on push
	RouteBlob                        // routed through the content-addressed blob tier
	RouteSkip                        // excluded, or over the per-workspace size cap
)

// defaultInlineMax is the inline/blob boundary applied at watcher time. It
// mirrors the fallback in Client.MaxInlineSize; the authoritative split at
// push time is the server handshake (Client.MaxInlineSize / Client.MaxBlobSize),
// so Route's classification is advisory until a client exists.
const defaultInlineMax int64 = 256 << 10

// defaultExclusionDirs are directory names excluded from sync anywhere in a
// document's path. These are tool-local or editor-local state that must never
// replicate (the protocol's default exclusion manifest). Absorbed verbatim
// from the watcher package's former defaultSyncExclusionDirs.
var defaultExclusionDirs = map[string]bool{
	".obsidian":   true, // Obsidian vault-local state
	".stfolder":   true, // Syncthing marker
	".stversions": true, // Syncthing versioning
	".cx":         true, // cx-local context state
	".artifacts":  true, // generated briefings/aggregated contexts
	".git":        true, // git object tree — a recursive walk would descend it
}

// DocSpace is the canonical "what syncs, and how" classifier for a workspace.
// It answers two questions about a slash-normalized workspace-relative path:
// whether the path is Included in sync at all (path-only, no I/O — so P2's
// recursive watch enumeration and P3's tree-walk reconcile can reuse it to
// filter directory walks), and, given a size, how the document should Route
// (inline / blob / skip).
//
// Compiled defaults (defaultExclusionDirs plus the suffix/basename rules) are
// overlaid with the per-workspace Excludes globs and an optional MaxFileSize
// cap from config.SyncWorkspace.
type DocSpace struct {
	excludes    []string // per-workspace extra exclusion globs
	maxFileSize int64    // per-workspace cap in bytes; 0 = unlimited
	inlineMax   int64    // inline/blob boundary in bytes
}

// NewDocSpace builds a DocSpace from a workspace subscription. A nil ws yields
// an all-defaults instance (no extra excludes, no size cap).
func NewDocSpace(ws *config.SyncWorkspace) *DocSpace {
	d := &DocSpace{inlineMax: defaultInlineMax}
	if ws != nil {
		d.excludes = append([]string(nil), ws.Excludes...)
		d.maxFileSize = ws.MaxFileSize
	}
	return d
}

// Included reports whether a slash-normalized workspace-relative path is
// synced at all. Path-only: no filesystem access and no size input, so it can
// filter directory walks (P2 ComputeWatchPaths, P3 walkLocalTree) as well as
// individual events.
func (d *DocSpace) Included(relPath string) bool {
	return !excluded(relPath, d.excludes)
}

// Route classifies a document by path and size: RouteSkip when excluded or
// over the per-workspace MaxFileSize; RouteBlob when larger than the inline
// boundary; else RouteInline. The inline/blob boundary is advisory — the
// push-time truth is the server handshake — so DrainOutbox consults the client
// accessors directly rather than a DocSpace (Phase 1 work item 3).
func (d *DocSpace) Route(relPath string, size int64) RouteDecision {
	if !d.Included(relPath) {
		return RouteSkip
	}
	if d.maxFileSize > 0 && size > d.maxFileSize {
		return RouteSkip
	}
	if size > d.inlineMax {
		return RouteBlob
	}
	return RouteInline
}

// excluded reports whether a slash-normalized workspace-relative path is
// excluded by the protocol's default exclusion manifest or by per-workspace
// extra exclusion globs (matched against both the full relative path and the
// basename). Moved verbatim from the watcher package's former syncExcluded.
func excluded(relPath string, extra []string) bool {
	relPath = strings.Trim(path.Clean(relPath), "/")
	base := path.Base(relPath)

	// Suffix/basename rules.
	switch {
	case base == ".DS_Store":
		return true
	case strings.Contains(base, ".sync-conflict-"): // Syncthing conflict copies
		return true
	case strings.HasSuffix(base, ".conflict.md"): // grove sync conflict copies
		return true
	case strings.HasSuffix(base, ".lock"): // flow plan locks etc.
		return true
	}

	// Directory-segment rules, including ".grove/rules" as a pair.
	segs := strings.Split(relPath, "/")
	for i, seg := range segs {
		if defaultExclusionDirs[seg] {
			return true
		}
		if seg == ".grove" && i+1 < len(segs) && segs[i+1] == "rules" {
			return true
		}
	}

	// Per-workspace extra exclusions from sync config.
	for _, pattern := range extra {
		if ok, _ := path.Match(pattern, relPath); ok {
			return true
		}
		if ok, _ := path.Match(pattern, base); ok {
			return true
		}
		if strings.HasSuffix(pattern, "/") && strings.HasPrefix(relPath, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
	}
	return false
}
