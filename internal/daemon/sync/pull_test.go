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
