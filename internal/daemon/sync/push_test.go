package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

// servePushStub builds a test server that answers the capabilities handshake
// and delegates /sync/push to the given handler.
func servePushStub(t *testing.T, push func(req *syncproto.PushRequest) *syncproto.PushResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/sync/capabilities" {
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
				Capabilities: syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersion}},
			})
			return
		}
		var req syncproto.PushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode push request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(push(&req))
	}))
}

// handshake performs the capabilities exchange the client requires before push.
func handshake(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Capabilities(ctx, "test"); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
}

// TestDrainOutboxConflictTerminates is the hot-loop regression: a conflicted
// entry stays in the outbox by design (the pull pipeline owns the merge), but
// DrainOutbox must return after one no-progress pass instead of refetching
// the same batch forever (observed in the cluster: ~2,300 log lines/sec,
// 2.9GB log file).
func TestDrainOutboxConflictTerminates(t *testing.T) {
	db := openTestDB(t)

	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "inbox", "note.md"), []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID:  "doc-1",
		Workspace:   "default",
		EventType:   syncproto.EventDocumentUpdated,
		Path:        "inbox/note.md",
		ContentHash: "deadbeef",
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	var pushCount atomic.Int64
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		pushCount.Add(1)
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i := range resp.Results {
			resp.Results[i] = syncproto.PushResult{
				Status:     syncproto.PushStatusConflict,
				DocumentID: req.Events[i].DocumentID,
				Version:    7, // server head; client's base is stale
			}
		}
		return resp
	})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	done := make(chan struct{})
	var n int
	var drainErr error
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		n, drainErr = pipeline.DrainOutbox(ctx, workspaceRoot)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DrainOutbox did not terminate with a conflicted outbox entry (hot-loop regression)")
	}

	if drainErr != nil {
		t.Fatalf("DrainOutbox: %v", drainErr)
	}
	if n != 0 {
		t.Fatalf("expected 0 acks, got %d", n)
	}
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 push attempt per drain pass, got %d", got)
	}
	// The conflicted entry must remain queued for a future merge+re-push.
	remaining, err := db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("expected conflicted entry to stay in outbox, found %d entries", remaining)
	}
}

// TestDrainOutboxParksConflictInIsolation is the S3 unit-level assertion: one
// conflicted document is parked (with a backoff) and isolates ONLY itself — the
// other documents in the same batch drain, the drain terminates (no hot loop),
// and a second drain before the backoff expires does not re-attempt the parked
// entry.
func TestDrainOutboxParksConflictInIsolation(t *testing.T) {
	db := openTestDB(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(root, "inbox", name+".md"), []byte("content "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.EnqueueOutbox(&OutboxEntry{
			DocumentID: "doc-" + name, Workspace: "default",
			EventType: syncproto.EventDocumentCreated, Path: "inbox/" + name + ".md", ContentHash: "h" + name,
		}); err != nil {
			t.Fatalf("EnqueueOutbox %s: %v", name, err)
		}
	}

	// Server conflicts inbox/a.md, accepts the rest.
	var pushCount atomic.Int64
	pushedPaths := map[string]int{}
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		pushCount.Add(1)
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i, ev := range req.Events {
			pushedPaths[ev.Path]++
			if ev.Path == "inbox/a.md" {
				resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusConflict, DocumentID: "doc-a", Version: 9}
				continue
			}
			resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: ev.DocumentID, Version: 1}
		}
		return resp
	})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	// Long backoff so the "second drain" below lands well inside the park window.
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{RetryBackoff: time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected the 2 non-conflicted docs to ack, got %d", n)
	}
	// Exactly one push batch — the conflict must not trigger a hot refetch loop.
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 push batch, got %d (hot-loop regression)", got)
	}

	// Only inbox/a.md remains, parked as a conflict with attempts=1.
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected only the conflicted entry to remain, got %d (err=%v)", len(entries), err)
	}
	e := entries[0]
	if e.Path != "inbox/a.md" || !e.Parked || e.ParkReason != "conflict" || e.Attempts != 1 {
		t.Fatalf("unexpected remaining entry: path=%q parked=%v reason=%q attempts=%d",
			e.Path, e.Parked, e.ParkReason, e.Attempts)
	}
	if parked, _ := db.CountOutboxParked(); parked != 1 {
		t.Fatalf("expected 1 parked entry, got %d", parked)
	}
	if !e.NextRetryAt.After(time.Now()) {
		t.Fatalf("parked conflict must have a future next_retry_at, got %v", e.NextRetryAt)
	}

	// Second drain before the backoff expires: the parked entry is skipped, so
	// no additional push happens and it stays parked.
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (second pass): %v", err)
	}
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("parked entry must not be re-pushed before its retry time, got %d push batches", got)
	}
	if remaining, _ := db.CountOutbox(); remaining != 1 {
		t.Fatalf("conflicted entry must stay queued, got %d", remaining)
	}
}

