package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

// TestCheckServerEpochMatrix is the stored-vs-received decision table:
// pre-epoch servers are ignored, first contact records without resetting,
// a matching epoch is a no-op, and a CHANGED epoch (the recreated-server
// signature) voids the synced state for a full re-push.
func TestCheckServerEpochMatrix(t *testing.T) {
	ctx := context.Background()
	log := logging.NewUnifiedLogger("test.epoch")

	seedSyncedDoc := func(t *testing.T, db *DB) {
		t.Helper()
		if err := db.InsertDocument(&Document{
			DocumentID: "doc-1", Workspace: "default", Path: "inbox/a.md",
			ContentHash: "hash", LastSyncedHash: "hash", LastSyncedVersion: 3,
		}); err != nil {
			t.Fatalf("InsertDocument: %v", err)
		}
	}
	syncedState := func(t *testing.T, db *DB) (string, int64) {
		t.Helper()
		doc, err := db.GetDocument("doc-1")
		if err != nil || doc == nil {
			t.Fatalf("GetDocument: %v", err)
		}
		return doc.LastSyncedHash, doc.LastSyncedVersion
	}

	t.Run("empty received is a no-op (pre-epoch server)", func(t *testing.T) {
		db := openTestDB(t)
		seedSyncedDoc(t, db)
		if err := db.SetServerEpoch("epoch-a"); err != nil {
			t.Fatal(err)
		}
		reset, err := CheckServerEpoch(ctx, db, "", log)
		if err != nil || reset {
			t.Fatalf("expected no-op, got reset=%v err=%v", reset, err)
		}
		if stored, _ := db.GetServerEpoch(); stored != "epoch-a" {
			t.Fatalf("stored epoch must be untouched, got %q", stored)
		}
		if h, v := syncedState(t, db); h != "hash" || v != 3 {
			t.Fatalf("synced state must be untouched: %q v%d", h, v)
		}
	})

	t.Run("empty stored records without reset (first contact)", func(t *testing.T) {
		db := openTestDB(t)
		seedSyncedDoc(t, db)
		reset, err := CheckServerEpoch(ctx, db, "epoch-a", log)
		if err != nil || reset {
			t.Fatalf("first contact must not reset, got reset=%v err=%v", reset, err)
		}
		if stored, _ := db.GetServerEpoch(); stored != "epoch-a" {
			t.Fatalf("first contact must record the epoch, got %q", stored)
		}
		if h, v := syncedState(t, db); h != "hash" || v != 3 {
			t.Fatalf("synced state must be untouched: %q v%d", h, v)
		}
	})

	t.Run("equal epochs are a no-op", func(t *testing.T) {
		db := openTestDB(t)
		seedSyncedDoc(t, db)
		if err := db.SetServerEpoch("epoch-a"); err != nil {
			t.Fatal(err)
		}
		reset, err := CheckServerEpoch(ctx, db, "epoch-a", log)
		if err != nil || reset {
			t.Fatalf("expected no-op, got reset=%v err=%v", reset, err)
		}
		if h, v := syncedState(t, db); h != "hash" || v != 3 {
			t.Fatalf("synced state must be untouched: %q v%d", h, v)
		}
	})

	t.Run("changed epoch resets for repush", func(t *testing.T) {
		db := openTestDB(t)
		seedSyncedDoc(t, db)
		if err := db.SetServerEpoch("epoch-a"); err != nil {
			t.Fatal(err)
		}
		if err := db.SetCursor("default", 42); err != nil {
			t.Fatal(err)
		}
		reset, err := CheckServerEpoch(ctx, db, "epoch-b", log)
		if err != nil || !reset {
			t.Fatalf("changed epoch must reset, got reset=%v err=%v", reset, err)
		}
		if stored, _ := db.GetServerEpoch(); stored != "epoch-b" {
			t.Fatalf("new epoch must be recorded, got %q", stored)
		}
		if h, v := syncedState(t, db); h != "" || v != 0 {
			t.Fatalf("synced state must be voided: %q v%d", h, v)
		}
		st, err := db.GetState("default")
		if err != nil || st == nil {
			t.Fatalf("GetState: %v", err)
		}
		if st.Cursor != 0 {
			t.Fatalf("cursor must reset to 0, got %d", st.Cursor)
		}
	})
}

