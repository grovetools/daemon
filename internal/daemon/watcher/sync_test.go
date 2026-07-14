package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/syncproto"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// newTestSyncHandler builds a SyncHandler against a temp workspace root and
// a temp sync.db, with the watch table injected directly (no workspace
// discovery machinery needed).
func newTestSyncHandler(t *testing.T, quietMs, maxWaitMs int) (*SyncHandler, string) {
	t.Helper()

	wsRoot := t.TempDir()
	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, nil, nil, db, quietMs, maxWaitMs)
	h.watchedPaths = map[string]*syncWatch{
		wsRoot: {workspace: "testws", root: wsRoot, space: syncdb.NewDocSpace(nil)},
	}
	return h, wsRoot
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The default exclusion-manifest table formerly tested here (TestSyncExclusion
// Manifest against syncExcluded) moved with the logic into the sync package's
// DocSpace — see daemon/internal/daemon/sync/docspace_test.go. MatchesEvent
// below still exercises the manifest end-to-end via the DocSpace delegation.

// newRecursiveTestWorkspace builds a workspace tree under a real
// `.../workspaces/<name>` path (so workspaceRootForDir resolves the root the
// way it does on a centralized notebook) containing a mix of Included content
// dirs and excluded subtrees. It returns the workspace root and the fabricated
// content-dir list (root as "notes", plans/ as "plans") — no locator needed,
// which is the whole point of extracting computeWorkspaceWatches.
func newRecursiveTestWorkspace(t *testing.T, subdirs []string) (string, []workspace.ContentDirectory) {
	t.Helper()
	wsRoot := filepath.Join(t.TempDir(), "workspaces", "testws")
	for _, d := range subdirs {
		if err := os.MkdirAll(filepath.Join(wsRoot, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	dirs := []workspace.ContentDirectory{
		{Path: wsRoot, Type: "notes"},
		{Path: filepath.Join(wsRoot, "plans"), Type: "plans"},
		{Path: filepath.Join(wsRoot, "chats"), Type: "chats"},
	}
	return wsRoot, dirs
}

// TestComputeWorkspaceWatchesRecursive is the S1 reproduction: files created
// under quick/, inbox/, and a plan sub-subdir must land in the watch set —
// today only the 3 whitelisted content dirs were registered. Also asserts the
// excluded subtrees (.git/, .artifacts/) are never registered, and that
// plans-only mode restricts to plans/ while keeping the workspace root as
// syncWatch.root.
func TestComputeWorkspaceWatchesRecursive(t *testing.T) {
	wsRoot, dirs := newRecursiveTestWorkspace(t, []string{
		"quick", "inbox", "daily", "concepts",
		filepath.Join("plans", "myplan", "sub"), "chats",
		".artifacts", ".git",
	})

	abs := func(rel string) string {
		if rel == "" {
			return wsRoot
		}
		return filepath.Join(wsRoot, filepath.FromSlash(rel))
	}

	// Full mode: every Included dir registered, excluded subtrees absent.
	full := computeWorkspaceWatches(&config.SyncWorkspace{Name: "testws", Mode: config.SyncModeFull}, dirs)
	for _, rel := range []string{"", "quick", "inbox", "daily", "concepts", "plans", "plans/myplan", "plans/myplan/sub", "chats"} {
		w, ok := full[abs(rel)]
		if !ok {
			t.Errorf("full mode: missing watch for %q (S1)", rel)
			continue
		}
		if w.root != wsRoot || w.workspace != "testws" {
			t.Errorf("full mode: watch %q has root=%q ws=%q, want root=%q ws=testws", rel, w.root, w.workspace, wsRoot)
		}
	}
	for path := range full {
		rel, _ := filepath.Rel(wsRoot, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".git") || strings.HasPrefix(rel, ".artifacts") {
			t.Errorf("full mode registered an excluded dir: %q", rel)
		}
	}

	// Plans-only mode: only plans/… dirs, syncWatch.root still the workspace root.
	plansOnly := computeWorkspaceWatches(&config.SyncWorkspace{Name: "testws", Mode: config.SyncModePlansOnly}, dirs)
	if _, ok := plansOnly[abs("plans/myplan/sub")]; !ok {
		t.Error("plans-only: missing plans/myplan/sub")
	}
	if _, ok := plansOnly[abs("quick")]; ok {
		t.Error("plans-only: quick/ should not be registered")
	}
	for path, w := range plansOnly {
		rel, _ := filepath.Rel(wsRoot, path)
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, "plans") {
			t.Errorf("plans-only registered non-plans dir: %q", rel)
		}
		if w.root != wsRoot {
			t.Errorf("plans-only: syncWatch.root = %q, want workspace root %q", w.root, wsRoot)
		}
	}
}

// TestRecursiveWatchFlushS1 is the literal S1 sentence made executable: seed the
// watch table from computeWorkspaceWatches, write a file under quick/ and under
// a plan sub-subdir, drive flush, and assert both reach the outbox with the
// correct workspace-root-relative wire paths.
func TestRecursiveWatchFlushS1(t *testing.T) {
	wsRoot, dirs := newRecursiveTestWorkspace(t, []string{
		"quick", filepath.Join("plans", "myplan", "sub"),
	})

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, nil, nil, db, 50, 500)
	h.watchedPaths = computeWorkspaceWatches(&config.SyncWorkspace{Name: "testws", Mode: config.SyncModeFull}, dirs)

	ctx := context.Background()
	quickFile := filepath.Join(wsRoot, "quick", "x.md")
	subFile := filepath.Join(wsRoot, "plans", "myplan", "sub", "y.md")
	writeFile(t, quickFile, "quick note")
	writeFile(t, subFile, "deep plan note")
	h.flush(ctx, quickFile)
	h.flush(ctx, subFile)

	entries, _ := db.ListOutbox("testws", 0)
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Path] = true
	}
	if !got["quick/x.md"] {
		t.Errorf("expected outbox entry for quick/x.md; got %+v", entries)
	}
	if !got["plans/myplan/sub/y.md"] {
		t.Errorf("expected outbox entry for plans/myplan/sub/y.md; got %+v", entries)
	}
}

