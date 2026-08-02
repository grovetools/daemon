package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	gosync "sync"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"

	// The REAL grove-syncd, in process — same test-only cross-module import
	// registry_twoorigin_test.go documents. Nothing in the shipped daemon
	// binary links grove-syncd.
	syncserver "github.com/grovetools/sync/pkg/server"
	syncstore "github.com/grovetools/sync/pkg/store"
)

// This file is job 14's acceptance suite for contract §3 P2b: the two things a
// destroyed-and-recreated sync server breaks, proven end to end against the
// real server rather than a stub.
//
//  1. Every document that predates the wipe re-pushes, WITHOUT deleting the
//     client's sync.db. The documented workaround for this was "stop groved,
//     rm ~/.local/share/grove/sync/sync.db*, start" — which costs a new
//     origin id and the whole local sync history.
//  2. A client holding a token minted by the destroyed server surfaces one
//     actionable error instead of an endless silent 401 loop, and recovers on
//     its own once the token is replaced.
//
// Environment note: the plan's acceptance line named the docker cluster
// harness. Under the owner's local-infrastructure posture (STATE.md, "Job
// amendments") an in-process sync/pkg/server is the named substitute, and it
// asserts considerably more than the cluster's Scenario 7 can — nothing here
// is stubbed but the transport, which is loopback.

// recreatableServer is one stable URL in front of a swappable grove-syncd.
// Swapping the handler for one backed by a FRESH store is exactly what
// destroying and recreating a server does to a client: a new server epoch,
// an empty document set, and (optionally) a new token set that invalidates
// every token the predecessor minted.
type recreatableServer struct {
	t   *testing.T
	ts  *httptest.Server
	mu  gosync.Mutex
	cur http.Handler
}

func (s *recreatableServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.cur
	s.mu.Unlock()
	h.ServeHTTP(w, r)
}

// recreate replaces the backing store with an empty one accepting exactly the
// given token. Returns nothing: the point is that the client is told nothing
// either, and has to work the change out from the handshake.
func (s *recreatableServer) recreate(token string) {
	s.t.Helper()
	dir := s.t.TempDir()

	st, err := syncstore.Open(filepath.Join(dir, "syncd.db"))
	if err != nil {
		s.t.Fatalf("syncstore.Open: %v", err)
	}
	s.t.Cleanup(func() { _ = st.Close() })

	sum := sha256.Sum256([]byte(token))
	if err := st.CreateToken(hex.EncodeToString(sum[:]), "lifecycle-acceptance", syncstore.OwnerUserID); err != nil {
		s.t.Fatalf("CreateToken: %v", err)
	}
	blobs, err := syncstore.NewFSBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		s.t.Fatalf("NewFSBlobStore: %v", err)
	}

	s.mu.Lock()
	s.cur = syncserver.New(syncserver.Options{Store: st, Blobs: blobs})
	s.mu.Unlock()
}

func newRecreatableServer(t *testing.T, token string) *recreatableServer {
	t.Helper()
	s := &recreatableServer{t: t}
	s.recreate(token)
	s.ts = httptest.NewServer(s)
	t.Cleanup(s.ts.Close)
	return s
}

// connect performs the handshake transportLoop performs, including the epoch
// check, and reports whether the check reset local state for a full re-push.
func connect(t *testing.T, db *DB, url, token string) (*Client, bool) {
	t.Helper()
	log := logging.NewUnifiedLogger("test.lifecycle")
	client, err := NewClientFromConfig(context.Background(),
		&config.SyncConfig{Server: url, Token: token}, "machine-a", db.OriginID(), "", log)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	reset, err := CheckServerEpoch(context.Background(), db, client.ServerEpoch(), log)
	if err != nil {
		t.Fatalf("CheckServerEpoch: %v", err)
	}
	return client, reset
}

// serverPaths lists what the server currently holds for a workspace, by path.
func serverPaths(t *testing.T, client *Client, workspace string) map[string]syncproto.DocumentSnapshot {
	t.Helper()
	manifest, err := client.Snapshot(context.Background(), workspace)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	out := make(map[string]syncproto.DocumentSnapshot, len(manifest.Documents))
	for _, d := range manifest.Documents {
		out[d.Path] = d
	}
	return out
}