// TestDrainOutboxPopulatesBaseVersion verifies update events carry the
// document's last-synced version as the OCC base — pushing base_version 0
// against a server head manufactures a conflict on every real edit.
func TestDrainOutboxPopulatesBaseVersion(t *testing.T) {
	db := openTestDB(t)

	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "inbox", "note.md"), []byte("v2 content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := db.UpsertDocument(&Document{
		DocumentID:        "doc-1",
		Workspace:         "default",
		Path:              "inbox/note.md",
		ContentHash:       "deadbeef",
		LastSyncedVersion: 6,
		UpdatedAt:         time.Now(),
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		Workspace:   "default",
		EventType:   syncproto.EventDocumentUpdated,
		Path:        "inbox/note.md",
		ContentHash: "deadbeef",
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	var gotBase atomic.Int64
	var gotDocID atomic.Value
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i, ev := range req.Events {
			gotBase.Store(ev.BaseVersion)
			gotDocID.Store(ev.DocumentID)
			resp.Results[i] = syncproto.PushResult{
				Status: syncproto.PushStatusAccepted, DocumentID: "doc-1", Version: ev.BaseVersion + 1,
			}
		}
		return resp
	})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := pipeline.DrainOutbox(ctx, workspaceRoot)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 ack, got %d", n)
	}
	if gotBase.Load() != 6 {
		t.Fatalf("expected base_version 6 from LastSyncedVersion, got %d", gotBase.Load())
	}
	if gotDocID.Load() != "doc-1" {
		t.Fatalf("expected document_id backfilled from sync record, got %v", gotDocID.Load())
	}

	// Accepted push of an existing doc must roll the last-synced state and
	// merge base forward to the pushed content — leaving them stale breaks
	// the pull pipeline's local-dirtiness check on the next remote update.
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 7 {
		t.Fatalf("expected LastSyncedVersion 7 after accepted push, got %d", doc.LastSyncedVersion)
	}
	if doc.LastSyncedHash != "deadbeef" {
		t.Fatalf("expected LastSyncedHash to advance to pushed hash, got %q", doc.LastSyncedHash)
	}
	if string(doc.BaseContent) != "v2 content" {
		t.Fatalf("expected BaseContent to become pushed content, got %q", doc.BaseContent)
	}
}

// TestDrainOutboxPrefersPayloadOverDisk: an update entry that carries its own
// bytes in Payload pushes those bytes, not the disk content (the S5 source: the
// merged rebase result travels as Payload so push never re-reads the file), and
// the local file is left untouched.
func TestDrainOutboxPrefersPayloadOverDisk(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	disk := []byte("DISK BYTES that must not be pushed")
	notePath := filepath.Join(root, "inbox", "note.md")
	if err := os.WriteFile(notePath, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	payload := "PAYLOAD BYTES the entry carries"

	if err := db.UpsertDocument(&Document{
		DocumentID: "doc-1", Workspace: "default", Path: "inbox/note.md",
		ContentHash: sha(disk), LastSyncedHash: sha([]byte("server-base")), LastSyncedVersion: 1,
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Workspace: "default", EventType: syncproto.EventDocumentUpdated,
		Path: "inbox/note.md", ContentHash: sha([]byte(payload)), Payload: payload,
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	var gotContent atomic.Value
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i, ev := range req.Events {
			gotContent.Store(string(ev.Content))
			resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: "doc-1", Version: 2}
		}
		return resp
	})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 ack, got %d", n)
	}
	if gotContent.Load() != payload {
		t.Fatalf("server received %q, want the payload bytes %q", gotContent.Load(), payload)
	}
	got, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(disk) {
		t.Fatalf("payload push must not touch disk: disk = %q", got)
	}
}

