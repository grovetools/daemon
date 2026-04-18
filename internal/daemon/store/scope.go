package store

import (
	"path/filepath"
	"strings"

	"github.com/grovetools/core/util/pathutil"
)

// IsInScope reports whether nodePath sits at or under scope. An empty
// scope matches everything — callers use that to represent the "global
// (unscoped) daemon" case without branching.
//
// Both sides are normalized via pathutil.NormalizeForLookup so callers
// can pass original-case filesystem paths (from workspace.GetProjects)
// and still match a pre-normalized scope on case-insensitive filesystems.
// Normalization failures fall back to the raw string — safer to match
// loosely than to silently drop an in-scope path.
func IsInScope(nodePath, scope string) bool {
	if scope == "" {
		return true
	}
	scopeNorm, err := pathutil.NormalizeForLookup(scope)
	if err != nil {
		scopeNorm = scope
	}
	pathNorm, err := pathutil.NormalizeForLookup(nodePath)
	if err != nil {
		pathNorm = nodePath
	}
	return pathNorm == scopeNorm || strings.HasPrefix(pathNorm, scopeNorm+string(filepath.Separator))
}
