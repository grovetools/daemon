package cmd

import (
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/paths"
)

func TestScopeHashFromPidFilename(t *testing.T) {
	cases := map[string]string{
		"groved.pid":                        "",
		"groved-env-continued-e2435831.pid": "e2435831",
		"groved-x-deadbeef.pid":             "deadbeef",
		"not-a-groved.pid":                  "",
		"groved-noHash.pid":                 "", // single segment after prefix: no hash suffix
	}
	for name, want := range cases {
		if got := scopeHashFromPidFilename(name); got != want {
			t.Errorf("scopeHashFromPidFilename(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestResolveUpgradeTarget(t *testing.T) {
	const cwdScope = "/eco/worktree-a"
	// The real pidfile name the running daemon would have for this scope.
	scopedPidBase := filepath.Base(paths.PidFilePath(cwdScope))

	unscoped := daemonEntry{Scope: "", PidPath: "/state/groved.pid"}
	scopedWithSidecar := daemonEntry{Scope: "worktree-a", ExactScope: cwdScope, PidPath: filepath.Join("/state", scopedPidBase)}
	scopedLegacy := daemonEntry{Scope: "worktree-a", ExactScope: "", PidPath: filepath.Join("/state", scopedPidBase)}
	otherScoped := daemonEntry{Scope: "worktree-b", ExactScope: "/eco/worktree-b", PidPath: "/state/groved-worktree-b-00000000.pid"}

	t.Run("global targets unscoped", func(t *testing.T) {
		m, _ := resolveUpgradeTarget(true, "", cwdScope)
		if !m(unscoped) {
			t.Error("global should match the unscoped daemon")
		}
		if m(scopedWithSidecar) {
			t.Error("global must not match a scoped daemon")
		}
	})

	t.Run("scope label override", func(t *testing.T) {
		m, _ := resolveUpgradeTarget(false, "worktree-a", "")
		if !m(scopedWithSidecar) {
			t.Error("label should match same-label daemon")
		}
		if m(unscoped) || m(otherScoped) {
			t.Error("label must not match unscoped or other-label daemons")
		}
	})

	t.Run("cwd scope matches by exact sidecar", func(t *testing.T) {
		m, _ := resolveUpgradeTarget(false, "", cwdScope)
		if !m(scopedWithSidecar) {
			t.Error("cwd scope should match daemon with exact sidecar")
		}
		if m(unscoped) || m(otherScoped) {
			t.Error("cwd scope must not match unscoped or other-scope daemons")
		}
	})

	t.Run("cwd scope legacy hash fallback", func(t *testing.T) {
		m, _ := resolveUpgradeTarget(false, "", cwdScope)
		if !m(scopedLegacy) {
			t.Error("cwd scope should match legacy daemon by pidfile hash")
		}
	})

	t.Run("empty cwd scope targets unscoped", func(t *testing.T) {
		m, _ := resolveUpgradeTarget(false, "", "")
		if !m(unscoped) {
			t.Error("empty cwd scope should target the unscoped daemon")
		}
		if m(scopedWithSidecar) {
			t.Error("empty cwd scope must not match a scoped daemon")
		}
	})
}