// TestDrainOutboxDropsHashEqualNoOp: an update whose push-content hash already
// equals the last-synced hash is a no-op — it is deleted client-side with NO
// push batch (S4: adoption-shaped edits die here instead of round-tripping to
// the server's inline-size rejection).
func TestDrainOutboxDropsHashEqualNoOp(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("identical content on both sides")
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// The doc's last-synced hash already equals the disk hash → pushing is a no-op.
	if err := db.UpsertDocument(&Document{
		DocumentID: "doc-1", Workspace: "default", Path: "inbox/note.md",
		ContentHash: sha(content), LastSyncedHash: sha(content), LastSyncedVersion: 3,
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Workspace: "default", EventType: syncproto.EventDocumentUpdated,
		Path: "inbox/note.md", ContentHash: sha(content),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	var pushCount atomic.Int64
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		pushCount.Add(1)
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i := range resp.Results {
			resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: "doc-1", Version: 4}
		}
		return resp
	})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("a no-op drop must not count as an ack, got %d", n)
	}
	if got := pushCount.Load(); got != 0 {
		t.Fatalf("no-op update must be dropped WITHOUT a push batch, got %d batches", got)
	}
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("no-op entry must be deleted from the outbox, got %d remaining", remaining)
	}
}

// serveRebaseStub builds a test server for the push-side rebase flow: it
// answers the capabilities handshake, delegates /sync/push to push, and
// serves /sync/history/blob from blob (called with the requested version).
// blob may have side effects (mid-rebase edit injection).
func serveRebaseStub(t *testing.T, push func(req *syncproto.PushRequest) *syncproto.PushResponse, blob func(version int64) ([]byte, error)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/capabilities":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
				Capabilities: syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersion}},
			})
		case "/sync/push":
			w.Header().Set("Content-Type", "application/json")
			var req syncproto.PushRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode push request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(push(&req))
		case "/sync/history/blob":
			var version int64
			if _, err := fmt.Sscanf(r.URL.Query().Get("version"), "%d", &version); err != nil {
				t.Errorf("parse blob version: %v", err)
			}
			content, err := blob(version)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			_, _ = w.Write(content)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
}

