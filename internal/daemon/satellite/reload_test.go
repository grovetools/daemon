package satellite

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// reload_test.go covers ConnManager.Reload — the satellite-registry hot-reload
// behind POST /api/satellites/reload. The lifecycle tests ride the same
// in-process SSH server as connmanager_test.go, so "reconnect" here means a
// real pinned dial, not a mock transition.

// assertSummary compares a ReloadSummary field-by-field (Reload sorts names,
// so expectations are plain sorted slices).
func assertSummary(t *testing.T, got *ReloadSummary, added, removed, changed, unchanged []string) {
	t.Helper()
	for _, f := range []struct {
		label string
		got   []string
		want  []string
	}{
		{"added", got.Added, added},
		{"removed", got.Removed, removed},
		{"changed", got.Changed, changed},
		{"unchanged", got.Unchanged, unchanged},
	} {
		if !reflect.DeepEqual(f.got, f.want) {
			t.Fatalf("summary.%s = %v, want %v (full: %+v)", f.label, f.got, f.want, got)
		}
	}
}

// TestReloadDiff pins the pure diff semantics across one call: an entry
// appearing is added, one disappearing is removed, any connection-shaping
// field differing (here host_key on the SAME addr — the VM-recreate case) is
// changed, and an identical entry (even via fresh pointers, as a re-run of
// LoadRegistry produces) is unchanged. No goroutines are involved: Start was
// never called, so Reload only swaps the registry.
func TestReloadDiff(t *testing.T) {
	old := NewRegistry(map[string]*SatelliteConfig{
		"gone":    {SSHAddr: "10.0.0.1:22", User: "u", HostKey: "ssh-ed25519 AAA"},
		"repin":   {SSHAddr: "10.0.0.2:22", User: "u", HostKey: "ssh-ed25519 OLD"},
		"stable":  {SSHAddr: "10.0.0.3:22", User: "u", HostKey: "ssh-ed25519 BBB"},
		"resynct": {SSHAddr: "10.0.0.4:22", User: "u", HostKey: "ssh-ed25519 CCC", SyncLocalPort: 8788},
	})
	cm := NewConnManager(old, nil)

	newReg := NewRegistry(map[string]*SatelliteConfig{
		"fresh":   {SSHAddr: "10.0.0.9:22", User: "u", HostKey: "ssh-ed25519 DDD"},
		"repin":   {SSHAddr: "10.0.0.2:22", User: "u", HostKey: "ssh-ed25519 NEW"}, // host_key changed, same addr
		"stable":  {SSHAddr: "10.0.0.3:22", User: "u", HostKey: "ssh-ed25519 BBB"},
		"resynct": {SSHAddr: "10.0.0.4:22", User: "u", HostKey: "ssh-ed25519 CCC", SyncLocalPort: 9999}, // forward port changed
	})
	summary := cm.Reload(newReg)

	assertSummary(t, summary,
		[]string{"fresh"},
		[]string{"gone"},
		[]string{"repin", "resynct"},
		[]string{"stable"},
	)

	// The SHARED registry object was updated in place — the collector holds
	// this pointer, so this is what makes hot-add federate at all.
	if _, ok := old.Get("gone"); ok {
		t.Fatal("removed entry still resolvable after Reload")
	}
	if sc, ok := old.Get("repin"); !ok || sc.HostKey != "ssh-ed25519 NEW" {
		t.Fatalf("changed entry not replaced in the shared registry: %+v", sc)
	}
	if !cm.HasSatellite("fresh") || cm.HasSatellite("gone") {
		t.Fatal("HasSatellite does not reflect the reloaded registry")
	}
}

