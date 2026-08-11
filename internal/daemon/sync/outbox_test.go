package sync

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/syncproto"
)

// enqueue is a test helper that appends an outbox entry and returns its id.
func enqueue(t *testing.T, db *DB, docID, eventType, path, prevPath string) int64 {
	t.Helper()
	id, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: docID,
		Notespace:  "default",
		EventType:  eventType,
		Path:       path,
		PrevPath:   prevPath,
	})
	if err != nil {
		t.Fatalf("EnqueueOutbox(%s): %v", path, err)
	}
	return id
}

// drainableIDs returns the ids ListOutboxDrainable yields, for order-independent
// set assertions.
func drainableIDs(t *testing.T, db *DB, now time.Time) map[int64]bool {
	t.Helper()
	entries, err := db.ListOutboxDrainable("default", 0, now)
	if err != nil {
		t.Fatalf("ListOutboxDrainable: %v", err)
	}
	out := map[int64]bool{}
	for _, e := range entries {
		out[e.ID] = true
	}
	return out
}

// TestParkOutboxRoundTrip: ParkOutbox persists parked/attempts/next_retry_at/
// park_reason, the entry stays visible to ListOutbox and CountOutbox (a parked
// entry is still unsynced), and CountOutboxParked counts it.
func TestParkOutboxRoundTrip(t *testing.T) {
	db := openTestDB(t)
	id := enqueue(t, db, "doc-x", syncproto.EventDocumentUpdated, "inbox/x.md", "")

	retry := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	if err := db.ParkOutbox(id, "conflict", retry); err != nil {
		t.Fatalf("ParkOutbox: %v", err)
	}

	// Still visible to the total-count and listing (parked IS pending).
	if n, _ := db.CountOutbox(); n != 1 {
		t.Fatalf("CountOutbox = %d, want 1 (parked entries count as pending)", n)
	}
	if n, _ := db.CountOutboxParked(); n != 1 {
		t.Fatalf("CountOutboxParked = %d, want 1", n)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListOutbox must still return the parked row: len=%d err=%v", len(entries), err)
	}
	e := entries[0]
	if !e.Parked || e.Attempts != 1 || e.ParkReason != "conflict" {
		t.Fatalf("parked fields not persisted: parked=%v attempts=%d reason=%q", e.Parked, e.Attempts, e.ParkReason)
	}
	if !e.NextRetryAt.Equal(retry) {
		t.Fatalf("next_retry_at = %v, want %v", e.NextRetryAt, retry)
	}

	// A second park (a repeat conflict) increments attempts → longer backoff.
	if err := db.ParkOutbox(id, "conflict", retry); err != nil {
		t.Fatalf("ParkOutbox (repeat): %v", err)
	}
	entries, _ = db.ListOutbox("default", 0)
	if entries[0].Attempts != 2 {
		t.Fatalf("attempts must increment on re-park, got %d", entries[0].Attempts)
	}
}

