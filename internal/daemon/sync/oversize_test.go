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

// serveBlobCeilingStub answers the capabilities handshake advertising the blob
// tier with the given inline/blob ceilings, and accepts every pushed event.
// pushCount records how many push batches arrived.
func serveBlobCeilingStub(t *testing.T, maxInline, maxBlob int64, pushCount *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sync/capabilities":
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
				ProtocolVersion: syncproto.ProtocolVersionLegacy,
				Capabilities: syncproto.Capabilities{
					ProtocolVersions: []int{syncproto.ProtocolVersionLegacy},
					Blobs:            true,
					MaxInlineSize:    maxInline,
					MaxBlobSize:      maxBlob,
				},
			})
		case "/sync/push":
			pushCount.Add(1)
			var req syncproto.PushRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode push request: %v", err)
			}
			resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events)), Cursor: 1}
			for i := range resp.Results {
				resp.Results[i] = syncproto.PushResult{
					Status: syncproto.PushStatusAccepted, DocumentID: "doc", Version: 1,
				}
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
}

// TestDrainOutboxSkipsOversize is the anti-livelock regression: a file larger
// than the server's advertised blob ceiling is skipped-with-warning (deleted
// from the outbox, surfaced via OnOversizeSkipped) while the rest of the batch
// pushes and the outbox drains — never a batch-error retry forever.
func TestDrainOutboxSkipsOversize(t *testing.T) {
	db := openTestDB(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The advertised ceiling (not the absolute byte count) is what the skip
	// keys on, so modest sizes exercise the same path as a 9MB file.
	const maxInline, maxBlob = 64, 1024
	small := []byte("small note")
	big := make([]byte, 2048) // > maxBlob → oversize skip
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "big.md"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "small.md"), small, 0o644); err != nil {
		t.Fatal(err)
	}

	// Enqueue big first so the oversize skip does not sit at the tail.
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-big", Workspace: "default",
		EventType: syncproto.EventDocumentCreated, Path: "inbox/big.md", ContentHash: sha(big),
	}); err != nil {
		t.Fatalf("EnqueueOutbox big: %v", err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-small", Workspace: "default",
		EventType: syncproto.EventDocumentCreated, Path: "inbox/small.md", ContentHash: sha(small),
	}); err != nil {
		t.Fatalf("EnqueueOutbox small: %v", err)
	}

	var pushCount atomic.Int64
	srv := serveBlobCeilingStub(t, maxInline, maxBlob, &pushCount)
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	var skippedPath string
	var skippedSize, skippedLimit int64
	var skipCount atomic.Int64
	pipeline.OnOversizeSkipped = func(ws, path string, size, limit int64) {
		skipCount.Add(1)
		skippedPath, skippedSize, skippedLimit = path, size, limit
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}

	// The small file pushed; the oversize file was skipped, not acked.
	if n != 1 {
		t.Fatalf("expected 1 successful push (the small file), got %d", n)
	}
	if got := skipCount.Load(); got != 1 {
		t.Fatalf("expected OnOversizeSkipped once, got %d", got)
	}
	if skippedPath != "inbox/big.md" || skippedSize != int64(len(big)) || skippedLimit != maxBlob {
		t.Fatalf("unexpected skip callback: path=%q size=%d limit=%d", skippedPath, skippedSize, skippedLimit)
	}
	// Exactly one push batch — the oversize file must not trigger a retry loop.
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 push batch, got %d (livelock regression)", got)
	}
	// Phase 4: the small one acked (deleted); the oversize one is PARKED, not
	// deleted — still counted as pending (unsynced), surfaced not lost.
	if remaining, _ := db.CountOutbox(); remaining != 1 {
		t.Fatalf("expected the oversize entry to stay parked, got %d entries", remaining)
	}
	if parked, _ := db.CountOutboxParked(); parked != 1 {
		t.Fatalf("expected 1 parked entry, got %d", parked)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 remaining outbox entry, got %d (err=%v)", len(entries), err)
	}
	if entries[0].Path != "inbox/big.md" || entries[0].ParkReason != "oversize_skipped" || entries[0].Attempts != 1 {
		t.Fatalf("unexpected parked entry: path=%q reason=%q attempts=%d",
			entries[0].Path, entries[0].ParkReason, entries[0].Attempts)
	}
}

// TestDrainOutboxAllOversizeDrains covers the all-skipped batch: every entry is
// oversize, so the batch deletes them and the loop continues to an empty outbox
// without spinning or pushing.
func TestDrainOutboxAllOversizeDrains(t *testing.T) {
	db := openTestDB(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	const maxInline, maxBlob = 64, 1024
	big := make([]byte, 2048)
	for i := range big {
		big[i] = 'y'
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "big.md"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-big", Workspace: "default",
		EventType: syncproto.EventDocumentCreated, Path: "inbox/big.md", ContentHash: sha(big),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	var pushCount atomic.Int64
	srv := serveBlobCeilingStub(t, maxInline, maxBlob, &pushCount)
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})

	var skipCount atomic.Int64
	pipeline.OnOversizeSkipped = func(ws, path string, size, limit int64) { skipCount.Add(1) }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 acks (nothing pushed), got %d", n)
	}
	if got := pushCount.Load(); got != 0 {
		t.Fatalf("expected no push batch for an all-oversize outbox, got %d", got)
	}
	if got := skipCount.Load(); got != 1 {
		t.Fatalf("expected OnOversizeSkipped once, got %d", got)
	}
	// Phase 4: the all-oversize batch parks its entries (rather than deleting)
	// and the loop still terminates — the parked entry drops out of the next
	// ListOutboxDrainable call (future retry time).
	if remaining, _ := db.CountOutbox(); remaining != 1 {
		t.Fatalf("expected the oversize entry to stay parked, got %d entries", remaining)
	}
	if parked, _ := db.CountOutboxParked(); parked != 1 {
		t.Fatalf("expected 1 parked entry, got %d", parked)
	}
}

// TestClientMaxBlobSize checks the accessor: populated from the handshake when
// the server advertises it, and 0 (the don't-enforce sentinel) when it does
// not — old-server compatibility.
func TestClientMaxBlobSize(t *testing.T) {
	// Server advertises a ceiling.
	var pushCount atomic.Int64
	srv := serveBlobCeilingStub(t, 256<<10, 8<<20, &pushCount)
	defer srv.Close()
	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t"})
	handshake(t, client)
	if got := client.MaxBlobSize(); got != 8<<20 {
		t.Fatalf("MaxBlobSize = %d, want %d", got, int64(8<<20))
	}

	// Old server: capabilities without the field → 0.
	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
			ProtocolVersion: syncproto.ProtocolVersionLegacy,
			Capabilities:    syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersionLegacy}, Blobs: true},
		})
	}))
	defer oldSrv.Close()
	oldClient := NewClient(ClientConfig{ServerURL: oldSrv.URL, Token: "t"})
	handshake(t, oldClient)
	if got := oldClient.MaxBlobSize(); got != 0 {
		t.Fatalf("MaxBlobSize against old server = %d, want 0", got)
	}
}
