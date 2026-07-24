package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/grovetools/core/pkg/syncproto"
)

func returnTestClient(t *testing.T, epoch string, snaps map[string]syncproto.SnapshotManifest, content map[string][]byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/capabilities":
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{Capabilities: syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersion}}, ServerEpoch: epoch})
		case "/sync/snapshot":
			_ = json.NewEncoder(w).Encode(snaps[r.URL.Query().Get("workspace")])
		case "/sync/history/blob":
			b, ok := content[r.URL.Query().Get("document_id")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{ServerURL: srv.URL, Token: "x", OriginID: "laptop"})
	if _, err := c.Capabilities(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	return c
}
func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestReturnManifestRepresentsCreateUpdateMoveDelete(t *testing.T) {
	db := openTestDB(t)
	old := []byte("old")
	same := []byte("same")
	for _, d := range []*Document{{DocumentID: "update", Workspace: "ws", Path: "u.md", ContentHash: hash(old)}, {DocumentID: "move", Workspace: "ws", Path: "before.md", ContentHash: hash(old)}, {DocumentID: "delete", Workspace: "ws", Path: "gone.md", ContentHash: hash(old), LastSyncedVersion: 1}, {DocumentID: "same", Workspace: "ws", Path: "same.md", ContentHash: hash(same)}} {
		if err := db.UpsertDocument(d); err != nil {
			t.Fatal(err)
		}
	}
	heads := map[string][]byte{"create": []byte("new"), "update": []byte("changed"), "move": []byte("moved"), "same": same}
	var docs []syncproto.DocumentSnapshot
	for _, x := range []struct{ id, path string }{{"create", "new.md"}, {"update", "u.md"}, {"move", "after.md"}, {"same", "same.md"}} {
		docs = append(docs, syncproto.DocumentSnapshot{ID: x.id, Path: x.path, Version: 2, Hash: hash(heads[x.id]), Size: int64(len(heads[x.id]))})
	}
	c := returnTestClient(t, "epoch-1", map[string]syncproto.SnapshotManifest{"ws": {Workspace: "ws", Cursor: 9, Documents: docs}}, heads)
	m, err := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, op := range m.Operations {
		got[op.Type] = true
	}
	for _, typ := range []string{"create", "update", "move", "delete"} {
		if !got[typ] {
			t.Errorf("missing %s: %+v", typ, m.Operations)
		}
	}
	if err = m.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReturnManifestIgnoresServerAbsenceForNeverSyncedLocalIdentity(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertDocument(&Document{DocumentID: "stale", Workspace: "ws", Path: "plans/old/a.md", ContentHash: hash([]byte("body")), LastSyncedVersion: 0}); err != nil {
		t.Fatal(err)
	}
	c := returnTestClient(t, "epoch-1", map[string]syncproto.SnapshotManifest{"ws": {Workspace: "ws", Cursor: 1}}, nil)
	m, err := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Operations) != 0 {
		t.Fatalf("never-synced local identity produced incoming operations: %+v", m.Operations)
	}
}

func TestReturnManifestLocalAndServerChangesInvalidateGeneration(t *testing.T) {
	db := openTestDB(t)
	body := []byte("head")
	if err := db.UpsertDocument(&Document{DocumentID: "d", Workspace: "ws", Path: "a.md", ContentHash: hash([]byte("base"))}); err != nil {
		t.Fatal(err)
	}
	snap := syncproto.SnapshotManifest{Workspace: "ws", Cursor: 1, Documents: []syncproto.DocumentSnapshot{{ID: "d", Path: "a.md", Version: 1, Hash: hash(body)}}}
	c := returnTestClient(t, "epoch-1", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": body})
	a, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	_ = db.UpsertDocument(&Document{DocumentID: "d", Workspace: "ws", Path: "a.md", ContentHash: hash([]byte("local edit"))})
	b, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if a.Generation == b.Generation {
		t.Fatal("local state change did not stale reviewed generation")
	}
	c2 := returnTestClient(t, "epoch-2", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": body})
	cman, _ := BuildReturnManifest(context.Background(), c2, db, []string{"ws"})
	if b.Generation == cman.Generation {
		t.Fatal("server epoch change did not change generation")
	}
}

func TestReviewedReturnManifestStaleRefusal(t *testing.T) {
	db := openTestDB(t)
	body := []byte("head")
	snap := syncproto.SnapshotManifest{Workspace: "ws", Cursor: 1, Documents: []syncproto.DocumentSnapshot{{ID: "d", Path: "a.md", Version: 1, Hash: hash(body)}}}
	c := returnTestClient(t, "epoch", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": body})
	reviewed, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err := db.UpsertDocument(&Document{DocumentID: "local", Workspace: "ws", Path: "local.md", ContentHash: hash([]byte("edit"))}); err != nil {
		t.Fatal(err)
	}
	current, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err := ValidateReviewedManifest(reviewed, current); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("got %v", err)
	}
}

func TestReturnEscrowDurableHashVerification(t *testing.T) {
	db := openTestDB(t)
	body := []byte("guest only")
	snap := syncproto.SnapshotManifest{Workspace: "ws", Cursor: 2, Documents: []syncproto.DocumentSnapshot{{ID: "d", Path: "new.md", Version: 1, Hash: hash(body)}}}
	c := returnTestClient(t, "epoch", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": body})
	m, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	path, err := WriteReturnEscrow(context.Background(), c, m, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyReturnEscrow(path, m.Generation); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	b = []byte(strings.Replace(string(b), "Z3Vlc3Qgb25seQ==", "dGFtcGVyZWQ=", 1))
	_ = os.WriteFile(path, b, 0o600)
	if err = VerifyReturnEscrow(path, m.Generation); err == nil {
		t.Fatal("tampered escrow verified")
	}
}
