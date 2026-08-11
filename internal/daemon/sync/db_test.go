package sync

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

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
		Notespace:   "ws",
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
		{DocumentID: "d1", Notespace: "ws", Path: "plans/b.md", ContentHash: "h1", LastSyncedHash: "h1"},
		{DocumentID: "d2", Notespace: "ws", Path: "plans/a.md", ContentHash: "dirty", LastSyncedHash: "clean"},
		{DocumentID: "d3", Notespace: "other", Path: "notes/x.md", ContentHash: "h3", LastSyncedHash: "h3"},
	}
	for _, d := range docs {
		if err := db.UpsertDocument(d); err != nil {
			t.Fatalf("UpsertDocument %s: %v", d.DocumentID, err)
		}
	}

	// All notespaces, ordered by notespace then path.
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

	// Notespace filter.
	wsDocs, err := db.ListDocuments("ws")
	if err != nil {
		t.Fatalf("ListDocuments(ws): %v", err)
	}
	if len(wsDocs) != 2 {
		t.Fatalf("expected 2 documents in ws, got %d", len(wsDocs))
	}
	for _, d := range wsDocs {
		if d.Notespace != "ws" {
			t.Fatalf("filter leaked notespace %q", d.Notespace)
		}
	}

	// Empty notespace match returns nothing, not an error.
	none, err := db.ListDocuments("missing")
	if err != nil {
		t.Fatalf("ListDocuments(missing): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no documents, got %d", len(none))
	}
}

