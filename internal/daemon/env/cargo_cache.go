package env

import (
	"context"
	"os"
	"path/filepath"
)

// computeCargoCacheEnv returns a "CARGO_TARGET_DIR=<abs-path>" string when
// svcConfig declares `rust_build_cache = true`, or "" otherwise.
//
// The target dir is shared across all worktrees of an ecosystem at
// <ecosystem-root>/.grove/shared-cargo-target. Cargo's per-artifact fingerprint
// locks make this safe for concurrent builds on the same toolchain.
//
// ecosystemRoot() strips a trailing .grove-worktrees/<name> suffix when present
// so worktrees resolve to the ecosystem root; a non-worktree workspace is
// returned as-is.
func computeCargoCacheEnv(ctx context.Context, m *Manager, svcName string, svcConfig map[string]interface{}, workspacePath string) string {
	useCache, _ := svcConfig["rust_build_cache"].(bool)
	if !useCache {
		return ""
	}
	ecoRoot := ecosystemRoot(workspacePath)
	targetDir := filepath.Join(ecoRoot, ".grove", "shared-cargo-target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		if m != nil {
			m.ulog.Warn("Failed to create shared cargo target dir").
				Err(err).
				Field("service", svcName).
				Field("dir", targetDir).
				Log(ctx)
		}
		return ""
	}
	if m != nil {
		m.ulog.Info("Using shared cargo target").
			Field("service", svcName).
			Field("dir", targetDir).
			Log(ctx)
	}
	return "CARGO_TARGET_DIR=" + targetDir
}

// ecosystemRoot returns the ecosystem root given a workspace path. If the
// workspace is a worktree under `<eco>/.grove-worktrees/<name>`, returns `<eco>`.
// Otherwise returns the workspace path unchanged.
func ecosystemRoot(workspacePath string) string {
	if workspacePath == "" {
		return workspacePath
	}
	parent := filepath.Dir(workspacePath)
	if filepath.Base(parent) == ".grove-worktrees" {
		return filepath.Dir(parent)
	}
	return workspacePath
}
