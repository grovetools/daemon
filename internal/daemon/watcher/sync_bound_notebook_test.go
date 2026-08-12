package watcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grovetools/core/config"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/paths"
)

// TestPullRootFollowsBoundNotebookNotDefault is the wrong-root regression:
// a notes-plane subscription with NO compiled code-root binding must route to
// the notebook its stamp is actually bound to, not to notebooks.rules.default.
//
// Before the fix, recordedNotebookRoot fell straight from the (absent) grove
// binding to the default notebook, so a notespace stamped under another
// notebook was looked up by NAME under the default root — the exact shape
// observed post-P2:
//
//	notespace "canary-nb-test" at ~/notebooks/nb/notespaces/canary-nb-test
//	has no .notespace.toml
//
// while it was correctly stamped at ~/notebooks/canary-nb/notespaces/…. The
// decoy directory below is the rehearsal scenario made executable: a
// same-display-name tree under the wrong (default) root must never win.
//
// The default rung is not removed, only demoted: "fresh" has no stamp anywhere
// (a pull replica the pipeline has not materialized yet) and must still resolve
// under the default notebook.
func TestPullRootFollowsBoundNotebookNotDefault(t *testing.T) {
	const (
		notespaceID = "01KZVCMCZ19M95YTJN3HC509P4"
		subjectID   = "local:01KZVCMCZ1GX41XHXZXH67CDT9"
	)

	t.Setenv("GROVE_HOME", t.TempDir())
	configDir := paths.ConfigDir()
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	machineTOML := "[machine]\nname = \"test-machine\"\n\n[primaries]\n" +
		"\"" + subjectID + "\" = \"" + notespaceID + "\"\n"
	if err := os.WriteFile(filepath.Join(configDir, "machine.toml"), []byte(machineTOML), 0o600); err != nil {
		t.Fatalf("write machine.toml: %v", err)
	}

	defaultRoot := filepath.Join(t.TempDir(), "notebooks", "nb")
	boundRoot := filepath.Join(t.TempDir(), "notebooks", "canary-nb")

	// The bound notebook holds the stamped notespace.
	stampedRoot := filepath.Join(boundRoot, "notespaces", "bound")
	if err := os.MkdirAll(stampedRoot, 0o755); err != nil {
		t.Fatalf("mkdir bound notespace: %v", err)
	}
	if _, err := notespacepkg.InstallNotespace(stampedRoot, notespacepkg.NotespaceStamp{
		ID: notespaceID, Name: "bound", Subject: subjectID, Kind: "notes",
	}); err != nil {
		t.Fatalf("install stamp: %v", err)
	}
	// The default notebook holds a same-name decoy with no stamp.
	decoyRoot := filepath.Join(defaultRoot, "notespaces", "bound")
	if err := os.MkdirAll(decoyRoot, 0o755); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}

	cfg := &config.Config{
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{
				"nb":        {RootDir: defaultRoot},
				"canary-nb": {RootDir: boundRoot},
			},
			Rules: &config.NotebookRules{Default: "nb"},
		},
	}
	syncCfg := &config.SyncConfig{Workspaces: []config.SyncWorkspace{
		{Name: "bound", Role: config.SyncRolePeer, Pull: true},
		{Name: "fresh", Role: config.SyncRolePeer, Pull: true},
	}}
	h := NewSyncHandler(nil, cfg, syncCfg, nil, 0, 0)

	if notebook, err := h.syntheticNodeFor("bound"); err != nil || notebook.NotebookName != "canary-nb" {
		t.Errorf("syntheticNodeFor(bound) = %+v, %v; want notebook canary-nb", notebook, err)
	}

	roots, err := h.configuredPullRoots()
	if err != nil {
		t.Fatalf("configuredPullRoots: %v", err)
	}
	if roots["bound"] != stampedRoot {
		t.Errorf("pull root for a bound notespace = %q, want %q (the default notebook must not be invented)", roots["bound"], stampedRoot)
	}
	if roots["bound"] == decoyRoot {
		t.Errorf("same-display-name tree under the wrong root resolved: %q", decoyRoot)
	}
	// Unstamped replica: the default rung still answers.
	if want := filepath.Join(defaultRoot, "notespaces", "fresh"); roots["fresh"] != want {
		t.Errorf("pull root for an unmaterialized replica = %q, want %q", roots["fresh"], want)
	}
}