// seedRebaseScenario sets up the standard rebase fixture: a synced doc at
// version 1 with the given base content, a local on-disk edit, and a parked
// outbox update for it. Returns the workspace root.
func seedRebaseScenario(t *testing.T, db *DB, base, local []byte) string {
	t.Helper()
	root := t.TempDir()
	seedSyncedDoc(t, db, root, "inbox/note.md", base)
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDocument(&Document{
		DocumentID: "doc-1", ContentHash: sha(local),
		LastSyncedHash: sha(base), LastSyncedVersion: 1, BaseContent: base,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID:  "doc-1",
		Workspace:   "default",
		EventType:   syncproto.EventDocumentUpdated,
		Path:        "inbox/note.md",
		ContentHash: sha(local),
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestDrainOutboxRebasesCleanConflict is the S5 unit regression: a Conflict
// whose server head touched a different body region than the local edit is
// rebased WITHOUT writing the workspace file. The merged content travels as the
// entry's Payload and converges on the server; the doc enters the `diverged`
// state; and — the security invariant — the local file is BYTE-IDENTICAL to the
// user's last save at every point in the sequence (sync never writes the tree).
func TestDrainOutboxRebasesCleanConflict(t *testing.T) {
	// Conflict artifacts land under paths.StateDir(); keep them hermetic
	// (GROVE_HOME would shadow the XDG override).
	t.Setenv("GROVE_HOME", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db := openTestDB(t)

	base := []byte("---\ntitle: note\n---\nline one\nline two\nline three\n")
	local := []byte("---\ntitle: note\n---\nLOCAL one\nline two\nline three\n")
	serverHead := []byte("---\ntitle: note\n---\nline one\nline two\nREMOTE three\n")
	root := seedRebaseScenario(t, db, base, local)
	notePath := filepath.Join(root, "inbox", "note.md")

	// S5 invariant helper: the local file must never be written by sync.
	assertLocalUntouched := func(when string) {
		t.Helper()
		got, err := os.ReadFile(notePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(local) {
			t.Fatalf("%s: sync wrote the workspace file (S5 violation): disk=%q want %q", when, got, local)
		}
	}

	var acceptedContent atomic.Value
	var acceptedBase atomic.Int64
	srv := serveRebaseStub(t,
		func(req *syncproto.PushRequest) *syncproto.PushResponse {
			resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
			for i, ev := range req.Events {
				if ev.BaseVersion < 7 {
					resp.Results[i] = syncproto.PushResult{
						Status: syncproto.PushStatusConflict, DocumentID: "doc-1", Version: 7,
					}
					continue
				}
				acceptedContent.Store(string(ev.Content))
				acceptedBase.Store(ev.BaseVersion)
				resp.Results[i] = syncproto.PushResult{
					Status: syncproto.PushStatusAccepted, DocumentID: "doc-1", Version: 8,
				}
			}
			return resp
		},
		func(version int64) ([]byte, error) {
			if version != 7 {
				t.Errorf("expected blob fetch for server head v7, got v%d", version)
			}
			return serverHead, nil
		})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	// A diverged rebase retargets the entry AND parks it with a backoff, so the
	// re-push happens on a later drain (after next_retry_at). Use a short backoff
	// so the test's second drain lands past it without a long sleep.
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{RetryBackoff: 100 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	assertLocalUntouched("before drain")

	// First drain: conflict → rebase (retarget, NO disk write) → park "diverged".
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (rebase pass): %v", err)
	}
	assertLocalUntouched("after rebase pass")

	wantMerged := "---\ntitle: note\n---\nLOCAL one\nline two\nREMOTE three\n"

	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if !doc.Diverged {
		t.Fatal("rebase that merged remote lines must mark the doc diverged")
	}
	if doc.LastSyncedVersion != 7 || doc.LastSyncedHash != sha(serverHead) || string(doc.BaseContent) != string(serverHead) {
		t.Fatalf("rebase must roll the merge base to the server head: v%d hash=%q", doc.LastSyncedVersion, doc.LastSyncedHash)
	}
	// content_hash tracks the DISK file (still `local`), NOT the merged bytes —
	// the local file no longer tracks the push.
	if doc.ContentHash != sha(local) {
		t.Fatalf("content_hash must track the disk file (local hash) for a diverged doc, got %q", doc.ContentHash)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 rebased entry still queued, got %d (err=%v)", len(entries), err)
	}
	if entries[0].ContentHash != sha([]byte(wantMerged)) || entries[0].Payload != wantMerged {
		t.Fatalf("outbox entry not retargeted at merged Payload: hash=%q payload=%q", entries[0].ContentHash, entries[0].Payload)
	}
	// The retargeted entry is parked with reason "diverged" until next_retry_at.
	if !entries[0].Parked || entries[0].ParkReason != "diverged" {
		t.Fatalf("rebased-diverged entry must be parked as diverged, got parked=%v reason=%q",
			entries[0].Parked, entries[0].ParkReason)
	}

	// Wait past the backoff so the parked entry becomes drainable, then re-drain:
	// the merged Payload pushes with the server head (v7) as base_version.
	time.Sleep(150 * time.Millisecond)
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox (re-push pass): %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 ack on re-push, got %d", n)
	}
	assertLocalUntouched("after re-push pass")
	if acceptedBase.Load() != 7 {
		t.Fatalf("re-push must carry the server head as base_version, got %d", acceptedBase.Load())
	}
	if acceptedContent.Load() != wantMerged {
		t.Fatalf("pushed content = %q, want merged %q", acceptedContent.Load(), wantMerged)
	}
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("expected empty outbox after rebased push, got %d", remaining)
	}

	// After the merged head is accepted, the doc stays diverged (the disk file
	// still lags), the merge base rolls to the merged content, and content_hash
	// still tracks the untouched disk file.
	doc, err = db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if !doc.Diverged {
		t.Fatal("doc must remain diverged after the merged head is accepted (disk still lags)")
	}
	if doc.LastSyncedVersion != 8 || doc.LastSyncedHash != sha([]byte(wantMerged)) || string(doc.BaseContent) != wantMerged {
		t.Fatalf("accepted merged head must roll the merge base: v%d hash=%q", doc.LastSyncedVersion, doc.LastSyncedHash)
	}
	if doc.ContentHash != sha(local) {
		t.Fatalf("content_hash must still track the untouched disk file, got %q", doc.ContentHash)
	}

	// No artifact for a clean rebase.
	conflictDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "grove", "sync", "conflicts", "default")
	if _, err := os.Stat(filepath.Join(conflictDir, "inbox/note.md.doc-1.conflict.md")); !os.IsNotExist(err) {
		t.Fatalf("clean rebase must not write a conflict artifact (stat err=%v)", err)
	}
}

// TestDrainOutboxRebaseOverlapParksWithArtifact: overlapping hunks park the
// entry exactly as before and write the conflict artifact once per
// divergence (the entry retries every tick; the artifact must not churn).
func TestDrainOutboxRebaseOverlapParksWithArtifact(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("GROVE_HOME", "")
	t.Setenv("XDG_STATE_HOME", stateHome)
	db := openTestDB(t)

	base := []byte("---\ntitle: note\n---\nline one\nline two\n")
	local := []byte("---\ntitle: note\n---\nLOCAL one\nline two\n")
	serverHead := []byte("---\ntitle: note\n---\nREMOTE one\nline two\n")
	root := seedRebaseScenario(t, db, base, local)

	srv := serveRebaseStub(t,
		func(req *syncproto.PushRequest) *syncproto.PushResponse {
			resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
			for i := range resp.Results {
				resp.Results[i] = syncproto.PushResult{
					Status: syncproto.PushStatusConflict, DocumentID: "doc-1", Version: 7,
				}
			}
			return resp
		},
		func(version int64) ([]byte, error) { return serverHead, nil })
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}

	// Parked exactly as today: entry queued, disk untouched.
	if remaining, _ := db.CountOutbox(); remaining != 1 {
		t.Fatalf("overlap conflict must stay parked, got %d entries", remaining)
	}
	got, err := os.ReadFile(filepath.Join(root, "inbox", "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(local) {
		t.Fatalf("local content must be untouched on overlap, got %q", got)
	}

	artifact := filepath.Join(stateHome, "grove", "sync", "conflicts", "default", "inbox/note.md.doc-1.conflict.md")
	fi1, err := os.Stat(artifact)
	if err != nil {
		t.Fatalf("expected conflict artifact at %s: %v", artifact, err)
	}
	artifactContent, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if string(artifactContent) != string(local) {
		t.Fatalf("artifact must hold the local content, got %q", artifactContent)
	}

	// Second tick: still parked, artifact written once (not rewritten).
	time.Sleep(10 * time.Millisecond)
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (second tick): %v", err)
	}
	if remaining, _ := db.CountOutbox(); remaining != 1 {
		t.Fatalf("entry must remain parked on second tick, got %d", remaining)
	}
	fi2, err := os.Stat(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("conflict artifact rewritten on retry; must be written once per divergence")
	}
}

// TestDrainOutboxRebaseNeverWritesLocalFile: even when the local file changes
// mid-rebase, sync writes NOTHING to the workspace tree (S5). The pre-S5 code
// had a mid-rebase re-read guard whose only purpose was to protect a disk write
// that no longer happens; deleting the write deletes the need for the guard. The
// disk holds exactly the bytes the user (here, the test) wrote — sync never
// overwrote it with the merged content — and the doc goes diverged, held for
// `nb sync adopt`.
func TestDrainOutboxRebaseNeverWritesLocalFile(t *testing.T) {
	t.Setenv("GROVE_HOME", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db := openTestDB(t)

	base := []byte("---\ntitle: note\n---\nline one\nline two\nline three\n")
	local := []byte("---\ntitle: note\n---\nLOCAL one\nline two\nline three\n")
	serverHead := []byte("---\ntitle: note\n---\nline one\nline two\nREMOTE three\n")
	midEdit := []byte("---\ntitle: note\n---\nLOCAL one\nMID EDIT two\nline three\n")
	root := seedRebaseScenario(t, db, base, local)
	notePath := filepath.Join(root, "inbox", "note.md")

	srv := serveRebaseStub(t,
		func(req *syncproto.PushRequest) *syncproto.PushResponse {
			resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
			for i := range resp.Results {
				resp.Results[i] = syncproto.PushResult{
					Status: syncproto.PushStatusConflict, DocumentID: "doc-1", Version: 7,
				}
			}
			return resp
		},
		func(version int64) ([]byte, error) {
			// The blob fetch happens after the rebase snapshots the local file;
			// mutating the file here is a mid-rebase local edit. Sync must still
			// never write the file, so this write is the ONLY writer of it.
			if err := os.WriteFile(notePath, midEdit, 0o644); err != nil {
				return nil, err
			}
			return serverHead, nil
		})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}

	// The S5 invariant: the disk holds exactly the mid-rebase bytes the test
	// wrote — sync did NOT overwrite it with the merged content.
	got, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(midEdit) {
		t.Fatalf("sync wrote the workspace file (S5 violation): disk = %q, want the test's mid-rebase bytes", got)
	}

	// The rebase still ran against the snapshot: doc rolled to the server head
	// and went diverged; the entry carries the merged payload, parked "diverged".
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if !doc.Diverged {
		t.Fatal("rebase must mark the doc diverged")
	}
	if doc.LastSyncedVersion != 7 || string(doc.BaseContent) != string(serverHead) {
		t.Fatalf("rebase must roll the merge base to the server head: v%d", doc.LastSyncedVersion)
	}
	wantMerged := "---\ntitle: note\n---\nLOCAL one\nline two\nREMOTE three\n"
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected the entry to stay parked, got %d (err=%v)", len(entries), err)
	}
	if entries[0].Payload != wantMerged || !entries[0].Parked || entries[0].ParkReason != "diverged" {
		t.Fatalf("entry must carry the merged payload parked as diverged: payload=%q parked=%v reason=%q",
			entries[0].Payload, entries[0].Parked, entries[0].ParkReason)
	}
}

