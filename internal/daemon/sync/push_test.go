package sync

import (
	"context"
	"encoding/json"
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
}
