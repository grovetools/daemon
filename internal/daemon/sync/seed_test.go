package sync

import (
	"testing"

	"github.com/grovetools/core/pkg/syncproto"
)

// TestInsertAndEnqueueCreate: a genuinely-new path yields a document_created
// event plus a tracked row with the content hash.
func TestInsertAndEnqueueCreate(t *testing.T) {
	db := openTestDB(t)
	content := []byte("# hello\n")

	reason, err := InsertAndEnqueue(db, "default", "inbox/a.md", content)
	if err != nil {
		t.Fatalf("InsertAndEnqueue: %v", err)
	}
	if reason != "" {
		t.Fatalf("unexpected quarantine reason %q", reason)
	}

	doc, err := db.GetDocumentByPath("default", "inbox/a.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: doc=%v err=%v", doc, err)
	}
	if doc.ContentHash != hashContent(content) {
		t.Fatalf("content hash = %q, want %q", doc.ContentHash, hashContent(content))
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox entries = %d, want 1", len(entries))
	}
	if entries[0].EventType != syncproto.EventDocumentCreated {
		t.Fatalf("event type = %q, want %q", entries[0].EventType, syncproto.EventDocumentCreated)
	}
	if entries[0].Path != "inbox/a.md" || entries[0].ContentHash != hashContent(content) {
		t.Fatalf("outbox entry mismatch: %+v", entries[0])
	}
}

// TestInsertAndEnqueueHashEqualNoop: identical content a second time is a
// no-op — no second outbox entry, same document id.
func TestInsertAndEnqueueHashEqualNoop(t *testing.T) {
	db := openTestDB(t)
	content := []byte("stable body\n")

	if _, err := InsertAndEnqueue(db, "default", "quick/n.md", content); err != nil {
		t.Fatalf("first InsertAndEnqueue: %v", err)
	}
	first, _ := db.GetDocumentByPath("default", "quick/n.md")

	if _, err := InsertAndEnqueue(db, "default", "quick/n.md", content); err != nil {
		t.Fatalf("second InsertAndEnqueue: %v", err)
	}
	second, _ := db.GetDocumentByPath("default", "quick/n.md")

	if first.DocumentID != second.DocumentID {
		t.Fatalf("document id changed on no-op: %q -> %q", first.DocumentID, second.DocumentID)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox entries = %d, want 1 (hash-equal must not re-enqueue)", len(entries))
	}
}

// TestInsertAndEnqueueUpdate: changed content on an existing path yields a
// document_updated event keyed on the SAME document id.
func TestInsertAndEnqueueUpdate(t *testing.T) {
	db := openTestDB(t)

	if _, err := InsertAndEnqueue(db, "default", "quick/n.md", []byte("v1\n")); err != nil {
		t.Fatalf("create: %v", err)
	}
	orig, _ := db.GetDocumentByPath("default", "quick/n.md")

	if _, err := InsertAndEnqueue(db, "default", "quick/n.md", []byte("v2\n")); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated, _ := db.GetDocumentByPath("default", "quick/n.md")

	if updated.DocumentID != orig.DocumentID {
		t.Fatalf("document id changed on update: %q -> %q", orig.DocumentID, updated.DocumentID)
	}
	if updated.ContentHash != hashContent([]byte("v2\n")) {
		t.Fatalf("content hash not refreshed: %q", updated.ContentHash)
	}

	entries, _ := db.ListOutbox("default", 0)
	if len(entries) != 2 {
		t.Fatalf("outbox entries = %d, want 2 (create + update)", len(entries))
	}
	if entries[1].EventType != syncproto.EventDocumentUpdated {
		t.Fatalf("second event type = %q, want %q", entries[1].EventType, syncproto.EventDocumentUpdated)
	}
}

// TestInsertAndEnqueueQuarantine: secret content returns the heuristic reason
// and never touches the row or the outbox.
func TestInsertAndEnqueueQuarantine(t *testing.T) {
	db := openTestDB(t)
	secret := []byte("token = github_pat_1234567890123456789012\n")

	reason, err := InsertAndEnqueue(db, "default", "inbox/secret.md", secret)
	if err != nil {
		t.Fatalf("InsertAndEnqueue: %v", err)
	}
	if reason == "" {
		t.Fatal("expected a quarantine reason for secret content")
	}

	doc, _ := db.GetDocumentByPath("default", "inbox/secret.md")
	if doc != nil {
		t.Fatalf("quarantined content must not be tracked, got %+v", doc)
	}
	entries, _ := db.ListOutbox("default", 0)
	if len(entries) != 0 {
		t.Fatalf("quarantined content must not enqueue, got %d entries", len(entries))
	}
}

// TestInsertAndEnqueueQuarantineOverride: an allow-listed secret path bypasses
// the scan and syncs — the watcher/reconcile-agreeing override consulted in
// the shared helper (regression net for cluster Scenario 4).
func TestInsertAndEnqueueQuarantineOverride(t *testing.T) {
	db := openTestDB(t)
	secret := []byte("token = github_pat_1234567890123456789012\n")

	if err := db.SetQuarantineOverride("default", "inbox/secret.md"); err != nil {
		t.Fatalf("SetQuarantineOverride: %v", err)
	}

	reason, err := InsertAndEnqueue(db, "default", "inbox/secret.md", secret)
	if err != nil {
		t.Fatalf("InsertAndEnqueue: %v", err)
	}
	if reason != "" {
		t.Fatalf("allow-listed path must not quarantine, got reason %q", reason)
	}

	doc, _ := db.GetDocumentByPath("default", "inbox/secret.md")
	if doc == nil {
		t.Fatal("allow-listed content should be tracked")
	}
	entries, _ := db.ListOutbox("default", 0)
	if len(entries) != 1 {
		t.Fatalf("allow-listed content should enqueue exactly once, got %d", len(entries))
	}
}

// TestInsertAndEnqueueSkipsDivergedDoc: a diverged document is frozen from BOTH
// producers routed through InsertAndEnqueue (the watcher flush and
// walkLocalTree). New content for a diverged path must NOT enqueue — the local
// file lags the merged server head on purpose and stays frozen until adopt.
func TestInsertAndEnqueueSkipsDivergedDoc(t *testing.T) {
	db := openTestDB(t)
	if err := db.InsertDocument(&Document{
		DocumentID: "doc-1", Workspace: "default", Path: "inbox/n.md",
		ContentHash: hashContent([]byte("old local")), LastSyncedHash: hashContent([]byte("merged head")),
		LastSyncedVersion: 7, Diverged: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Even brand-new content (a fresh local edit) must be dropped while diverged.
	reason, err := InsertAndEnqueue(db, "default", "inbox/n.md", []byte("a newer local edit"))
	if err != nil {
		t.Fatalf("InsertAndEnqueue: %v", err)
	}
	if reason != "" {
		t.Fatalf("unexpected quarantine reason %q", reason)
	}
	if n, _ := db.CountOutbox(); n != 0 {
		t.Fatalf("diverged doc must not enqueue via InsertAndEnqueue, got %d entries", n)
	}
	// content_hash must be untouched (the frozen state).
	doc, _ := db.GetDocumentByPath("default", "inbox/n.md")
	if doc == nil || doc.ContentHash != hashContent([]byte("old local")) {
		t.Fatalf("diverged doc content_hash must not change, got %+v", doc)
	}
}
