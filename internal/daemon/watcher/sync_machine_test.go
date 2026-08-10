package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// The daemon consumes only the compiled recorded view: roots.toml's literal
// route carries both notebook name and resolved root into cfg.Groves.
func TestSyntheticNodeResolvesThroughCompiledRecordedRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)
	config.ResetLoadCache()
	t.Cleanup(config.ResetLoadCache)

	configDir := filepath.Join(home, "config", "grove")
	notebookRoot := filepath.Join(home, "notebooks", "machine-nb")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	writeTestFile(t, filepath.Join(configDir, "notebooks.toml"), `default = "machine-nb"

[notebooks.machine-nb]
root = "`+notebookRoot+`"
`)
	writeTestFile(t, filepath.Join(configDir, "roots.toml"), `[roots.grovetools]
path = "`+filepath.Join(home, "code", "grovetools")+`"
notebook = "machine-nb"
`)

	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if _, ok := cfg.Groves["grovetools"]; !ok {
		t.Fatalf("recorded root did not compile into Groves: %v", cfg.Groves)
	}

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, cfg, nil, db, 50, 500)
	node, err := h.syntheticNodeFor("grovetools")
	if err != nil {
		t.Fatal(err)
	}
	if node.NotebookName != "machine-nb" {
		t.Fatalf("syntheticNodeFor NotebookName = %q, want machine-nb (via the compiled grove)", node.NotebookName)
	}
	if root, err := h.nodeWorkspaceRoot(node); err != nil || root == "" || !filepath.IsAbs(root) {
		t.Fatalf("nodeWorkspaceRoot = %q, %v; want an absolute root under %s", root, err, notebookRoot)
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
