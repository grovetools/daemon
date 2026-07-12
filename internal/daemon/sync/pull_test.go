package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
