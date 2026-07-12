package satellite

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/daemon/internal/daemon/store"
)

// --- helpers -----------------------------------------------------------------

// freeLocalPort grabs an ephemeral loopback port and releases it. Standard
// slightly-racy test pattern; fine at test scale.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// waitForward polls the store until the satellite's Forward status contains
// wantSub.
func waitForward(t *testing.T, st *store.Store, name, wantSub string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s, ok := st.GetSatelliteStatuses()[name]; ok && strings.Contains(s.Forward, wantSub) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := "<none>"
	if s, ok := st.GetSatelliteStatuses()[name]; ok {
		got = s.Forward
	}
	t.Fatalf("satellite %q: forward status %q not reached within %s (last=%q)", name, wantSub, timeout, got)
}

// stubSyncd runs a plain-TCP HTTP server standing in for the satellite-side
// loopback syncd. Returns its host:port.
func stubSyncd(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// httpGetBody GETs a URL with a short timeout and returns the body.
func httpGetBody(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// syncForwardRegistry builds a one-satellite registry with the sync forward
// enabled, using the shared in-test sshd harness fields.
func syncForwardRegistry(addr, hostKey, idFile, remoteSock string, localPort int, remoteAddr string) *Registry {
	return &Registry{byName: map[string]*SatelliteConfig{
		"sat": {
			Name:           "sat",
			SSHAddr:        addr,
			User:           "grovedev",
			HostKey:        hostKey,
			IdentityFile:   idFile,
			SocketPath:     remoteSock,
			SyncLocalPort:  localPort,
			SyncRemoteAddr: remoteAddr,
		},
	}}
}

// --- tests -------------------------------------------------------------------

// TestSyncForwardEndToEnd (gate a, plus teardown): a local HTTP GET against
// 127.0.0.1:<sync_local_port> rides a direct-tcpip channel to the stub syncd
// on the "remote" loopback, and daemon shutdown releases the port.
func TestSyncForwardEndToEnd(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	syncdAddr := stubSyncd(t, "sync-pong")
	localPort := freeLocalPort(t)

	reg := syncForwardRegistry(ts.addr(), authorizedKeyLine(hostSigner), idFile, remoteSock, localPort, syncdAddr)
	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	waitState(t, st, "sat", stateConnected, 5*time.Second)
	waitForward(t, st, "sat", "active on 127.0.0.1:", 5*time.Second)

	body := httpGetBody(t, fmt.Sprintf("http://127.0.0.1:%d/", localPort))
	if body != "sync-pong" {
		t.Fatalf("expected forwarded body %q, got %q", "sync-pong", body)
	}

	// Teardown: cancelling the daemon ctx must release the port (no leaks).
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
		if err == nil {
			ln.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync forward port %d not released after shutdown: %v", localPort, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSyncForwardFailsFastWhileDisconnected (gate b): the listener is bound
// even while the satellite cannot connect, and accepted connections are closed
// immediately — the sync client's own retry loop owns the waiting.
func TestSyncForwardFailsFastWhileDisconnected(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	// An SSH endpoint that refuses connections: listen, note the addr, close.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := deadLn.Addr().String()
	deadLn.Close()

	localPort := freeLocalPort(t)
	reg := syncForwardRegistry(deadAddr, authorizedKeyLine(hostSigner), idFile, "/nonexistent", localPort, "")
	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	waitState(t, st, "sat", stateBackoff, 5*time.Second)
	waitForward(t, st, "sat", "active on 127.0.0.1:", 5*time.Second)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		t.Fatalf("dial local forward: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, rerr := conn.Read(buf); rerr != io.EOF {
		t.Fatalf("expected immediate EOF (fail fast) while disconnected, got %v", rerr)
	}
}

// TestSyncForwardAcrossReconnect (gate c): the listener stays bound across a
// dropped satellite connection; conns fail fast during backoff and the forward
// works again after the automatic reconnect.
func TestSyncForwardAcrossReconnect(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	syncdAddr := stubSyncd(t, "sync-pong")
	localPort := freeLocalPort(t)
	localURL := fmt.Sprintf("http://127.0.0.1:%d/", localPort)

	reg := syncForwardRegistry(ts.addr(), authorizedKeyLine(hostSigner), idFile, remoteSock, localPort, syncdAddr)
	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	waitState(t, st, "sat", stateConnected, 5*time.Second)
	waitForward(t, st, "sat", "active on 127.0.0.1:", 5*time.Second)
	if body := httpGetBody(t, localURL); body != "sync-pong" {
		t.Fatalf("pre-drop: expected %q, got %q", "sync-pong", body)
	}

	// Kill the live SSH connection (listener stays open for reconnect).
	ts.dropConns()
	waitState(t, st, "sat", stateBackoff, 5*time.Second)

	// While disconnected: fail fast, never hold the connection.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		t.Fatalf("dial local forward during backoff: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, rerr := conn.Read(buf); rerr != io.EOF {
		t.Fatalf("expected EOF during backoff, got %v", rerr)
	}
	conn.Close()

	// After the automatic reconnect the same listener forwards again.
	waitState(t, st, "sat", stateConnected, 5*time.Second)
	if body := httpGetBody(t, localURL); body != "sync-pong" {
		t.Fatalf("post-reconnect: expected %q, got %q", "sync-pong", body)
	}
}

// TestSyncForwardPortBusy (gate d): a port already bound by another process
// yields a clear "port busy" Forward status without crashing the ConnManager
// (the SSH connection still comes up), and the bind is retaken on the next
// reconnect once the port frees up.
func TestSyncForwardPortBusy(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	syncdAddr := stubSyncd(t, "sync-pong")

	// Occupy the port before the daemon can (the "stale manual tunnel").
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen squatter: %v", err)
	}
	localPort := squatter.Addr().(*net.TCPAddr).Port

	reg := syncForwardRegistry(ts.addr(), authorizedKeyLine(hostSigner), idFile, remoteSock, localPort, syncdAddr)
	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	// Clear status, and the ConnManager itself is unharmed: still connects.
	waitForward(t, st, "sat", "port busy on 127.0.0.1:", 5*time.Second)
	waitState(t, st, "sat", stateConnected, 5*time.Second)

	// Free the port, force a reconnect: the bind is retried on connect.
	squatter.Close()
	ts.dropConns()
	waitState(t, st, "sat", stateBackoff, 5*time.Second)
	waitState(t, st, "sat", stateConnected, 5*time.Second)
	waitForward(t, st, "sat", "active on 127.0.0.1:", 5*time.Second)

	body := httpGetBody(t, fmt.Sprintf("http://127.0.0.1:%d/", localPort))
	if body != "sync-pong" {
		t.Fatalf("expected forwarded body after port takeover, got %q", body)
	}
}
