package watcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

func TestWarnDeduperFirstOccurrenceLogs(t *testing.T) {
	d := newWarnDeduper(5 * time.Minute)
	now := time.Now()

	logNow, suppressed := d.shouldLog("delete stale chunk vectors: database is locked", now)
	if !logNow || suppressed != 0 {
		t.Fatalf("first occurrence: got (logNow=%v, suppressed=%d), want (true, 0)", logNow, suppressed)
	}

	// A different error string is its own key and logs immediately.
	logNow, suppressed = d.shouldLog("insert document: disk I/O error", now)
	if !logNow || suppressed != 0 {
		t.Fatalf("distinct key: got (logNow=%v, suppressed=%d), want (true, 0)", logNow, suppressed)
	}
}

func TestWarnDeduperSuppressesWithinInterval(t *testing.T) {
	d := newWarnDeduper(5 * time.Minute)
	now := time.Now()
	key := "database is locked"

	d.shouldLog(key, now)
	for i := 1; i <= 10; i++ {
		logNow, suppressed := d.shouldLog(key, now.Add(time.Duration(i)*time.Second))
		if logNow || suppressed != 0 {
			t.Fatalf("repeat %d within interval: got (logNow=%v, suppressed=%d), want (false, 0)", i, logNow, suppressed)
		}
	}
}

func TestWarnDeduperFlushesSummaryAfterInterval(t *testing.T) {
	d := newWarnDeduper(5 * time.Minute)
	now := time.Now()
	key := "database is locked"

	d.shouldLog(key, now)
	for i := 0; i < 7; i++ {
		d.shouldLog(key, now.Add(time.Minute))
	}

	logNow, suppressed := d.shouldLog(key, now.Add(5*time.Minute))
	if !logNow || suppressed != 7 {
		t.Fatalf("after interval: got (logNow=%v, suppressed=%d), want (true, 7)", logNow, suppressed)
	}

	// The flush resets the window and the counter.
	logNow, suppressed = d.shouldLog(key, now.Add(5*time.Minute+time.Second))
	if logNow || suppressed != 0 {
		t.Fatalf("repeat after flush: got (logNow=%v, suppressed=%d), want (false, 0)", logNow, suppressed)
	}
	logNow, suppressed = d.shouldLog(key, now.Add(10*time.Minute+time.Second))
	if !logNow || suppressed != 1 {
		t.Fatalf("second flush: got (logNow=%v, suppressed=%d), want (true, 1)", logNow, suppressed)
	}
}

// notebookHandler builds a MemoryHandler wired to a centralized notebook at
// root, with the initial full sync consumed: ComputeWatchPaths would otherwise
// spawn fullSync against a nil document store.
func notebookHandler(root string) *MemoryHandler {
	cfg := &config.Config{
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{"nb": {RootDir: root}},
			Rules:       &config.NotebookRules{Default: "nb"},
		},
	}
	h := &MemoryHandler{
		cfg:          cfg,
		locator:      workspace.NewNotebookLocator(cfg),
		watchedPaths: make(map[string]*workspace.WorkspaceNode),
		codePaths:    make(map[string]bool),
		timers:       make(map[string]*time.Timer),
	}
	h.initialSync.Do(func() {})
	return h
}

// TestAssistantMemoryDirIsWatched pins the fix for the Phase 0 finding: before
// it, ComputeWatchPaths watched only skills/concepts/issues/inbox/completed/
// context-presets/code, so "the assistant drops a file and the daemon indexes
// it" was simply false.
func TestAssistantMemoryDirIsWatched(t *testing.T) {
	root := t.TempDir()
	eco := t.TempDir()

	memoryDir := filepath.Join(root, "workspaces", "grovetools", "steward", "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "workspaces", "grovetools", "plans"), 0o755); err != nil {
		t.Fatal(err)
	}

	node := &workspace.WorkspaceNode{
		Name:         "grovetools",
		Path:         eco,
		Kind:         workspace.KindEcosystemRoot,
		NotebookName: "nb",
	}

	h := notebookHandler(root)
	paths := h.ComputeWatchPaths([]*models.EnrichedWorkspace{{WorkspaceNode: node}})
	if !slices.Contains(paths, memoryDir) {
		t.Fatalf("assistant memory dir %s not watched; got %v", memoryDir, paths)
	}
	// It is notebook content, not code: only markdown-ish files may match.
	if h.codePaths[memoryDir] {
		t.Errorf("assistant memory dir was registered as a code path")
	}
}

