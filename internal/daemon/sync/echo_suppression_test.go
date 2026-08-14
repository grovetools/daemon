package sync

// Regression tests for the S2.2 round-trip wedge (2026-08-14, canary A→B):
// the receiving daemon's own pull-apply write was captured by the watcher's
// debounced flush as a local edit — an echo outbox entry that can never push
// (its base is stale by construction) plus v-era bookkeeping stamped over the
// doc row — and the next remote update then false-conflicted against a bogus
// merge base. Cross-machine propagation wedged until manual action.
//
// The fixes under test:
//  1. self-write suppression (selfwrite.go + InsertAndEnqueue): the apply
//     registers what it writes; the shared capture chokepoint drops
//     byte-identical content without touching the row;
//  2. applyCreate adopts in place over an existing row instead of failing
//     UNIQUE(notespace, path) after the file write (half-applied state);
//  3. merge-base verification (repairMergeBase): a base_content that does not
//     hash to last_synced_hash is re-fetched from server history before diff3;
//  4. echo dissolution in the push rebase: a conflicted entry whose local
//     bytes are verbatim the server head or a historical server version is
//     retired instead of parking forever.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

// TestInsertAndEnqueueSuppressesRegisteredSelfWrite covers the registry
// semantics at the capture chokepoint: registered bytes are dropped without
// creating a row or an outbox entry; a different hash supersedes the
// registration; after supersession even the originally-registered bytes flow
// again (a revert is user intent).
func TestInsertAndEnqueueSuppressesRegisteredSelfWrite(t *testing.T) {
	db := openTestDB(t)

	applied := []byte("---\ntitle: note\n---\nserver content\n")
	db.NoteSelfWrite("default", "inbox/note.md", sha(applied))

	reason, err := InsertAndEnqueue(db, "default", "inbox/note.md", applied, time.Now())
	if err != nil || reason != "" {
		t.Fatalf("InsertAndEnqueue(applied): reason=%q err=%v", reason, err)
	}
	if n, _ := db.CountOutbox(); n != 0 {
		t.Fatalf("registered self-write must not enqueue, outbox has %d entries", n)
	}
	if doc, _ := db.GetDocumentByPath("default", "inbox/note.md"); doc != nil {
		t.Fatal("registered self-write must not create a doc row (the apply owns bookkeeping)")
	}

	// A real local edit supersedes the registration and flows normally.
	edit := []byte("---\ntitle: note\n---\nserver content\nlocal line\n")
	if _, err := InsertAndEnqueue(db, "default", "inbox/note.md", edit, time.Now()); err != nil {
		t.Fatalf("InsertAndEnqueue(edit): %v", err)
	}
	if n, _ := db.CountOutbox(); n != 1 {
		t.Fatalf("real edit after self-write must enqueue, outbox has %d entries", n)
	}

	// A revert back to the applied bytes AFTER a real edit is user intent —
	// the stale registration must not swallow it.
	if _, err := InsertAndEnqueue(db, "default", "inbox/note.md", applied, time.Now()); err != nil {
		t.Fatalf("InsertAndEnqueue(revert): %v", err)
	}
	if n, _ := db.CountOutbox(); n != 2 {
		t.Fatalf("revert after supersession must enqueue, outbox has %d entries", n)
	}
}