// satelliteConfig mirrors the config shape satellite-bootstrap.sh writes on a
// prebuilt VM: one grove pointing at a (possibly empty) code dir, referencing
// one centralized notebook definition. No notebooks.rules.
func satelliteConfig(notebookRoot string) *config.Config {
	return &config.Config{
		Groves: map[string]config.GroveSourceConfig{
			"grovetools": {Path: "/nonexistent/code/grovetools", Notebook: "grovetools"},
		},
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{
				"grovetools": {RootDir: notebookRoot},
			},
		},
	}
}

// TestConfiguredPullRootsZeroDiscovery is the empty-~/code satellite case made
// executable: pull = true subscriptions must resolve their workspace roots
// from sync.toml + notebook definitions alone — no discovered WorkspaceNode,
// no notebook tree on disk yet — so ensurePipelines can spawn pull pipelines
// that materialize the replica from nothing.
func TestConfiguredPullRootsZeroDiscovery(t *testing.T) {
	notebookRoot := filepath.Join(t.TempDir(), "notebooks", "grovetools") // does NOT exist
	syncCfg := &config.SyncConfig{Workspaces: []config.SyncWorkspace{
		{Name: "grovetools", Pull: true},
		{Name: "cloud", Pull: true},
		{Name: "push-only"},                                             // pull=false: excluded
		{Name: "searcher", Pull: true, Mode: config.SyncModeSearchOnly}, // no replica: excluded
	}}

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, satelliteConfig(notebookRoot), syncCfg, db, 50, 500)

	// Zero discovered workspaces: the watch set stays empty (nothing on disk
	// to watch), which must NOT starve the pull roots below.
	if paths := h.ComputeWatchPaths(nil); len(paths) != 0 {
		t.Fatalf("expected no watch paths for a nonexistent tree, got %v", paths)
	}

	roots := h.configuredPullRoots()
	want := map[string]string{
		"grovetools": filepath.Join(notebookRoot, "workspaces", "grovetools"),
		"cloud":      filepath.Join(notebookRoot, "workspaces", "cloud"),
	}
	if len(roots) != len(want) {
		t.Fatalf("configuredPullRoots = %v, want %v", roots, want)
	}
	for name, root := range want {
		if roots[name] != root {
			t.Errorf("configuredPullRoots[%q] = %q, want %q", name, roots[name], root)
		}
	}
}

