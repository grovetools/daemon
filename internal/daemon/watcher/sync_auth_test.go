package watcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/syncproto"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// tokenServer is a syncd stand-in whose capabilities handshake accepts or
// rejects on a switch — the local equivalent of destroying a server and
// recreating it with freshly minted tokens.
type tokenServer struct {
	*httptest.Server
	reject     atomic.Bool
	handshakes atomic.Int32
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	ts := &tokenServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/capabilities" {
			http.Error(w, "not implemented in this stub", http.StatusNotFound)
			return
		}
		ts.handshakes.Add(1)
		if ts.reject.Load() {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"protocol_version":%d,"capabilities":{"protocol_versions":[%d]}}`,
			syncproto.ProtocolVersionLegacy, syncproto.ProtocolVersionLegacy)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newAuthTestHandler builds a handler wired to a sync server, with the
// transport cadence and backoff window compressed so a test can watch several
// reconnect attempts go by in milliseconds.
func newAuthTestHandler(t *testing.T, server string) (*SyncHandler, *store.Store) {
	t.Helper()
	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.New()
	h := NewSyncHandler(st, nil, &config.SyncConfig{
		Server:     server,
		Token:      "stale-token",
		Workspaces: []config.SyncWorkspace{{Name: "testws"}},
	}, db, 50, 500)
	h.transportInterval = 10 * time.Millisecond
	h.authRetryBase = 80 * time.Millisecond
	h.authRetryMax = 160 * time.Millisecond
	return h, st
}

// TestStaleTokenSurfacesOneActionableError is the P2b acceptance for scope 1:
// a server that rejects this machine's token must produce ONE actionable
// error, not an unbounded silent retry loop.
//
// Before this, transportLoop could not tell a rejected token from an
// unreachable server: both were logged at debug ("sync server not reachable
// yet") and retried every 10 seconds forever. A destroyed-and-recreated server
// therefore left a client 401-ing indefinitely with nothing on any surface
// saying the word "token".
func TestStaleTokenSurfacesOneActionableError(t *testing.T) {
	srv := newTokenServer(t)
	srv.reject.Store(true)

	h, st := newAuthTestHandler(t, srv.URL)
	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.transportLoop(ctx)

	// One auth_failed update, carrying the remediation.
	select {
	case update := <-sub:
		if update.Type != store.UpdateSyncConflict {
			t.Fatalf("expected a sync_conflict update, got %s", update.Type)
		}
		payload, ok := update.Payload.(*store.SyncConflictPayload)
		if !ok || payload.Kind != "auth_failed" {
			t.Fatalf("unexpected payload: %+v", update.Payload)
		}
		for _, want := range []string{"token", "grove-syncd token create", "sync.toml"} {
			if !strings.Contains(payload.Detail, want) {
				t.Fatalf("the alert must name the fix; %q missing from %q", want, payload.Detail)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the token rejection never surfaced — this is the silent 401 loop")
	}

	detail, since, failing := h.AuthFailure()
	if !failing || detail == "" || since.IsZero() {
		t.Fatalf("status surface must report the rejection: %q %v %v", detail, since, failing)
	}

	// It stays ONE error: no further broadcasts while the same episode runs,
	// and the reconnect attempts back off instead of hammering a handshake per
	// tick. With a 10ms tick and an 80ms floor, a flat retry would produce
	// tens of handshakes in the window below.
	before := srv.handshakes.Load()
	deadline := time.After(400 * time.Millisecond)
	for done := false; !done; {
		select {
		case update := <-sub:
			if p, ok := update.Payload.(*store.SyncConflictPayload); ok && p.Kind == "auth_failed" {
				t.Fatal("the rejection was re-broadcast: one episode must produce one error")
			}
		case <-deadline:
			done = true
		}
	}
	if attempts := srv.handshakes.Load() - before; attempts > 6 {
		t.Fatalf("reconnects are not backing off: %d handshakes in 400ms", attempts)
	}

	// No client was ever published — a rejected token must not look connected.
	h.clientMu.RLock()
	client := h.client
	h.clientMu.RUnlock()
	if client != nil {
		t.Fatal("a rejected handshake published a client")
	}
}

// TestStaleTokenRecoversWithoutRestart: replacing the token must take effect on
// its own. The token is resolved fresh on every connect attempt
// (SyncConfig.ResolveToken), so once the server accepts it the transport comes
// up and the failure state clears — no daemon restart, no sync.db surgery.
func TestStaleTokenRecoversWithoutRestart(t *testing.T) {
	srv := newTokenServer(t)
	srv.reject.Store(true)

	h, _ := newAuthTestHandler(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.transportLoop(ctx)

	waitForAuth(t, 3*time.Second, "the rejection to be observed", func() bool {
		_, _, failing := h.AuthFailure()
		return failing
	})

	// The operator mints a replacement and the server starts accepting it.
	srv.reject.Store(false)

	waitForAuth(t, 5*time.Second, "the transport to reconnect on its own", func() bool {
		h.clientMu.RLock()
		defer h.clientMu.RUnlock()
		return h.client != nil
	})
	if _, _, failing := h.AuthFailure(); failing {
		t.Fatal("the failure state survived a successful handshake")
	}
}

// TestMidRunRejectionResetsTheTransport covers the half a handshake cannot see.
// transportLoop connects ONCE and caches the client forever, so a server
// recreated (or a token revoked) under a running daemon left every pipeline
// 401-ing against a client holding a dead token — and because nothing ever
// re-resolved the token, replacing it on disk changed nothing until the daemon
// was restarted. A live rejection now drops the client and its pipelines so
// the next tick rebuilds both.
func TestMidRunRejectionResetsTheTransport(t *testing.T) {
	srv := newTokenServer(t)
	h, _ := newAuthTestHandler(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.transportLoop(ctx)

	waitForAuth(t, 5*time.Second, "the initial connection", func() bool {
		h.clientMu.RLock()
		defer h.clientMu.RUnlock()
		return h.client != nil
	})

	// Register a pipeline the way ensurePipelines does, so the reset's
	// teardown is observable.
	pctx, pcancel := context.WithCancel(ctx)
	h.pipelinesMu.Lock()
	h.pipelines["testws"] = pcancel
	h.pipelinesMu.Unlock()

	// A live pipeline meets a rejected token; the server keeps rejecting so
	// the reconnect cannot immediately paper over the reset.
	srv.reject.Store(true)
	h.noteAuthFailure(ctx, authSourcePipeline, fmt.Errorf("push rejected: %w", syncdb.ErrUnauthorized))

	select {
	case <-pctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the stale-token pipelines were never torn down")
	}

	h.clientMu.RLock()
	client := h.client
	h.clientMu.RUnlock()
	if client != nil {
		t.Fatal("the client holding the dead token survived the reset")
	}
	h.pipelinesMu.Lock()
	remaining := len(h.pipelines)
	h.pipelinesMu.Unlock()
	if remaining != 0 {
		t.Fatalf("pipelines still registered after the reset: %d", remaining)
	}

	// And it recovers on its own once the token is good again.
	srv.reject.Store(false)
	waitForAuth(t, 5*time.Second, "the transport to rebuild", func() bool {
		h.clientMu.RLock()
		defer h.clientMu.RUnlock()
		return h.client != nil
	})
}

// TestUnreachableServerIsNotATokenProblem guards the discriminator from the
// other side: a server that is merely down must stay quiet and keep the
// ordinary retry cadence. Reporting it as a dead token would send operators
// chasing a credential that is fine.
func TestUnreachableServerIsNotATokenProblem(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := down.URL
	down.Close()

	h, st := newAuthTestHandler(t, url)
	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.transportLoop(ctx)

	deadline := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case update := <-sub:
			if p, ok := update.Payload.(*store.SyncConflictPayload); ok && p.Kind == "auth_failed" {
				t.Fatal("an unreachable server was reported as a token rejection")
			}
		case <-deadline:
			done = true
		}
	}
	if _, _, failing := h.AuthFailure(); failing {
		t.Fatal("an unreachable server must not latch the auth-failure state")
	}
}

// TestKickAntiEntropyExceptSkipsTheDetector is the watcher half of the epoch
// fan-out: the pass that detected the recreated server has already swept, so
// re-kicking it would buy a redundant full pass on every recreate.
func TestKickAntiEntropyExceptSkipsTheDetector(t *testing.T) {
	h, _ := newAuthTestHandler(t, "http://127.0.0.1:1")
	db := h.database()

	// Two registered passes; Kick is observable through the buffered channel
	// each pass owns, which is what RunAntiEntropyLoop selects on.
	client := syncdb.NewClient(syncdb.ClientConfig{ServerURL: "http://127.0.0.1:1"})
	detector := syncdb.NewAntiEntropyPass(db, client, "detector", t.TempDir(), nil, h.ulog, syncdb.AntiEntropyConfig{})
	bystander := syncdb.NewAntiEntropyPass(db, client, "bystander", t.TempDir(), nil, h.ulog, syncdb.AntiEntropyConfig{})
	h.pipelinesMu.Lock()
	h.aePasses["detector"] = detector
	h.aePasses["bystander"] = bystander
	h.pipelinesMu.Unlock()

	h.kickAntiEntropyExcept("detector")

	if detector.KickPending() {
		t.Fatal("the detecting notespace was kicked again; it already swept in its own pass")
	}
	if !bystander.KickPending() {
		t.Fatal("the bystander was not kicked — it is voided with an empty outbox and would wait an hour")
	}
}

// TestEpochProbeDetectsARecreatedServerUnderARunningDaemon closes the
// detection-latency half of scope 2. transportLoop builds a client exactly
// once and checks the epoch there, and a quiet push-only machine sends nothing
// else — no local edits, no pushes — so a server wiped and restarted under a
// running daemon was invisible until the hourly anti-entropy tick, with the
// machine's whole document set existing nowhere but locally in the meantime.
// The transport now re-probes the epoch on a slow cadence and kicks every
// notespace's sweep when it changes.
func TestEpochProbeDetectsARecreatedServerUnderARunningDaemon(t *testing.T) {
	var epoch atomic.Value
	epoch.Store("epoch-a")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/capabilities" {
			http.Error(w, "not implemented in this stub", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"protocol_version":%d,"server_epoch":%q,"capabilities":{"protocol_versions":[%d]}}`,
			syncproto.ProtocolVersionLegacy, epoch.Load().(string), syncproto.ProtocolVersionLegacy)
	}))
	defer srv.Close()

	h, _ := newAuthTestHandler(t, srv.URL)
	h.epochProbeInterval = 20 * time.Millisecond
	db := h.database()

	// A document the daemon believes is safely on the server.
	if err := db.InsertDocument(&syncdb.Document{
		DocumentID: "doc-1", Notespace: "testws", Path: "inbox/a.md",
		ContentHash: "h", LastSyncedHash: "h", LastSyncedVersion: 3,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.transportLoop(ctx)

	waitForAuth(t, 5*time.Second, "the initial connection", func() bool {
		h.clientMu.RLock()
		defer h.clientMu.RUnlock()
		return h.client != nil
	})

	// Register a pass so the fan-out kick is observable.
	client := syncdb.NewClient(syncdb.ClientConfig{ServerURL: srv.URL})
	pass := syncdb.NewAntiEntropyPass(db, client, "testws", t.TempDir(), nil, h.ulog, syncdb.AntiEntropyConfig{})
	h.pipelinesMu.Lock()
	h.aePasses["testws"] = pass
	h.pipelinesMu.Unlock()

	// The server is destroyed and recreated. Nothing tells the daemon.
	epoch.Store("epoch-b")

	waitForAuth(t, 5*time.Second, "the recreated server to be noticed", func() bool {
		stored, err := db.GetServerEpoch()
		return err == nil && stored == "epoch-b"
	})

	doc, err := db.GetDocument("doc-1")
	if err != nil || doc == nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if doc.LastSyncedHash != "" || doc.LastSyncedVersion != 0 {
		t.Fatalf("the epoch change must void the synced state so the sweep re-pushes: %+v", doc)
	}
	if !pass.KickPending() {
		t.Fatal("the sweep was not kicked; the re-push would wait for the hourly tick")
	}
}

func waitForAuth(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
