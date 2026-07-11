package sync

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/syncproto"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOriginIDPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	origin := db.OriginID()
	if origin == "" {
		t.Fatal("expected non-empty origin id")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if db2.OriginID() != origin {
		t.Fatalf("origin id changed across reopen: %q != %q", db2.OriginID(), origin)
	}
}

func TestDocumentLifecycle(t *testing.T) {
	db := openTestDB(t)

	doc := &Document{
		DocumentID:  "doc-1",
		Workspace:   "ws",
		Path:        "plans/a.md",
		ContentHash: "hash1",
	}
	if err := db.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}

	got, err := db.GetDocumentByPath("ws", "plans/a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if got == nil || got.DocumentID != "doc-1" || got.ContentHash != "hash1" {
		t.Fatalf("unexpected document: %+v", got)
	}

	// Update content hash for the same UUID; last-synced fields must survive
	// (they only advance on server confirmation).
	doc.ContentHash = "hash2"
	if err := db.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument update: %v", err)
	}
	got, _ = db.GetDocumentByPath("ws", "plans/a.md")
	if got.ContentHash != "hash2" {
		t.Fatalf("content hash not updated: %+v", got)
	}

	// Rename: same UUID, new path.
	if err := db.MoveDocument("doc-1", "plans/b.md"); err != nil {
		t.Fatalf("MoveDocument: %v", err)
	}
	if got, _ := db.GetDocumentByPath("ws", "plans/a.md"); got != nil {
		t.Fatalf("old path still resolves after move: %+v", got)
	}
	got, _ = db.GetDocumentByPath("ws", "plans/b.md")
	if got == nil || got.DocumentID != "doc-1" {
		t.Fatalf("new path does not resolve after move: %+v", got)
	}

	if err := db.DeleteDocument("doc-1"); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if n, _ := db.CountDocuments(); n != 0 {
		t.Fatalf("expected 0 documents after delete, got %d", n)
	}
}

func TestListDocuments(t *testing.T) {
	db := openTestDB(t)

	docs := []*Document{
		{DocumentID: "d1", Workspace: "ws", Path: "plans/b.md", ContentHash: "h1", LastSyncedHash: "h1"},
		{DocumentID: "d2", Workspace: "ws", Path: "plans/a.md", ContentHash: "dirty", LastSyncedHash: "clean"},
		{DocumentID: "d3", Workspace: "other", Path: "notes/x.md", ContentHash: "h3", LastSyncedHash: "h3"},
	}
	for _, d := range docs {
		if err := db.UpsertDocument(d); err != nil {
			t.Fatalf("UpsertDocument %s: %v", d.DocumentID, err)
		}
	}

	// All workspaces, ordered by workspace then path.
	all, err := db.ListDocuments("")
	if err != nil {
		t.Fatalf("ListDocuments(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 documents, got %d", len(all))
	}
	wantOrder := []string{"d3", "d2", "d1"} // other/notes/x.md, ws/plans/a.md, ws/plans/b.md
	for i, want := range wantOrder {
		if all[i].DocumentID != want {
			t.Fatalf("order mismatch at %d: got %s want %s", i, all[i].DocumentID, want)
		}
	}

	// Workspace filter.
	wsDocs, err := db.ListDocuments("ws")
	if err != nil {
		t.Fatalf("ListDocuments(ws): %v", err)
	}
	if len(wsDocs) != 2 {
		t.Fatalf("expected 2 documents in ws, got %d", len(wsDocs))
	}
	for _, d := range wsDocs {
		if d.Workspace != "ws" {
			t.Fatalf("filter leaked workspace %q", d.Workspace)
		}
	}

	// Empty workspace match returns nothing, not an error.
	none, err := db.ListDocuments("missing")
	if err != nil {
		t.Fatalf("ListDocuments(missing): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no documents, got %d", len(none))
	}
}

