package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
)

// TestAggregator_EmptyConfig returns a valid but empty snapshot when no
// groves are configured — happy path for a fresh install.
func TestAggregator_EmptyConfig(t *testing.T) {
	agg := New(nil)
	s := agg.Build(context.Background(), &config.Config{}, false)
	if s.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is zero")
	}
	if len(s.Ecosystems) != 0 {
		t.Errorf("expected 0 ecosystems, got %d", len(s.Ecosystems))
	}
}

// TestAggregator_DiscoversEcosystem builds a throwaway grove source with a
// .grove-worktrees marker and confirms the aggregator picks it up as an
// ecosystem root even without a populated workspace provider.
func TestAggregator_DiscoversEcosystem(t *testing.T) {
	tmp := t.TempDir()
	eco := filepath.Join(tmp, "my-eco")
	if err := os.MkdirAll(filepath.Join(eco, ".grove-worktrees"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Groves: map[string]config.GroveSourceConfig{
		"tmp": {Path: tmp},
	}}

	roots := ecosystemRoots(cfg)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
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