// TestPullRootsResolveDeclaredNotebookSpellings is the companion regression to
// the test above, and the one that catches the failure the test above cannot.
//
// Both rungs it exercises read Notebooks.Definitions[...].RootDir, which is a
// RECORDED value. core resolves those at config compile time, so a
// post-migration machine never sees a tilde here — but a config shape without
// a recorded notebooks.toml (pre-migration, a sandbox, a seeded satellite)
// keeps the legacy 'root_dir = ~/notebooks/<name>' spelling, and then:
//
//   - the identity rung compares an absolute resolved root against a declared
//     one, never matches, and silently falls through to the default;
//   - the default rung returns the declared spelling verbatim, so every pull
//     root becomes "~/notebooks/nb/notespaces/<name>" — a string filepath.Join
//     builds without complaint and nothing downstream rejects.
//
// The fixture above declares both roots as absolute t.TempDir() paths, so it
// passes identically against the broken and the fixed code. That fixture
// convention is the reason this class shipped twice; this test breaks it
// deliberately.
func TestPullRootsResolveDeclaredNotebookSpellings(t *testing.T) {
	const (
		notespaceID = "01KZVCMCZ19M95YTJN3HC509P4"
		subjectID   = "local:01KZVCMCZ1GX41XHXZXH67CDT9"
	)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROVE_HOME", t.TempDir())
	configDir := paths.ConfigDir()
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	machineTOML := "[machine]\nname = \"test-machine\"\n\n[primaries]\n" +
		"\"" + subjectID + "\" = \"" + notespaceID + "\"\n"
	if err := os.WriteFile(filepath.Join(configDir, "machine.toml"), []byte(machineTOML), 0o600); err != nil {
		t.Fatalf("write machine.toml: %v", err)
	}

	// Declared exactly as notebooks.legacy-compat.toml writes them.
	const declaredDefault, declaredBound = "~/notebooks/nb", "~/notebooks/canary-nb"
	defaultRoot := filepath.Join(home, "notebooks", "nb")
	boundRoot := filepath.Join(home, "notebooks", "canary-nb")

	stampedRoot := filepath.Join(boundRoot, "notespaces", "bound")
	if err := os.MkdirAll(stampedRoot, 0o755); err != nil {
		t.Fatalf("mkdir bound notespace: %v", err)
	}
	if _, err := notespacepkg.InstallNotespace(stampedRoot, notespacepkg.NotespaceStamp{
		ID: notespaceID, Name: "bound", Subject: subjectID, Kind: "notes",
	}); err != nil {
		t.Fatalf("install stamp: %v", err)
	}

	cfg := &config.Config{
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{
				"nb":        {RootDir: declaredDefault},
				"canary-nb": {RootDir: declaredBound},
			},
			Rules: &config.NotebookRules{Default: "nb"},
		},
	}
	syncCfg := &config.SyncConfig{Workspaces: []config.SyncWorkspace{
		{Name: "bound", Role: config.SyncRolePeer, Pull: true},
		{Name: "fresh", Role: config.SyncRolePeer, Pull: true},
	}}
	h := NewSyncHandler(nil, cfg, syncCfg, nil, 0, 0)

	// Identity rung: a declared spelling on the config side must not stop the
	// stamped notespace from being recognised as this notebook's.
	if node, err := h.syntheticNodeFor("bound"); err != nil || node.NotebookName != "canary-nb" {
		t.Errorf("syntheticNodeFor(bound) = %+v, %v; want notebook canary-nb", node, err)
	}

	roots, err := h.configuredPullRoots()
	if err != nil {
		t.Fatalf("configuredPullRoots: %v", err)
	}
	if roots["bound"] != stampedRoot {
		t.Errorf("pull root for a bound notespace = %q, want %q", roots["bound"], stampedRoot)
	}
	// Default rung: the declared spelling must not survive into a watch path.
	if want := filepath.Join(defaultRoot, "notespaces", "fresh"); roots["fresh"] != want {
		t.Errorf("pull root via the default rung = %q, want %q", roots["fresh"], want)
	}
	for name, root := range roots {
		if strings.Contains(root, "~") {
			t.Errorf("pull root for %q is a declared spelling, not a usable path: %q", name, root)
		}
		if !filepath.IsAbs(root) {
			t.Errorf("pull root for %q is not absolute: %q", name, root)
		}
	}
}
