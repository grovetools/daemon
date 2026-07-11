package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

// serveSnapshotStub builds a test server that answers the capabilities
// handshake and serves a fixed snapshot manifest.
func serveSnapshotStub(t *testing.T, manifest *syncproto.SnapshotManifest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sync/capabilities":
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
				Capabilities: syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersion}},
			})
		case "/sync/snapshot":
			_ = json.NewEncoder(w).Encode(manifest)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
}

func newTestAntiEntropy(db *DB, client *Client, root string) *AntiEntropyPass {
	return NewAntiEntropyPass(db, client, "default", root,
		NewDocSpace(nil), logging.NewUnifiedLogger("test.antientropy"), AntiEntropyConfig{})
}

// TestAntiEntropyAdoptRollsMergeBase is the defect-#13 regression: adopting
// server state for a hash-equal file must roll version + last_synced_hash +
// base_content together. The old adopt path went through UpsertDocument,
// whose conflict clause only refreshes content_hash — last_synced_hash stayed
// stale, producing a permanent false-dirty, an hourly self-sustaining
// re-adopt loop, and the stale-merge-base phantom-conflict trap (observed
// live: dev-b stuck dirty at v12 on inbox/20260612-edit-prop-test.md).
func TestAntiEntropyAdoptRollsMergeBase(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	content := []byte("---\ntitle: note\n---\nv12 body\n")
	stale := []byte("---\ntitle: note\n---\nold body\n")
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// Tracked doc: content_hash already matches the server head, but the
	// last-synced fields and merge base are stuck at an older version.
	if err := db.InsertDocument(&Document{
		DocumentID:        "doc-1",
		Workspace:         "default",
		Path:              "inbox/note.md",
		ContentHash:       sha(content),
		LastSyncedHash:    sha(stale),
		LastSyncedVersion: 3,
		BaseContent:       stale,
	}); err != nil {
		t.Fatal(err)
	}

	srv := serveSnapshotStub(t, &syncproto.SnapshotManifest{
		Workspace: "default",
		Cursor:    42,
		Documents: []syncproto.DocumentSnapshot{
			{ID: "doc-1", Path: "inbox/note.md", Version: 12, Hash: sha(content), Size: int64(len(content))},
		},
	})
	defer srv.Close()
	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})

	ae := newTestAntiEntropy(db, client, root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 12 {
		t.Fatalf("expected LastSyncedVersion 12 after adopt, got %d", doc.LastSyncedVersion)
	}
	if doc.LastSyncedHash != sha(content) {
		t.Fatalf("adopt left last_synced_hash stale (permanent false-dirty): got %q", doc.LastSyncedHash)
	}
	if string(doc.BaseContent) != string(content) {
		t.Fatalf("adopt left base_content stale (phantom-conflict trap): got %q", doc.BaseContent)
	}
	if doc.ContentHash != sha(content) {
		t.Fatalf("content_hash regressed: got %q", doc.ContentHash)
	}

	// The false-dirty state self-repaired: the push sweep (which runs after
	// the adopt in the same pass) must NOT re-push the now-clean document,
	// and a second pass must be a quiet no-op (the old code re-adopted the
	// same document every hour, forever).
	if n, err := db.CountOutbox(); err != nil || n != 0 {
		t.Fatalf("expected empty outbox after adopt (no pointless re-push), got %d (err=%v)", n, err)
	}
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if n, err := db.CountOutbox(); err != nil || n != 0 {
		t.Fatalf("expected empty outbox after second pass, got %d (err=%v)", n, err)
	}
}

