package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// newTestSyncHandler builds a SyncHandler against a temp notespace root and
// a temp sync.db, with the watch table injected directly (no notespace
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
		wsRoot: {notespace: "testws", root: wsRoot, space: syncdb.NewDocSpace(nil)},
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

// newRecursiveTestNotespace builds a notespace tree under a real
// `.../notespaces/<name>` path (so notespaceRootForDir resolves the root the
// way it does on a centralized notebook) containing a mix of Included content
// dirs and excluded subtrees. It returns the notespace root and the fabricated
// content-dir list (root as "notes", plans/ as "plans") — no locator needed,
// which is the whole point of extracting computeNotespaceWatches.
func newRecursiveTestNotespace(t *testing.T, subdirs []string) (string, []workspace.ContentDirectory) {
	t.Helper()
	wsRoot := filepath.Join(t.TempDir(), "notespaces", "testws")
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

// TestComputeNotespaceWatchesRecursive is the S1 reproduction: files created
// under quick/, inbox/, and a plan sub-subdir must land in the watch set —
// today only the 3 whitelisted content dirs were registered. Also asserts the
// excluded subtrees (.git/, .artifacts/) are never registered, and that
// plans-only mode restricts to plans/ while keeping the notespace root as
// syncWatch.root.
func TestComputeNotespaceWatchesRecursive(t *testing.T) {
	wsRoot, dirs := newRecursiveTestNotespace(t, []string{
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
	full := computeNotespaceWatches(&config.SyncWorkspace{Name: "testws", Mode: config.SyncModeFull}, wsRoot, dirs)
	for _, rel := range []string{"", "quick", "inbox", "daily", "concepts", "plans", "plans/myplan", "plans/myplan/sub", "chats"} {
		w, ok := full[abs(rel)]
		if !ok {
			t.Errorf("full mode: missing watch for %q (S1)", rel)
			continue
		}
		if w.root != wsRoot || w.displayName != "testws" || w.notespace != "" {
			t.Errorf("full mode: watch %q = %+v, want display=testws with no routing id before registration", rel, w)
		}
	}
	for path := range full {
		rel, _ := filepath.Rel(wsRoot, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".git") || strings.HasPrefix(rel, ".artifacts") {
			t.Errorf("full mode registered an excluded dir: %q", rel)
		}
	}

	// Plans-only mode: only plans/… dirs, syncWatch.root still the notespace root.
	plansOnly := computeNotespaceWatches(&config.SyncWorkspace{Name: "testws", Mode: config.SyncModePlansOnly}, wsRoot, dirs)
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
			t.Errorf("plans-only: syncWatch.root = %q, want notespace root %q", w.root, wsRoot)
		}
	}
}

func TestComputeNotespaceWatchesUsesRecordedRootNotFirstStatableDir(t *testing.T) {
	recorded, dirs := newRecursiveTestNotespace(t, []string{"inbox"})
	decoy := filepath.Join(t.TempDir(), "notespaces", "testws")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	dirs = append([]workspace.ContentDirectory{{Path: decoy, Type: "notes"}}, dirs...)

	watches := computeNotespaceWatches(&config.SyncWorkspace{Name: "testws", Mode: config.SyncModeFull}, recorded, dirs)
	if _, ok := watches[decoy]; ok {
		t.Fatalf("first stat-able decoy %q became a watch root", decoy)
	}
	for path, watch := range watches {
		if watch.root != recorded {
			t.Errorf("watch %q root = %q, want recorded root %q", path, watch.root, recorded)
		}
	}
}

// TestRecursiveWatchFlushS1 is the literal S1 sentence made executable: seed the
// watch table from computeNotespaceWatches, write a file under quick/ and under
// a plan sub-subdir, drive flush, and assert both reach the outbox with the
// correct notespace-root-relative wire paths.
func TestRecursiveWatchFlushS1(t *testing.T) {
	wsRoot, dirs := newRecursiveTestNotespace(t, []string{
		"quick", filepath.Join("plans", "myplan", "sub"),
	})

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, nil, nil, db, 50, 500)
	h.watchedPaths = computeNotespaceWatches(&config.SyncWorkspace{Name: "testws", Mode: config.SyncModeFull}, wsRoot, dirs)
	// Simulate successful v3 registration; capture must use the immutable id,
	// never the display subscription name.
	for _, watch := range h.watchedPaths {
		watch.notespace = "test-id"
	}

	ctx := context.Background()
	quickFile := filepath.Join(wsRoot, "quick", "x.md")
	subFile := filepath.Join(wsRoot, "plans", "myplan", "sub", "y.md")
	writeFile(t, quickFile, "quick note")
	writeFile(t, subFile, "deep plan note")
	h.flush(ctx, quickFile)
	h.flush(ctx, subFile)

	entries, _ := db.ListOutbox("test-id", 0)
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
			"grovetools": {Path: "/nonexistent/code/grovetools", Notebook: "grovetools", NotebookRoot: notebookRoot},
		},
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{
				"grovetools": {RootDir: notebookRoot},
			},
			Rules: &config.NotebookRules{Default: "grovetools"},
		},
	}
}

