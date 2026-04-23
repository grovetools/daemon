package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
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
	if err := os.MkdirAll(filepath.Dir(state), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte(`{"provider":"docker"}`), 0644); err != nil {
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
