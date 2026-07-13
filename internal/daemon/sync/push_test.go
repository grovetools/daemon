package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// The pushed hash is recomputed from the disk bytes at drain time (B9),
	// so the enqueue-time "deadbeef" placeholder never reaches the wire.
	if want := sha([]byte("v2 content")); doc.LastSyncedHash != want {
		t.Fatalf("expected LastSyncedHash to advance to pushed hash %q, got %q", want, doc.LastSyncedHash)
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
	hash    string
	content []byte
}

// serveOCCStoreStub builds a stateful push stub that enforces the server
// store's exact OCC semantics for create/update/delete/move
// (sync/pkg/store/sqlite.go applyUpsert/applyDelete/applyMove): base_version
// must equal the current head version or the event conflicts; deleting an
// unknown doc is idempotent-accepted; a create — or an update under an
// unknown document id — landing on an OCCUPIED path answers with the existing
// identity (hash-equal idempotent accept, structured conflict otherwise, B8)
// instead of a reject. It also serves /sync/history/blob from the current
// heads so the push-side rebase can shadow-read. docs maps wire path -> head;
// record (optional) sees every pushed event. Requests are serial (DrainOutbox
// is synchronous), so no locking is needed.
func serveOCCStoreStub(t *testing.T, docs map[string]*occDoc, record func(ev syncproto.SyncEvent)) *httptest.Server {
	t.Helper()
	apply := func(req *syncproto.PushRequest) *syncproto.PushResponse {
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i, ev := range req.Events {
			if record != nil {
				record(ev)
			}
			switch ev.Type {
			case syncproto.EventDocumentCreated:
				if doc, ok := docs[ev.Path]; ok {
					// Occupied path (B8): existing identity, never a reject.
					if ev.ContentHash == doc.hash {
						resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: doc.id, Version: doc.version}
					} else {
						resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusConflict, DocumentID: doc.id, Version: doc.version}
					}
					continue
				}
				// Mirror applyUpsert's create branch: a pushed document id is
				// KEPT (stable ids across a server recreate); only an id-less
				// create mints one.
				id := ev.DocumentID
				if id == "" {
					id = fmt.Sprintf("doc-%d", len(docs)+1)
				}
				doc := &occDoc{
					id: id, version: 1,
					hash: ev.ContentHash, content: append([]byte(nil), ev.Content...),
				}
				docs[ev.Path] = doc
				resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: doc.id, Version: doc.version}
			case syncproto.EventDocumentUpdated:
				var doc *occDoc
				if ev.DocumentID != "" {
					for _, d := range docs {
						if d.id == ev.DocumentID {
							doc = d
							break
						}
					}
					if doc == nil {
						// Unknown id at a live path (B8): answer with the
						// real identity so the client can re-map.
						if d, ok := docs[ev.Path]; ok {
							if ev.ContentHash == d.hash {
								resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: d.id, Version: d.version}
							} else {
								resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusConflict, DocumentID: d.id, Version: d.version}
							}
							continue
						}
					}
				} else if d, ok := docs[ev.Path]; ok {
					doc = d
				}
				if doc == nil {
					resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusRejected, Error: "unknown document"}
					continue
				}
				if ev.BaseVersion != doc.version {
					if ev.ContentHash == doc.hash {
						// Stale base, identical content: absorbed.
						resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusAccepted, DocumentID: doc.id, Version: doc.version}
					} else {
						resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusConflict, DocumentID: doc.id, Version: doc.version}
					}
					continue
				}
				doc.version++
				doc.hash = ev.ContentHash
				doc.content = append([]byte(nil), ev.Content...)
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
	}
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
			_ = json.NewEncoder(w).Encode(apply(&req))
		case "/sync/history/blob":
			id := r.URL.Query().Get("document_id")
			var version int64
			if _, err := fmt.Sscanf(r.URL.Query().Get("version"), "%d", &version); err != nil {
				t.Errorf("parse blob version: %v", err)
			}
			for _, d := range docs {
				if d.id == id && d.version == version {
					_, _ = w.Write(d.content)
					return
				}
			}
			http.Error(w, "version not found", http.StatusNotFound)
		default:
			t.Errorf("occ stub: unexpected request %s", r.URL.Path)
		}
	}))
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