// TestConfiguredPullRootsZeroDiscovery is the empty-~/code satellite case made
// executable: pull = true subscriptions must resolve their notespace roots
// from sync.toml + notebook definitions alone — no discovered NotespaceNode,
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

	// Zero discovered notespaces: the watch set stays empty (nothing on disk
	// to watch), which must NOT starve the pull roots below.
	if paths := h.ComputeWatchPaths(nil); len(paths) != 0 {
		t.Fatalf("expected no watch paths for a nonexistent tree, got %v", paths)
	}

	roots, err := h.configuredPullRoots()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"grovetools": filepath.Join(notebookRoot, "notespaces", "grovetools"),
		"cloud":      filepath.Join(notebookRoot, "notespaces", "cloud"),
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

// TestSyntheticNodeNotebookResolution pins recorded-only routing: an exact
// compiled root binding wins regardless of same-name directories elsewhere;
// the explicit default is next; without either, resolution refuses.
func TestSyntheticNodeNotebookResolution(t *testing.T) {
	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	existingRoot := t.TempDir()
	literalRoot := filepath.Join(t.TempDir(), "literal-notebooks", "grove-nb")
	cfg := &config.Config{
		Groves: map[string]config.GroveSourceConfig{
			"ws": {Path: "/nonexistent/code", Notebook: "grove-nb", NotebookRoot: literalRoot},
		},
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{
				"grove-nb": {RootDir: "/nonexistent/notebooks/grove-nb"},
				"other-nb": {RootDir: existingRoot},
			},
			Rules: &config.NotebookRules{Default: "other-nb"},
		},
	}
	h := NewSyncHandler(nil, cfg, nil, db, 50, 500)

	// The exact compiled binding wins without an existence probe, and its root
	// identity survives a conflicting same-name Definitions entry.
	node, err := h.syntheticNodeFor("ws")
	if err != nil || node.NotebookName != "grove-nb" {
		t.Errorf("no dirs on disk: node=%+v err=%v, want grove-nb", node, err)
	}
	wantRoot := filepath.Join(literalRoot, "notespaces", "ws")
	if root, err := h.nodeNotespaceRoot(node); err != nil || root != wantRoot {
		t.Errorf("literal root = %q, %v; want %q without name re-resolution", root, err, wantRoot)
	}

	// Even three same-name trees elsewhere cannot override the recorded route.
	if err := os.MkdirAll(filepath.Join(existingRoot, "notespaces", "ws"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, extra := range []string{"third-nb", "fourth-nb"} {
		if err := os.MkdirAll(filepath.Join(existingRoot, extra, "notespaces", "ws"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if node, err := h.syntheticNodeFor("ws"); err != nil || node.NotebookName != "grove-nb" {
		t.Errorf("same-name dirs changed node to %+v, %v; want grove-nb", node, err)
	}
	if err := os.MkdirAll(filepath.Join(wantRoot, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	h = NewSyncHandler(nil, cfg, &config.SyncConfig{Workspaces: []config.SyncWorkspace{{Name: "ws", Mode: config.SyncModePlansOnly}}}, db, 50, 500)
	paths := h.ComputeWatchPaths(nil)
	if !slices.Contains(paths, filepath.Join(wantRoot, "plans")) {
		t.Errorf("plans-only watch paths = %v, want literal-root plans directory", paths)
	}

	// No binding and no recorded default is an explicit diagnostic, not an
	// empty node/root that downstream loops can silently omit.
	missingSync := &config.SyncConfig{Workspaces: []config.SyncWorkspace{{Name: "ws", Pull: true}}}
	h = NewSyncHandler(nil, &config.Config{}, missingSync, db, 50, 500)
	if node, err := h.syntheticNodeFor("ws"); err == nil || node.NotebookName != "" {
		t.Errorf("bare config: node=%+v err=%v, want missing-binding error", node, err)
	}
	if roots, err := h.configuredPullRoots(); err == nil || roots != nil {
		t.Errorf("configuredPullRoots = %v, %v; want explicit missing-binding error", roots, err)
	}
	h.ComputeWatchPaths(nil)
	if errors := h.RoutingErrors(); len(errors) != 1 || !strings.Contains(errors[0], "no recorded code-root/notebook binding") {
		t.Errorf("RoutingErrors = %v, want doctor-visible missing-binding condition", errors)
	}
}

// TestComputeWatchPathsSyntheticCoverage: once a pull pipeline has
// materialized the replica tree, ComputeWatchPaths must cover it via the
// synthetic (config-derived) path even with zero discovered notespaces — so
// satellite-side edits are captured and pushed. Push-only subscriptions get the
// same treatment (see TestComputeWatchPathsPushOnlyBareNotebook), and the
// discovery-driven path is unchanged: a discovered node covers its own
// subscription, with no duplicate synthetic entries.
func TestComputeWatchPathsSyntheticCoverage(t *testing.T) {
	notebookRoot := t.TempDir()
	wsRoot := filepath.Join(notebookRoot, "notespaces", "pulled")
	for _, d := range []string{"inbox", "plans"} {
		if err := os.MkdirAll(filepath.Join(wsRoot, d), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// A second, push-only notespace tree that exists on disk. It is picked up
	// synthetically too — see the F8 note in ComputeWatchPaths.
	pushRoot := filepath.Join(notebookRoot, "notespaces", "pushonly")
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

	// Zero discovered notespaces: both subscriptions' trees are watched.
	paths := h.ComputeWatchPaths(nil)
	got := make(map[string]bool, len(paths))
	for _, p := range paths {
		got[p] = true
	}
	for _, want := range []string{
		wsRoot, filepath.Join(wsRoot, "inbox"), filepath.Join(wsRoot, "plans"),
		pushRoot, filepath.Join(pushRoot, "inbox"),
	} {
		if !got[want] {
			t.Errorf("synthetic coverage: missing watch for %q (got %v)", want, paths)
		}
	}
	w, rel := h.lookupWatch(filepath.Join(wsRoot, "inbox", "note.md"))
	if w == nil || w.displayName != "pulled" || w.notespace != "" || w.root != wsRoot || rel != "inbox/note.md" {
		t.Errorf("synthetic watch lookup before registration: got (%+v, %q)", w, rel)
	}

	// A discovered node for the same subscription covers it: the discovery
	// path (not the synthetic one) produces the watches, and push-only
	// notespaces are watched when discovered. NotebookName comes resolved on
	// discovered nodes, so both notespaces resolve into the same notebook.
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
		t.Errorf("discovered pull notespace lost coverage: %v", paths)
	}
	if !got[pushRoot] || !got[filepath.Join(pushRoot, "inbox")] {
		t.Errorf("discovery-driven push path regressed: %v", paths)
	}
}

// TestComputeWatchPathsPushOnlyBareNotebook is the F8 residual made executable:
// a notebook-only notespace with pull = false. Nothing under any grove path
// yields it, so code discovery never produces a NotespaceNode for it, and
// configuredPullRoots skips it because it is not a pull target. Before the fix
// the synthetic loop's `!sub.Pull` filter dropped it too, so it got no watches —
// and because ensurePipelines derives its roots from this same watch set, no
// push pipeline either. A bare notebook simply never synced outbound.
//
// It must now get watches, and the watch must carry the notespace root so
// ensurePipelines can spawn its push transport. Pull stays off: this asserts
// only the capture side, and ensurePipelines still gates its pull loop on
// sub.Pull, so the legacy push-only invariant is untouched.
func TestComputeWatchPathsPushOnlyBareNotebook(t *testing.T) {
	notebookRoot := t.TempDir()
	wsRoot := filepath.Join(notebookRoot, "notespaces", "bare")
	for _, d := range []string{"inbox", "plans"} {
		if err := os.MkdirAll(filepath.Join(wsRoot, d), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	syncCfg := &config.SyncConfig{Workspaces: []config.SyncWorkspace{
		{Name: "bare"}, // legacy push-only: no Pull, no Role, no Mode
	}}

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, satelliteConfig(notebookRoot), syncCfg, db, 50, 500)

	// configuredPullRoots is pull-side only — it must still ignore this
	// subscription, which is precisely why the watch loop has to cover it.
	if roots, err := h.configuredPullRoots(); err != nil || len(roots) != 0 {
		t.Fatalf("configuredPullRoots = %v, %v; want empty for a push-only subscription", roots, err)
	}

	// Zero discovered notespaces, exactly as on a machine with no source tree.
	paths := h.ComputeWatchPaths(nil)
	got := make(map[string]bool, len(paths))
	for _, p := range paths {
		got[p] = true
	}
	for _, want := range []string{wsRoot, filepath.Join(wsRoot, "inbox"), filepath.Join(wsRoot, "plans")} {
		if !got[want] {
			t.Errorf("push-only bare notebook: missing watch for %q (got %v)", want, paths)
		}
	}

	// The watch must resolve to the notespace root, which is what
	// ensurePipelines reads to start the push transport.
	w, rel := h.lookupWatch(filepath.Join(wsRoot, "inbox", "note.md"))
	if w == nil || w.displayName != "bare" || w.notespace != "" || w.root != wsRoot || rel != "inbox/note.md" {
		t.Fatalf("push-only watch lookup before registration: got (%+v, %q), want display=bare and no routing id root=%q rel=inbox/note.md", w, rel, wsRoot)
	}

	// search-only still keeps no local replica, pull or not.
	h.syncCfg = &config.SyncConfig{Workspaces: []config.SyncWorkspace{
		{Name: "bare", Mode: config.SyncModeSearchOnly},
	}}
	if paths := h.ComputeWatchPaths(nil); len(paths) != 0 {
		t.Errorf("search-only push subscription got watches: %v", paths)
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
		{filepath.Join(wsRoot, ".obsidian", "notespace.json"), fsnotify.Write, false},       // manifest
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
	entries, _ := h.database().ListOutbox("testws", 0)
	if len(entries) != 1 || entries[0].EventType != syncproto.EventDocumentCreated {
		t.Fatalf("expected 1 created event, got %+v", entries)
	}
	doc, _ := h.database().GetDocumentByPath("testws", "notes/inbox/idea.md")
	if doc == nil || doc.DocumentID == "" {
		t.Fatalf("expected tracked document, got %+v", doc)
	}

	// Same content: hash-gated, no new outbox row.
	h.flush(ctx, notePath)
	if entries, _ = h.database().ListOutbox("testws", 0); len(entries) != 1 {
		t.Fatalf("hash-gate failed: expected 1 entry, got %d", len(entries))
	}

	// Changed content: document_updated with the SAME document id.
	writeFile(t, notePath, "second draft")
	h.flush(ctx, notePath)
	entries, _ = h.database().ListOutbox("testws", 0)
	if len(entries) != 2 || entries[1].EventType != syncproto.EventDocumentUpdated {
		t.Fatalf("expected created+updated, got %+v", entries)
	}
	if entries[1].DocumentID != doc.DocumentID {
		t.Fatalf("document id changed on update: %q != %q", entries[1].DocumentID, doc.DocumentID)
	}
}

func TestSyncCreatedDirectoryCapturesPreWatchBurst(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 10, 100)
	ctx := context.Background()

	// fsnotify reports the directory creation through the already-watched
	// parent, but it is non-recursive: these children can all exist before the
	// unified watcher gets a chance to install a watch on burst/.
	burstDir := filepath.Join(wsRoot, "quick")
	for i := 0; i < 100; i++ {
		writeFile(t, filepath.Join(burstDir, fmt.Sprintf("flood-%03d.md", i)), "fresh\n")
	}
	if err := h.HandleEvents(ctx, []fsnotify.Event{{Name: burstDir, Op: fsnotify.Create}}); err != nil {
		t.Fatalf("HandleEvents: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, err := h.database().ListOutbox("testws", 0)
		if err != nil {
			t.Fatalf("ListOutbox: %v", err)
		}
		if len(entries) == 100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("directory registration gap lost files: captured %d/100", len(entries))
		}
		time.Sleep(10 * time.Millisecond)
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
		entries, _ := h.database().ListOutbox("testws", 0)
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
	if entries, _ := h.database().ListOutbox("testws", 0); len(entries) != 0 {
		t.Fatalf("quarantined file reached outbox: %+v", entries)
	}
	if doc, _ := h.database().GetDocumentByPath("testws", "notes/leaky.md"); doc != nil {
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
	if entries, _ := h.database().ListOutbox("testws", 0); len(entries) != 1 {
		t.Fatalf("cleaned file did not sync: %+v", entries)
	}
}

func TestSyncDeleteCapture(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "gone.md")
	writeFile(t, notePath, "ephemeral")
	h.flush(ctx, notePath)

	doc, _ := h.database().GetDocumentByPath("testws", "notes/gone.md")
	if doc == nil {
		t.Fatal("expected tracked document before delete")
	}

	if err := os.Remove(notePath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.flush(ctx, notePath)

	entries, _ := h.database().ListOutbox("testws", 0)
	if len(entries) != 2 || entries[1].EventType != syncproto.EventDocumentDeleted {
		t.Fatalf("expected created+deleted, got %+v", entries)
	}
	if entries[1].DocumentID != doc.DocumentID {
		t.Fatalf("delete event has wrong document id: %+v", entries[1])
	}
	if doc, _ := h.database().GetDocumentByPath("testws", "notes/gone.md"); doc != nil {
		t.Fatalf("document still tracked after delete: %+v", doc)
	}

	// Deleting an untracked path records nothing.
	h.flush(ctx, filepath.Join(wsRoot, "notes", "never-existed.md"))
	if entries, _ := h.database().ListOutbox("testws", 0); len(entries) != 2 {
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

	doc, _ := h.database().GetDocumentByPath("testws", "notes/inbox/task.md")
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
			Event:         models.NoteEventMoved,
			NotespaceID:   "test-id",
			NotespaceName: "testws",
			Path:          newPath,
			PrevPath:      oldPath,
		},
	})

	entries, _ := h.database().ListOutbox("testws", 0)
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
	if old, _ := h.database().GetDocumentByPath("testws", "notes/inbox/task.md"); old != nil {
		t.Fatalf("old path still tracked after move: %+v", old)
	}
	cur, _ := h.database().GetDocumentByPath("testws", "notes/current/task.md")
	if cur == nil || cur.DocumentID != doc.DocumentID {
		t.Fatalf("new path not tracked with stable id: %+v", cur)
	}

	// Hash-gate: the follow-up fsnotify create on the new path is a no-op
	// because the content hash is unchanged.
	h.flush(ctx, newPath)
	if entries, _ := h.database().ListOutbox("testws", 0); len(entries) != 2 {
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

	doc, _ := h.database().GetDocumentByPath("testws", "notes/synced.md")
	if doc == nil {
		t.Fatal("expected tracked document before delete")
	}
	// Server confirms the create at version 6 (what the push pipeline records
	// on an accepted push).
	if err := h.database().MarkDocumentSynced(doc.DocumentID, 6, doc.ContentHash, []byte("pushed content")); err != nil {
		t.Fatalf("MarkDocumentSynced: %v", err)
	}

	if err := os.Remove(notePath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.flush(ctx, notePath)

	entries, _ := h.database().ListOutbox("testws", 0)
	if len(entries) != 2 || entries[1].EventType != syncproto.EventDocumentDeleted {
		t.Fatalf("expected created+deleted, got %+v", entries)
	}
	if entries[1].BaseVersion != 6 {
		t.Fatalf("deleted entry must carry the last-synced version 6 as base_version, got %d", entries[1].BaseVersion)
	}
	// The identity-map row is still dropped at enqueue time (keeping it alive
	// would break delete-then-recreate on UNIQUE(notespace, path)).
	if doc, _ := h.database().GetDocumentByPath("testws", "notes/synced.md"); doc != nil {
		t.Fatalf("document still tracked after delete: %+v", doc)
	}
}

func TestEnsurePipelinesRegistersBeforeRoutingAndParksDuplicateID(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	stamp := notespacepkg.NotespaceStamp{ID: id, Name: "display", Subject: "local:01ARZ3NDEKTSV4RRFFQ69G5FAW", Kind: "notes"}
	if err := os.MkdirAll(paths.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	machineTOML := "[primaries]\n\"" + stamp.Subject + "\" = \"01ARZ3NDEKTSV4RRFFQ69G5FAX\"\n"
	if err := os.WriteFile(config.MachineConfigPath(), []byte(machineTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	rootA := filepath.Join(t.TempDir(), "a")
	rootB := filepath.Join(t.TempDir(), "b")
	for _, root := range []string{rootA, rootB} {
		if _, err := notespacepkg.InstallNotespace(root, stamp); err != nil {
			t.Fatal(err)
		}
	}

	registrations := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/register" {
			http.NotFound(w, r)
			return
		}
		registrations++
		var req syncproto.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.ProposedNotespaceID.String() != id || req.Intent != syncproto.RegistrationIntentCreateSibling {
			t.Errorf("registration id=%q intent=%q", req.ProposedNotespaceID, req.Intent)
		}
		_ = json.NewEncoder(w).Encode(syncproto.RegisterResponse{NotespaceID: syncproto.NotespaceID(id)})
	}))
	defer ts.Close()

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := store.New()
	h := NewSyncHandler(st, nil, &config.SyncConfig{Workspaces: []config.SyncWorkspace{{Name: "a"}, {Name: "b"}}}, db, 50, 500)
	h.baseCtx = context.Background()
	h.client = syncdb.NewClient(syncdb.ClientConfig{ServerURL: ts.URL, Token: "fixture", DeviceID: "device", OriginID: "origin"})
	h.watchedPaths = map[string]*syncWatch{
		rootA: {displayName: "a", root: rootA, space: syncdb.NewDocSpace(nil)},
		rootB: {displayName: "b", root: rootB, space: syncdb.NewDocSpace(nil)},
	}
	// Keep the test synchronous: this id is treated as an already-running
	// transport after the registration/duplicate classification boundary.
	h.pipelines[id] = func() {}
	updates := st.Subscribe()
	defer st.Unsubscribe(updates)

	h.ensurePipelines()
	if registrations != 1 {
		t.Fatalf("registrations=%d, want only first root registered", registrations)
	}
	if h.watchedPaths[rootA].notespace != id || h.watchedPaths[rootB].notespace != "" {
		t.Fatalf("routing: first=%q duplicate=%q", h.watchedPaths[rootA].notespace, h.watchedPaths[rootB].notespace)
	}
	select {
	case update := <-updates:
		payload, ok := update.Payload.(*store.SyncConflictPayload)
		if update.Type != store.UpdateSyncConflict || !ok || payload.Kind != syncdb.ConflictKindRegistration || payload.NotespaceID != id {
			t.Fatalf("conflict update=%+v payload=%+v", update, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate registration conflict was not broadcast")
	}
	matches, err := filepath.Glob(filepath.Join(paths.StateDir(), "sync", "conflicts", id, "*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("registration conflict artifact matches=%v err=%v", matches, err)
	}
}