func TestAssistantMemoryDirFollowsGroveTomlPlanName(t *testing.T) {
	root := t.TempDir()
	eco := t.TempDir()

	if err := os.WriteFile(filepath.Join(eco, "grove.toml"), []byte("[assistant]\nenabled = true\nplan = \"front-desk\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(root, "workspaces", "grovetools", "front-desk", "memory")
	steward := filepath.Join(root, "workspaces", "grovetools", "steward", "memory")
	for _, dir := range []string{renamed, steward} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	node := &workspace.WorkspaceNode{Name: "grovetools", Path: eco, Kind: workspace.KindEcosystemRoot, NotebookName: "nb"}
	paths := notebookHandler(root).ComputeWatchPaths([]*models.EnrichedWorkspace{{WorkspaceNode: node}})
	if !slices.Contains(paths, renamed) {
		t.Errorf("configured assistant plan %q not watched; got %v", renamed, paths)
	}
	// The default name is a fallback for an unconfigured ecosystem, not an
	// additional watch on a configured one.
	if slices.Contains(paths, steward) {
		t.Errorf("default plan dir %s watched despite an explicit [assistant] plan", steward)
	}
}

func TestAssistantMemoryDirSkipsNonEcosystemAndMissingDirs(t *testing.T) {
	root := t.TempDir()
	h := notebookHandler(root)

	// The assistant is ecosystem-scoped (spec §3.1): a member repo must not
	// contribute a second watch on its ecosystem's memory directory.
	member := &workspace.WorkspaceNode{
		Name: "daemon", Path: filepath.Join(root, "daemon"), Kind: workspace.KindEcosystemSubProject,
		NotebookName: "nb", ParentEcosystemPath: filepath.Join(root, "eco"),
	}
	if dir := h.assistantMemoryDir(member); dir != "" {
		t.Errorf("member repo resolved an assistant memory dir: %s", dir)
	}
	if dir := h.assistantMemoryDir(nil); dir != "" {
		t.Errorf("nil node resolved an assistant memory dir: %s", dir)
	}

	// An ecosystem that has never written a memory file is resolved but never
	// watched: addDir stats the directory first.
	eco := &workspace.WorkspaceNode{Name: "grovetools", Path: t.TempDir(), Kind: workspace.KindEcosystemRoot, NotebookName: "nb"}
	want := filepath.Join(root, "workspaces", "grovetools", "steward", "memory")
	if dir := h.assistantMemoryDir(eco); dir != want {
		t.Errorf("assistantMemoryDir = %q, want %q", dir, want)
	}
	if paths := h.ComputeWatchPaths([]*models.EnrichedWorkspace{{WorkspaceNode: eco}}); slices.Contains(paths, want) {
		t.Errorf("watched a memory directory that does not exist: %v", paths)
	}
}

func TestNotebookDocType(t *testing.T) {
	cases := map[string]string{
		"/n/workspaces/grovetools/steward/memory/steward-session-1.md": memoryDocType,
		"/n/workspaces/grovetools/steward/memory/index.md":             memoryDocType,
		// Note types win over the memory directory, so a concept, plan or
		// issue that happens to be about memory is not reclassified.
		"/n/workspaces/grovetools/concepts/memory/overview.md": "concept",
		"/n/workspaces/grovetools/plans/memory/01-design.md":   "plan",
		"/n/workspaces/grovetools/issues/memory/20260801-x.md": "issue",
		"/n/workspaces/grovetools/skills/memory/SKILL.md":      "skill",
		"/n/workspaces/grovetools/inbox/20260801-deep-work.md": "note",
		"/n/workspaces/grovetools/completed/20260731-thing.md": "note",
	}
	for path, want := range cases {
		if got := notebookDocType(path); got != want {
			t.Errorf("notebookDocType(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestIsDatabaseBusyErr(t *testing.T) {
	busy := []error{
		fmt.Errorf("delete stale chunk vectors: %w", errors.New("database is locked")),
		errors.New("database table is locked"),
		errors.New("SQLITE_BUSY: cannot start a transaction within a transaction"),
	}
	for _, err := range busy {
		if !isDatabaseBusyErr(err) {
			t.Errorf("expected busy: %v", err)
		}
	}

	notBusy := []error{
		nil,
		errors.New("disk I/O error"),
		errors.New("no such table: documents"),
	}
	for _, err := range notBusy {
		if isDatabaseBusyErr(err) {
			t.Errorf("expected not busy: %v", err)
		}
	}
}
