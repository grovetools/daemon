package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	gosync "sync"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// seedSyncedDoc writes a synced document to disk and the identity map:
// disk == last-synced == merge base, version 1.
func seedSyncedDoc(t *testing.T, db *DB, root, relPath string, content []byte) {
	t.Helper()
	full := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertDocument(&Document{
		DocumentID:        "doc-1",
		Workspace:         "default",
		Path:              relPath,
		ContentHash:       sha(content),
		LastSyncedHash:    sha(content),
		LastSyncedVersion: 1,
		BaseContent:       content,
		UpdatedAt:         time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func newTestPullPipeline(t *testing.T, db *DB) *PullPipeline {
	t.Helper()
	// Conflict artifacts land under paths.StateDir(); keep them hermetic.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return NewPullPipeline(&config.SyncWorkspace{Name: "default"}, nil, db, logging.NewUnifiedLogger("test.pull"))
}

// TestApplyUpdatePreservesUnpushedLocalEdit is the silent-data-loss
// regression: a remote update arriving while the local file holds an
// unpushed edit must NOT fast-forward over it. The old dirty-check compared
// disk against doc.ContentHash — which the watcher refreshes on every local
// save — so a locally-edited file always looked "clean" and the edit was
// clobbered by the remote version (observed live in the cluster playground:
// concurrent different-line edits on dev-a/dev-b, loser's edit vanished).
func TestApplyUpdatePreservesUnpushedLocalEdit(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	base := []byte("---\ntitle: note\n---\nshared base body\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", base)

	// Local edit (as the watcher would see it: ContentHash tracks the edit)
	local := []byte("---\ntitle: note\n---\nshared base body\nlocal line\n")
	if err := os.WriteFile(filepath.Join(root, "inbox/note.md"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	doc.ContentHash = sha(local) // watcher's update on local save
	if err := db.UpdateDocument(&Document{
		DocumentID: "doc-1", ContentHash: sha(local),
		LastSyncedHash: sha(base), LastSyncedVersion: 1, BaseContent: base,
	}); err != nil {
		t.Fatal(err)
	}

	// Remote update: the other machine changed a different part of the body
	remote := []byte("---\ntitle: note\n---\nshared base body\nremote line\n")
	p := newTestPullPipeline(t, db)
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentUpdated, Workspace: "default",
		DocumentID: "doc-1", Path: "inbox/note.md",
		Content: remote, ContentHash: sha(remote), Version: 2,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "inbox/note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == string(remote) {
		t.Fatal("local unpushed edit was overwritten by remote content (silent data loss)")
	}
	if string(got) != string(local) {
		t.Fatalf("local file unexpectedly rewritten: %q", got)
	}
}

// TestApplyUpdateMergesDisjointRemoteEdits: a remote update arriving on a
// dirty local file 3-way merges via diff3 when the edits touch different
// base regions — both edits land on disk, no conflict artifact, and the doc
// record rolls to the remote head while ContentHash tracks the merged bytes.
// Any parked push of the pre-merge local edit is retargeted at the merged
// content (a stale entry hash would fail the server's integrity check and be
// dropped, silently losing the local half of the merge).
func TestApplyUpdateMergesDisjointRemoteEdits(t *testing.T) {
	t.Setenv("GROVE_HOME", "") // keep paths.StateDir() on the XDG override below
	db := openTestDB(t)
	root := t.TempDir()

	base := []byte("---\ntitle: note\n---\nline one\nline two\nline three\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", base)

	// Local unpushed edit on line one (watcher tracked it and queued a push).
	local := []byte("---\ntitle: note\n---\nLOCAL one\nline two\nline three\n")
	if err := os.WriteFile(filepath.Join(root, "inbox/note.md"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDocument(&Document{
		DocumentID: "doc-1", ContentHash: sha(local),
		LastSyncedHash: sha(base), LastSyncedVersion: 1, BaseContent: base,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID: "doc-1", Workspace: "default",
		EventType: syncproto.EventDocumentUpdated,
		Path:      "inbox/note.md", ContentHash: sha(local),
	}); err != nil {
		t.Fatal(err)
	}

	// Remote edit on line three: disjoint from the local edit.
	remote := []byte("---\ntitle: note\n---\nline one\nline two\nREMOTE three\n")
	p := newTestPullPipeline(t, db)
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentUpdated, Workspace: "default",
		DocumentID: "doc-1", Path: "inbox/note.md",
		Content: remote, ContentHash: sha(remote), Version: 2,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	want := "---\ntitle: note\n---\nLOCAL one\nline two\nREMOTE three\n"
	got, err := os.ReadFile(filepath.Join(root, "inbox/note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("merged disk content = %q, want %q (both edits present)", got, want)
	}

	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 2 || doc.LastSyncedHash != sha(remote) || string(doc.BaseContent) != string(remote) {
		t.Fatalf("doc record must roll to the remote head: v%d hash=%q", doc.LastSyncedVersion, doc.LastSyncedHash)
	}
	if doc.ContentHash != sha([]byte(want)) {
		t.Fatalf("content_hash must track merged bytes, got %q", doc.ContentHash)
	}

	entries, err := db.ListOutbox("default", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected the queued push to remain, got %d (err=%v)", len(entries), err)
	}
	if entries[0].ContentHash != sha([]byte(want)) {
		t.Fatalf("queued push not retargeted at merged content: %q", entries[0].ContentHash)
	}

	// Disjoint merge must not record a conflict artifact.
	artifact := filepath.Join(os.Getenv("XDG_STATE_HOME"), "grove", "sync", "conflicts", "default", "inbox/note.md.doc-1.conflict.md")
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("clean pull merge must not write a conflict artifact (stat err=%v)", err)
	}
}

// TestApplyUpdateOverlappingRemoteEditConflicts: a remote update overlapping
// the dirty local edit keeps the local content and records a conflict
// artifact — exactly the pre-diff3 parking behavior.
func TestApplyUpdateOverlappingRemoteEditConflicts(t *testing.T) {
	t.Setenv("GROVE_HOME", "")
	db := openTestDB(t)
	root := t.TempDir()

	base := []byte("---\ntitle: note\n---\nline one\nline two\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", base)

	local := []byte("---\ntitle: note\n---\nLOCAL one\nline two\n")
	if err := os.WriteFile(filepath.Join(root, "inbox/note.md"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDocument(&Document{
		DocumentID: "doc-1", ContentHash: sha(local),
		LastSyncedHash: sha(base), LastSyncedVersion: 1, BaseContent: base,
	}); err != nil {
		t.Fatal(err)
	}

	remote := []byte("---\ntitle: note\n---\nREMOTE one\nline two\n")
	p := newTestPullPipeline(t, db)
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentUpdated, Workspace: "default",
		DocumentID: "doc-1", Path: "inbox/note.md",
		Content: remote, ContentHash: sha(remote), Version: 2,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "inbox/note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(local) {
		t.Fatalf("overlapping conflict must keep local content, got %q", got)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 1 {
		t.Fatalf("conflict must not advance the doc record, got v%d", doc.LastSyncedVersion)
	}
	artifact := filepath.Join(os.Getenv("XDG_STATE_HOME"), "grove", "sync", "conflicts", "default", "inbox/note.md.doc-1.conflict.md")
	content, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("expected conflict artifact: %v", err)
	}
	if string(content) != string(local) {
		t.Fatalf("artifact must hold local content, got %q", content)
	}
}

// TestApplyUpdateFastForwardsCleanLocal: when the local file matches the
// last server-confirmed content, the remote update applies directly.
func TestApplyUpdateFastForwardsCleanLocal(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	base := []byte("---\ntitle: note\n---\nv1 body\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", base)

	remote := []byte("---\ntitle: note\n---\nv2 body\n")
	p := newTestPullPipeline(t, db)
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentUpdated, Workspace: "default",
		DocumentID: "doc-1", Path: "inbox/note.md",
		Content: remote, ContentHash: sha(remote), Version: 2,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "inbox/note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(remote) {
		t.Fatalf("clean local file should fast-forward to remote, got %q", got)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/note.md")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.LastSyncedVersion != 2 || doc.LastSyncedHash != sha(remote) {
		t.Fatalf("doc record not advanced: version=%d hash=%s", doc.LastSyncedVersion, doc.LastSyncedHash)
	}
}

// TestApplyCreateRestoresMtime is the replica half of the end-to-end mtime
// round trip: a created event carrying the origin's file mtime materializes
// the file with that mtime restored via os.Chtimes (the hydration-burst
// regression: every replica file used to show the write time).
func TestApplyCreateRestoresMtime(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	p := newTestPullPipeline(t, db)

	mtime := time.Date(2026, 7, 11, 8, 15, 30, 0, time.Local)
	content := []byte("---\ntitle: note\n---\nbody\n")
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentCreated, Workspace: "default",
		DocumentID: "doc-1", Path: "inbox/new.md",
		Content: content, ContentHash: sha(content), Version: 1,
		Mtime: mtime,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	fi, err := os.Stat(filepath.Join(root, "inbox/new.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Fatalf("replica mtime = %v, want origin mtime %v", fi.ModTime(), mtime)
	}
}

// TestApplyCreateEmptyDocumentSkipsBlobFetch is the B10 client-side
// regression: a legitimately empty document arrives with no content and the
// empty-content hash, and must materialize as a zero-byte file directly —
// NOT be mistaken for blob-tier. The pipeline's nil client makes the
// assertion structural: any FetchBlob attempt panics.
func TestApplyCreateEmptyDocumentSkipsBlobFetch(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	p := newTestPullPipeline(t, db)

	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentCreated, Workspace: "default",
		DocumentID: "doc-1", Path: "rules/placeholder.md.rules",
		ContentHash: emptyContentHash, Version: 1,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "rules/placeholder.md.rules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero-byte file, got %d bytes", len(got))
	}
	doc, err := db.GetDocumentByPath("default", "rules/placeholder.md.rules")
	if err != nil || doc == nil {
		t.Fatalf("GetDocumentByPath: doc=%v err=%v", doc, err)
	}
	if doc.LastSyncedHash != emptyContentHash || doc.LastSyncedVersion != 1 {
		t.Fatalf("unexpected doc record: %+v", doc)
	}
}

// TestApplyUpdateFastForwardRestoresMtime: a clean fast-forward (local file
// matches the last server-confirmed content) rewrites the file to the remote
// bytes AND restores the remote's mtime with them.
func TestApplyUpdateFastForwardRestoresMtime(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	base := []byte("---\ntitle: note\n---\nv1 body\n")
	seedSyncedDoc(t, db, root, "inbox/note.md", base)
	p := newTestPullPipeline(t, db)

	mtime := time.Date(2026, 7, 11, 12, 0, 5, 0, time.Local)
	remote := []byte("---\ntitle: note\n---\nv2 body\n")
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentUpdated, Workspace: "default",
		DocumentID: "doc-1", Path: "inbox/note.md",
		Content: remote, ContentHash: sha(remote), Version: 2,
		Mtime: mtime,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	fi, err := os.Stat(filepath.Join(root, "inbox/note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Fatalf("replica mtime = %v, want origin mtime %v", fi.ModTime(), mtime)
	}
}

// TestApplyCreateZeroMtimeKeepsWriteTime is the backward-compatibility gate:
// an event from a pre-mtime server/client (zero Mtime) must behave exactly as
// today — the file carries its write time, no Chtimes into the past.
func TestApplyCreateZeroMtimeKeepsWriteTime(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	p := newTestPullPipeline(t, db)

	before := time.Now().Add(-time.Minute)
	content := []byte("---\ntitle: legacy\n---\nbody\n")
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentCreated, Workspace: "default",
		DocumentID: "doc-legacy", Path: "inbox/legacy.md",
		Content: content, ContentHash: sha(content), Version: 1,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	fi, err := os.Stat(filepath.Join(root, "inbox/legacy.md"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Before(before) {
		t.Fatalf("zero-mtime event must keep the write time, got %v", fi.ModTime())
	}
}

// ---------------------------------------------------------------------------
// C20 regression: 410 Gone + snapshot_required must reach the resync branch.
// ---------------------------------------------------------------------------

// gcStoreStub is a watermark-faithful pull-side server stub, mirroring
// sync/pkg/server's exact wire behavior the way push_test.go's
// serveOCCStoreStub mirrors the OCC store:
//
//   - GET /sync/events with cursor < watermark answers 410 Gone with a
//     decodable PullResponse{snapshot_required:true, cursor, error} body
//     (handleEvents' C20 contract);
//   - cursor >= watermark answers 200 with an empty tail at that cursor;
//   - GET /sync/snapshot serves the manifest (cursor + document heads);
//   - GET /sync/history/blob serves head content for the resync fetch.
//
// pulls records every /sync/events cursor in arrival order.
type gcStoreStub struct {
	watermark int64
	manifest  syncproto.SnapshotManifest
	contents  map[string][]byte // document_id -> head content

	mu    gosync.Mutex
	pulls []int64
}

func (s *gcStoreStub) recordedPulls() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.pulls...)
}

func serveGCStoreStub(t *testing.T, s *gcStoreStub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/sync/events", func(w http.ResponseWriter, r *http.Request) {
		cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
		s.mu.Lock()
		s.pulls = append(s.pulls, cursor)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if cursor < s.watermark {
			// The server's exact 410 body shape (handleEvents).
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(syncproto.PullResponse{
				SnapshotRequired: true,
				Cursor:           cursor,
				Error:            "cursor predates the event retention window; resync from snapshot",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(syncproto.PullResponse{
			Events: []syncproto.SyncEvent{}, Cursor: cursor,
		})
	})
	mux.HandleFunc("/sync/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.manifest)
	})
	mux.HandleFunc("/sync/history/blob", func(w http.ResponseWriter, r *http.Request) {
		content, ok := s.contents[r.URL.Query().Get("document_id")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(content)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestPullEventsDecodes410SnapshotRequired: the client half of the C20
// contract. A 410 Gone answer is a protocol response, not a transport error —
// PullEvents must decode its PullResponse body and surface
// SnapshotRequired=true (the bug: any non-200 returned an error, so the pull
// loop's resync branch was unreachable and a below-watermark client spun on
// "pull failed ... status 410" every backoff forever). Any other non-200
// stays an error.
func TestPullEventsDecodes410SnapshotRequired(t *testing.T) {
	stub := &gcStoreStub{watermark: 5}
	srv := serveGCStoreStub(t, stub)
	client := NewClient(ClientConfig{ServerURL: srv.URL, Logger: logging.NewUnifiedLogger("test.pull")})

	resp, err := client.PullEvents(context.Background(), "default", 0, 100, 0)
	if err != nil {
		t.Fatalf("PullEvents must decode a 410 body, got error: %v", err)
	}
	if !resp.SnapshotRequired {
		t.Fatalf("expected SnapshotRequired=true from the 410 body, got %+v", resp)
	}

	// Negative control: every other non-200 remains an error.
	boom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer boom.Close()
	client = NewClient(ClientConfig{ServerURL: boom.URL, Logger: logging.NewUnifiedLogger("test.pull")})
	if _, err := client.PullEvents(context.Background(), "default", 0, 100, 0); err == nil {
		t.Fatal("PullEvents must still error on non-200/non-410 statuses")
	}
}

// TestRunPullLoopSnapshotResyncOn410 is the loop-level regression: a client
// whose cursor sits below the GC watermark (wiped sync.db → cursor 0) must
// take RunPullLoop's SnapshotRequired branch — snapshot manifest fetch, head
// materialization with the manifest's fidelity mtime restored, cursor
// persisted at the manifest cursor — and then resume tailing FROM that
// cursor. Exactly one below-watermark pull is allowed: resuming at 0 after
// the resync (the old code) re-trips the 410 forever.
func TestRunPullLoopSnapshotResyncOn410(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	content := []byte("---\ntitle: gc\n---\nsurvived the watermark\n")
	mtime := time.Date(2011, 3, 13, 7, 7, 24, 444444444, time.Local)
	stub := &gcStoreStub{
		watermark: 5,
		manifest: syncproto.SnapshotManifest{
			Workspace: "default",
			Cursor:    5,
			Documents: []syncproto.DocumentSnapshot{{
				ID: "doc-gc", Path: "inbox/gc-note.md", Version: 3,
				Hash: sha(content), Size: int64(len(content)), Mtime: mtime,
			}},
		},
		contents: map[string][]byte{"doc-gc": content},
	}
	srv := serveGCStoreStub(t, stub)

	client := NewClient(ClientConfig{ServerURL: srv.URL, Logger: logging.NewUnifiedLogger("test.pull")})
	p := newTestPullPipeline(t, db)
	p.client = client
	p.pollWait = 0

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.RunPullLoop(ctx, root) }()

	// Converged when: file on disk with the manifest mtime, cursor persisted
	// at the manifest cursor, and the loop has pulled at/above the watermark.
	filePath := filepath.Join(root, "inbox/gc-note.md")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fi, ferr := os.Stat(filePath)
		cur, _ := db.GetWorkspaceCursor("default")
		pulls := stub.recordedPulls()
		if ferr == nil && fi.ModTime().Equal(mtime) && cur == 5 &&
			len(pulls) >= 2 && pulls[len(pulls)-1] == 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunPullLoop did not stop on context cancel")
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("snapshot resync never materialized the document: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("materialized content = %q, want %q", got, content)
	}
	fi, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Fatalf("materialized mtime = %v, want the manifest fidelity mtime %v", fi.ModTime(), mtime)
	}
	if cur, _ := db.GetWorkspaceCursor("default"); cur != 5 {
		t.Fatalf("workspace cursor = %d, want the manifest cursor 5", cur)
	}
	doc, err := db.GetDocumentByPath("default", "inbox/gc-note.md")
	if err != nil || doc == nil {
		t.Fatalf("document not tracked after resync: %v", err)
	}
	if doc.DocumentID != "doc-gc" || doc.LastSyncedVersion != 3 {
		t.Fatalf("tracked identity = %s v%d, want doc-gc v3", doc.DocumentID, doc.LastSyncedVersion)
	}

	// Exactly one below-watermark pull: the first (cursor 0, answered 410).
	// Every pull after the resync must resume at the manifest cursor — a
	// second below-watermark pull is the endless 410→resync loop.
	pulls := stub.recordedPulls()
	if len(pulls) < 2 {
		t.Fatalf("expected the loop to keep tailing after the resync, got pulls %v", pulls)
	}
	below := 0
	for i, c := range pulls {
		if c < stub.watermark {
			below++
			if i > 0 {
				t.Fatalf("pull %d used below-watermark cursor %d after the resync (410 spin): %v", i, c, pulls)
			}
		}
	}
	if below != 1 {
		t.Fatalf("expected exactly one below-watermark pull, got %d in %v", below, pulls)
	}
	if fmt.Sprintf("%d", pulls[0]) != "0" {
		t.Fatalf("first pull cursor = %d, want the wiped client's 0", pulls[0])
	}
}

// TestApplyMoveRestoresMtime: a moved event carrying the origin's mtime
// restores it on the renamed replica file (a bare rename would keep the
// replica's old timestamp).
func TestApplyMoveRestoresMtime(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	base := []byte("---\ntitle: note\n---\nbody\n")
	seedSyncedDoc(t, db, root, "inbox/old.md", base)
	p := newTestPullPipeline(t, db)

	mtime := time.Date(2026, 7, 11, 17, 45, 0, 0, time.Local)
	ev := &syncproto.SyncEvent{
		Type: syncproto.EventDocumentMoved, Workspace: "default",
		DocumentID: "doc-1", PrevPath: "inbox/old.md", Path: "inbox/new.md",
		Version: 2, Mtime: mtime,
	}
	if err := p.applyEvent(context.Background(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	fi, err := os.Stat(filepath.Join(root, "inbox/new.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Fatalf("moved replica mtime = %v, want origin mtime %v", fi.ModTime(), mtime)
	}
}
