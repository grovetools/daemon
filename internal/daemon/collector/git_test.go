package collector

import (
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

// wsMap builds a workspace map keyed by path, mirroring store State.Workspaces.
func wsMap(paths ...string) map[string]*models.EnrichedWorkspace {
	m := make(map[string]*models.EnrichedWorkspace, len(paths))
	for _, p := range paths {
		m[p] = &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{Path: p}}
	}
	return m
}

func TestDynamicIntervalHasFocusedFloor(t *testing.T) {
	base := 10 * time.Second
	for _, count := range []int{0, 1, 5, 6, 15} {
		if got := dynamicInterval(count, base); got < focusedScanFloor {
			t.Fatalf("count %d interval = %s, below floor %s", count, got, focusedScanFloor)
		}
	}
	if got := dynamicInterval(5, base); got != 5*time.Second {
		t.Fatalf("small focus interval = %s, want 5s", got)
	}
	if got := dynamicInterval(20, base); got != base {
		t.Fatalf("medium focus interval = %s, want %s", got, base)
	}
}

func TestFocusedFileDataDecisionUsesStatusFingerprint(t *testing.T) {
	status := &git.ExtendedGitStatus{}
	if shouldComputeFocusedFileData(false, false, nil, status) {
		t.Fatal("unfocused repo requested per-file data")
	}
	if !shouldComputeFocusedFileData(true, false, status, status) {
		t.Fatal("first focused snapshot must backfill per-file data")
	}
	if shouldComputeFocusedFileData(true, true, status, status) {
		t.Fatal("unchanged status fingerprint recomputed per-file data")
	}
	changed := &git.ExtendedGitStatus{LinesAdded: 1}
	if !shouldComputeFocusedFileData(true, true, status, changed) {
		t.Fatal("changed status fingerprint did not recompute per-file data")
	}
}

func scopedPaths(c *GitStatusCollector, workspaces map[string]*models.EnrichedWorkspace) []string {
	var paths []string
	for _, ws := range c.scopedWorkspaces(workspaces) {
		paths = append(paths, ws.Path)
	}
	sort.Strings(paths)
	return paths
}

func TestScopedWorkspaceSelection(t *testing.T) {
	workspaces := wsMap("/a/b", "/a/b/c", "/a/bc", "/other")
	want := []string{"/a/b", "/a/b/c"}

	got := scopedPaths(NewGitStatusCollector(0, "/a/b"), workspaces)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("scoped selection = %v, want %v", got, want)
	}

	// A trailing separator on the configured scope must not change selection.
	got = scopedPaths(NewGitStatusCollector(0, "/a/b/"), workspaces)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("trailing-slash scope selection = %v, want %v", got, want)
	}
}

func TestScopedWorkspaceSelectionMatchesFocusNormalization(t *testing.T) {
	// The store's focus normalization lowercases on case-insensitive
	// filesystems; scope selection must match a workspace whose discovered
	// spelling differs from the scope's only by case.
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("case-insensitive path normalization only applies on darwin/windows")
	}
	workspaces := wsMap("/a/b/c", "/a/bc")
	got := scopedPaths(NewGitStatusCollector(0, "/A/B"), workspaces)
	if len(got) != 1 || got[0] != "/a/b/c" {
		t.Fatalf("case-mismatched scope selection = %v, want [/a/b/c]", got)
	}
}

func TestUnscopedCollectorSelectsAllWorkspaces(t *testing.T) {
	workspaces := wsMap("/a/b", "/a/bc", "/other")
	got := scopedPaths(NewGitStatusCollector(0, ""), workspaces)
	if len(got) != len(workspaces) {
		t.Fatalf("unscoped selection = %v, want all %d workspaces", got, len(workspaces))
	}
}

func TestPathRefreshCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	last := map[string]time.Time{}
	if !pathRefreshDue(last, "/repo", now) {
		t.Fatal("first refresh should be due")
	}
	last["/repo"] = now
	if pathRefreshDue(last, "/repo", now.Add(pathRefreshCooldown-time.Nanosecond)) {
		t.Fatal("refresh inside cooldown should be suppressed")
	}
	if !pathRefreshDue(last, "/repo", now.Add(pathRefreshCooldown)) {
		t.Fatal("refresh at cooldown boundary should be due")
	}
}
