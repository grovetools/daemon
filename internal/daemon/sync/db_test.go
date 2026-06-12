package sync

import (
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
