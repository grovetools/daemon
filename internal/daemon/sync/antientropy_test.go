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
		logging.NewUnifiedLogger("test.antientropy"), AntiEntropyConfig{})
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