// occDoc is one server-side document head for serveOCCStoreStub.
type occDoc struct {
	id      string
	version int64
}

// serveOCCStoreStub builds a stateful push stub that enforces the server
// store's exact OCC semantics for create/delete/move (sync/pkg/store/sqlite.go
// applyCreate/applyDelete/applyMove): base_version must equal the current head
// version or the event conflicts; deleting an unknown doc is
// idempotent-accepted. docs maps wire path -> head; record (optional) sees
// every pushed event. Requests are serial (DrainOutbox is synchronous), so no
// locking is needed.
func serveOCCStoreStub(t *testing.T, docs map[string]*occDoc, record func(ev syncproto.SyncEvent)) *httptest.Server {
	t.Helper()
	return servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i, ev := range req.Events {
			if record != nil {
				record(ev)
			}
			switch ev.Type {
			case syncproto.EventDocumentCreated:
				doc := &occDoc{id: fmt.Sprintf("doc-%d", len(docs)+1), version: 1}
				docs[ev.Path] = doc
				resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: doc.id, Version: doc.version}
			case syncproto.EventDocumentDeleted:
				doc, ok := docs[ev.Path]
				if !ok {
					// Idempotent: already gone.
					resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: ev.DocumentID}
					continue
				}
				if ev.BaseVersion != doc.version {
					// Stale delete: edit wins over delete.
					resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusConflict, DocumentID: doc.id, Version: doc.version}
					continue
				}
				delete(docs, ev.Path)
				resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: doc.id, Version: doc.version + 1}
			case syncproto.EventDocumentMoved:
				doc, ok := docs[ev.PrevPath]
				if !ok {
					resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusRejected, Error: "unknown document"}
					continue
				}
				if ev.BaseVersion != doc.version {
					resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusConflict, DocumentID: doc.id, Version: doc.version}
					continue
				}
				delete(docs, ev.PrevPath)
				doc.version++
				docs[ev.Path] = doc
				resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: doc.id, Version: doc.version}
			default:
				t.Errorf("occ stub: unexpected event type %q", ev.Type)
			}
		}
		return resp
	})
}

