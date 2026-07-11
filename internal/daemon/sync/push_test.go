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

// TestDrainOutboxRebasesCleanConflict: a Conflict whose server head touched a
// different body region than the local edit is rebased — the merged content
// (both edits) lands on disk, the entry unparks, and the next drain pushes it
// with the server head as base_version.
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

	var pushCount atomic.Int64
	var acceptedContent atomic.Value
	var acceptedBase atomic.Int64
	srv := serveRebaseStub(t,
		func(req *syncproto.PushRequest) *syncproto.PushResponse {
			pushCount.Add(1)
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
	// Phase 4: a clean rebase retargets the entry AND parks it with a backoff,
	// so the re-push happens on a later drain (after next_retry_at). Use a short
	// backoff so the test's second drain lands past it without a long sleep; the
	// backoff (100ms) still far exceeds the sub-ms localhost drain loop, so the
	// entry reliably stays parked for the intermediate assertions below.
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{RetryBackoff: 100 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First drain: conflict → rebase (retarget) → park with backoff; the entry
	// stays queued (parked) for the re-push after next_retry_at.
	if _, err := pipeline.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("DrainOutbox (rebase pass): %v", err)
	}

	wantMerged := "---\ntitle: note\n---\nLOCAL one\nline two\nREMOTE three\n"
	got, err := os.ReadFile(filepath.Join(root, "inbox", "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wantMerged {
		t.Fatalf("disk after rebase = %q, want %q (both edits present)", got, wantMerged)
	}

	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 7 || doc.LastSyncedHash != sha(serverHead) || string(doc.BaseContent) != string(serverHead) {
		t.Fatalf("rebase must roll the merge base to the server head: v%d hash=%q", doc.LastSyncedVersion, doc.LastSyncedHash)
	}
	if doc.ContentHash != sha([]byte(wantMerged)) {
		t.Fatalf("content_hash must track merged bytes (watcher echo gate), got %q", doc.ContentHash)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 rebased entry still queued, got %d (err=%v)", len(entries), err)
	}
	if entries[0].ContentHash != sha([]byte(wantMerged)) {
		t.Fatalf("outbox entry not retargeted at merged content: %q", entries[0].ContentHash)
	}
	// The retargeted entry is parked with a conflict backoff until next_retry_at.
	if !entries[0].Parked || entries[0].ParkReason != "conflict" {
		t.Fatalf("rebased entry must be parked as conflict, got parked=%v reason=%q",
			entries[0].Parked, entries[0].ParkReason)
	}

	// Wait past the conflict backoff so the parked entry becomes drainable, then
	// re-drain: it pushes cleanly with the server head (v7) as base_version.
	time.Sleep(150 * time.Millisecond)
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox (re-push pass): %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 ack on re-push, got %d", n)
	}
	if acceptedBase.Load() != 7 {
		t.Fatalf("re-push must carry the server head as base_version, got %d", acceptedBase.Load())
	}
	if acceptedContent.Load() != wantMerged {
		t.Fatalf("pushed content = %q, want merged %q", acceptedContent.Load(), wantMerged)
	}
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("expected empty outbox after rebased push, got %d", remaining)
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

// TestDrainOutboxRebaseAbortsOnMidRebaseEdit: a local edit landing while the
// rebase is in flight (between the local read and the merge write) aborts the
// attempt — nothing is written, the doc record stays put, and the next tick
// retries against the new local state.
func TestDrainOutboxRebaseAbortsOnMidRebaseEdit(t *testing.T) {
	t.Setenv("GROVE_HOME", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db := openTestDB(t)

	base := []byte("---\ntitle: note\n---\nline one\nline two\nline three\n")
	local := []byte("---\ntitle: note\n---\nLOCAL one\nline two\nline three\n")
	serverHead := []byte("---\ntitle: note\n---\nline one\nline two\nREMOTE three\n")
	midEdit := []byte("---\ntitle: note\n---\nLOCAL one\nMID EDIT two\nline three\n")
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
		func(version int64) ([]byte, error) {
			// The blob fetch happens after the rebase snapshots the local
			// file; mutating the file here is a mid-rebase local edit.
			if err := os.WriteFile(filepath.Join(root, "inbox", "note.md"), midEdit, 0o644); err != nil {
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

	// Aborted cleanly: the mid-rebase edit is intact on disk, the doc record
	// and outbox entry are untouched, no artifact.
	got, err := os.ReadFile(filepath.Join(root, "inbox", "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(midEdit) {
		t.Fatalf("mid-rebase edit clobbered: disk = %q", got)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 1 || string(doc.BaseContent) != string(base) {
		t.Fatalf("aborted rebase must not roll the merge base: v%d", doc.LastSyncedVersion)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected the entry to stay parked, got %d (err=%v)", len(entries), err)
	}
	if entries[0].ContentHash != sha(local) {
		t.Fatalf("aborted rebase must not retarget the outbox entry: %q", entries[0].ContentHash)
	}
}