// TestSweepEnqueuesCreateForOfflineBornDoc is the defect-#14 regression: a
// document created while the daemon was down sits in sync_documents at
// version 0 with last_synced_hash "" and no outbox entry — no watcher event
// will ever come, so without the sweep it is invisible to sync forever
// (observed live: dev-a's inbox/20260612-tmux-e2e.md).
func TestSweepEnqueuesCreateForOfflineBornDoc(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	content := []byte("---\ntitle: born offline\n---\nbody\n")
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "offline.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDocument(&Document{
		DocumentID:  "doc-1",
		Workspace:   "default",
		Path:        "inbox/offline.md",
		ContentHash: sha(content),
		// never synced: version 0, last_synced_hash ""
	}); err != nil {
		t.Fatal(err)
	}

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.sweepLocalDocuments(context.Background()); err != nil {
		t.Fatalf("sweepLocalDocuments: %v", err)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 outbox entry, got %d", len(entries))
	}
	e := entries[0]
	if e.EventType != syncproto.EventDocumentCreated {
		t.Fatalf("expected document_created for never-synced doc, got %s", e.EventType)
	}
	if e.DocumentID != "doc-1" || e.Path != "inbox/offline.md" || e.ContentHash != sha(content) {
		t.Fatalf("unexpected outbox entry: %+v", e)
	}
}

// TestSweepEnqueuesUpdateForDirtyTrackedDoc: an edit made during daemon
// downtime leaves disk hash != last_synced_hash with no outbox entry; the
// sweep enqueues an update and refreshes content_hash from disk.
func TestSweepEnqueuesUpdateForDirtyTrackedDoc(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	base := []byte("---\ntitle: note\n---\nsynced body\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", base) // doc-1, version 1, synced

	// Offline edit: the watcher never saw it, so content_hash is stale too.
	edited := []byte("---\ntitle: note\n---\nsynced body\nedited offline\n")
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), edited, 0o644); err != nil {
		t.Fatal(err)
	}

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.sweepLocalDocuments(context.Background()); err != nil {
		t.Fatalf("sweepLocalDocuments: %v", err)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 outbox entry, got %d", len(entries))
	}
	e := entries[0]
	if e.EventType != syncproto.EventDocumentUpdated {
		t.Fatalf("expected document_updated for previously-synced doc, got %s", e.EventType)
	}
	if e.ContentHash != sha(edited) {
		t.Fatalf("expected outbox hash of disk content, got %q", e.ContentHash)
	}

	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.ContentHash != sha(edited) {
		t.Fatalf("content_hash not refreshed from disk: got %q", doc.ContentHash)
	}
	if doc.LastSyncedHash != sha(base) || doc.LastSyncedVersion != 1 {
		t.Fatalf("sweep must not advance last-synced state (server confirmation only): %q v%d",
			doc.LastSyncedHash, doc.LastSyncedVersion)
	}
}

// TestSweepSkipsDocsWithPendingOutbox: a document with a pending outbox entry
// — a parked conflict awaiting merge, for instance — is off-limits to the
// sweep (head-of-line parking is intentional; the pull pipeline owns the merge).
func TestSweepSkipsDocsWithPendingOutbox(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	base := []byte("---\ntitle: note\n---\nsynced body\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", base)

	edited := []byte("---\ntitle: note\n---\nconflicting local edit\n")
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	// Parked conflict: the entry stays queued until the pull pipeline merges.
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID:  "doc-1",
		Workspace:   "default",
		EventType:   syncproto.EventDocumentUpdated,
		Path:        "inbox/note.md",
		ContentHash: sha(edited),
	}); err != nil {
		t.Fatal(err)
	}

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.sweepLocalDocuments(context.Background()); err != nil {
		t.Fatalf("sweepLocalDocuments: %v", err)
	}

	n, err := db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sweep must not touch docs with pending outbox entries: expected 1 entry, got %d", n)
	}
}

// TestSweepSkipsQuarantinedContent: the sweep applies the same secret
// quarantine gate as the watcher's flush path — quarantined content never
// reaches the outbox via reconciliation either.
func TestSweepSkipsQuarantinedContent(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	secret := []byte("---\ntitle: oops\n---\ntoken: github_pat_11ABCDEFGHIJKLMNOPQRSTUVWXYZ123456\n")
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "secret.md"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDocument(&Document{
		DocumentID:  "doc-1",
		Workspace:   "default",
		Path:        "inbox/secret.md",
		ContentHash: sha(secret),
	}); err != nil {
		t.Fatal(err)
	}

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.sweepLocalDocuments(context.Background()); err != nil {
		t.Fatalf("sweepLocalDocuments: %v", err)
	}

	n, err := db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("quarantined content must never reach the outbox, found %d entries", n)
	}
}

