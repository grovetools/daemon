package sync

// Regression tests for the containment findings of the W3.2 review (F2, F3,
// F11). Each reproduces a probe that succeeded in DESTROYING a file outside
// the notespace root against the reviewed tree.
//
// The commit added requireUnderRoot to every write and move call site and to
// neither of the two delete call sites, which is the opposite of the risk
// gradient: a write at least has to name a document the server can produce
// content for, while applyPrefixDelete needs no precondition at all — no DB
// row, no hash, no prior state — and recurses.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/syncproto"
)

// containmentFixture builds a notespace root with a victim file and directory
// sitting OUTSIDE it, as a sibling of the root — the shape "../victim" reaches.
type containmentFixture struct {
	root      string
	victimDir string
	victimFwd string
}

func newContainmentFixture(t *testing.T) *containmentFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "notespace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	victimDir := filepath.Join(base, "victim")
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victimDir, "keep.md"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	victimFwd := filepath.Join(base, "precious.txt")
	if err := os.WriteFile(victimFwd, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &containmentFixture{root: root, victimDir: victimDir, victimFwd: victimFwd}
}

// F2 (CRITICAL): one pulled prefix_deleted event recursively removed any
// directory the daemon could reach, and reported success while doing it.
func TestPrefixDeleteCannotEscapeTheNotespaceRoot(t *testing.T) {
	fx := newContainmentFixture(t)
	db := openTestDB(t)
	p := newTestPullPipeline(t, db)

	err := p.applyEvent(context.Background(), fx.root, &syncproto.SyncEvent{
		Type: syncproto.EventPrefixDeleted,
		Path: "../victim",
	})
	if err == nil {
		t.Fatal("a prefix_deleted event escaping the root was accepted")
	}
	if _, statErr := os.Stat(fx.victimDir); statErr != nil {
		t.Fatalf("a pulled prefix_deleted event removed %s, outside the notespace root", fx.victimDir)
	}
}

// A prefix delete addressing the root itself would take the whole tree. Sync
// never creates a notespace root, so it must not remove one either.
func TestPrefixDeleteCannotRemoveTheNotespaceRootItself(t *testing.T) {
	fx := newContainmentFixture(t)
	db := openTestDB(t)
	p := newTestPullPipeline(t, db)

	for _, path := range []string{"", ".", "./"} {
		err := p.applyEvent(context.Background(), fx.root, &syncproto.SyncEvent{
			Type: syncproto.EventPrefixDeleted,
			Path: path,
		})
		if err == nil {
			t.Fatalf("a prefix_deleted event addressing the root itself (%q) was accepted", path)
		}
		if _, statErr := os.Stat(fx.root); statErr != nil {
			t.Fatalf("prefix_deleted %q removed the notespace root", path)
		}
	}
}