func TestUniqueWorkspacePathConstraint(t *testing.T) {
	db := openTestDB(t)

	if err := db.UpsertDocument(&Document{DocumentID: "doc-1", Workspace: "ws", Path: "notes/x.md"}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// A different UUID claiming the same (workspace, path) must be rejected.
	if err := db.UpsertDocument(&Document{DocumentID: "doc-2", Workspace: "ws", Path: "notes/x.md"}); err == nil {
		t.Fatal("expected UNIQUE(workspace, path) violation, got nil error")
	}
	// Same path in a different workspace is a distinct document.
	if err := db.UpsertDocument(&Document{DocumentID: "doc-3", Workspace: "other", Path: "notes/x.md"}); err != nil {
		t.Fatalf("cross-workspace upsert: %v", err)
	}
}

func TestOutboxQueue(t *testing.T) {
	db := openTestDB(t)

	for _, e := range []*OutboxEntry{
		{DocumentID: "d1", Workspace: "ws", EventType: syncproto.EventDocumentCreated, Path: "notes/a.md", ContentHash: "h1"},
		{DocumentID: "d1", Workspace: "ws", EventType: syncproto.EventDocumentUpdated, Path: "notes/a.md", ContentHash: "h2"},
		{DocumentID: "d2", Workspace: "other", EventType: syncproto.EventDocumentCreated, Path: "notes/b.md", ContentHash: "h3"},
	} {
		if _, err := db.EnqueueOutbox(e); err != nil {
			t.Fatalf("EnqueueOutbox: %v", err)
		}
	}

	all, err := db.ListOutbox("", 0)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 outbox entries, got %d", len(all))
	}
	if all[0].EventType != syncproto.EventDocumentCreated || all[1].EventType != syncproto.EventDocumentUpdated {
		t.Fatalf("outbox not in insertion order: %+v", all)
	}

	wsOnly, err := db.ListOutbox("ws", 0)
	if err != nil {
		t.Fatalf("ListOutbox(ws): %v", err)
	}
	if len(wsOnly) != 2 {
		t.Fatalf("expected 2 ws entries, got %d", len(wsOnly))
	}

	if err := db.DeleteOutbox([]int64{all[0].ID, all[1].ID}); err != nil {
		t.Fatalf("DeleteOutbox: %v", err)
	}
	if n, _ := db.CountOutbox(); n != 1 {
		t.Fatalf("expected 1 entry after ack, got %d", n)
	}
}

func TestCursorState(t *testing.T) {
	db := openTestDB(t)

	if st, err := db.GetState("ws"); err != nil || st != nil {
		t.Fatalf("expected nil state for unsynced workspace, got %+v err=%v", st, err)
	}

	if err := db.SetCursor("ws", 42); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	st, err := db.GetState("ws")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if st.Cursor != 42 || st.OriginID != db.OriginID() {
		t.Fatalf("unexpected state: %+v", st)
	}
	if st.LastSyncedAt.IsZero() {
		t.Fatal("expected last_synced_at to be stamped")
	}

	if err := db.SetCursor("ws", 99); err != nil {
		t.Fatalf("SetCursor advance: %v", err)
	}
	st, _ = db.GetState("ws")
	if st.Cursor != 99 {
		t.Fatalf("cursor not advanced: %+v", st)
	}

	states, err := db.ListStates()
	if err != nil || len(states) != 1 {
		t.Fatalf("ListStates: %v (%d states)", err, len(states))
	}
}

// TestMigrateDocumentsIdempotent: Open twice succeeds (the ALTER-swallow path),
// and an OLD DB whose sync_documents predates the diverged column is migrated in
// place so MarkDiverged works.
func TestMigrateDocumentsIdempotent(t *testing.T) {
	// Double-Open: schema + migrateDocuments run twice against the same file.
	path := filepath.Join(t.TempDir(), "sync.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = db1.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (migration must be idempotent): %v", err)
	}
	defer db2.Close()

	// Old-shaped DB: sync_documents WITHOUT the diverged column, then Open must
	// ALTER it in so the diverged flag works.
	oldPath := filepath.Join(t.TempDir(), "old.db")
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", oldPath)
	raw, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE sync_documents (
		document_id         TEXT PRIMARY KEY,
		workspace           TEXT NOT NULL,
		path                TEXT NOT NULL,
		content_hash        TEXT NOT NULL DEFAULT '',
		last_synced_hash    TEXT NOT NULL DEFAULT '',
		last_synced_version INTEGER NOT NULL DEFAULT 0,
		base_content        BLOB,
		updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(workspace, path)
	)`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO sync_documents (document_id, workspace, path) VALUES ('doc-old', 'default', 'inbox/old.md')`); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	_ = raw.Close()

	db, err := Open(oldPath)
	if err != nil {
		t.Fatalf("Open old DB (must migrate): %v", err)
	}
	defer db.Close()

	doc, err := db.GetDocumentByPath("default", "inbox/old.md")
	if err != nil || doc == nil {
		t.Fatalf("old row not readable after migration: err=%v", err)
	}
	if doc.Diverged {
		t.Fatal("migrated row must default diverged=false")
	}
	if err := db.MarkDiverged("doc-old"); err != nil {
		t.Fatalf("MarkDiverged after migration: %v", err)
	}
	if n, _ := db.CountDocumentsDiverged(); n != 1 {
		t.Fatalf("diverged count after migration = %d, want 1", n)
	}
}