// TestSweepHandlesMissingFiles: a previously-synced document whose file
// vanished while the daemon was down gets a document_deleted event (mirroring
// the watcher's recordDelete); a never-synced row with no file is skipped.
func TestSweepHandlesMissingFiles(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	// Synced doc, file deleted offline.
	if err := db.InsertDocument(&Document{
		DocumentID:        "doc-1",
		Workspace:         "default",
		Path:              "inbox/deleted.md",
		ContentHash:       "aaaa",
		LastSyncedHash:    "aaaa",
		LastSyncedVersion: 4,
		BaseContent:       []byte("gone"),
	}); err != nil {
		t.Fatal(err)
	}
	// Never-synced doc, file gone: nothing to replicate.
	if err := db.InsertDocument(&Document{
		DocumentID:  "doc-2",
		Workspace:   "default",
		Path:        "inbox/ghost.md",
		ContentHash: "bbbb",
	}); err != nil {
		t.Fatal(err)
	}

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.sweepLocalDocuments(context.Background()); err != nil {
		t.Fatalf("sweepLocalDocuments: %v", err)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 delete entry, got %d", len(entries))
	}
	if entries[0].EventType != syncproto.EventDocumentDeleted || entries[0].DocumentID != "doc-1" {
		t.Fatalf("unexpected outbox entry: %+v", entries[0])
	}
	if doc, _ := db.GetDocument("doc-1"); doc != nil {
		t.Fatal("deleted document row should be dropped from the identity map")
	}
}

// writeTreeFile writes content to root/rel, creating parent dirs.
func writeTreeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWalkLocalTreeHydrates: on an empty sync.db, the tree walk enqueues every
// Included file (first-sync = hydration) and never enqueues excluded content
// (.git/, .artifacts/).
func TestWalkLocalTreeHydrates(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	writeTreeFile(t, root, "inbox/a.md", "alpha\n")
	writeTreeFile(t, root, "quick/deep/b.md", "beta\n")
	writeTreeFile(t, root, "concepts/c.md", "gamma\n")
	// Excluded: must never enqueue.
	writeTreeFile(t, root, ".git/config", "[core]\n")
	writeTreeFile(t, root, "plans/p/.artifacts/x.md", "generated\n")
	writeTreeFile(t, root, "inbox/.DS_Store", "junk\n")

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.walkLocalTree(context.Background()); err != nil {
		t.Fatalf("walkLocalTree: %v", err)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Path] = e.EventType
	}
	want := []string{"inbox/a.md", "quick/deep/b.md", "concepts/c.md"}
	if len(entries) != len(want) {
		t.Fatalf("outbox entries = %d (%v), want %d", len(entries), got, len(want))
	}
	for _, p := range want {
		if got[p] != syncproto.EventDocumentCreated {
			t.Fatalf("path %q event = %q, want document_created", p, got[p])
		}
	}
	for excluded := range map[string]bool{".git/config": true, "plans/p/.artifacts/x.md": true, "inbox/.DS_Store": true} {
		if _, ok := got[excluded]; ok {
			t.Fatalf("excluded path %q was enqueued", excluded)
		}
	}
}

// TestWalkLocalTreeSkipsTracked is the create-storm guard: a file that already
// has a sync.db row (a partial sync.db meeting a full tree) is skipped, never
// re-pushed. Drift on tracked docs is sweepLocalDocuments' job.
func TestWalkLocalTreeSkipsTracked(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	writeTreeFile(t, root, "inbox/tracked.md", "already synced\n")
	writeTreeFile(t, root, "inbox/fresh.md", "brand new\n")

	// Pre-seed a row for tracked.md, mimicking a partial sync.db.
	if err := db.UpsertDocument(&Document{
		DocumentID:  "doc-tracked",
		Workspace:   "default",
		Path:        "inbox/tracked.md",
		ContentHash: hashContent([]byte("already synced\n")),
	}); err != nil {
		t.Fatal(err)
	}

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.walkLocalTree(context.Background()); err != nil {
		t.Fatalf("walkLocalTree: %v", err)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox entries = %d, want 1 (only the untracked file)", len(entries))
	}
	if entries[0].Path != "inbox/fresh.md" {
		t.Fatalf("enqueued %q, want inbox/fresh.md (tracked file must be skipped)", entries[0].Path)
	}
}