// TestDrainOutboxDeleteCarriesBaseVersion is the B7 push-side regression, run
// as the full create -> push-accept -> delete -> push lifecycle against an
// OCC-faithful fake store: the deleted event must carry the enqueue-time
// base_version (the doc row is gone by drain time — the entry is the only
// carrier) and be ACCEPTED. Before the fix every delete of a server-known doc
// pushed base_version 0 and parked as a conflict permanently.
func TestDrainOutboxDeleteCarriesBaseVersion(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("short-lived note")
	notePath := filepath.Join(root, "inbox", "note.md")
	if err := os.WriteFile(notePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		Workspace: "default", EventType: syncproto.EventDocumentCreated,
		Path: "inbox/note.md", ContentHash: sha(content),
	}); err != nil {
		t.Fatalf("EnqueueOutbox (created): %v", err)
	}

	serverDocs := map[string]*occDoc{}
	var deleteBase atomic.Int64
	deleteBase.Store(-1)
	srv := serveOCCStoreStub(t, serverDocs, func(ev syncproto.SyncEvent) {
		if ev.Type == syncproto.EventDocumentDeleted {
			deleteBase.Store(ev.BaseVersion)
		}
	})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Leg 1: the create is accepted and the client records the server version.
	if n, err := pipeline.DrainOutbox(ctx, root); err != nil || n != 1 {
		t.Fatalf("DrainOutbox (create): n=%d err=%v", n, err)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil || doc.LastSyncedVersion == 0 {
		t.Fatalf("expected synced doc after accepted create, got %+v (err=%v)", doc, err)
	}

	// Leg 2: the delete, enqueued exactly as the watcher's recordDelete does —
	// base captured onto the entry, then the doc row destroyed, file removed.
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: doc.DocumentID, Workspace: "default",
		EventType: syncproto.EventDocumentDeleted, Path: "inbox/note.md",
		BaseVersion: doc.LastSyncedVersion,
	}); err != nil {
		t.Fatalf("EnqueueOutbox (deleted): %v", err)
	}
	if err := db.DeleteDocument(doc.DocumentID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if err := os.Remove(notePath); err != nil {
		t.Fatal(err)
	}

	if n, err := pipeline.DrainOutbox(ctx, root); err != nil || n != 1 {
		t.Fatalf("DrainOutbox (delete): n=%d err=%v — a correct-base delete must be accepted, not parked", n, err)
	}
	if got := deleteBase.Load(); got != doc.LastSyncedVersion {
		t.Fatalf("delete pushed base_version %d, want the last-synced version %d (0 = the B7 bug)", got, doc.LastSyncedVersion)
	}
	if _, ok := serverDocs["inbox/note.md"]; ok {
		t.Fatal("server store still holds the doc — delete did not converge")
	}
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("expected empty outbox after accepted delete, got %d entries", remaining)
	}
}

