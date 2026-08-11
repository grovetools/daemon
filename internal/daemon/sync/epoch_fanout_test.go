package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/syncproto"
)

// TestEpochResetFansOutToOtherNotespaces is the second half of the
// recreated-server recovery (contract §3 P2b, scope 2).
//
// The detection is per-pass but the reset is GLOBAL: CheckServerEpoch calls
// ResetForRepushAll, which voids the synced state of every notespace and
// deletes their non-diverged outbox entries. The pass that detected it sweeps
// itself back into the outbox immediately — but every OTHER notespace was left
// voided, with an empty outbox and no pass scheduled, so nothing of theirs
// re-pushed until their own hourly tick. On a laptop with a notes notespace
// and a registry notespace that is up to an hour of a recreated server holding
// none of one of them.
//
// The pass now reports the reset so the transport owner can kick the others.
func TestEpochResetFansOutToOtherNotespaces(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	content := []byte("---\ntitle: a\n---\nbody\n")
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inbox", "a.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// "default" is the notespace whose pass runs; "registry" is the bystander
	// the reset also voids.
	for _, ws := range []string{"default", "registry"} {
		if err := db.InsertDocument(&Document{
			DocumentID: "doc-" + ws, Notespace: ws, Path: "inbox/a.md",
			ContentHash: sha(content), LastSyncedHash: sha(content), LastSyncedVersion: 7,
			BaseContent: content,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetServerEpoch("epoch-a"); err != nil {
		t.Fatal(err)
	}

	epoch := "epoch-b"
	srv := serveEpochStoreStub(t, &epoch, map[string]*occDoc{})
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var fanout []string
	ae := newTestAntiEntropy(db, client, root)
	ae.OnEpochReset = func(notespace string) { fanout = append(fanout, notespace) }

	if err := ae.Run(ctx); err != nil {
		t.Fatalf("anti-entropy Run: %v", err)
	}

	if len(fanout) != 1 || fanout[0] != "default" {
		t.Fatalf("expected exactly one fan-out naming the detecting notespace, got %v", fanout)
	}

	// The detecting notespace swept itself; the bystander is voided and idle —
	// which is exactly why the callback has to exist.
	entries, err := db.ListOutbox("default", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].EventType != syncproto.EventDocumentCreated {
		t.Fatalf("detecting notespace must re-enqueue a create, got %+v", entries)
	}
	bystander, err := db.GetDocumentByPath("registry", "inbox/a.md")
	if err != nil || bystander == nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if bystander.LastSyncedHash != "" || bystander.LastSyncedVersion != 0 {
		t.Fatalf("the global reset must have voided the bystander too: %+v", bystander)
	}
	if e, _ := db.ListOutbox("registry", 0); len(e) != 0 {
		t.Fatalf("the bystander sweeps only when kicked, not from this pass: %+v", e)
	}

	// A stable epoch never fans out — the kick is not free (a full pass per
	// notespace) and must not fire on every hourly tick.
	fanout = nil
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("second anti-entropy Run: %v", err)
	}
	if len(fanout) != 0 {
		t.Fatalf("a stable epoch must not fan out, got %v", fanout)
	}
}