// TestWalkLocalTreeIdempotent: a second reconcile pass over an unchanged tree
// enqueues nothing new — every file now has a row from the first pass.
func TestWalkLocalTreeIdempotent(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	writeTreeFile(t, root, "inbox/a.md", "alpha\n")
	writeTreeFile(t, root, "quick/b.md", "beta\n")

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.walkLocalTree(context.Background()); err != nil {
		t.Fatalf("first walkLocalTree: %v", err)
	}
	firstCount, err := db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if firstCount != 2 {
		t.Fatalf("after first pass outbox = %d, want 2", firstCount)
	}

	if err := ae.walkLocalTree(context.Background()); err != nil {
		t.Fatalf("second walkLocalTree: %v", err)
	}
	secondCount, err := db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if secondCount != firstCount {
		t.Fatalf("second pass enqueued %d new entries, want 0 (idempotence)", secondCount-firstCount)
	}
}

// TestWalkLocalTreeQuarantines: a secret file is logged + counted but never
// enqueued, and produces no tracked row.
func TestWalkLocalTreeQuarantines(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	writeTreeFile(t, root, "inbox/ok.md", "safe\n")
	writeTreeFile(t, root, "inbox/secret.md", "github_pat_1234567890123456789012\n")

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.walkLocalTree(context.Background()); err != nil {
		t.Fatalf("walkLocalTree: %v", err)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "inbox/ok.md" {
		t.Fatalf("outbox = %+v, want only inbox/ok.md (secret quarantined)", entries)
	}
	if doc, _ := db.GetDocumentByPath("default", "inbox/secret.md"); doc != nil {
		t.Fatal("quarantined secret must not be tracked")
	}

	// Progress registry reflects one quarantine.
	prog := HydrationStatus("default")
	if prog == nil {
		t.Fatal("expected hydration progress to be recorded")
	}
	if prog.Quarantined != 1 {
		t.Fatalf("quarantined count = %d, want 1", prog.Quarantined)
	}
	if prog.Enqueued != 1 {
		t.Fatalf("enqueued count = %d, want 1", prog.Enqueued)
	}
	if prog.Running {
		t.Fatal("hydration should be marked finished after the pass returns")
	}
}

// TestSweepSkipsDivergedDoc: a diverged document's disk hash deliberately
// differs from last_synced_hash (the local file lags the merged server head),
// but the push sweep must NOT re-enqueue it — doing so would clobber the merged
// head (finding-6 livelock). It stays frozen until `nb sync adopt`.
func TestSweepSkipsDivergedDoc(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	local := []byte("---\ntitle: note\n---\nLOCAL body\n")
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	// Diverged doc: content_hash tracks disk (local), last_synced_hash is the
	// merged server head — disk != last_synced, exactly the shape the sweep
	// would normally re-enqueue.
	if err := db.InsertDocument(&Document{
		DocumentID: "doc-1", Workspace: "default", Path: "inbox/note.md",
		ContentHash: sha(local), LastSyncedHash: sha([]byte("merged head")), LastSyncedVersion: 7,
		BaseContent: []byte("merged head"), Diverged: true,
	}); err != nil {
		t.Fatal(err)
	}

	ae := newTestAntiEntropy(db, nil, root)
	if err := ae.sweepLocalDocuments(context.Background()); err != nil {
		t.Fatalf("sweepLocalDocuments: %v", err)
	}
	if n, _ := db.CountOutbox(); n != 0 {
		t.Fatalf("diverged doc must not be enqueued by the sweep, got %d outbox entries", n)
	}
}