// TestDrainOutboxDeleteGenuineConflictParks: when the server head has GENUINELY
// moved past the client's last-synced version (another origin edited the doc
// after this client last saw it), the delete must still park with reason
// "conflict" — edit-wins-over-delete is the intended policy, and B7 must not
// turn every stale delete into an accepted one.
func TestDrainOutboxDeleteGenuineConflictParks(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	// Client last saw version 2; the server head is at 5.
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Workspace: "default",
		EventType: syncproto.EventDocumentDeleted, Path: "inbox/note.md",
		BaseVersion: 2,
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	serverDocs := map[string]*occDoc{"inbox/note.md": {id: "doc-1", version: 5}}
	srv := serveOCCStoreStub(t, serverDocs, nil)
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("a genuinely stale delete must not ack, got %d", n)
	}

	// Edit wins over delete: the server doc survives, the entry parks.
	if _, ok := serverDocs["inbox/note.md"]; !ok {
		t.Fatal("server doc was deleted despite the stale base (edit-wins-over-delete violated)")
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected the delete to stay queued, got %d (err=%v)", len(entries), err)
	}
	e := entries[0]
	if !e.Parked || e.ParkReason != "conflict" || e.Attempts != 1 {
		t.Fatalf("stale delete must park as a conflict: parked=%v reason=%q attempts=%d",
			e.Parked, e.ParkReason, e.Attempts)
	}
	if e.BaseVersion != 2 {
		t.Fatalf("parked delete must keep its captured base_version 2, got %d", e.BaseVersion)
	}
}