// serveEpochStoreStub is serveOCCStoreStub's sibling for the recreated-server
// lifecycle: its capabilities response advertises a MUTABLE server epoch and
// it additionally serves /sync/snapshot from the current heads, so a full
// anti-entropy pass (handshake → snapshot → sweep) plus the push drain can
// run against it in-process.
func serveEpochStoreStub(t *testing.T, epoch *string, docs map[string]*occDoc) *httptest.Server {
	t.Helper()
	occ := serveOCCStoreStub(t, docs, nil)
	t.Cleanup(occ.Close)
	occURL := occ.URL

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/capabilities":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
				ServerEpoch:  *epoch,
				Capabilities: syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersion}},
			})
		case "/sync/snapshot":
			w.Header().Set("Content-Type", "application/json")
			manifest := &syncproto.SnapshotManifest{Workspace: "default"}
			for path, d := range docs {
				manifest.Documents = append(manifest.Documents, syncproto.DocumentSnapshot{
					ID: d.id, Path: path, Version: d.version, Hash: d.hash, Size: int64(len(d.content)),
				})
			}
			_ = json.NewEncoder(w).Encode(manifest)
		default:
			// Delegate push/history to the OCC stub's exact server semantics.
			req, err := http.NewRequestWithContext(r.Context(), r.Method, occURL+r.URL.String(), r.Body)
			if err != nil {
				t.Errorf("proxy request: %v", err)
				return
			}
			req.Header = r.Header.Clone()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("proxy do: %v", err)
				return
			}
			defer resp.Body.Close()
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}
	}))
}

// TestServerRecreateTriggersFullRepush is the end-to-end (in-process) leg of
// the recreated-server recovery: a client whose sync.db says two documents
// are fully synced reconnects to a FRESH, EMPTY server advertising a new
// epoch. The anti-entropy pass detects the epoch change, voids the synced
// state, sweeps every document back into the outbox as a document_created,
// and the push drain re-populates the server — under the ORIGINAL stable
// document ids.
func TestServerRecreateTriggersFullRepush(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	// Two documents, previously synced against the old server (epoch-a).
	contents := map[string][]byte{
		"inbox/a.md": []byte("---\ntitle: a\n---\nbody a\n"),
		"inbox/b.md": []byte("---\ntitle: b\n---\nbody b\n"),
	}
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	i := 0
	for path, content := range contents {
		i++
		if err := os.WriteFile(filepath.Join(root, syncproto.LocalizePath(path)), content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertDocument(&Document{
			DocumentID: fmt.Sprintf("doc-stable-%d", i), Workspace: "default", Path: path,
			ContentHash: sha(content), LastSyncedHash: sha(content), LastSyncedVersion: 5,
			BaseContent: content,
		}); err != nil {
			t.Fatalf("InsertDocument: %v", err)
		}
	}
	if err := db.SetServerEpoch("epoch-a"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCursor("default", 99); err != nil {
		t.Fatal(err)
	}

	// The recreated server: empty store, new epoch.
	epoch := "epoch-b"
	serverDocs := map[string]*occDoc{}
	srv := serveEpochStoreStub(t, &epoch, serverDocs)
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Anti-entropy pass: re-handshake detects the epoch change → reset →
	// sweep enqueues creates for both documents.
	ae := newTestAntiEntropy(db, client, root)
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("anti-entropy Run: %v", err)
	}
	if stored, _ := db.GetServerEpoch(); stored != "epoch-b" {
		t.Fatalf("epoch must be rolled to the new server's, got %q", stored)
	}
	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(entries) != len(contents) {
		t.Fatalf("expected %d re-push creates enqueued, got %d", len(contents), len(entries))
	}
	for _, e := range entries {
		if e.EventType != syncproto.EventDocumentCreated {
			t.Fatalf("re-push of %s must be a create, got %s", e.Path, e.EventType)
		}
	}

	// Drain: the empty server accepts every create, preserving the ids.
	pipeline := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})
	n, err := pipeline.DrainOutbox(ctx, root)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if n != len(contents) {
		t.Fatalf("expected %d acks, got %d", len(contents), n)
	}
	if remaining, _ := db.CountOutbox(); remaining != 0 {
		t.Fatalf("expected empty outbox after re-push, got %d", remaining)
	}

	for path, content := range contents {
		d, ok := serverDocs[path]
		if !ok {
			t.Fatalf("server missing re-pushed document %s", path)
		}
		if string(d.content) != string(content) {
			t.Fatalf("server content mismatch for %s", path)
		}
		local, err := db.GetDocumentByPath("default", path)
		if err != nil || local == nil {
			t.Fatalf("GetDocumentByPath %s: %v", path, err)
		}
		if d.id != local.DocumentID {
			t.Fatalf("document id must stay stable across the recreate: server %q local %q", d.id, local.DocumentID)
		}
		if local.LastSyncedVersion != d.version || local.LastSyncedHash != d.hash {
			t.Fatalf("local record must confirm the re-pushed head: v%d hash=%q (server v%d %q)",
				local.LastSyncedVersion, local.LastSyncedHash, d.version, d.hash)
		}
	}

	// A second pass against the SAME epoch is quiet: no reset, no re-enqueue.
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("second anti-entropy Run: %v", err)
	}
	if n, _ := db.CountOutbox(); n != 0 {
		t.Fatalf("stable epoch must not re-trigger a repush, got %d entries", n)
	}
}
