package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// newSyncTestServer builds a global (unscoped) server backed by a fresh sync
// database at the given path so the read-only introspection handlers can be
// exercised directly.
func newSyncTestServer(t *testing.T, dbPath string) *Server {
	t.Helper()
	db, err := syncdb.Open(dbPath)
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(false)
	s.SetSyncDB(db)
	return s
}

func TestHandleSyncDocuments(t *testing.T) {
	db, _ := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	t.Cleanup(func() { _ = db.Close() })
	s := New(false)
	s.SetSyncDB(db)

	// One clean (content == last_synced), one dirty (content != last_synced).
	if err := db.UpsertDocument(&syncdb.Document{DocumentID: "d1", Workspace: "ws", Path: "a.md", ContentHash: "h", LastSyncedHash: "h", LastSyncedVersion: 3}); err != nil {
		t.Fatalf("upsert clean: %v", err)
	}
	if err := db.UpsertDocument(&syncdb.Document{DocumentID: "d2", Workspace: "ws", Path: "b.md", ContentHash: "local", LastSyncedHash: "server"}); err != nil {
		t.Fatalf("upsert dirty: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sync/documents", nil)
	w := httptest.NewRecorder()
	s.handleSyncDocuments(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var docs []syncDocumentResponse
	if err := json.NewDecoder(w.Body).Decode(&docs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	byID := map[string]syncDocumentResponse{}
	for _, d := range docs {
		byID[d.DocumentID] = d
	}
	if d := byID["d1"]; d.IsDirty || d.Version != 3 {
		t.Errorf("d1 should be clean v3, got %+v", d)
	}
	if d := byID["d2"]; !d.IsDirty {
		t.Errorf("d2 should be dirty, got %+v", d)
	}

	// Workspace filter excludes everything when it doesn't match.
	req = httptest.NewRequest(http.MethodGet, "/api/sync/documents?workspace=none", nil)
	w = httptest.NewRecorder()
	s.handleSyncDocuments(w, req)
	var filtered []syncDocumentResponse
	if err := json.NewDecoder(w.Body).Decode(&filtered); err != nil {
		t.Fatalf("decode filtered: %v", err)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected 0 docs for unknown workspace, got %d", len(filtered))
	}
}

func TestHandleSyncDocumentsMethodNotAllowed(t *testing.T) {
	s := newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))
	req := httptest.NewRequest(http.MethodPost, "/api/sync/documents", nil)
	w := httptest.NewRecorder()
	s.handleSyncDocuments(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleSyncOutbox(t *testing.T) {
	db, _ := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	t.Cleanup(func() { _ = db.Close() })
	s := New(false)
	s.SetSyncDB(db)

	if _, err := db.EnqueueOutbox(&syncdb.OutboxEntry{DocumentID: "d1", Workspace: "ws", EventType: "document.updated", Path: "a.md", ContentHash: "h1", Payload: "secret-body"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := db.EnqueueOutbox(&syncdb.OutboxEntry{DocumentID: "d2", Workspace: "other", EventType: "document.created", Path: "b.md", ContentHash: "h2"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sync/outbox", nil)
	w := httptest.NewRecorder()
	s.handleSyncOutbox(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	// The payload field must not leak — the response struct omits it.
	if body := w.Body.String(); strings.Contains(body, "secret-body") {
		t.Fatalf("outbox response leaked payload content: %s", body)
	}
	var entries []syncOutboxResponse
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 outbox entries, got %d", len(entries))
	}
	if entries[0].Path != "a.md" || entries[0].EventType != "document.updated" {
		t.Errorf("unexpected first entry: %+v", entries[0])
	}

	// Workspace filter.
	req = httptest.NewRequest(http.MethodGet, "/api/sync/outbox?workspace=other", nil)
	w = httptest.NewRecorder()
	s.handleSyncOutbox(w, req)
	var filtered []syncOutboxResponse
	if err := json.NewDecoder(w.Body).Decode(&filtered); err != nil {
		t.Fatalf("decode filtered: %v", err)
	}
	if len(filtered) != 1 || filtered[0].DocumentID != "d2" {
		t.Fatalf("expected only the 'other' workspace entry, got %+v", filtered)
	}
}

func TestHandleSyncConflicts(t *testing.T) {
	// GROVE_HOME controls paths.StateDir (and DataDir), giving the handler a
	// scratch conflict store to scan.
	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)

	db, _ := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	t.Cleanup(func() { _ = db.Close() })
	s := New(false)
	s.SetSyncDB(db)

	// A tracked document with a 3-way base, whose pull produced a conflict
	// artifact on disk (named <path>.<document_id>.conflict.md by pull.go).
	const docID = "11111111-2222-3333-4444-555555555555"
	if err := db.UpsertDocument(&syncdb.Document{
		DocumentID:  docID,
		Workspace:   "ws",
		Path:        "notes/foo.md",
		ContentHash: "local",
		BaseContent: []byte("base body"),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	conflictDir := filepath.Join(paths.StateDir(), "sync", "conflicts", "ws", "notes")
	if err := os.MkdirAll(conflictDir, 0o700); err != nil {
		t.Fatalf("mkdir conflicts: %v", err)
	}
	artifact := filepath.Join(conflictDir, "foo.md."+docID+".conflict.md")
	if err := os.WriteFile(artifact, []byte("local conflicted body"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sync/conflicts", nil)
	w := httptest.NewRecorder()
	s.handleSyncConflicts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var conflicts []syncConflictResponse
	if err := json.NewDecoder(w.Body).Decode(&conflicts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d (%+v)", len(conflicts), conflicts)
	}
	c := conflicts[0]
	if c.Workspace != "ws" {
		t.Errorf("workspace: got %q want ws", c.Workspace)
	}
	if c.Path != "notes/foo.md" {
		t.Errorf("path: got %q want notes/foo.md", c.Path)
	}
	if c.DocumentID != docID {
		t.Errorf("document_id: got %q want %q", c.DocumentID, docID)
	}
	if c.ArtifactContent != "local conflicted body" {
		t.Errorf("artifact content: got %q", c.ArtifactContent)
	}
	if c.BaseContent != "base body" {
		t.Errorf("base content: got %q want %q", c.BaseContent, "base body")
	}
}

func TestHandleSyncConflictsEmptyWhenNoStore(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	s := newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))

	req := httptest.NewRequest(http.MethodGet, "/api/sync/conflicts", nil)
	w := httptest.NewRecorder()
	s.handleSyncConflicts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for missing conflict store, got %d", w.Code)
	}
	var conflicts []syncConflictResponse
	if err := json.NewDecoder(w.Body).Decode(&conflicts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected empty list, got %d", len(conflicts))
	}
}

// serveAdoptSyncStub mimics grove-syncd for the adopt endpoint test: the
// capabilities handshake, a snapshot manifest holding the target doc at head,
// and the head blob.
func serveAdoptSyncStub(t *testing.T, workspace, docID, docPath string, version int64, head []byte) *httptest.Server {
	t.Helper()
	hash := sha256.Sum256(head)
	hashHex := hex.EncodeToString(hash[:])
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/capabilities":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
				Capabilities: syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersion}},
			})
		case "/sync/snapshot":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(syncproto.SnapshotManifest{
				Workspace: workspace,
				Cursor:    version,
				Documents: []syncproto.DocumentSnapshot{
					{ID: docID, Path: docPath, Version: version, Hash: hashHex, Size: int64(len(head))},
				},
			})
		case "/sync/history/blob":
			_, _ = w.Write(head)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
}

// writeSyncConfig points config.LoadSyncConfig (used by historyClient) at the
// stub server. GROVE_HOME must already be set to a temp dir.
func writeSyncConfig(t *testing.T, serverURL string) {
	t.Helper()
	dir := paths.ConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "server = \"" + serverURL + "\"\ntoken = \"test-token\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sync.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleSyncAdopt(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	t.Setenv("GROVE_SYNC_TOKEN", "")

	head := []byte("---\ntitle: note\n---\nmerged server head body\n")
	sum := sha256.Sum256(head)
	headHash := hex.EncodeToString(sum[:])
	srv := serveAdoptSyncStub(t, "ws", "doc-1", "inbox/note.md", 8, head)
	defer srv.Close()
	writeSyncConfig(t, srv.URL)

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(false)
	s.SetSyncDB(db)

	// A diverged doc: the local file lags the merged head; content_hash tracks
	// the (untouched) disk file, last_synced already rolled to the head.
	localHash := "abc123localdiskhash"
	if err := db.InsertDocument(&syncdb.Document{
		DocumentID: "doc-1", Workspace: "ws", Path: "inbox/note.md",
		ContentHash: localHash, LastSyncedHash: headHash, LastSyncedVersion: 8,
		BaseContent: head, Diverged: true,
	}); err != nil {
		t.Fatalf("InsertDocument: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"workspace": "ws", "path": "inbox/note.md"})
	req := httptest.NewRequest(http.MethodPost, "/api/sync/adopt", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSyncAdopt(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), head) {
		t.Fatalf("response body = %q, want head content %q", w.Body.Bytes(), head)
	}
	if got := w.Header().Get("X-Content-Hash"); got != headHash {
		t.Fatalf("X-Content-Hash = %q, want %q", got, headHash)
	}

	// The DB rolled to the head and cleared diverged.
	doc, err := db.GetDocumentByPath("ws", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.Diverged {
		t.Fatal("adopt must clear the diverged flag")
	}
	if doc.LastSyncedVersion != 8 || doc.LastSyncedHash != headHash || string(doc.BaseContent) != string(head) {
		t.Fatalf("adopt must roll base_content/last_synced_* to head: v%d hash=%q", doc.LastSyncedVersion, doc.LastSyncedHash)
	}
}

func TestHandleSyncAdoptNotTracked(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	s := newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))

	body, _ := json.Marshal(map[string]string{"workspace": "ws", "path": "inbox/missing.md"})
	req := httptest.NewRequest(http.MethodPost, "/api/sync/adopt", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSyncAdopt(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for untracked path, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestHandleSyncAdoptConflictsWithPendingPush(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(false)
	s.SetSyncDB(db)

	if err := db.InsertDocument(&syncdb.Document{
		DocumentID: "doc-1", Workspace: "ws", Path: "inbox/note.md",
		ContentHash: "local", LastSyncedHash: "head", LastSyncedVersion: 8, Diverged: true,
	}); err != nil {
		t.Fatalf("InsertDocument: %v", err)
	}
	// An unpushed (merged) entry still queued for this path: adopting past it
	// would drop the user's merged-in lines from the hub, so adopt must 409.
	if _, err := db.EnqueueOutbox(&syncdb.OutboxEntry{
		DocumentID: "doc-1", Workspace: "ws", EventType: "document.updated",
		Path: "inbox/note.md", ContentHash: "merged", Payload: "merged body",
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"workspace": "ws", "path": "inbox/note.md"})
	req := httptest.NewRequest(http.MethodPost, "/api/sync/adopt", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSyncAdopt(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 when an outbox entry still exists, got %d (%s)", w.Code, w.Body.String())
	}
}