// seedOrphanRecreate builds the B8 fixture, observed live: the server holds a
// live doc at the path (serverDocs seeded by the caller) while the client has
// LOST its record of it — the watcher then recreated the file and minted a
// fresh local document id. Returns the workspace root.
func seedOrphanRecreate(t *testing.T, db *DB, localID, eventType string, local []byte) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	// The watcher's InsertAndEnqueue outcome: a fresh row with a minted id,
	// never synced (version 0, no base), plus the queued event.
	if err := db.UpsertDocument(&Document{
		DocumentID: localID, Workspace: "default", Path: "inbox/note.md",
		ContentHash: sha(local),
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: localID, Workspace: "default", EventType: eventType,
		Path: "inbox/note.md", ContentHash: sha(local),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	return root
}

// TestDrainOutboxCreateAtOrphanedPathHashEqualAdoptsIdentity is the B8
// hash-equal leg: the recreated file matches the orphaned server doc byte for
// byte, so the create is absorbed as an idempotent accept carrying the
// server's identity, and the client re-maps its minted id onto it. Before the
// fix this was a raw UNIQUE-constraint Rejected and the entry was silently
// deleted — the path never synced again.
func TestDrainOutboxCreateAtOrphanedPathHashEqualAdoptsIdentity(t *testing.T) {
	db := openTestDB(t)
	content := []byte("---\ntitle: note\n---\nsame on both sides\n")
	root := seedOrphanRecreate(t, db, "doc-local-2", syncproto.EventDocumentCreated, content)

	serverDocs := map[string]*occDoc{
		"inbox/note.md": {id: "doc-server-1", version: 1, hash: sha(content), content: content},
	}
	srv := serveOCCStoreStub(t, serverDocs, nil)
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil || n != 1 {
		t.Fatalf("DrainOutbox: n=%d err=%v — a hash-equal recreate must be absorbed, not rejected", n, err)
	}

	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.DocumentID != "doc-server-1" {
		t.Fatalf("local row must adopt the server identity, got %q", doc.DocumentID)
	}
	if doc.LastSyncedVersion != 1 || doc.LastSyncedHash != sha(content) || string(doc.BaseContent) != string(content) {
		t.Fatalf("adopted row must be fully synced at the server head: v%d hash=%q", doc.LastSyncedVersion, doc.LastSyncedHash)
	}
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("expected empty outbox, got %d", remaining)
	}
	if d := serverDocs["inbox/note.md"]; d.version != 1 {
		t.Fatalf("idempotent absorb must not advance the server head, got v%d", d.version)
	}
}

