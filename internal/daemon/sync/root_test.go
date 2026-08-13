package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

func TestRequireNotespaceRootRefusals(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		root    string
		refused bool
	}{
		{name: "existing directory", root: dir},
		{name: "unresolved root", root: "", refused: true},
		{name: "relative root", root: "notebooks/default", refused: true},
		{name: "absent root", root: filepath.Join(dir, "gone"), refused: true},
		{name: "regular file", root: file, refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireNotespaceRoot(tc.root)
			if IsMissingRoot(err) != tc.refused {
				t.Fatalf("RequireNotespaceRoot(%q) = %v, refused=%v", tc.root, err, tc.refused)
			}
		})
	}
}

func TestWriteFileUnderRootNeverResurrectsTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "notebooks", "default", "notespaces", "default")

	err := writeFileUnderRoot(root, filepath.Join(root, "inbox", "note.md"), []byte("hello"), time.Time{})
	if !IsMissingRoot(err) {
		t.Fatalf("write into a missing root = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatal("the refusal created the notespace root anyway")
	}

	// With the root materialized, directories BENEATH it are still created.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileUnderRoot(root, filepath.Join(root, "inbox", "note.md"), []byte("hello"), time.Time{}); err != nil {
		t.Fatalf("write into an existing root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "inbox", "note.md")); err != nil {
		t.Fatal(err)
	}

	// A server-supplied path may not climb out of the notespace.
	escape := filepath.Join(filepath.Dir(root), "sibling", "stolen.md")
	if err := writeFileUnderRoot(root, escape, []byte("x"), time.Time{}); err == nil {
		t.Fatal("a path outside the notespace root was accepted")
	}
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatal("the escaping write landed on disk")
	}
}

// A vanished notespace root used to look exactly like "every document was
// deleted" to the anti-entropy sweep, which would have replicated a
// notespace-wide deletion to the server and from there to every machine.
func TestAntiEntropyRefusesVanishedRootWithoutEnqueueingDeletes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "notespaces", "default")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const ns = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := db.InsertDocument(&Document{
		DocumentID:        "doc-1",
		Notespace:         ns,
		Path:              "inbox/note.md",
		ContentHash:       "abc",
		LastSyncedHash:    "abc",
		LastSyncedVersion: 3,
	}); err != nil {
		t.Fatal(err)
	}

	// The client is deliberately nil: the refusal has to happen before the
	// pass talks to anything.
	pass := NewAntiEntropyPass(db, nil, ns, root, NewDocSpace(nil), logging.NewUnifiedLogger("test"), AntiEntropyConfig{})
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := pass.Run(context.Background()); !IsMissingRoot(err) {
		t.Fatalf("anti-entropy over a vanished root = %v, want a refusal", err)
	}

	outbox, err := db.ListOutbox(ns, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 0 {
		t.Fatalf("a vanished root enqueued %d events: %+v", len(outbox), outbox)
	}
	if doc, err := db.GetDocumentByPath(ns, "inbox/note.md"); err != nil || doc == nil {
		t.Fatalf("the tracked document was dropped: doc=%+v err=%v", doc, err)
	}
}

func TestPullApplyRefusesMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "notespaces", "default")
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pull := NewPullPipeline(&config.SyncWorkspace{Name: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Pull: true},
		nil, db, logging.NewUnifiedLogger("test"))
	err = pull.applyEvent(context.Background(), root, &syncproto.SyncEvent{
		Type:        syncproto.EventDocumentCreated,
		DocumentID:  "doc-1",
		Path:        "inbox/note.md",
		Version:     1,
		Content:     []byte("hello"),
		ContentHash: hashContent([]byte("hello")),
	})
	if !IsMissingRoot(err) {
		t.Fatalf("apply into a missing root = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatal("the refused apply created the notespace root")
	}
	if docs, err := db.ListDocuments("01ARZ3NDEKTSV4RRFFQ69G5FAV"); err != nil || len(docs) != 0 {
		t.Fatalf("the refused apply recorded documents: %+v err=%v", docs, err)
	}
}

func TestRefuseMissingRootReportsOncePerEpisode(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	root := filepath.Join(t.TempDir(), "notespaces", "default")
	const ns = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	var conflicts []string
	pull := NewPullPipeline(&config.SyncWorkspace{Name: ns}, nil, nil, logging.NewUnifiedLogger("test"))
	pull.OnConflict = func(kind, notespace, path, documentID, detail string) {
		conflicts = append(conflicts, kind)
	}

	for range 3 {
		if err := pull.refuseMissingRoot(context.Background(), root); !IsMissingRoot(err) {
			t.Fatalf("refuseMissingRoot = %v, want a refusal", err)
		}
	}
	if len(conflicts) != 1 || conflicts[0] != ConflictKindMissingRoot {
		t.Fatalf("conflicts = %v, want one %s", conflicts, ConflictKindMissingRoot)
	}
	matches, err := filepath.Glob(filepath.Join(state, "grove", "sync", "conflicts", ns, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("missing-root evidence files = %v, want exactly one", matches)
	}

	// Materializing the root clears the episode, so a recurrence reports again.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pull.refuseMissingRoot(context.Background(), root); err != nil {
		t.Fatalf("materialized root still refused: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := pull.refuseMissingRoot(context.Background(), root); !IsMissingRoot(err) {
		t.Fatalf("recurrence = %v, want a refusal", err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("a recurrence must be reported again: %v", conflicts)
	}
}