// TestSyntheticNodeNotebookResolution pins the NotebookName preference order:
// an on-disk workspace root under a definition wins over the grove-referenced
// notebook; with nothing on disk the grove reference wins; with neither, the
// node is left for the locator's default fallback chain.
func TestSyntheticNodeNotebookResolution(t *testing.T) {
	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	existingRoot := t.TempDir()
	cfg := &config.Config{
		Groves: map[string]config.GroveSourceConfig{
			"main": {Path: "/nonexistent/code", Notebook: "grove-nb"},
		},
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{
				"grove-nb": {RootDir: "/nonexistent/notebooks/grove-nb"},
				"other-nb": {RootDir: existingRoot},
			},
		},
	}
	h := NewSyncHandler(nil, cfg, nil, db, 50, 500)

	// Nothing on disk: the grove-referenced notebook wins.
	if node := h.syntheticNodeFor("ws"); node.NotebookName != "grove-nb" {
		t.Errorf("no dirs on disk: NotebookName = %q, want grove-nb", node.NotebookName)
	}

	// The workspace root exists under other-nb: existence wins over the grove
	// reference.
	if err := os.MkdirAll(filepath.Join(existingRoot, "workspaces", "ws"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if node := h.syntheticNodeFor("ws"); node.NotebookName != "other-nb" {
		t.Errorf("existing dir: NotebookName = %q, want other-nb", node.NotebookName)
	}

	// No groves, no definitions: bare node, locator default fallback applies
	// (builtin centralized default -> still an absolute, usable root).
	h.cfg = &config.Config{}
	node := h.syntheticNodeFor("ws")
	if node.NotebookName != "" {
		t.Errorf("bare config: NotebookName = %q, want empty", node.NotebookName)
	}
	if root := h.nodeWorkspaceRoot(node); root == "" || !filepath.IsAbs(root) {
		t.Errorf("bare config: nodeWorkspaceRoot = %q, want absolute builtin-default root", root)
	}
}

// TestComputeWatchPathsSyntheticPullCoverage: once a pull pipeline has
// materialized the replica tree, ComputeWatchPaths must cover it via the
// synthetic (config-derived) path even with zero discovered workspaces — so
// satellite-side edits are captured and pushed. Also asserts the
// discovery-driven path is unchanged: a discovered node covers its own
// subscription (no duplicate/synthetic entries), and a pull=false
// subscription gains no synthetic watches.
func TestComputeWatchPathsSyntheticPullCoverage(t *testing.T) {
	notebookRoot := t.TempDir()
	wsRoot := filepath.Join(notebookRoot, "workspaces", "pulled")
	for _, d := range []string{"inbox", "plans"} {
		if err := os.MkdirAll(filepath.Join(wsRoot, d), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// A second, push-only workspace tree that exists on disk but must NOT be
	// picked up synthetically (discovery owns the push side).
	pushRoot := filepath.Join(notebookRoot, "workspaces", "pushonly")
	if err := os.MkdirAll(filepath.Join(pushRoot, "inbox"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	syncCfg := &config.SyncConfig{Workspaces: []config.SyncWorkspace{
		{Name: "pulled", Pull: true},
		{Name: "pushonly"},
	}}

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, satelliteConfig(notebookRoot), syncCfg, db, 50, 500)

	// Zero discovered workspaces: only the pull subscription's tree is watched.
	paths := h.ComputeWatchPaths(nil)
	got := make(map[string]bool, len(paths))
	for _, p := range paths {
		got[p] = true
	}
	for _, want := range []string{wsRoot, filepath.Join(wsRoot, "inbox"), filepath.Join(wsRoot, "plans")} {
		if !got[want] {
			t.Errorf("synthetic pull coverage: missing watch for %q (got %v)", want, paths)
		}
	}
	for p := range got {
		if strings.HasPrefix(p, pushRoot) {
			t.Errorf("push-only workspace watched synthetically: %q", p)
		}
	}
	w, rel := h.lookupWatch(filepath.Join(wsRoot, "inbox", "note.md"))
	if w == nil || w.workspace != "pulled" || w.root != wsRoot || rel != "inbox/note.md" {
		t.Errorf("synthetic watch lookup: got (%+v, %q)", w, rel)
	}

	// A discovered node for the same subscription covers it: the discovery
	// path (not the synthetic one) produces the watches, and push-only
	// workspaces are watched when discovered. NotebookName comes resolved on
	// discovered nodes, so both workspaces resolve into the same notebook.
	enriched := []*models.EnrichedWorkspace{
		{WorkspaceNode: &workspace.WorkspaceNode{Name: "pulled", NotebookName: "grovetools"}},
		{WorkspaceNode: &workspace.WorkspaceNode{Name: "pushonly", NotebookName: "grovetools"}},
	}
	paths = h.ComputeWatchPaths(enriched)
	got = make(map[string]bool, len(paths))
	for _, p := range paths {
		got[p] = true
	}
	if !got[wsRoot] || !got[filepath.Join(wsRoot, "inbox")] {
		t.Errorf("discovered pull workspace lost coverage: %v", paths)
	}
	if !got[pushRoot] || !got[filepath.Join(pushRoot, "inbox")] {
		t.Errorf("discovery-driven push path regressed: %v", paths)
	}
}

func TestSyncMatchesEvent(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)

	cases := []struct {
		path    string
		op      fsnotify.Op
		matches bool
	}{
		{filepath.Join(wsRoot, "notes", "inbox", "a.md"), fsnotify.Write, true},
		{filepath.Join(wsRoot, "plans", "p", "spec.md"), fsnotify.Create, true},
		{filepath.Join(wsRoot, "notes", "a.md"), fsnotify.Chmod, false},                     // chmod noise
		{filepath.Join(wsRoot, ".obsidian", "workspace.json"), fsnotify.Write, false},       // manifest
		{filepath.Join(wsRoot, "plans", "p", ".artifacts", "b.xml"), fsnotify.Write, false}, // manifest
		{filepath.Join(wsRoot, "plans", "p.lock"), fsnotify.Create, false},                  // manifest
		{filepath.Join(wsRoot, "notes", ".hidden.md"), fsnotify.Write, false},               // hidden file
		{filepath.Join(t.TempDir(), "outside.md"), fsnotify.Write, false},                   // unsubscribed
		{filepath.Join(wsRoot, "notes", "a.sync-conflict-1-A.md"), fsnotify.Create, false},  // manifest
		{filepath.Join(wsRoot, ".grove", "rules", "x.rules"), fsnotify.Write, false},        // manifest
	}

	for _, tc := range cases {
		ev := fsnotify.Event{Name: tc.path, Op: tc.op}
		if got := h.MatchesEvent(ev); got != tc.matches {
			t.Errorf("MatchesEvent(%q, %v) = %v, want %v", tc.path, tc.op, got, tc.matches)
		}
	}
}

func TestSyncFlushHashGating(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "inbox", "idea.md")
	writeFile(t, notePath, "first draft")

	// First flush: document_created.
	h.flush(ctx, notePath)
	entries, _ := h.db.ListOutbox("testws", 0)
	if len(entries) != 1 || entries[0].EventType != syncproto.EventDocumentCreated {
		t.Fatalf("expected 1 created event, got %+v", entries)
	}
	doc, _ := h.db.GetDocumentByPath("testws", "notes/inbox/idea.md")
	if doc == nil || doc.DocumentID == "" {
		t.Fatalf("expected tracked document, got %+v", doc)
	}

	// Same content: hash-gated, no new outbox row.
	h.flush(ctx, notePath)
	if entries, _ = h.db.ListOutbox("testws", 0); len(entries) != 1 {
		t.Fatalf("hash-gate failed: expected 1 entry, got %d", len(entries))
	}

	// Changed content: document_updated with the SAME document id.
	writeFile(t, notePath, "second draft")
	h.flush(ctx, notePath)
	entries, _ = h.db.ListOutbox("testws", 0)
	if len(entries) != 2 || entries[1].EventType != syncproto.EventDocumentUpdated {
		t.Fatalf("expected created+updated, got %+v", entries)
	}
	if entries[1].DocumentID != doc.DocumentID {
		t.Fatalf("document id changed on update: %q != %q", entries[1].DocumentID, doc.DocumentID)
	}
}

func TestSyncDebounceCoalescing(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 40, 400)
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "log.md")
	writeFile(t, notePath, "line 1\n")

	// Rapid event bursts (simulating appends) must coalesce into one flush.
	for i := 0; i < 5; i++ {
		err := h.HandleEvents(ctx, []fsnotify.Event{{Name: notePath, Op: fsnotify.Write}})
		if err != nil {
			t.Fatalf("HandleEvents: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := h.db.ListOutbox("testws", 0)
		if len(entries) > 0 {
			if len(entries) != 1 {
				t.Fatalf("debounce failed to coalesce: %d outbox entries", len(entries))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("debounced flush never fired")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSyncSecretQuarantine(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	st := store.New()
	h.store = st
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "leaky.md")
	writeFile(t, notePath, "remote token: github_pat_11ABCDEF0123456789_abcdefghij\n")

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	h.flush(ctx, notePath)

	// Quarantined: nothing reaches the outbox or the identity map.
	if entries, _ := h.db.ListOutbox("testws", 0); len(entries) != 0 {
		t.Fatalf("quarantined file reached outbox: %+v", entries)
	}
	if doc, _ := h.db.GetDocumentByPath("testws", "notes/leaky.md"); doc != nil {
		t.Fatalf("quarantined file tracked in sync_documents: %+v", doc)
	}

	// An SSE-visible sync_conflict update is broadcast.
	select {
	case update := <-sub:
		if update.Type != store.UpdateSyncConflict {
			t.Fatalf("expected sync_conflict update, got %s", update.Type)
		}
		payload, ok := update.Payload.(*store.SyncConflictPayload)
		if !ok || payload.Kind != "secret_quarantine" || payload.Path != "notes/leaky.md" {
			t.Fatalf("unexpected payload: %+v", update.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("no quarantine update broadcast")
	}

	// Once the secret is removed the file syncs normally.
	writeFile(t, notePath, "remote token: managed via token_command now\n")
	h.flush(ctx, notePath)
	if entries, _ := h.db.ListOutbox("testws", 0); len(entries) != 1 {
		t.Fatalf("cleaned file did not sync: %+v", entries)
	}
}

func TestSyncDeleteCapture(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "gone.md")
	writeFile(t, notePath, "ephemeral")
	h.flush(ctx, notePath)

	doc, _ := h.db.GetDocumentByPath("testws", "notes/gone.md")
	if doc == nil {
		t.Fatal("expected tracked document before delete")
	}

	if err := os.Remove(notePath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.flush(ctx, notePath)

	entries, _ := h.db.ListOutbox("testws", 0)
	if len(entries) != 2 || entries[1].EventType != syncproto.EventDocumentDeleted {
		t.Fatalf("expected created+deleted, got %+v", entries)
	}
	if entries[1].DocumentID != doc.DocumentID {
		t.Fatalf("delete event has wrong document id: %+v", entries[1])
	}
	if doc, _ := h.db.GetDocumentByPath("testws", "notes/gone.md"); doc != nil {
		t.Fatalf("document still tracked after delete: %+v", doc)
	}

	// Deleting an untracked path records nothing.
	h.flush(ctx, filepath.Join(wsRoot, "notes", "never-existed.md"))
	if entries, _ := h.db.ListOutbox("testws", 0); len(entries) != 2 {
		t.Fatalf("untracked delete produced outbox entries: %+v", entries)
	}
}

func TestSyncTypedMoveEvent(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	ctx := context.Background()

	oldPath := filepath.Join(wsRoot, "notes", "inbox", "task.md")
	newPath := filepath.Join(wsRoot, "notes", "current", "task.md")
	writeFile(t, oldPath, "task body")
	h.flush(ctx, oldPath)

	doc, _ := h.db.GetDocumentByPath("testws", "notes/inbox/task.md")
	if doc == nil {
		t.Fatal("expected tracked document before move")
	}

	// nb promotes the note: typed event with PrevPath populated.
	writeFile(t, newPath, "task body")
	_ = os.Remove(oldPath)
	h.HandleStoreUpdate(store.Update{
		Type:   store.UpdateNoteEvent,
		Source: "nb",
		Payload: &models.NoteEvent{
			Event:     models.NoteEventMoved,
			Workspace: "testws",
			Path:      newPath,
			PrevPath:  oldPath,
		},
	})

	entries, _ := h.db.ListOutbox("testws", 0)
	if len(entries) != 2 {
		t.Fatalf("expected created+moved, got %+v", entries)
	}
	moved := entries[1]
	if moved.EventType != syncproto.EventDocumentMoved ||
		moved.DocumentID != doc.DocumentID ||
		moved.Path != "notes/current/task.md" ||
		moved.PrevPath != "notes/inbox/task.md" {
		t.Fatalf("unexpected moved event: %+v", moved)
	}

	// Identity map follows the rename: same UUID, new path.
	if old, _ := h.db.GetDocumentByPath("testws", "notes/inbox/task.md"); old != nil {
		t.Fatalf("old path still tracked after move: %+v", old)
	}
	cur, _ := h.db.GetDocumentByPath("testws", "notes/current/task.md")
	if cur == nil || cur.DocumentID != doc.DocumentID {
		t.Fatalf("new path not tracked with stable id: %+v", cur)
	}

	// Hash-gate: the follow-up fsnotify create on the new path is a no-op
	// because the content hash is unchanged.
	h.flush(ctx, newPath)
	if entries, _ := h.db.ListOutbox("testws", 0); len(entries) != 2 {
		t.Fatalf("post-move flush was not hash-gated: %+v", entries)
	}
}

// TestSyncDeleteCapturesBaseVersion is the B7 watcher-side regression: the
// deleted outbox entry must carry the doc's last-synced version as its OCC
// base, captured BEFORE recordDelete destroys the doc row (the row is the only
// other record of that version). Without it every delete of a server-known doc
// pushes base_version 0, which the server's applyDelete OCC check rejects as a
// conflict — permanently, since retries resend the same 0 (found live: a
// push-only delete parked as conflict forever).
func TestSyncDeleteCapturesBaseVersion(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "synced.md")
	writeFile(t, notePath, "pushed content")
	h.flush(ctx, notePath)

	doc, _ := h.db.GetDocumentByPath("testws", "notes/synced.md")
	if doc == nil {
		t.Fatal("expected tracked document before delete")
	}
	// Server confirms the create at version 6 (what the push pipeline records
	// on an accepted push).
	if err := h.db.MarkDocumentSynced(doc.DocumentID, 6, doc.ContentHash, []byte("pushed content")); err != nil {
		t.Fatalf("MarkDocumentSynced: %v", err)
	}

	if err := os.Remove(notePath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.flush(ctx, notePath)

	entries, _ := h.db.ListOutbox("testws", 0)
	if len(entries) != 2 || entries[1].EventType != syncproto.EventDocumentDeleted {
		t.Fatalf("expected created+deleted, got %+v", entries)
	}
	if entries[1].BaseVersion != 6 {
		t.Fatalf("deleted entry must carry the last-synced version 6 as base_version, got %d", entries[1].BaseVersion)
	}
	// The identity-map row is still dropped at enqueue time (keeping it alive
	// would break delete-then-recreate on UNIQUE(workspace, path)).
	if doc, _ := h.db.GetDocumentByPath("testws", "notes/synced.md"); doc != nil {
		t.Fatalf("document still tracked after delete: %+v", doc)
	}
}
