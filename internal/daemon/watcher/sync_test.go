package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/syncproto"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// newTestSyncHandler builds a SyncHandler against a temp workspace root and
// a temp sync.db, with the watch table injected directly (no workspace
// discovery machinery needed).
func newTestSyncHandler(t *testing.T, quietMs, maxWaitMs int) (*SyncHandler, string) {
	t.Helper()

	wsRoot := t.TempDir()
	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, nil, nil, db, quietMs, maxWaitMs)
	h.watchedPaths = map[string]*syncWatch{
		wsRoot: {workspace: "testws", root: wsRoot, space: syncdb.NewDocSpace(nil)},
	}
	return h, wsRoot
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The default exclusion-manifest table formerly tested here (TestSyncExclusion
// Manifest against syncExcluded) moved with the logic into the sync package's
// DocSpace — see daemon/internal/daemon/sync/docspace_test.go. MatchesEvent
// below still exercises the manifest end-to-end via the DocSpace delegation.

func TestSyncMatchesEvent(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)

	cases := []struct {
		path    string
		op      fsnotify.Op
		matches bool
	}{
		{filepath.Join(wsRoot, "notes", "inbox", "a.md"), fsnotify.Write, true},
		{filepath.Join(wsRoot, "plans", "p", "spec.md"), fsnotify.Create, true},
		{filepath.Join(wsRoot, "notes", "a.md"), fsnotify.Chmod, false},                     // chmod noise
		{filepath.Join(wsRoot, ".obsidian", "workspace.json"), fsnotify.Write, false},       // manifest
		{filepath.Join(wsRoot, "plans", "p", ".artifacts", "b.xml"), fsnotify.Write, false}, // manifest
		{filepath.Join(wsRoot, "plans", "p.lock"), fsnotify.Create, false},                  // manifest
		{filepath.Join(wsRoot, "notes", ".hidden.md"), fsnotify.Write, false},               // hidden file
		{filepath.Join(t.TempDir(), "outside.md"), fsnotify.Write, false},                   // unsubscribed
		{filepath.Join(wsRoot, "notes", "a.sync-conflict-1-A.md"), fsnotify.Create, false},  // manifest
		{filepath.Join(wsRoot, ".grove", "rules", "x.rules"), fsnotify.Write, false},        // manifest
	}

	for _, tc := range cases {
		ev := fsnotify.Event{Name: tc.path, Op: tc.op}
		if got := h.MatchesEvent(ev); got != tc.matches {
			t.Errorf("MatchesEvent(%q, %v) = %v, want %v", tc.path, tc.op, got, tc.matches)
		}
	}
}

func TestSyncFlushHashGating(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "inbox", "idea.md")
	writeFile(t, notePath, "first draft")

	// First flush: document_created.
	h.flush(ctx, notePath)
	entries, _ := h.db.ListOutbox("testws", 0)
	if len(entries) != 1 || entries[0].EventType != syncproto.EventDocumentCreated {
		t.Fatalf("expected 1 created event, got %+v", entries)
	}
	doc, _ := h.db.GetDocumentByPath("testws", "notes/inbox/idea.md")
	if doc == nil || doc.DocumentID == "" {
		t.Fatalf("expected tracked document, got %+v", doc)
	}

	// Same content: hash-gated, no new outbox row.
	h.flush(ctx, notePath)
	if entries, _ = h.db.ListOutbox("testws", 0); len(entries) != 1 {
		t.Fatalf("hash-gate failed: expected 1 entry, got %d", len(entries))
	}

	// Changed content: document_updated with the SAME document id.
	writeFile(t, notePath, "second draft")
	h.flush(ctx, notePath)
	entries, _ = h.db.ListOutbox("testws", 0)
	if len(entries) != 2 || entries[1].EventType != syncproto.EventDocumentUpdated {
		t.Fatalf("expected created+updated, got %+v", entries)
	}
	if entries[1].DocumentID != doc.DocumentID {
		t.Fatalf("document id changed on update: %q != %q", entries[1].DocumentID, doc.DocumentID)
	}
}

