package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/workspace"
)

// TestAggregator_GeneratedAt stamps every payload so the browser can show
// a "last updated" label. This is the only aspect of Build() that does
// not depend on workspace.GetProjects (global state).
func TestAggregator_GeneratedAt(t *testing.T) {
	agg := New(nil)
	s := agg.Build(context.Background(), &config.Config{}, false)
	if s.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is zero")
	}
}

// TestAggregator_PicksEcosystemNodes filters allNodes down to just the
// ecosystem roots — submodules and standalone projects should not make it
// into the dashboard. This guards the regression that caused every grove
// submodule to appear as its own "ecosystem" when we were globbing
// grove.toml marker files.
func TestAggregator_PicksEcosystemNodes(t *testing.T) {
	nodes := []*workspace.WorkspaceNode{
		{Name: "my-eco", Path: "/tmp/my-eco", Kind: workspace.KindEcosystemRoot},
		{Name: "my-eco", Path: "/tmp/my-eco", Kind: workspace.KindEcosystemRoot}, // dup
		{Name: "daemon", Path: "/tmp/daemon", Kind: workspace.KindStandaloneProject},
		{Name: "wt", Path: "/tmp/my-eco/.grove-worktrees/wt", Kind: workspace.KindEcosystemWorktree},
	}
	roots := ecosystemRoots(nil, nodes)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d: %+v", len(roots), roots)
	}
	if roots[0].Name != "my-eco" {
		t.Errorf("name = %q", roots[0].Name)
	}
}

// TestAggregator_OrphanDetection exercises the local state.json orphan
// walker — the Dashboard uses this to surface stale worktrees.
func TestAggregator_OrphanDetection(t *testing.T) {
	tmp := t.TempDir()
	state := filepath.Join(tmp, ".grove-worktrees", "ghost", ".grove", "env", "state.json")
	if err := os.MkdirAll(filepath.Dir(state), 0o755); err != nil { //nolint:gosec // G301: test directory
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte(`{"provider":"docker"}`), 0o644); err != nil { //nolint:gosec // G306: test file
		t.Fatal(err)
	}

	orphans := detectOrphans(tmp, nil)
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(orphans))
	}
	if orphans[0].Name != "ghost" {
		t.Errorf("name = %q", orphans[0].Name)
	}
}

// sandboxXDG isolates a test from the host grove data dir. GROVE_HOME must
// be cleared explicitly — it beats XDG_DATA_HOME in paths.getDataHome().
func sandboxXDG(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("GROVE_HOME", "")
}

// TestBelongsToEcosystem_XDGNodeViaParentEcosystemPath verifies that an XDG
// worktree node — whose Path lives under the shared data dir, NOT under the
// ecosystem root — still groups under its ecosystem. The node-identity
// contract requires ParentEcosystemPath/RootEcosystemPath to point at the
// original checkout, and dashboard grouping relies on exactly that (a
// path-prefix check against root.Path would never match the XDG container).
func TestBelongsToEcosystem_XDGNodeViaParentEcosystemPath(t *testing.T) {
	sandboxXDG(t)

	ecoPath := filepath.Join(t.TempDir(), "my-eco")
	root := ecosystemRoot{Name: "my-eco", Path: ecoPath}

	// XDG worktree: Path is under the shared data dir, not under ecoPath.
	xdgWtPath := filepath.Join(paths.WorktreesDir(), workspace.DirIdentifier(ecoPath), "wt1")
	xdgNode := &workspace.WorkspaceNode{
		Name:                "wt1",
		Path:                xdgWtPath,
		Kind:                workspace.KindEcosystemWorktree,
		ParentProjectPath:   ecoPath,
		ParentEcosystemPath: ecoPath,
		RootEcosystemPath:   ecoPath,
	}
	if !belongsToEcosystem(xdgNode, root) {
		t.Errorf("XDG node %q should group under ecosystem %q via ParentEcosystemPath", xdgWtPath, ecoPath)
	}

	// RootEcosystemPath-only fallback (no ParentEcosystemPath) must also group.
	rootOnly := &workspace.WorkspaceNode{
		Name:              "wt2",
		Path:              filepath.Join(paths.WorktreesDir(), workspace.DirIdentifier(ecoPath), "wt2"),
		Kind:              workspace.KindEcosystemWorktree,
		RootEcosystemPath: ecoPath,
	}
	if !belongsToEcosystem(rootOnly, root) {
		t.Errorf("XDG node should group under ecosystem via RootEcosystemPath fallback")
	}

	// A node belonging to a different ecosystem must NOT group here.
	other := &workspace.WorkspaceNode{
		Name:                "wt3",
		Path:                filepath.Join(paths.WorktreesDir(), "other-deadbeef", "wt3"),
		Kind:                workspace.KindEcosystemWorktree,
		ParentEcosystemPath: filepath.Join(t.TempDir(), "other-eco"),
	}
	if belongsToEcosystem(other, root) {
		t.Errorf("node from a different ecosystem must not group under %q", ecoPath)
	}
}

// TestDetectOrphans_XDGOrphan verifies the dashboard orphan walker finds a
// stale env state.json that lives under the XDG worktree base
// (WorktreesDir()/<DirIdentifier>/<name>/.grove/env/state.json), not just the
// legacy <ecoRoot>/.grove-worktrees path.
func TestDetectOrphans_XDGOrphan(t *testing.T) {
	sandboxXDG(t)

	ecoRoot := filepath.Join(t.TempDir(), "my-eco")
	xdgBase := filepath.Join(paths.WorktreesDir(), workspace.DirIdentifier(ecoRoot))
	state := filepath.Join(xdgBase, "ghost-xdg", ".grove", "env", "state.json")
	if err := os.MkdirAll(filepath.Dir(state), 0o755); err != nil { //nolint:gosec // G301: test directory
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte(`{"provider":"docker"}`), 0o644); err != nil { //nolint:gosec // G306: test file
		t.Fatal(err)
	}

	orphans := detectOrphans(ecoRoot, nil)
	if len(orphans) != 1 {
		t.Fatalf("expected 1 XDG orphan, got %d: %+v", len(orphans), orphans)
	}
	if orphans[0].Name != "ghost-xdg" {
		t.Errorf("name = %q, want ghost-xdg", orphans[0].Name)
	}
	if orphans[0].Path != state {
		t.Errorf("path = %q, want %q", orphans[0].Path, state)
	}

	// An XDG worktree that IS known (active) must not be reported as orphan.
	knownState := filepath.Join(xdgBase, "alive", ".grove", "env", "state.json")
	if err := os.MkdirAll(filepath.Dir(knownState), 0o755); err != nil { //nolint:gosec // G301: test directory
		t.Fatal(err)
	}
	if err := os.WriteFile(knownState, []byte(`{"provider":"docker"}`), 0o644); err != nil { //nolint:gosec // G306: test file
		t.Fatal(err)
	}
	known := []*workspace.WorkspaceNode{
		{Name: "alive", Path: filepath.Join(xdgBase, "alive"), Kind: workspace.KindEcosystemWorktree},
	}
	orphans = detectOrphans(ecoRoot, known)
	if len(orphans) != 1 || orphans[0].Name != "ghost-xdg" {
		t.Fatalf("expected only ghost-xdg orphan with active worktree filtered, got %+v", orphans)
	}
}