// TestMarkClearDiverged round-trips the diverged flag and its count, and
// verifies UpsertDocument's ON CONFLICT never touches diverged (only explicit
// calls do) while AdoptDocument clears it.
func TestMarkClearDiverged(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertDocument(&Document{
		DocumentID: "doc-1", Workspace: "default", Path: "inbox/n.md",
		ContentHash: "local", LastSyncedHash: "server", LastSyncedVersion: 2, BaseContent: []byte("base"),
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}

	if err := db.MarkDiverged("doc-1"); err != nil {
		t.Fatalf("MarkDiverged: %v", err)
	}
	doc, _ := db.GetDocumentByPath("default", "inbox/n.md")
	if doc == nil || !doc.Diverged {
		t.Fatalf("expected diverged=true, got %+v", doc)
	}
	if n, _ := db.CountDocumentsDiverged(); n != 1 {
		t.Fatalf("CountDocumentsDiverged = %d, want 1", n)
	}

	// A content-tracking UpsertDocument (ON CONFLICT) must NOT clear diverged.
	if err := db.UpsertDocument(&Document{
		DocumentID: "doc-1", Workspace: "default", Path: "inbox/n.md", ContentHash: "local2",
	}); err != nil {
		t.Fatalf("UpsertDocument (conflict): %v", err)
	}
	doc, _ = db.GetDocumentByPath("default", "inbox/n.md")
	if doc == nil || !doc.Diverged {
		t.Fatal("UpsertDocument ON CONFLICT must preserve diverged")
	}

	if err := db.ClearDiverged("doc-1"); err != nil {
		t.Fatalf("ClearDiverged: %v", err)
	}
	doc, _ = db.GetDocumentByPath("default", "inbox/n.md")
	if doc == nil || doc.Diverged {
		t.Fatalf("expected diverged=false after ClearDiverged, got %+v", doc)
	}
	if n, _ := db.CountDocumentsDiverged(); n != 0 {
		t.Fatalf("CountDocumentsDiverged = %d, want 0", n)
	}

	// AdoptDocument also clears diverged as part of rolling the merge base.
	if err := db.MarkDiverged("doc-1"); err != nil {
		t.Fatalf("MarkDiverged (2): %v", err)
	}
	if err := db.AdoptDocument("default", "inbox/n.md", "doc-1", 5, "head", []byte("head content")); err != nil {
		t.Fatalf("AdoptDocument: %v", err)
	}
	doc, _ = db.GetDocumentByPath("default", "inbox/n.md")
	if doc == nil || doc.Diverged {
		t.Fatal("AdoptDocument must clear diverged")
	}
}

// TestUpdateOutboxEntryContent retargets a single entry's payload + hash by id
// without disturbing sibling entries.
func TestUpdateOutboxEntryContent(t *testing.T) {
	db := openTestDB(t)
	id, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Workspace: "default", EventType: syncproto.EventDocumentUpdated,
		Path: "inbox/n.md", ContentHash: "old",
	})
	if err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	if err := db.UpdateOutboxEntryContent(id, "merged bytes", "newhash"); err != nil {
		t.Fatalf("UpdateOutboxEntryContent: %v", err)
	}
	entries, _ := db.ListOutbox("default", 0)
	if len(entries) != 1 || entries[0].Payload != "merged bytes" || entries[0].ContentHash != "newhash" {
		t.Fatalf("entry not retargeted: %+v", entries)
	}
}