// F3 (HIGH): the same gap on document_deleted. It needs a tracked row whose
// path escapes, which is obtainable — AdoptDocument accepted an escaping
// manifest path without complaint and snaphotResync feeds it doc.Path straight
// from the server manifest. The containment check belongs at the delete, not
// at the odds of guessing a hash.
func TestDocumentDeleteCannotEscapeTheNotespaceRoot(t *testing.T) {
	fx := newContainmentFixture(t)
	db := openTestDB(t)
	p := newTestPullPipeline(t, db)

	// Poison the identity map directly: a row whose path climbs out of the
	// tree, with last_synced_hash matching the victim's real content so the
	// edit-wins-over-delete branch does not save it.
	content := []byte("precious")
	if err := db.UpsertDocument(&Document{
		DocumentID:        "doc-escape",
		Notespace:         "default",
		Path:              "../precious.txt",
		ContentHash:       sha(content),
		LastSyncedHash:    sha(content),
		LastSyncedVersion: 1,
		BaseContent:       content,
		UpdatedAt:         time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	err := p.applyEvent(context.Background(), fx.root, &syncproto.SyncEvent{
		Type: syncproto.EventDocumentDeleted,
		Path: "../precious.txt",
	})
	if err == nil {
		t.Fatal("a document_deleted event escaping the root was accepted")
	}
	if _, statErr := os.Stat(fx.victimFwd); statErr != nil {
		t.Fatalf("a pulled document_deleted event removed %s, outside the notespace root", fx.victimFwd)
	}
}

// The other event shapes are gated by the same central check, so a new event
// type cannot be added without one.
func TestEveryEventShapeIsContained(t *testing.T) {
	fx := newContainmentFixture(t)
	db := openTestDB(t)
	p := newTestPullPipeline(t, db)

	cases := []struct {
		name string
		ev   *syncproto.SyncEvent
	}{
		{"created", &syncproto.SyncEvent{Type: syncproto.EventDocumentCreated, Path: "../escape.md", DocumentID: "d1"}},
		{"updated", &syncproto.SyncEvent{Type: syncproto.EventDocumentUpdated, Path: "../escape.md", DocumentID: "d1"}},
		{"moved", &syncproto.SyncEvent{Type: syncproto.EventDocumentMoved, Path: "in.md", PrevPath: "../escape.md"}},
		{"moved-dest", &syncproto.SyncEvent{Type: syncproto.EventDocumentMoved, Path: "../escape.md", PrevPath: "in.md"}},
		{"prefix-moved", &syncproto.SyncEvent{Type: syncproto.EventPrefixMoved, Path: "../out", PrevPath: "in"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.applyEvent(context.Background(), fx.root, tc.ev); err == nil {
				t.Fatalf("%s event escaping the root was accepted", tc.name)
			}
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(fx.root), "escape.md")); statErr == nil {
				t.Fatalf("%s event wrote outside the notespace root", tc.name)
			}
		})
	}
}

// F11: requireUnderRoot was lexical only. filepath.Rel cleans
// "notes/link/../../../etc/x" without ever asking what `link` points at, so a
// symlink inside the tree is a legitimate route out of it and the write
// follows the link. Under the interim trust model this is hardening rather
// than a live exploit, but it is the residual after F2/F3 are fixed.
func TestContainmentRejectsASymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "notespace")
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink INSIDE the notespace pointing out of it.
	link := filepath.Join(root, "notes", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	escaping := filepath.Join(root, "notes", "link", "stolen.md")
	if err := requireUnderRoot(root, escaping); err == nil {
		t.Fatal("a path traversing a symlink out of the root passed containment")
	}

	// The write helper refuses it too, and nothing lands outside.
	if err := writeFileUnderRoot(root, escaping, []byte("x"), time.Time{}); err == nil {
		t.Fatal("writeFileUnderRoot followed a symlink out of the notespace root")
	}
	if _, err := os.Stat(filepath.Join(outside, "stolen.md")); err == nil {
		t.Fatalf("a write escaped through a symlink into %s", outside)
	}
}

// Containment must not become so strict that ordinary layouts break. A
// notespace root reached THROUGH a symlink is a legitimate, common layout —
// it is exactly the macOS /var -> /private/var aliasing the rest of the daemon
// tolerates — so both sides are canonicalized before comparison.
func TestContainmentAllowsARootReachedThroughASymlink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real-notebook", "notespaces", "alpha")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base, "linked")
	if err := os.Symlink(filepath.Join(base, "real-notebook"), linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	root := filepath.Join(linked, "notespaces", "alpha")
	// A document that does not exist yet, in a directory that does not exist
	// yet — the ordinary case for an incoming write.
	dst := filepath.Join(root, "plans", "2026", "note.md")
	if err := requireUnderRoot(root, dst); err != nil {
		t.Fatalf("a root reached through a symlink was rejected: %v", err)
	}
	if err := writeFileUnderRoot(root, dst, []byte("hello"), time.Time{}); err != nil {
		t.Fatalf("writeFileUnderRoot through a symlinked root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "plans", "2026", "note.md")); err != nil {
		t.Fatalf("the write did not land in the real tree: %v", err)
	}

	// Deletes work through the same layout.
	if err := deleteFileUnderRoot(root, dst); err != nil {
		t.Fatalf("deleteFileUnderRoot through a symlinked root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "plans", "2026", "note.md")); !os.IsNotExist(err) {
		t.Fatal("the contained delete did not remove the file")
	}
}