// TestDrainOutboxCreateAtOrphanedPathMergeableConverges is the B8 lifecycle:
// the recreated file shares its body with the orphaned server doc but carries
// extra frontmatter. The conflicted create adopts the server identity, the
// rebase composes cleanly over the empty base, and the retyped
// document_updated re-push advances the server head with the merged content —
// full convergence, no operator involvement.
func TestDrainOutboxCreateAtOrphanedPathMergeableConverges(t *testing.T) {
	t.Setenv("GROVE_HOME", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db := openTestDB(t)

	serverHead := []byte("---\ntitle: note\n---\nshared body\n")
	local := []byte("---\ntitle: note\nstatus: new\n---\nshared body\n")
	root := seedOrphanRecreate(t, db, "doc-local-2", syncproto.EventDocumentCreated, local)

	serverDocs := map[string]*occDoc{
		"inbox/note.md": {id: "doc-server-1", version: 1, hash: sha(serverHead), content: serverHead},
	}
	srv := serveOCCStoreStub(t, serverDocs, nil)
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{RetryBackoff: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Drain 1: conflict -> adopt identity + clean rebase -> parked for re-push.
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (conflict pass): %v", err)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.DocumentID != "doc-server-1" {
		t.Fatalf("local row must adopt the server identity, got %q", doc.DocumentID)
	}
	if doc.LastSyncedVersion != 1 || string(doc.BaseContent) != string(serverHead) {
		t.Fatalf("rebase must roll the merge base to the server head: v%d", doc.LastSyncedVersion)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 parked entry, got %d (err=%v)", len(entries), err)
	}
	if entries[0].EventType != syncproto.EventDocumentUpdated || entries[0].DocumentID != "doc-server-1" {
		t.Fatalf("conflicted create must be retyped to an update under the server id, got %q/%q",
			entries[0].EventType, entries[0].DocumentID)
	}
	if !entries[0].Parked {
		t.Fatal("rebased entry must be parked for the backoff re-push")
	}

	// Drain 2 (past the backoff): merged content pushes as an update on the
	// server head — the server version advances and content converges.
	time.Sleep(120 * time.Millisecond)
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (re-push pass): %v", err)
	}
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("expected empty outbox after convergence, got %d", remaining)
	}
	d := serverDocs["inbox/note.md"]
	if d.id != "doc-server-1" || d.version != 2 {
		t.Fatalf("server head must advance under the same identity: %+v", d)
	}
	merged := string(d.content)
	if merged != string(local) {
		// The merge must carry both sides regardless of exact serialization.
		for _, want := range []string{"status: new", "shared body", "title: note"} {
			if !strings.Contains(merged, want) {
				t.Fatalf("merged server head missing %q: %q", want, merged)
			}
		}
	}
	doc, err = db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 2 || doc.LastSyncedHash != d.hash {
		t.Fatalf("client must record the advanced head: v%d hash=%q want v2 %q",
			doc.LastSyncedVersion, doc.LastSyncedHash, d.hash)
	}
	// S5: the workspace file was never written.
	got, err := os.ReadFile(filepath.Join(root, "inbox", "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(local) {
		t.Fatalf("sync wrote the workspace file (S5 violation): %q", got)
	}
}