// TestListOutboxDrainableRetryWindow: a parked entry whose next_retry_at is in
// the future is skipped; one whose retry time has passed is returned again.
func TestListOutboxDrainableRetryWindow(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()

	future := enqueue(t, db, "doc-future", syncproto.EventDocumentUpdated, "inbox/future.md", "")
	expired := enqueue(t, db, "doc-expired", syncproto.EventDocumentUpdated, "inbox/expired.md", "")
	pending := enqueue(t, db, "doc-pending", syncproto.EventDocumentUpdated, "inbox/pending.md", "")

	if err := db.ParkOutbox(future, "conflict", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.ParkOutbox(expired, "conflict", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := drainableIDs(t, db, now)
	if got[future] {
		t.Error("entry parked into the future must be skipped")
	}
	if !got[expired] {
		t.Error("entry whose retry time has passed must be drainable again")
	}
	if !got[pending] {
		t.Error("an unparked pending entry must be drainable")
	}
}

// TestDrainableBarrierDocID (barrier a): a parked entry blocks a later pending
// entry sharing its document_id, but not an unrelated document.
func TestDrainableBarrierDocID(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()

	first := enqueue(t, db, "doc-x", syncproto.EventDocumentUpdated, "inbox/x.md", "")
	sameDoc := enqueue(t, db, "doc-x", syncproto.EventDocumentUpdated, "inbox/x.md", "")
	otherDoc := enqueue(t, db, "doc-y", syncproto.EventDocumentUpdated, "inbox/y.md", "")

	if err := db.ParkOutbox(first, "conflict", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := drainableIDs(t, db, now)
	if got[first] {
		t.Error("parked entry must not be drained")
	}
	if got[sameDoc] {
		t.Error("a later entry sharing the parked doc's id must be blocked (FIFO per-doc)")
	}
	if !got[otherDoc] {
		t.Error("an unrelated document must keep flowing past a parked doc")
	}
}

// TestDrainableBarrierSamePath (barrier a, path half): a parked delete of doc A
// at path p blocks a later create of a DIFFERENT document B at the same path —
// otherwise the create overtakes the delete and the server applies them
// inverted (create then delete), losing the recreated file. Once the delete is
// gone, the create flows. Different paths/doc-ids remain unblocked throughout.
func TestDrainableBarrierSamePath(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()

	parkedDelete := enqueue(t, db, "doc-a", syncproto.EventDocumentDeleted, "inbox/p.md", "")
	recreate := enqueue(t, db, "doc-b", syncproto.EventDocumentCreated, "inbox/p.md", "")
	unrelated := enqueue(t, db, "doc-c", syncproto.EventDocumentCreated, "inbox/q.md", "")

	if err := db.ParkOutbox(parkedDelete, "conflict", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := drainableIDs(t, db, now)
	if got[parkedDelete] {
		t.Error("parked delete must not be drained")
	}
	if got[recreate] {
		t.Error("a create at the same path must not overtake the parked delete (different doc ids)")
	}
	if !got[unrelated] {
		t.Error("a different path/doc must keep flowing")
	}

	// Once the delete has drained, the create at the same path flows.
	if err := db.DeleteOutbox([]int64{parkedDelete}); err != nil {
		t.Fatal(err)
	}
	got = drainableIDs(t, db, now)
	if !got[recreate] {
		t.Error("the create must flow once the earlier same-path delete is gone")
	}
}

// TestDrainableBarrierPrefixForward (barrier b): a parked prefix op blocks later
// entries under its prefix (matched on both path and prev_path), not siblings
// outside the subtree.
func TestDrainableBarrierPrefixForward(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()

	prefixOp := enqueue(t, db, "", syncproto.EventPrefixMoved, "plans/foo", "")
	underPath := enqueue(t, db, "doc-a", syncproto.EventDocumentUpdated, "plans/foo/01.md", "")
	underPrev := enqueue(t, db, "doc-b", syncproto.EventDocumentMoved, "elsewhere/z.md", "plans/foo/z.md")
	outside := enqueue(t, db, "doc-c", syncproto.EventDocumentUpdated, "inbox/a.md", "")

	if err := db.ParkOutbox(prefixOp, "conflict", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := drainableIDs(t, db, now)
	if got[underPath] {
		t.Error("entry whose path is under a parked prefix op must be blocked")
	}
	if got[underPrev] {
		t.Error("entry whose prev_path is under a parked prefix op must be blocked")
	}
	if !got[outside] {
		t.Error("entry outside the parked prefix subtree must keep flowing")
	}
}

// TestDrainableBarrierPrefixReverse (barrier c): a later prefix op must not
// overtake an earlier parked update that lives inside its subtree.
func TestDrainableBarrierPrefixReverse(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()

	parkedUnder := enqueue(t, db, "doc-a", syncproto.EventDocumentUpdated, "plans/foo/01.md", "")
	prefixOp := enqueue(t, db, "", syncproto.EventPrefixMoved, "plans/foo", "")
	unrelated := enqueue(t, db, "doc-z", syncproto.EventDocumentUpdated, "inbox/z.md", "")

	if err := db.ParkOutbox(parkedUnder, "conflict", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := drainableIDs(t, db, now)
	if got[prefixOp] {
		t.Error("a prefix op must not overtake an earlier parked entry inside its subtree")
	}
	if !got[unrelated] {
		t.Error("an unrelated entry must keep flowing")
	}
}

// TestDrainableBarrierTransitive: a prefix op parks; B is blocked by the prefix
// (not by doc); C shares B's document_id but lives outside the prefix — C must
// still be blocked, transitively, through B.
func TestDrainableBarrierTransitive(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()

	prefixOp := enqueue(t, db, "", syncproto.EventPrefixMoved, "plans/foo", "")
	blockedByPrefix := enqueue(t, db, "doc-b", syncproto.EventDocumentUpdated, "plans/foo/01.md", "")
	sharesBsDoc := enqueue(t, db, "doc-b", syncproto.EventDocumentUpdated, "inbox/elsewhere.md", "")
	unrelated := enqueue(t, db, "doc-d", syncproto.EventDocumentUpdated, "inbox/d.md", "")

	if err := db.ParkOutbox(prefixOp, "conflict", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := drainableIDs(t, db, now)
	if got[blockedByPrefix] {
		t.Error("entry under the parked prefix must be blocked")
	}
	if got[sharesBsDoc] {
		t.Error("entry sharing a transitively-blocked entry's doc must also be blocked")
	}
	if !got[unrelated] {
		t.Error("an unrelated document must keep flowing")
	}
}

// TestDrainableLimit: the limit caps returned drainable entries in FIFO order.
func TestDrainableLimit(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		enqueue(t, db, fmt.Sprintf("doc-%d", i), syncproto.EventDocumentUpdated, fmt.Sprintf("inbox/%d.md", i), "")
	}
	entries, err := db.ListOutboxDrainable("default", 2, now)
	if err != nil {
		t.Fatalf("ListOutboxDrainable: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("limit not honored: got %d entries, want 2", len(entries))
	}
	if entries[0].ID >= entries[1].ID {
		t.Fatalf("entries not FIFO ordered: %d then %d", entries[0].ID, entries[1].ID)
	}
}

// TestMigrateOutboxIdempotent: Open twice on the same file succeeds (the
// ALTER-swallow path), and an OLD DB whose sync_outbox predates the parking
// columns is migrated in place so ParkOutbox works.
func TestMigrateOutboxIdempotent(t *testing.T) {
	// Double-Open: schema + migrateOutbox run twice against the same file.
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

	// Old-shaped DB: create sync_outbox WITHOUT the Phase 4 columns, then Open
	// must ALTER them in so parking works.
	oldPath := filepath.Join(t.TempDir(), "old.db")
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", oldPath)
	raw, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE sync_outbox (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		document_id  TEXT NOT NULL DEFAULT '',
		notespace    TEXT NOT NULL,
		event_type   TEXT NOT NULL,
		path         TEXT NOT NULL,
		prev_path    TEXT NOT NULL DEFAULT '',
		content_hash TEXT NOT NULL DEFAULT '',
		payload      TEXT NOT NULL DEFAULT '',
		created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO sync_outbox (document_id, notespace, event_type, path) VALUES ('doc-old', 'default', ?, 'inbox/old.md')`, syncproto.EventDocumentUpdated); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	_ = raw.Close()

	db, err := Open(oldPath)
	if err != nil {
		t.Fatalf("Open old DB (must migrate): %v", err)
	}
	defer db.Close()

	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("old row not readable after migration: len=%d err=%v", len(entries), err)
	}
	if entries[0].Parked {
		t.Fatalf("migrated row must default parked=false")
	}
	if err := db.ParkOutbox(entries[0].ID, "conflict", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("ParkOutbox after migration: %v", err)
	}
	if n, _ := db.CountOutboxParked(); n != 1 {
		t.Fatalf("parked count after migration = %d, want 1", n)
	}
}