func TestSyncDebounceCoalescing(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 40, 400)
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "log.md")
	writeFile(t, notePath, "line 1\n")

	// Rapid event bursts (simulating appends) must coalesce into one flush.
	for i := 0; i < 5; i++ {
		err := h.HandleEvents(ctx, []fsnotify.Event{{Name: notePath, Op: fsnotify.Write}})
		if err != nil {
			t.Fatalf("HandleEvents: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := h.db.ListOutbox("testws", 0)
		if len(entries) > 0 {
			if len(entries) != 1 {
				t.Fatalf("debounce failed to coalesce: %d outbox entries", len(entries))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("debounced flush never fired")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSyncSecretQuarantine(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	st := store.New()
	h.store = st
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "leaky.md")
	writeFile(t, notePath, "remote token: github_pat_11ABCDEF0123456789_abcdefghij\n")

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	h.flush(ctx, notePath)

	// Quarantined: nothing reaches the outbox or the identity map.
	if entries, _ := h.db.ListOutbox("testws", 0); len(entries) != 0 {
		t.Fatalf("quarantined file reached outbox: %+v", entries)
	}
	if doc, _ := h.db.GetDocumentByPath("testws", "notes/leaky.md"); doc != nil {
		t.Fatalf("quarantined file tracked in sync_documents: %+v", doc)
	}

	// An SSE-visible sync_conflict update is broadcast.
	select {
	case update := <-sub:
		if update.Type != store.UpdateSyncConflict {
			t.Fatalf("expected sync_conflict update, got %s", update.Type)
		}
		payload, ok := update.Payload.(*store.SyncConflictPayload)
		if !ok || payload.Kind != "secret_quarantine" || payload.Path != "notes/leaky.md" {
			t.Fatalf("unexpected payload: %+v", update.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("no quarantine update broadcast")
	}

	// Once the secret is removed the file syncs normally.
	writeFile(t, notePath, "remote token: managed via token_command now\n")
	h.flush(ctx, notePath)
	if entries, _ := h.db.ListOutbox("testws", 0); len(entries) != 1 {
		t.Fatalf("cleaned file did not sync: %+v", entries)
	}
}

func TestSyncDeleteCapture(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	ctx := context.Background()

	notePath := filepath.Join(wsRoot, "notes", "gone.md")
	writeFile(t, notePath, "ephemeral")
	h.flush(ctx, notePath)

	doc, _ := h.db.GetDocumentByPath("testws", "notes/gone.md")
	if doc == nil {
		t.Fatal("expected tracked document before delete")
	}

	if err := os.Remove(notePath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.flush(ctx, notePath)

	entries, _ := h.db.ListOutbox("testws", 0)
	if len(entries) != 2 || entries[1].EventType != syncproto.EventDocumentDeleted {
		t.Fatalf("expected created+deleted, got %+v", entries)
	}
	if entries[1].DocumentID != doc.DocumentID {
		t.Fatalf("delete event has wrong document id: %+v", entries[1])
	}
	if doc, _ := h.db.GetDocumentByPath("testws", "notes/gone.md"); doc != nil {
		t.Fatalf("document still tracked after delete: %+v", doc)
	}

	// Deleting an untracked path records nothing.
	h.flush(ctx, filepath.Join(wsRoot, "notes", "never-existed.md"))
	if entries, _ := h.db.ListOutbox("testws", 0); len(entries) != 2 {
		t.Fatalf("untracked delete produced outbox entries: %+v", entries)
	}
}

func TestSyncTypedMoveEvent(t *testing.T) {
	h, wsRoot := newTestSyncHandler(t, 50, 500)
	ctx := context.Background()

	oldPath := filepath.Join(wsRoot, "notes", "inbox", "task.md")
	newPath := filepath.Join(wsRoot, "notes", "current", "task.md")
	writeFile(t, oldPath, "task body")
	h.flush(ctx, oldPath)

	doc, _ := h.db.GetDocumentByPath("testws", "notes/inbox/task.md")
	if doc == nil {
		t.Fatal("expected tracked document before move")
	}

	// nb promotes the note: typed event with PrevPath populated.
	writeFile(t, newPath, "task body")
	_ = os.Remove(oldPath)
	h.HandleStoreUpdate(store.Update{
		Type:   store.UpdateNoteEvent,
		Source: "nb",
		Payload: &models.NoteEvent{
			Event:     models.NoteEventMoved,
			Workspace: "testws",
			Path:      newPath,
			PrevPath:  oldPath,
		},
	})

	entries, _ := h.db.ListOutbox("testws", 0)
	if len(entries) != 2 {
		t.Fatalf("expected created+moved, got %+v", entries)
	}
	moved := entries[1]
	if moved.EventType != syncproto.EventDocumentMoved ||
		moved.DocumentID != doc.DocumentID ||
		moved.Path != "notes/current/task.md" ||
		moved.PrevPath != "notes/inbox/task.md" {
		t.Fatalf("unexpected moved event: %+v", moved)
	}

	// Identity map follows the rename: same UUID, new path.
	if old, _ := h.db.GetDocumentByPath("testws", "notes/inbox/task.md"); old != nil {
		t.Fatalf("old path still tracked after move: %+v", old)
	}
	cur, _ := h.db.GetDocumentByPath("testws", "notes/current/task.md")
	if cur == nil || cur.DocumentID != doc.DocumentID {
		t.Fatalf("new path not tracked with stable id: %+v", cur)
	}

	// Hash-gate: the follow-up fsnotify create on the new path is a no-op
	// because the content hash is unchanged.
	h.flush(ctx, newPath)
	if entries, _ := h.db.ListOutbox("testws", 0); len(entries) != 2 {
		t.Fatalf("post-move flush was not hash-gated: %+v", entries)
	}
}