// TestDrainOutboxOrphanedPathDivergentParksDiverged is the B8 genuine-
// divergence leg, for both the recreated-file create and the follow-on edit
// that pushes an update under the lost id: the local and server contents share
// no common ancestor and their bodies collide, so nothing can merge. The doc
// adopts the server identity, the server head is preserved (NOT clobbered),
// the local file stays untouched on disk (S5) and is captured in the conflict
// artifact, and the doc ends in the parked 'diverged' + `nb sync adopt` flow —
// never silently dropped.
func TestDrainOutboxOrphanedPathDivergentParksDiverged(t *testing.T) {
	for _, eventType := range []string{syncproto.EventDocumentCreated, syncproto.EventDocumentUpdated} {
		t.Run(eventType, func(t *testing.T) {
			stateHome := t.TempDir()
			t.Setenv("GROVE_HOME", "")
			t.Setenv("XDG_STATE_HOME", stateHome)
			db := openTestDB(t)

			serverHead := []byte("---\ntitle: note\n---\nSERVER body\n")
			local := []byte("---\ntitle: note\n---\nLOCAL body\n")
			root := seedOrphanRecreate(t, db, "doc-local-2", eventType, local)

			serverDocs := map[string]*occDoc{
				"inbox/note.md": {id: "doc-server-1", version: 1, hash: sha(serverHead), content: serverHead},
			}
			srv := serveOCCStoreStub(t, serverDocs, nil)
			defer srv.Close()

			client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
			handshake(t, client)
			pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{RetryBackoff: 50 * time.Millisecond})
			var divergedPath atomic.Value
			pipeline.OnDiverged = func(workspace, path string) { divergedPath.Store(workspace + "/" + path) }

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
				t.Fatalf("DrainOutbox: %v", err)
			}

			// The entry is parked 'diverged' — NOT deleted (the pre-fix bug).
			entries, err := db.ListOutbox("default", 0)
			if err != nil || len(entries) != 1 {
				t.Fatalf("expected 1 parked entry (not silently dropped), got %d (err=%v)", len(entries), err)
			}
			e := entries[0]
			if !e.Parked || e.ParkReason != "diverged" {
				t.Fatalf("divergent recreate must park as diverged: parked=%v reason=%q", e.Parked, e.ParkReason)
			}
			if e.EventType != syncproto.EventDocumentUpdated || e.DocumentID != "doc-server-1" {
				t.Fatalf("entry must be an update under the adopted server id, got %q/%q", e.EventType, e.DocumentID)
			}
			if e.Payload != string(serverHead) {
				t.Fatalf("entry must be retargeted at the adopted server head, got %q", e.Payload)
			}

			// The doc adopted the server identity and state, marked diverged.
			doc, err := db.GetDocumentByPath("default", "inbox/note.md")
			if err != nil || doc == nil {
				t.Fatalf("GetDocumentByPath: %v", err)
			}
			if doc.DocumentID != "doc-server-1" || !doc.Diverged {
				t.Fatalf("doc must adopt the server identity and go diverged: id=%q diverged=%v", doc.DocumentID, doc.Diverged)
			}
			if doc.LastSyncedVersion != 1 || string(doc.BaseContent) != string(serverHead) || doc.ContentHash != sha(local) {
				t.Fatalf("doc must roll to the server head while content_hash tracks disk: %+v", doc)
			}
			if got := divergedPath.Load(); got != "default/inbox/note.md" {
				t.Fatalf("OnDiverged hook not fired for the diverged doc, got %v", got)
			}

			// The local bytes are preserved twice over: untouched on disk (S5)
			// and captured in the conflict artifact.
			got, err := os.ReadFile(filepath.Join(root, "inbox", "note.md"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(local) {
				t.Fatalf("sync wrote the workspace file (S5 violation): %q", got)
			}
			artifact := filepath.Join(stateHome, "grove", "sync", "conflicts", "default", "inbox/note.md.doc-server-1.conflict.md")
			artifactContent, err := os.ReadFile(artifact)
			if err != nil {
				t.Fatalf("expected conflict artifact at %s: %v", artifact, err)
			}
			if string(artifactContent) != string(local) {
				t.Fatalf("artifact must hold the local content, got %q", artifactContent)
			}

			// Drain 2 (past the backoff): the retargeted entry is a no-op
			// against the adopted head and retires without touching the server
			// — the server head is preserved, no data loss.
			time.Sleep(120 * time.Millisecond)
			if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
				t.Fatalf("DrainOutbox (retire pass): %v", err)
			}
			if remaining, _ := db.CountOutbox(); remaining != 0 {
				t.Fatalf("no-op retargeted entry must retire, got %d entries", remaining)
			}
			d := serverDocs["inbox/note.md"]
			if d.version != 1 || string(d.content) != string(serverHead) {
				t.Fatalf("server head must be preserved (no clobber): v%d %q", d.version, d.content)
			}

			// The adopt flow then converges the user's content by explicit
			// choice: `nb sync adopt` rolls the base (AdoptDocument), the next
			// sweep/edit re-pushes on the real head, and the server advances.
			if err := db.AdoptDocument("default", "inbox/note.md", "doc-server-1", 1, sha(serverHead), serverHead); err != nil {
				t.Fatalf("AdoptDocument: %v", err)
			}
			if _, err := db.EnqueueOutbox(&OutboxEntry{
				DocumentID: "doc-server-1", Workspace: "default",
				EventType: syncproto.EventDocumentUpdated,
				Path:      "inbox/note.md", ContentHash: sha(local),
			}); err != nil {
				t.Fatalf("EnqueueOutbox (post-adopt): %v", err)
			}
			if n, err := pipeline.DrainOutbox(ctx, root); err != nil || n != 1 {
				t.Fatalf("DrainOutbox (post-adopt): n=%d err=%v", n, err)
			}
			d = serverDocs["inbox/note.md"]
			if d.version != 2 || string(d.content) != string(local) {
				t.Fatalf("post-adopt push must advance the server head with the local content: v%d %q", d.version, d.content)
			}
		})
	}
}