// TestServerRecreateRepushesEverythingWithoutTouchingSyncDB is the P2b
// acceptance for scope 2, against the real server.
func TestServerRecreateRepushesEverythingWithoutTouchingSyncDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const token = "lifecycle-token-1"
	srv := newRecreatableServer(t, token)

	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{}
	for i := 1; i <= 5; i++ {
		rel := fmt.Sprintf("inbox/pre-existing-%d.md", i)
		content := []byte(fmt.Sprintf("---\ntitle: doc %d\n---\n\nwritten long before the wipe\n", i))
		if err := os.WriteFile(filepath.Join(root, syncproto.LocalizePath(rel)), content, 0o644); err != nil {
			t.Fatal(err)
		}
		want[rel] = content
	}

	// Steady state: hydrate, push, and confirm the server holds all five.
	client, reset := connect(t, db, srv.ts.URL, token)
	if reset {
		t.Fatal("first contact must record the epoch, not reset")
	}
	ae := newTestAntiEntropy(db, client, root)
	push := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("initial anti-entropy: %v", err)
	}
	if _, err := push.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("initial drain: %v", err)
	}
	before := serverPaths(t, client, "default")
	if len(before) != len(want) {
		t.Fatalf("server holds %d documents before the wipe, want %d", len(before), len(want))
	}

	// The guard the whole fix exists to make unnecessary: sync.db is not to be
	// deleted, moved, or recreated by the recovery. Its identity — file path,
	// origin id, and every document id — must survive.
	dbPath := db.Path()
	dbInfoBefore, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat sync.db: %v", err)
	}
	originBefore := db.OriginID()
	idsBefore := map[string]string{}
	for path := range want {
		doc, err := db.GetDocumentByPath("default", path)
		if err != nil || doc == nil {
			t.Fatalf("GetDocumentByPath %s: %v", path, err)
		}
		idsBefore[path] = doc.DocumentID
	}

	// Disaster: the server is destroyed and recreated. Same URL, same token,
	// empty store, new epoch — the disposable-VM redeploy.
	srv.recreate(token)
	if empty := serverPaths(t, client, "default"); len(empty) != 0 {
		t.Fatalf("the recreated server is not empty: %d documents", len(empty))
	}

	// The daemon reconnects (transportLoop rebuilds its client) and the epoch
	// check fires. Everything after this point is what the daemon does on its
	// own — no operator, no sync.db surgery.
	client, reset = connect(t, db, srv.ts.URL, token)
	if !reset {
		t.Fatal("the epoch change was not detected — this is the trap: every " +
			"later edit pushes an UPDATE the empty server rejects as unknown")
	}
	ae = newTestAntiEntropy(db, client, root)
	push = NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("recovery anti-entropy: %v", err)
	}
	if _, err := push.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("recovery drain: %v", err)
	}

	// Acceptance: every pre-existing document is back on the server, byte
	// identical, under its ORIGINAL id.
	after := serverPaths(t, client, "default")
	if len(after) != len(want) {
		missing := []string{}
		for path := range want {
			if _, ok := after[path]; !ok {
				missing = append(missing, path)
			}
		}
		sort.Strings(missing)
		t.Fatalf("server holds %d/%d documents after recovery; missing %v", len(after), len(want), missing)
	}
	for path, content := range want {
		snap, ok := after[path]
		if !ok {
			t.Fatalf("document %s never re-pushed", path)
		}
		if snap.Hash != sha(content) {
			t.Errorf("%s re-pushed with the wrong content hash", path)
		}
		if snap.ID != idsBefore[path] {
			t.Errorf("%s changed identity across the recreate: %s -> %s", path, idsBefore[path], snap.ID)
		}
	}
	if pending, _ := db.CountOutbox(); pending != 0 {
		t.Errorf("outbox not drained after recovery: %d entries", pending)
	}

	// And the guard: sync.db was never deleted or replaced.
	dbInfoAfter, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("sync.db is gone after recovery: %v", err)
	}
	if !os.SameFile(dbInfoBefore, dbInfoAfter) {
		t.Fatal("sync.db was replaced — recovery must be push-side re-enqueue only")
	}
	if db.OriginID() != originBefore {
		t.Fatalf("origin id changed (%s -> %s): the database was recreated", originBefore, db.OriginID())
	}

	// Steady state again: a second cycle against the same epoch is a no-op.
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("post-recovery anti-entropy: %v", err)
	}
	if pending, _ := db.CountOutbox(); pending != 0 {
		t.Fatalf("a stable epoch re-enqueued %d entries", pending)
	}
}

// TestRecreatedServerRejectsTheOldTokenActionably is the P2b acceptance for
// scope 1, against the real server: a recreated server mints its own tokens,
// so the client's token is not merely wrong, it is unknown. That must be
// distinguishable from an unreachable server — it is the only failure mode
// retrying cannot fix — and replacing the token must be enough to recover.
func TestRecreatedServerRejectsTheOldTokenActionably(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const oldToken, newToken = "lifecycle-token-old", "lifecycle-token-new"
	srv := newRecreatableServer(t, oldToken)

	db := openTestDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("---\ntitle: survivor\n---\n\nmust outlive the server\n")
	if err := os.WriteFile(filepath.Join(root, "inbox", "survivor.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	client, _ := connect(t, db, srv.ts.URL, oldToken)
	ae := newTestAntiEntropy(db, client, root)
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("initial anti-entropy: %v", err)
	}
	push := NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})
	if _, err := push.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("initial drain: %v", err)
	}

	// The server is recreated and mints a new token set. The client's token
	// was issued by a database that no longer exists.
	srv.recreate(newToken)

	log := logging.NewUnifiedLogger("test.lifecycle")
	_, err := NewClientFromConfig(ctx, &config.SyncConfig{Server: srv.ts.URL, Token: oldToken},
		"machine-a", db.OriginID(), "", log)
	if err == nil {
		t.Fatal("the recreated server accepted a token it never minted")
	}
	if !IsAuthError(err) {
		t.Fatalf("a rejected token must be classified, not left as a generic failure: %v", err)
	}

	// The live client is rejected the same way, which is what lets a running
	// daemon notice without waiting for a reconnect.
	if _, err := client.Snapshot(ctx, "default"); !IsAuthError(err) {
		t.Fatalf("the live client's requests must classify too: %v", err)
	}

	// Recovery is exactly "replace the token": the same sync.db, the same
	// origin, and the re-push then proceeds on the epoch change.
	client, reset := connect(t, db, srv.ts.URL, newToken)
	if !reset {
		t.Fatal("the epoch change must still be detected once the token is fixed")
	}
	ae = newTestAntiEntropy(db, client, root)
	push = NewPushPipeline(db, client, "default", logging.NewUnifiedLogger("test.push"), PushConfig{})
	if err := ae.Run(ctx); err != nil {
		t.Fatalf("recovery anti-entropy: %v", err)
	}
	if _, err := push.DrainOutbox(ctx, root); err != nil {
		t.Fatalf("recovery drain: %v", err)
	}
	if got := serverPaths(t, client, "default"); len(got) != 1 {
		t.Fatalf("the survivor did not re-push after the token was replaced: %v", got)
	}
}