// TestDrainOutboxMoveCarriesBaseVersion is the B7 move-side regression: a
// moved event must push the doc's last-synced version as base_version
// (resolved at drain time from the doc row, which MoveDocument keeps alive,
// already repointed at the new path) and be accepted by the OCC store. The
// accepted move advances the client's last-synced version without wiping the
// merge base (a move pushes no content).
func TestDrainOutboxMoveCarriesBaseVersion(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	content := []byte("task body")

	// The doc row as handleNoteEvent leaves it: already at the NEW path
	// (MoveDocument ran at enqueue time), synced at version 3.
	if err := db.UpsertDocument(&Document{
		DocumentID: "doc-1", Workspace: "default", Path: "notes/current/task.md",
		ContentHash: sha(content), LastSyncedHash: sha(content),
		LastSyncedVersion: 3, BaseContent: content,
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Workspace: "default",
		EventType: syncproto.EventDocumentMoved,
		Path:      "notes/current/task.md", PrevPath: "notes/inbox/task.md",
		ContentHash: sha(content),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	serverDocs := map[string]*occDoc{"notes/inbox/task.md": {id: "doc-1", version: 3}}
	var moveBase atomic.Int64
	moveBase.Store(-1)
	srv := serveOCCStoreStub(t, serverDocs, func(ev syncproto.SyncEvent) {
		if ev.Type == syncproto.EventDocumentMoved {
			moveBase.Store(ev.BaseVersion)
		}
	})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected the move to be accepted, got %d acks (base 0 = the B7 bug)", n)
	}
	if got := moveBase.Load(); got != 3 {
		t.Fatalf("move pushed base_version %d, want the last-synced version 3", got)
	}
	if doc, ok := serverDocs["notes/current/task.md"]; !ok || doc.version != 4 {
		t.Fatalf("server store not moved to the new path at v4: %+v", serverDocs)
	}
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("expected empty outbox after accepted move, got %d", remaining)
	}

	// Client record: version advanced to the server's post-move head, merge
	// base and hashes untouched (the move carried no content bytes).
	doc, err := db.GetDocumentByPath("default", "notes/current/task.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 4 {
		t.Fatalf("expected LastSyncedVersion 4 after accepted move, got %d", doc.LastSyncedVersion)
	}
	if string(doc.BaseContent) != string(content) || doc.LastSyncedHash != sha(content) {
		t.Fatalf("accepted move must not wipe the merge base: hash=%q base=%q", doc.LastSyncedHash, doc.BaseContent)
	}
}