// TestDrainOutboxRejectedParksNotDeleted is the B8 disposition regression: a
// genuinely rejected push must no longer be silently deleted from the outbox —
// it parks with reason 'rejected' and a long flat backoff, visible to
// operators, without hot-spinning the drain loop.
func TestDrainOutboxRejectedParksNotDeleted(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("some content")
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Workspace: "default",
		EventType: syncproto.EventDocumentUpdated,
		Path:      "inbox/note.md", ContentHash: sha(content),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	var pushCount atomic.Int64
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		pushCount.Add(1)
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i := range resp.Results {
			resp.Results[i] = syncproto.PushResult{Status: syncproto.PushStatusRejected, Error: "malformed"}
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
		t.Fatalf("a rejected push must not ack, got %d", n)
	}
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 push attempt (no hot spin), got %d", got)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("rejected entry must stay in the outbox, got %d (err=%v)", len(entries), err)
	}
	e := entries[0]
	if !e.Parked || e.ParkReason != "rejected" || e.Attempts != 1 {
		t.Fatalf("rejected entry must park as 'rejected': parked=%v reason=%q attempts=%d", e.Parked, e.ParkReason, e.Attempts)
	}
	if until := time.Until(e.NextRetryAt); until < 30*time.Minute {
		t.Fatalf("rejected entry must park with a long flat backoff, retries in %s", until)
	}

	// A second drain before the backoff must not re-push the parked entry.
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (second pass): %v", err)
	}
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("parked rejected entry re-pushed before its retry time, %d pushes", got)
	}
}