func TestUniqueNotespacePathConstraint(t *testing.T) {
	db := openTestDB(t)

	if err := db.UpsertDocument(&Document{DocumentID: "doc-1", Notespace: "ws", Path: "notes/x.md"}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// A different UUID claiming the same (notespace, path) must be rejected.
	if err := db.UpsertDocument(&Document{DocumentID: "doc-2", Notespace: "ws", Path: "notes/x.md"}); err == nil {
		t.Fatal("expected UNIQUE(notespace, path) violation, got nil error")
	}
	// Same path in a different notespace is a distinct document.
	if err := db.UpsertDocument(&Document{DocumentID: "doc-3", Notespace: "other", Path: "notes/x.md"}); err != nil {
		t.Fatalf("cross-notespace upsert: %v", err)
	}
}

func TestOutboxQueue(t *testing.T) {
	db := openTestDB(t)

	for _, e := range []*OutboxEntry{
		{DocumentID: "d1", Notespace: "ws", EventType: syncproto.EventDocumentCreated, Path: "notes/a.md", ContentHash: "h1"},
		{DocumentID: "d1", Notespace: "ws", EventType: syncproto.EventDocumentUpdated, Path: "notes/a.md", ContentHash: "h2"},
		{DocumentID: "d2", Notespace: "other", EventType: syncproto.EventDocumentCreated, Path: "notes/b.md", ContentHash: "h3"},
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
		t.Fatalf("expected nil state for unsynced notespace, got %+v err=%v", st, err)
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
		notespace           TEXT NOT NULL,
		path                TEXT NOT NULL,
		content_hash        TEXT NOT NULL DEFAULT '',
		last_synced_hash    TEXT NOT NULL DEFAULT '',
		last_synced_version INTEGER NOT NULL DEFAULT 0,
		base_content        BLOB,
		updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(notespace, path)
	)`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO sync_documents (document_id, notespace, path) VALUES ('doc-old', 'default', 'inbox/old.md')`); err != nil {
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
		DocumentID: "doc-1", Notespace: "default", Path: "inbox/n.md",
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
		DocumentID: "doc-1", Notespace: "default", Path: "inbox/n.md", ContentHash: "local2",
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
		DocumentID: "doc-1", Notespace: "default", EventType: syncproto.EventDocumentUpdated,
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

// TestOutboxBaseVersionMigration: a sync.db created before the base_version
// column existed (pre-B7) gains it on Open via the idempotent ALTER-swallow
// migration and round-trips the value.
func TestOutboxBaseVersionMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")

	// Hand-create the pre-B7 sync_outbox shape (no base_version).
	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE sync_outbox (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		document_id   TEXT NOT NULL DEFAULT '',
		notespace     TEXT NOT NULL,
		event_type    TEXT NOT NULL,
		path          TEXT NOT NULL,
		prev_path     TEXT NOT NULL DEFAULT '',
		content_hash  TEXT NOT NULL DEFAULT '',
		payload       TEXT NOT NULL DEFAULT '',
		created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		parked        INTEGER NOT NULL DEFAULT 0,
		attempts      INTEGER NOT NULL DEFAULT 0,
		next_retry_at DATETIME,
		park_reason   TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create old-schema table: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO sync_outbox (notespace, event_type, path) VALUES ('default', ?, 'old.md')`,
		syncproto.EventDocumentUpdated); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrating): %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Notespace: "default",
		EventType: syncproto.EventDocumentDeleted, Path: "new.md", BaseVersion: 7,
	}); err != nil {
		t.Fatalf("EnqueueOutbox on migrated db: %v", err)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ListOutbox: got %d entries (err=%v)", len(entries), err)
	}
	if entries[0].BaseVersion != 0 {
		t.Fatalf("pre-migration row must default base_version to 0, got %d", entries[0].BaseVersion)
	}
	if entries[1].BaseVersion != 7 {
		t.Fatalf("base_version did not round-trip on migrated db, got %d", entries[1].BaseVersion)
	}
}

// TestResetForRepush is the recreated-server recovery primitive: every
// NON-diverged document's server-confirmed state is voided (last_synced_*
// zeroed → the sweep re-enqueues it as a create) while its document_id is
// KEPT (stable identities across the recreate), the notespace's obsolete
// outbox entries are dropped, and the pull cursor resets — with diverged
// documents (and their outbox entries, which may carry an unpushed merged
// payload) left strictly untouched.
func TestResetForRepush(t *testing.T) {
	db := openTestDB(t)

	seedDoc := func(id, ws, path string, diverged bool) {
		t.Helper()
		if err := db.InsertDocument(&Document{
			DocumentID: id, Notespace: ws, Path: path,
			ContentHash:    "hash-" + id,
			LastSyncedHash: "synced-" + id, LastSyncedVersion: 7,
			BaseContent: []byte("base"), Diverged: diverged,
		}); err != nil {
			t.Fatalf("InsertDocument %s: %v", id, err)
		}
	}
	seedDoc("doc-1", "default", "inbox/a.md", false)
	seedDoc("doc-2", "default", "inbox/b.md", false)
	seedDoc("doc-3", "default", "inbox/c.md", true) // diverged: untouched
	seedDoc("doc-4", "other", "inbox/d.md", false)  // other notespace: untouched

	// Outbox: a parked update against the dead server (obsolete), plus the
	// diverged doc's entry (must survive — its payload can carry an unpushed
	// merge) and another notespace's entry (out of scope).
	staleID, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Notespace: "default",
		EventType: syncproto.EventDocumentUpdated, Path: "inbox/a.md",
	})
	if err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	if err := db.ParkOutbox(staleID, "rejected", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("ParkOutbox: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-3", Notespace: "default",
		EventType: syncproto.EventDocumentUpdated, Path: "inbox/c.md",
		Payload: "merged bytes",
	}); err != nil {
		t.Fatalf("EnqueueOutbox (diverged): %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-4", Notespace: "other",
		EventType: syncproto.EventDocumentUpdated, Path: "inbox/d.md",
	}); err != nil {
		t.Fatalf("EnqueueOutbox (other ws): %v", err)
	}

	if err := db.SetCursor("default", 42); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}

	n, err := db.ResetForRepush("default")
	if err != nil {
		t.Fatalf("ResetForRepush: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 documents reset, got %d", n)
	}

	// Non-diverged docs: last_synced_* voided, document_id preserved.
	for _, id := range []string{"doc-1", "doc-2"} {
		doc, err := db.GetDocument(id)
		if err != nil || doc == nil {
			t.Fatalf("GetDocument %s: %v", id, err)
		}
		if doc.LastSyncedHash != "" || doc.LastSyncedVersion != 0 {
			t.Fatalf("%s: synced state must be voided, got hash=%q v%d", id, doc.LastSyncedHash, doc.LastSyncedVersion)
		}
		if doc.ContentHash != "hash-"+id {
			t.Fatalf("%s: content_hash must be untouched, got %q", id, doc.ContentHash)
		}
	}

	// Diverged doc and the other notespace's doc: fully untouched.
	for _, id := range []string{"doc-3", "doc-4"} {
		doc, err := db.GetDocument(id)
		if err != nil || doc == nil {
			t.Fatalf("GetDocument %s: %v", id, err)
		}
		if doc.LastSyncedHash != "synced-"+id || doc.LastSyncedVersion != 7 {
			t.Fatalf("%s must be untouched by the reset: hash=%q v%d", id, doc.LastSyncedHash, doc.LastSyncedVersion)
		}
	}

	// Outbox: the notespace's non-diverged entry is gone; the diverged doc's
	// entry and the other notespace's entry survive.
	entries, err := db.ListOutbox("", 0)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 surviving outbox entries, got %d", len(entries))
	}
	for _, e := range entries {
		if e.DocumentID != "doc-3" && e.DocumentID != "doc-4" {
			t.Fatalf("unexpected surviving outbox entry for %s", e.DocumentID)
		}
	}

	// Cursor reset to 0.
	st, err := db.GetState("default")
	if err != nil || st == nil {
		t.Fatalf("GetState: %v", err)
	}
	if st.Cursor != 0 {
		t.Fatalf("cursor must reset to 0, got %d", st.Cursor)
	}

	// The all-notespaces variant sweeps the remaining notespace too. (The
	// idempotent re-touch of default's already-reset rows is fine — the count
	// is diagnostic, the state is what matters.)
	_, notespaces, err := db.ResetForRepushAll()
	if err != nil {
		t.Fatalf("ResetForRepushAll: %v", err)
	}
	if len(notespaces) != 2 {
		t.Fatalf("expected 2 notespaces, got %v", notespaces)
	}
	doc, err := db.GetDocument("doc-4")
	if err != nil || doc == nil {
		t.Fatalf("GetDocument doc-4: %v", err)
	}
	if doc.LastSyncedHash != "" || doc.LastSyncedVersion != 0 {
		t.Fatalf("doc-4 must be reset by the all-notespaces variant: hash=%q v%d", doc.LastSyncedHash, doc.LastSyncedVersion)
	}
}
