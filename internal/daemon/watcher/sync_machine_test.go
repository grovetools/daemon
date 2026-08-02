package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// The last link of the machine-config fixture chain: a subscription declared
// ONLY in machine.toml compiles into cfg.Groves, and syntheticNodeFor — which
// iterates cfg.Groves to pick a workspace's notebook — resolves through it.
// Nothing in the daemon knows machine.toml exists; that is the point of
// compiling instead of teaching every consumer a second config shape.
func TestSyntheticNodeResolvesThroughCompiledMachineGroves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)
	config.ResetLoadCache()
	t.Cleanup(config.ResetLoadCache)

	configDir := filepath.Join(home, "config", "grove")
	notebookRoot := filepath.Join(home, "notebooks", "machine-nb")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	// The notebook definition lives in the ordinary global config; only the
	// grove declaration moves to machine.toml.
	writeTestFile(t, filepath.Join(configDir, "grove.toml"), `name = "fixture"

[notebooks.definitions.machine-nb]
root_dir = "`+notebookRoot+`"
`)
	writeTestFile(t, filepath.Join(configDir, "machine.toml"), `[machine]
name = "fixture"

[machine.ecosystems.grovetools]
path = "`+filepath.Join(home, "code", "grovetools")+`"
notebook = "machine-nb"
`)

	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if _, ok := cfg.Groves["grovetools"]; !ok {
		t.Fatalf("machine.toml subscription did not compile into Groves: %v", cfg.Groves)
	}

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, cfg, nil, db, 50, 500)
	node := h.syntheticNodeFor("grovetools")
	if node.NotebookName != "machine-nb" {
		t.Fatalf("syntheticNodeFor NotebookName = %q, want machine-nb (via the compiled grove)", node.NotebookName)
	}
	if root := h.nodeWorkspaceRoot(node); root == "" || !filepath.IsAbs(root) {
		t.Fatalf("nodeWorkspaceRoot = %q, want an absolute root under %s", root, notebookRoot)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