// TestDrainOutboxCarriesMtime is the client half of the end-to-end mtime
// round trip: the enqueue-time stat mtime survives the outbox row (sqlite
// nanosecond round trip), and the pushed wire event carries the file's mtime
// as of the drain-time disk read — fidelity metadata only, never an OCC
// input (base_version behavior is untouched).
func TestDrainOutboxCarriesMtime(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("mtime-carrying body\n")
	localPath := filepath.Join(root, "inbox", "note.md")
	if err := os.WriteFile(localPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 7, 11, 9, 30, 42, 0, time.Local)
	if err := os.Chtimes(localPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	if _, err := db.EnqueueOutbox(&OutboxEntry{
		Workspace: "default", EventType: syncproto.EventDocumentCreated,
		Path: "inbox/note.md", ContentHash: sha(content), Mtime: mtime,
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	// The outbox row round-trips the enqueue-time mtime exactly.
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListOutbox: entries=%d err=%v", len(entries), err)
	}
	if !entries[0].Mtime.Equal(mtime) {
		t.Fatalf("outbox row mtime = %v, want %v", entries[0].Mtime, mtime)
	}

	var pushed atomic.Pointer[syncproto.SyncEvent]
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i := range req.Events {
			ev := req.Events[i]
			pushed.Store(&ev)
			resp.Results[i] = syncproto.PushResult{
				Status: syncproto.PushStatusAccepted, DocumentID: "doc-1", Version: 1, Seq: int64(i) + 1,
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
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}

	ev := pushed.Load()
	if ev == nil {
		t.Fatal("no event pushed")
	}
	// The wire event carries the disk file's mtime (drain re-reads content
	// from disk and refreshes the stat alongside it).
	if !ev.Mtime.Equal(mtime) {
		t.Fatalf("pushed event mtime = %v, want %v", ev.Mtime, mtime)
	}
	// Never an OCC input: a create still pushes base_version 0.
	if ev.BaseVersion != 0 {
		t.Fatalf("mtime must not affect OCC: base_version = %d, want 0", ev.BaseVersion)
	}
}

// TestDrainOutboxRecomputesHashOnDiskReRead is the B9 regression: when drain
// re-reads content from disk (empty Payload), the wire event must carry the
// hash/size of the bytes actually pushed, not the enqueue-time snapshot.
// Sending the frozen hash makes the server's validateContent reject the push
// ("content does not match content_hash") on every retry forever — the hourly
// re-park re-reads fresh bytes but re-sends the stale hash.
func TestDrainOutboxRecomputesHashOnDiskReRead(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldContent := []byte("enqueue-time body\n")
	localPath := filepath.Join(root, "inbox", "note.md")
	if err := os.WriteFile(localPath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Enqueue with the hash/size/mtime captured at enqueue time.
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		Workspace: "default", EventType: syncproto.EventDocumentCreated,
		Path: "inbox/note.md", ContentHash: sha(oldContent), Mtime: statMtime(localPath),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	// The file changes between enqueue and drain (different length too, so a
	// stale Size would also be caught).
	newContent := []byte("drain-time body — changed after enqueue\n")
	if err := os.WriteFile(localPath, newContent, 0o644); err != nil {
		t.Fatal(err)
	}
	newMtime := time.Date(2026, 7, 12, 10, 15, 33, 0, time.Local)
	if err := os.Chtimes(localPath, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}

	var pushed atomic.Pointer[syncproto.SyncEvent]
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i := range req.Events {
			ev := req.Events[i]
			pushed.Store(&ev)
			resp.Results[i] = syncproto.PushResult{
				Status: syncproto.PushStatusAccepted, DocumentID: "doc-1", Version: 1, Seq: int64(i) + 1,
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
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}

	ev := pushed.Load()
	if ev == nil {
		t.Fatal("no event pushed")
	}
	// Hash, size, and mtime all describe the NEW bytes — the ones on the wire.
	if got, want := ev.ContentHash, sha(newContent); got != want {
		t.Fatalf("pushed content_hash = %s, want hash of drain-time bytes %s", got, want)
	}
	if got, want := ev.Size, int64(len(newContent)); got != want {
		t.Fatalf("pushed size = %d, want %d", got, want)
	}
	if string(ev.Content) != string(newContent) {
		t.Fatalf("pushed content = %q, want drain-time bytes %q", ev.Content, newContent)
	}
	if !ev.Mtime.Equal(newMtime) {
		t.Fatalf("pushed mtime = %v, want drain-time %v", ev.Mtime, newMtime)
	}
}

// TestDrainOutboxDropsHashEqualNoOpCreate extends the S4 no-op drop to
// creates: a document_created entry whose pushed bytes already equal the
// server-confirmed head (doc row exists with matching last_synced_hash) is
// retired client-side instead of round-tripping to a guaranteed
// occupied-path collision.
func TestDrainOutboxDropsHashEqualNoOpCreate(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("already synced body\n")
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// The doc row says the server already holds exactly these bytes.
	if err := db.UpsertDocument(&Document{
		DocumentID: "doc-1", Workspace: "default", Path: "inbox/note.md",
		ContentHash: sha(content), LastSyncedHash: sha(content), LastSyncedVersion: 3,
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		Workspace: "default", EventType: syncproto.EventDocumentCreated,
		Path: "inbox/note.md", ContentHash: sha(content),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	var pushCount atomic.Int64
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		pushCount.Add(1)
		return &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
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

	if got := pushCount.Load(); got != 0 {
		t.Fatalf("hash-equal create must be dropped client-side, but %d push(es) reached the server", got)
	}
	remaining, err := db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expected no-op create to be retired from outbox, found %d entries", remaining)
	}
}

// TestDrainOutboxUnknownDocumentSelfHeals is the recreated-server per-entry
// recovery: an UPDATE rejected as "unknown document" (the server holds
// neither the id nor the path — the fresh-empty-DB signature) must not park
// for an hour and then reject forever. Below the attempts cap the pipeline
// voids the doc's server-confirmed state and drops the entry, so the next
// anti-entropy sweep re-enqueues a document_created — which then succeeds,
// preserving the stable document id.
func TestDrainOutboxUnknownDocumentSelfHeals(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("---\ntitle: note\n---\nsurvives the recreate\n")
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// A doc the client believes is synced (against the dead server) with a
	// queued edit.
	if err := db.InsertDocument(&Document{
		DocumentID: "doc-stable", Workspace: "default", Path: "inbox/note.md",
		ContentHash: sha(content), LastSyncedHash: "old-hash", LastSyncedVersion: 9,
		BaseContent: []byte("old base"),
	}); err != nil {
		t.Fatalf("InsertDocument: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-stable", Workspace: "default",
		EventType: syncproto.EventDocumentUpdated,
		Path:      "inbox/note.md", ContentHash: sha(content),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	// The recreated server: an empty OCC store rejects the update with the
	// real handler's "unknown document" text and accepts the follow-up create.
	serverDocs := map[string]*occDoc{}
	srv := serveOCCStoreStub(t, serverDocs, nil)
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if n, err := pipeline.DrainOutbox(ctx, root); err != nil || n != 0 {
		t.Fatalf("DrainOutbox: n=%d err=%v", n, err)
	}

	// Self-heal: entry dropped (not parked), synced state voided, id kept.
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("self-healed entry must be dropped from the outbox, got %d", remaining)
	}
	doc, err := db.GetDocument("doc-stable")
	if err != nil || doc == nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if doc.LastSyncedHash != "" || doc.LastSyncedVersion != 0 {
		t.Fatalf("synced state must be voided for the create re-push: hash=%q v%d",
			doc.LastSyncedHash, doc.LastSyncedVersion)
	}

	// The sweep now re-enqueues a create (LastSyncedHash empty), and the
	// drain re-populates the empty server under the stable id.
	ae := newTestAntiEntropy(db, client, root)
	if err := ae.sweepLocalDocuments(ctx); err != nil {
		t.Fatalf("sweepLocalDocuments: %v", err)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected the sweep to re-enqueue 1 entry, got %d (err=%v)", len(entries), err)
	}
	if entries[0].EventType != syncproto.EventDocumentCreated {
		t.Fatalf("re-enqueued entry must be a create, got %s", entries[0].EventType)
	}
	if n, err := pipeline.DrainOutbox(ctx, root); err != nil || n != 1 {
		t.Fatalf("DrainOutbox (re-push): n=%d err=%v", n, err)
	}
	d, ok := serverDocs["inbox/note.md"]
	if !ok || d.id != "doc-stable" || string(d.content) != string(content) {
		t.Fatalf("re-pushed create must land under the stable id: %+v", d)
	}
}

// TestDrainOutboxUnknownDocumentParksAtAttemptCap is the self-heal loop
// guard: an entry that has already burned unknownDocSelfHealMaxAttempts park
// attempts stops self-healing and parks as a plain reject, so a pathological
// reject cycle cannot ping-pong between self-heal and sweep forever.
func TestDrainOutboxUnknownDocumentParksAtAttemptCap(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("capped content")
	if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDocument(&Document{
		DocumentID: "doc-capped", Workspace: "default", Path: "inbox/note.md",
		ContentHash: sha(content), LastSyncedHash: "old-hash", LastSyncedVersion: 4,
	}); err != nil {
		t.Fatalf("InsertDocument: %v", err)
	}
	id, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-capped", Workspace: "default",
		EventType: syncproto.EventDocumentUpdated,
		Path:      "inbox/note.md", ContentHash: sha(content),
	})
	if err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	// Burn the self-heal budget: attempts reaches the cap via prior parks
	// (retry times in the past so the entry stays drainable).
	for i := 0; i < unknownDocSelfHealMaxAttempts; i++ {
		if err := db.ParkOutbox(id, "rejected", time.Now().Add(-time.Minute)); err != nil {
			t.Fatalf("ParkOutbox: %v", err)
		}
	}

	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i := range resp.Results {
			resp.Results[i] = syncproto.PushResult{
				Status: syncproto.PushStatusRejected,
				Error:  "unknown document doc-capped at inbox/note.md",
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
	if n, err := pipeline.DrainOutbox(ctx, root); err != nil || n != 0 {
		t.Fatalf("DrainOutbox: n=%d err=%v", n, err)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("capped entry must stay in the outbox, got %d (err=%v)", len(entries), err)
	}
	e := entries[0]
	if !e.Parked || e.ParkReason != "rejected" || e.Attempts != unknownDocSelfHealMaxAttempts+1 {
		t.Fatalf("capped entry must park as rejected: parked=%v reason=%q attempts=%d",
			e.Parked, e.ParkReason, e.Attempts)
	}
	// At the cap the doc's synced state is NOT voided (no silent repush).
	doc, err := db.GetDocument("doc-capped")
	if err != nil || doc == nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if doc.LastSyncedHash != "old-hash" || doc.LastSyncedVersion != 4 {
		t.Fatalf("capped reject must not void synced state: hash=%q v%d", doc.LastSyncedHash, doc.LastSyncedVersion)
	}
}
