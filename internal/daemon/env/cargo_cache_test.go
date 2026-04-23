package env

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceSpawn_RustBuildCacheInjectsEnvVar(t *testing.T) {
	tmp := t.TempDir()
	eco := filepath.Join(tmp, "kitchen-env")
	worktree := filepath.Join(eco, ".grove-worktrees", "polyglot-a")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	svcConfig := map[string]interface{}{
		"rust_build_cache": true,
	}
	m := NewManager()
	got := computeCargoCacheEnv(context.Background(), m, "api", svcConfig, worktree)

	want := "CARGO_TARGET_DIR=" + filepath.Join(eco, ".grove", "shared-cargo-target")
	if got != want {
		t.Fatalf("computeCargoCacheEnv = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(eco, ".grove", "shared-cargo-target")); err != nil {
		t.Fatalf("target dir not created: %v", err)
	}
}

func TestServiceSpawn_RustBuildCacheOmittedWhenFlagFalse(t *testing.T) {
	svcConfig := map[string]interface{}{
		"rust_build_cache": false,
	}
	m := NewManager()
	got := computeCargoCacheEnv(context.Background(), m, "api", svcConfig, t.TempDir())
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestServiceSpawn_RustBuildCacheOmittedWhenFlagMissing(t *testing.T) {
	m := NewManager()
	got := computeCargoCacheEnv(context.Background(), m, "api", map[string]interface{}{}, t.TempDir())
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestEcosystemRoot_NonWorktreeReturnsAsIs(t *testing.T) {
	p := "/Users/foo/Code/kitchen-env"
	if got := ecosystemRoot(p); got != p {
		t.Fatalf("ecosystemRoot(%q) = %q, want %q", p, got, p)
	}
}

func TestEcosystemRoot_WorktreeStripsSuffix(t *testing.T) {
	got := ecosystemRoot("/Users/foo/Code/kitchen-env/.grove-worktrees/branch-a")
	want := "/Users/foo/Code/kitchen-env"
	if got != want {
		t.Fatalf("ecosystemRoot = %q, want %q", got, want)
	}
}