// TestApplyUpdateFastForwardSuppressesWatcherEcho is the direct S2.2
// regression: the fast-forward apply registers its write, so a watcher flush
// that observes the write while the doc row still holds PRE-APPLY state (the
// flush raced the apply's bookkeeping, or that bookkeeping was lost) neither
// enqueues an echo nor stamps the row. Before the fix this exact sequence
// produced the observed wedge row: content_hash=v2, last_synced_*=v1-era,
// plus a parked base_version-stale echo entry.
func TestApplyUpdateFastForwardSuppressesWatcherEcho(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	v1 := []byte("---\ntitle: note\n---\nbody line\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", v1)

	v2 := []byte("---\ntitle: note\n---\nbody line\n\n")
	p := newTestPullPipeline(t, db)
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentUpdated, NotespaceID: "default",
		DocumentID: "doc-1", Path: "inbox/note.md",
		Content: v2, ContentHash: sha(v2), Version: 2,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	// Simulate the half-applied wedge: the apply's row update is lost — the
	// row reverts to its pre-apply state while disk holds v2.
	preShare := []byte("---\ntitle: note\n---\n")
	if err := db.UpdateDocument(&Document{
		DocumentID: "doc-1", ContentHash: sha(v1),
		LastSyncedHash: sha(v1), LastSyncedVersion: 1, BaseContent: preShare,
	}); err != nil {
		t.Fatal(err)
	}

	// The watcher's debounced flush fires ~2s later and captures disk (v2)
	// against the stale row. It must recognize the apply's own write.
	reason, err := InsertAndEnqueue(db, "default", "inbox/note.md", v2, time.Now())
	if err != nil || reason != "" {
		t.Fatalf("flush capture: reason=%q err=%v", reason, err)
	}
	if n, _ := db.CountOutbox(); n != 0 {
		t.Fatalf("apply echo was enqueued for push-back (the S2.2 wedge), outbox has %d entries", n)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.ContentHash != sha(v1) || doc.LastSyncedVersion != 1 {
		t.Fatalf("suppressed flush must not touch the doc row, got content_hash=%q v%d",
			doc.ContentHash, doc.LastSyncedVersion)
	}

	// A genuine local edit on top of the applied content still flows.
	v4 := []byte("---\ntitle: note\n---\nbody line\n\nuser addition\n")
	if _, err := InsertAndEnqueue(db, "default", "inbox/note.md", v4, time.Now()); err != nil {
		t.Fatalf("InsertAndEnqueue(user edit): %v", err)
	}
	if n, _ := db.CountOutbox(); n != 1 {
		t.Fatalf("real user edit after apply must enqueue, outbox has %d entries", n)
	}
}

// TestApplyCreateOverExistingRowAdoptsInPlace: a document_created event
// landing on a path sync.db already tracks must not fail UNIQUE(notespace,
// path) AFTER writing the file — that leaves disk at the server head with the
// old row's bookkeeping (the half-applied state behind the wedge). It adopts
// the event's identity and synced state in place instead.
func TestApplyCreateOverExistingRowAdoptsInPlace(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	old := []byte("---\ntitle: note\n---\nold body\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", old)

	incoming := []byte("---\ntitle: note\n---\nserver head body\n")
	p := newTestPullPipeline(t, db)
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentCreated, NotespaceID: "default",
		DocumentID: "doc-2", Path: "inbox/note.md",
		Content: incoming, ContentHash: sha(incoming), Version: 5,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent(create over existing row): %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "inbox", "note.md"))
	if err != nil || string(got) != string(incoming) {
		t.Fatalf("disk = %q, want server head (err=%v)", got, err)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.DocumentID != "doc-2" {
		t.Fatalf("row must adopt the event's identity, got %q", doc.DocumentID)
	}
	if doc.LastSyncedVersion != 5 || doc.LastSyncedHash != sha(incoming) ||
		doc.ContentHash != sha(incoming) || string(doc.BaseContent) != string(incoming) {
		t.Fatalf("row must roll to the created head: v%d lsh=%q ch=%q base=%q",
			doc.LastSyncedVersion, doc.LastSyncedHash, doc.ContentHash, doc.BaseContent)
	}

	// And the watcher flush observing the create's write is suppressed.
	if _, err := InsertAndEnqueue(db, "default", "inbox/note.md", incoming, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountOutbox(); n != 0 {
		t.Fatalf("apply-create echo was enqueued, outbox has %d entries", n)
	}
}

// TestApplyUpdateRepairsStaleMergeBase: a doc row whose base_content predates
// its last_synced_hash (the observed wedge row held a pre-share revision
// under a v1 hash) must not feed the bogus ancestor to diff3 — that turns
// disjoint edits into overlapping hunks and refuses the incoming update as a
// false "merge conflict". The true base is re-fetched from server history at
// last_synced_version and the merge composes cleanly.
func TestApplyUpdateRepairsStaleMergeBase(t *testing.T) {
	t.Setenv("GROVE_HOME", "")
	db := openTestDB(t)
	root := t.TempDir()

	trueBase := []byte("---\ntitle: note\n---\nline one\nline two\nline three\n")
	local := []byte("---\ntitle: note\n---\nLOCAL one\nline two\nline three\n")
	remote := []byte("---\ntitle: note\n---\nline one\nline two\nREMOTE three\n")
	bogusBase := []byte("---\ntitle: note\n---\n") // pre-share revision: empty body

	seedSyncedDoc(t, db, root, "inbox/note.md", trueBase)
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	// The wedge row: last_synced points at the true v1, base_content lies.
	if err := db.UpdateDocument(&Document{
		DocumentID: "doc-1", ContentHash: sha(local),
		LastSyncedHash: sha(trueBase), LastSyncedVersion: 1, BaseContent: bogusBase,
	}); err != nil {
		t.Fatal(err)
	}

	srv := serveRebaseStub(t,
		func(req *syncproto.PushRequest) *syncproto.PushResponse {
			t.Error("pull-side merge must not push")
			return &syncproto.PushResponse{}
		},
		func(version int64) ([]byte, error) {
			if version != 1 {
				t.Errorf("expected base fetch for v1, got v%d", version)
			}
			return trueBase, nil
		})
	defer srv.Close()
	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewPullPipeline(&config.SyncWorkspace{Name: "default"}, client, db, logging.NewUnifiedLogger("test.pull"))
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentUpdated, NotespaceID: "default",
		DocumentID: "doc-1", Path: "inbox/note.md",
		Content: remote, ContentHash: sha(remote), Version: 2,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	wantMerged := "---\ntitle: note\n---\nLOCAL one\nline two\nREMOTE three\n"
	got, err := os.ReadFile(filepath.Join(root, "inbox", "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wantMerged {
		t.Fatalf("disjoint edits must merge cleanly over the repaired base:\n got %q\nwant %q", got, wantMerged)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 2 || doc.LastSyncedHash != sha(remote) || string(doc.BaseContent) != string(remote) {
		t.Fatalf("merge must roll to the remote head: v%d lsh=%q", doc.LastSyncedVersion, doc.LastSyncedHash)
	}
	// No conflict artifact for what is now a clean merge.
	conflictDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "grove", "sync", "conflicts", "default")
	if _, err := os.Stat(conflictDir); !os.IsNotExist(err) {
		t.Fatalf("clean repaired merge must not write a conflict artifact (stat err=%v)", err)
	}
}

// seedWedgedEchoScenario builds the observed solom4 state: disk holds server
// v2 verbatim, the doc row lags at v1 with a lying base, and an echo update
// is queued for push-back.
func seedWedgedEchoScenario(t *testing.T, db *DB, disk, trueBase, bogusBase []byte) string {
	t.Helper()
	root := t.TempDir()
	seedSyncedDoc(t, db, root, "inbox/note.md", trueBase)
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), disk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDocument(&Document{
		DocumentID: "doc-1", ContentHash: sha(disk),
		LastSyncedHash: sha(trueBase), LastSyncedVersion: 1, BaseContent: bogusBase,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID:  "doc-1",
		Notespace:   "default",
		EventType:   syncproto.EventDocumentUpdated,
		Path:        "inbox/note.md",
		ContentHash: sha(disk),
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRebaseDissolvesEchoOfServerHistory: a conflicted entry whose local
// content is byte-identical to a HISTORICAL server version (the echoed apply
// of v2, observed parked after 9 stale-base retries) is dissolved — the doc
// row converges on that version so future remote heads fast-forward instead
// of phantom-conflicting, and the entry retires via drain's no-op guard on
// the next pass. The notespace file is never touched (strict push-only).
func TestRebaseDissolvesEchoOfServerHistory(t *testing.T) {
	t.Setenv("GROVE_HOME", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db := openTestDB(t)

	v1 := []byte("---\ntitle: note\n---\nline\n")
	v2 := []byte("---\ntitle: note\n---\nline\nfoo\n")
	v3 := []byte("---\ntitle: note\n---\nline\nbar\n")
	bogusBase := []byte("---\ntitle: note\n---\n")
	root := seedWedgedEchoScenario(t, db, v2, v1, bogusBase)
	notePath := filepath.Join(root, "inbox", "note.md")

	var pushCount atomic.Int64
	srv := serveRebaseStub(t,
		func(req *syncproto.PushRequest) *syncproto.PushResponse {
			pushCount.Add(1)
			resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
			for i := range resp.Results {
				resp.Results[i] = syncproto.PushResult{
					Status: syncproto.PushStatusConflict, DocumentID: "doc-1", Version: 3,
				}
			}
			return resp
		},
		func(version int64) ([]byte, error) {
			switch version {
			case 1:
				return v1, nil
			case 2:
				return v2, nil
			case 3:
				return v3, nil
			}
			t.Errorf("unexpected blob fetch for v%d", version)
			return nil, os.ErrNotExist
		})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{RetryBackoff: 100 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Drain 1: conflict → rebase → overlap over the repaired base → echo
	// probe matches server v2 → dissolution.
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (dissolution pass): %v", err)
	}
	if got, err := os.ReadFile(notePath); err != nil || string(got) != string(v2) {
		t.Fatalf("sync wrote the notespace file (S5 violation): disk=%q err=%v", got, err)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 2 || doc.LastSyncedHash != sha(v2) ||
		doc.ContentHash != sha(v2) || string(doc.BaseContent) != string(v2) {
		t.Fatalf("dissolution must converge the row on matched server v2: v%d lsh=%q ch=%q",
			doc.LastSyncedVersion, doc.LastSyncedHash, doc.ContentHash)
	}
	if doc.Diverged {
		t.Fatal("a dissolved echo is not a divergence")
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected the dissolved entry still parked until its no-op drop, got %d (err=%v)", len(entries), err)
	}
	if entries[0].Payload != string(v2) || entries[0].ContentHash != sha(v2) {
		t.Fatalf("entry must be retargeted at the local bytes: hash=%q", entries[0].ContentHash)
	}
	// No conflict artifact: nothing conflicted, the entry was an echo.
	conflictDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "grove", "sync", "conflicts", "default")
	if _, err := os.Stat(filepath.Join(conflictDir, "inbox", "note.md.doc-1.conflict.md")); !os.IsNotExist(err) {
		t.Fatalf("echo dissolution must not write a conflict artifact (stat err=%v)", err)
	}

	// Drain 2 (past the backoff): the no-op guard deletes the entry without a
	// server round trip.
	time.Sleep(150 * time.Millisecond)
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (retire pass): %v", err)
	}
	if n, _ := db.CountOutbox(); n != 0 {
		t.Fatalf("dissolved echo must retire from the outbox, %d entries remain", n)
	}
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("the echo must never re-push after dissolution, saw %d pushes", got)
	}
}

// TestRebaseDissolvesLocalEqualsHead: the free case — the local file already
// IS the server head; the stale-base conflict dissolves at the head version
// and the doc lands fully synced.
func TestRebaseDissolvesLocalEqualsHead(t *testing.T) {
	t.Setenv("GROVE_HOME", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db := openTestDB(t)

	v1 := []byte("---\ntitle: note\n---\nline\n")
	v3 := []byte("---\ntitle: note\n---\nline\nbar\n")
	bogusBase := []byte("---\ntitle: note\n---\n")
	root := seedWedgedEchoScenario(t, db, v3, v1, bogusBase)

	srv := serveRebaseStub(t,
		func(req *syncproto.PushRequest) *syncproto.PushResponse {
			resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
			for i := range resp.Results {
				resp.Results[i] = syncproto.PushResult{
					Status: syncproto.PushStatusConflict, DocumentID: "doc-1", Version: 3,
				}
			}
			return resp
		},
		func(version int64) ([]byte, error) {
			if version != 3 {
				t.Errorf("head-equal dissolution needs only the head blob, fetched v%d", version)
			}
			return v3, nil
		})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{RetryBackoff: 100 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}

	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 3 || doc.LastSyncedHash != sha(v3) || doc.ContentHash != sha(v3) {
		t.Fatalf("head-equal dissolution must land fully synced at v3, got v%d lsh=%q",
			doc.LastSyncedVersion, doc.LastSyncedHash)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (retire pass): %v", err)
	}
	if n, _ := db.CountOutbox(); n != 0 {
		t.Fatalf("dissolved entry must retire, %d remain", n)
	}
}