// TestReloadAddConnects proves the `grove satellite up` path end-to-end: a
// ConnManager started over an EMPTY registry (the always-on global-daemon
// shape) picks up a hot-added satellite, dials it with the pinned key, and
// serves DialSatelliteSocket — no restart, no Start re-run.
func TestReloadAddConnects(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	st := store.New()
	cm := newTestConnManager(NewRegistry(nil), st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	summary := cm.Reload(NewRegistry(map[string]*SatelliteConfig{
		"sat": {
			SSHAddr:      ts.addr(),
			User:         "grovedev",
			HostKey:      authorizedKeyLine(hostSigner),
			IdentityFile: idFile,
			SocketPath:   remoteSock,
		},
	}))
	assertSummary(t, summary, []string{"sat"}, []string{}, []string{}, []string{})

	waitState(t, st, "sat", stateConnected, 5*time.Second)

	conn, err := cm.DialSatelliteSocket("sat")
	if err != nil {
		t.Fatalf("DialSatelliteSocket after hot-add: %v", err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: unix\r\nConnection: close\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	respBytes, _ := io.ReadAll(conn)
	if !strings.Contains(string(respBytes), "pong") {
		t.Fatalf("expected forwarded response after hot-add, got:\n%s", respBytes)
	}
}

// TestReloadRemoveTearsDown proves the `grove satellite down` path: removing a
// CONNECTED satellite stops its goroutine, drops its status from the store
// (the stateRemoved tombstone), purges its federated rows, and makes
// DialSatelliteSocket fail as unknown. The unwinding goroutine must not
// resurrect the status afterwards (the cfg-identity guards).
func TestReloadRemoveTearsDown(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	st := store.New()
	cm := newTestConnManager(NewRegistry(map[string]*SatelliteConfig{
		"sat": {
			SSHAddr:      ts.addr(),
			User:         "grovedev",
			HostKey:      authorizedKeyLine(hostSigner),
			IdentityFile: idFile,
			SocketPath:   remoteSock,
		},
	}), st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)
	waitState(t, st, "sat", stateConnected, 5*time.Second)

	// Seed one federated row for the origin, as the collector would have.
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSatelliteSnapshot,
		Origin: "sat",
		Payload: &store.SatelliteSnapshotPayload{
			Origin: "sat",
			Jobs:   []*models.JobInfo{{ID: "J", Status: "running", Origin: "sat"}},
		},
	})

	summary := cm.Reload(NewRegistry(nil))
	assertSummary(t, summary, []string{}, []string{"sat"}, []string{}, []string{})

	if _, ok := st.GetSatelliteStatuses()["sat"]; ok {
		t.Fatal("removed satellite still has a store status (tombstone not applied)")
	}
	for _, j := range st.GetJobs() {
		if j.Origin == "sat" {
			t.Fatalf("removed satellite's federated job row survived the tombstone: %+v", j)
		}
	}
	if cm.HasSatellite("sat") {
		t.Fatal("HasSatellite still true after removal")
	}
	if _, err := cm.DialSatelliteSocket("sat"); err == nil || !strings.Contains(err.Error(), "not in registry") {
		t.Fatalf("DialSatelliteSocket after removal: err = %v, want 'not in registry'", err)
	}

	// The stopped goroutine unwinds asynchronously (keepalive exit, deferred
	// state writes); none of that may re-emit a status for the tombstoned name.
	time.Sleep(150 * time.Millisecond)
	if s, ok := st.GetSatelliteStatuses()["sat"]; ok {
		t.Fatalf("stale goroutine resurrected removed satellite's status: %+v", s)
	}
}

// TestReloadHostKeyChangeReconnects is the VM-recreate case on a stable addr:
// the pinned key starts WRONG (backoff — never TOFU), and a Reload carrying
// the corrected host_key for the SAME addr must be reported as changed and
// produce a fresh, successfully pinned connection.
func TestReloadHostKeyChangeReconnects(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	wrongSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	mkReg := func(hostKey string) *Registry {
		return NewRegistry(map[string]*SatelliteConfig{
			"sat": {
				SSHAddr:      ts.addr(),
				User:         "grovedev",
				HostKey:      hostKey,
				IdentityFile: idFile,
				SocketPath:   remoteSock,
			},
		})
	}

	st := store.New()
	cm := newTestConnManager(mkReg(authorizedKeyLine(wrongSigner)), st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)
	waitState(t, st, "sat", stateBackoff, 5*time.Second)

	summary := cm.Reload(mkReg(authorizedKeyLine(hostSigner)))
	assertSummary(t, summary, []string{}, []string{}, []string{"sat"}, []string{})

	waitState(t, st, "sat", stateConnected, 5*time.Second)
}

// TestReloadUnchangedKeepsConnection: an identical registry (fresh pointers,
// equal values — exactly what re-running LoadRegistry yields) must leave the
// live connection completely untouched: same satConn, same ssh.Client, no
// state flap.
func TestReloadUnchangedKeepsConnection(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	mkReg := func() *Registry {
		return NewRegistry(map[string]*SatelliteConfig{
			"sat": {
				SSHAddr:      ts.addr(),
				User:         "grovedev",
				HostKey:      authorizedKeyLine(hostSigner),
				IdentityFile: idFile,
				SocketPath:   remoteSock,
			},
		})
	}

	st := store.New()
	cm := newTestConnManager(mkReg(), st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)
	waitState(t, st, "sat", stateConnected, 5*time.Second)

	cm.mu.Lock()
	scBefore := cm.conns["sat"]
	clientBefore := scBefore.client
	cm.mu.Unlock()

	summary := cm.Reload(mkReg())
	assertSummary(t, summary, []string{}, []string{}, []string{}, []string{"sat"})

	cm.mu.Lock()
	scAfter := cm.conns["sat"]
	clientAfter := scAfter.client
	cm.mu.Unlock()
	if scBefore != scAfter || clientBefore != clientAfter || clientAfter == nil {
		t.Fatal("unchanged entry did not keep its live satConn/ssh.Client across Reload")
	}
	if s := st.GetSatelliteStatuses()["sat"]; s == nil || s.State != stateConnected {
		t.Fatalf("unchanged entry's state flapped across Reload: %+v", s)
	}
}

// TestReloadBeforeStart: a reload arriving in the early-bind window (HTTP
// socket accepting before the watcher stage runs Start) must only swap the
// registry — Start then spawns the goroutines from it.
func TestReloadBeforeStart(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	st := store.New()
	cm := newTestConnManager(NewRegistry(nil), st)

	summary := cm.Reload(NewRegistry(map[string]*SatelliteConfig{
		"sat": {
			SSHAddr:      ts.addr(),
			User:         "grovedev",
			HostKey:      authorizedKeyLine(hostSigner),
			IdentityFile: idFile,
			SocketPath:   remoteSock,
		},
	}))
	assertSummary(t, summary, []string{"sat"}, []string{}, []string{}, []string{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)
	waitState(t, st, "sat", stateConnected, 5*time.Second)
}
