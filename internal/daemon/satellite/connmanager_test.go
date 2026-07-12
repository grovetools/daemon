package satellite

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grovetools/daemon/internal/daemon/store"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// --- test key helpers -------------------------------------------------------

func genSigner(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer, priv
}

func authorizedKeyLine(signer ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func writeIdentity(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	return path
}

// shortTempSocket returns a short unix-socket path (macOS caps sun_path at ~104
// bytes, which t.TempDir() paths can exceed).
func shortTempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sat")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// --- in-process SSH test server --------------------------------------------

// testServer is a minimal x/crypto/ssh server that accepts any client key,
// replies to keepalive@openssh.com global requests, and forwards every
// direct-streamlocal@openssh.com channel to a real unix socket.
type testServer struct {
	ln         net.Listener
	hostSigner ssh.Signer
	extraHKeys []ssh.Signer
	remoteSock string

	mu    sync.Mutex
	conns []net.Conn
}

func newTestServer(t *testing.T, hostSigner ssh.Signer, remoteSock string, extraHostKeys ...ssh.Signer) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ts := &testServer{ln: ln, hostSigner: hostSigner, extraHKeys: extraHostKeys, remoteSock: remoteSock}
	go ts.acceptLoop()
	t.Cleanup(ts.close)
	return ts
}

func (ts *testServer) addr() string { return ts.ln.Addr().String() }

func (ts *testServer) acceptLoop() {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(ts.hostSigner)
	for _, hk := range ts.extraHKeys {
		cfg.AddHostKey(hk)
	}

	for {
		nConn, err := ts.ln.Accept()
		if err != nil {
			return
		}
		ts.mu.Lock()
		ts.conns = append(ts.conns, nConn)
		ts.mu.Unlock()
		go ts.serveConn(nConn, cfg)
	}
}

func (ts *testServer) serveConn(nConn net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		nConn.Close()
		return
	}
	defer sconn.Close()

	// Reply to keepalive@openssh.com (and any other global request).
	go func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "direct-streamlocal@openssh.com":
			ch, chReqs, err := newChan.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(chReqs)
			go ts.pipeToRemote(ch)
		case "direct-tcpip":
			// Wire format per RFC 4254 §7.2 (what x/crypto/ssh Client.Dial
			// sends for "tcp" networks).
			var msg struct {
				DestAddr string
				DestPort uint32
				OrigAddr string
				OrigPort uint32
			}
			if err := ssh.Unmarshal(newChan.ExtraData(), &msg); err != nil {
				_ = newChan.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
				continue
			}
			ch, chReqs, err := newChan.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(chReqs)
			go ts.pipeToTCP(ch, net.JoinHostPort(msg.DestAddr, fmt.Sprint(msg.DestPort)))
		default:
			_ = newChan.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

// pipeToTCP forwards a direct-tcpip channel to a real TCP address — the
// "remote loopback syncd" side of the sync-forward tests.
func (ts *testServer) pipeToTCP(ch ssh.Channel, addr string) {
	defer ch.Close()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}
	defer c.Close()
	go func() { _, _ = io.Copy(c, ch) }()
	_, _ = io.Copy(ch, c)
}

func (ts *testServer) pipeToRemote(ch ssh.Channel) {
	defer ch.Close()
	c, err := net.Dial("unix", ts.remoteSock)
	if err != nil {
		return
	}
	defer c.Close()
	go func() { _, _ = io.Copy(c, ch) }()
	_, _ = io.Copy(ch, c)
}

// dropConns closes accepted server-side connections (simulating a dropped
// satellite) while leaving the listener open so the client can reconnect.
func (ts *testServer) dropConns() {
	ts.mu.Lock()
	conns := ts.conns
	ts.conns = nil
	ts.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (ts *testServer) close() {
	ts.ln.Close()
	ts.dropConns()
}

// serveCannedUnixHTTP serves a fixed HTTP body on a unix socket.
func serveCannedUnixHTTP(t *testing.T, sockPath, body string) {
	t.Helper()
	ul, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	})}
	go srv.Serve(ul)
	t.Cleanup(func() { srv.Close(); ul.Close() })
}

// --- assertions -------------------------------------------------------------

func newTestConnManager(reg *Registry, st *store.Store) *ConnManager {
	cm := NewConnManager(reg, st)
	cm.backoffBase = 10 * time.Millisecond
	cm.backoffCap = 40 * time.Millisecond
	cm.keepaliveInterval = 50 * time.Millisecond
	cm.dialTimeout = 3 * time.Second
	return cm
}

func waitState(t *testing.T, st *store.Store, name, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s, ok := st.GetSatelliteStatuses()[name]; ok && s.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := "<none>"
	if s, ok := st.GetSatelliteStatuses()[name]; ok {
		got = s.State
	}
	t.Fatalf("satellite %q: state %q not reached within %s (last=%q)", name, want, timeout, got)
}

// --- tests ------------------------------------------------------------------

// TestChannelForwarding proves DialSatelliteSocket opens a direct-streamlocal
// channel that round-trips a raw HTTP request to the remote unix socket.
func TestChannelForwarding(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	reg := &Registry{byName: map[string]*SatelliteConfig{
		"sat": {
			Name:         "sat",
			SSHAddr:      ts.addr(),
			User:         "grovedev",
			HostKey:      authorizedKeyLine(hostSigner),
			IdentityFile: idFile,
			SocketPath:   remoteSock,
		},
	}}

	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	waitState(t, st, "sat", stateConnected, 5*time.Second)

	conn, err := cm.DialSatelliteSocket("sat")
	if err != nil {
		t.Fatalf("DialSatelliteSocket: %v", err)
	}
	defer conn.Close()

	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: unix\r\nConnection: close\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	respBytes, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(respBytes), "pong") {
		t.Fatalf("expected forwarded response to contain %q, got:\n%s", "pong", respBytes)
	}
}

// TestPinningRejection asserts a host-key mismatch hard-fails and never accepts
// the connection (no TOFU — C2).
func TestPinningRejection(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	wrongSigner, _ := genSigner(t) // pinned key: NOT the server's key
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	reg := &Registry{byName: map[string]*SatelliteConfig{
		"sat": {
			Name:         "sat",
			SSHAddr:      ts.addr(),
			User:         "grovedev",
			HostKey:      authorizedKeyLine(wrongSigner),
			IdentityFile: idFile,
			SocketPath:   remoteSock,
		},
	}}

	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	// It must reach an error/backoff state with a recorded error...
	waitState(t, st, "sat", stateBackoff, 5*time.Second)
	if s := st.GetSatelliteStatuses()["sat"]; s == nil || s.LastError == "" {
		t.Fatalf("expected non-empty LastError on pinning rejection, got %+v", s)
	}

	// ...and never TOFU into a connected state. Give it several backoff cycles.
	time.Sleep(200 * time.Millisecond)
	if s := st.GetSatelliteStatuses()["sat"]; s != nil && s.State == stateConnected {
		t.Fatalf("pinning rejection must never reach connected (TOFU), got state=%q", s.State)
	}
	if _, err := cm.DialSatelliteSocket("sat"); err == nil {
		t.Fatalf("DialSatelliteSocket should error while not connected")
	}
}

// TestEmptyHostKeyRefused asserts an empty pinned key is refused before any
// network I/O (permanent hard-fail, no dial).
func TestEmptyHostKeyRefused(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	// A listener that counts accepts; the ConnManager must never touch it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()

	reg := &Registry{byName: map[string]*SatelliteConfig{
		"sat": {
			Name:    "sat",
			SSHAddr: ln.Addr().String(),
			User:    "grovedev",
			HostKey: "", // empty → refuse
		},
	}}

	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	waitState(t, st, "sat", stateDisconnected, 2*time.Second)

	s := st.GetSatelliteStatuses()["sat"]
	if s == nil || !strings.Contains(s.LastError, "host_key") {
		t.Fatalf("expected host_key error, got %+v", s)
	}
	if _, err := cm.DialSatelliteSocket("sat"); err == nil {
		t.Fatalf("DialSatelliteSocket should error for refused satellite")
	}
	time.Sleep(100 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("empty host key must be refused before any network I/O, but server accepted %d connections", n)
	}
}

// TestReconnectAfterDrop asserts the manager reconnects and re-emits
// satellite_status after the connection drops (reconnect + backoff — the
// backoff resets to base on each successful dial, see runSatellite).
func TestReconnectAfterDrop(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	hostSigner, _ := genSigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	ts := newTestServer(t, hostSigner, remoteSock)

	reg := &Registry{byName: map[string]*SatelliteConfig{
		"sat": {
			Name:         "sat",
			SSHAddr:      ts.addr(),
			User:         "grovedev",
			HostKey:      authorizedKeyLine(hostSigner),
			IdentityFile: idFile,
			SocketPath:   remoteSock,
		},
	}}

	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	waitState(t, st, "sat", stateConnected, 5*time.Second)

	// Kill the live connection (listener stays open for reconnect).
	ts.dropConns()
	waitState(t, st, "sat", stateBackoff, 5*time.Second)

	// It must dial back to connected on its own.
	waitState(t, st, "sat", stateConnected, 5*time.Second)

	// And forwarding still works over the fresh connection.
	conn, err := cm.DialSatelliteSocket("sat")
	if err != nil {
		t.Fatalf("DialSatelliteSocket after reconnect: %v", err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: unix\r\nConnection: close\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	respBytes, _ := io.ReadAll(conn)
	if !strings.Contains(string(respBytes), "pong") {
		t.Fatalf("expected forwarded response after reconnect, got:\n%s", respBytes)
	}
}

// genECDSASigner returns an ecdsa-sha2-nistp256 host signer — a second key
// *type* so multi-hostkey negotiation can pick something other than ed25519.
func genECDSASigner(t *testing.T) ssh.Signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new ecdsa signer: %v", err)
	}
	return signer
}

// TestPinnedKeyTypeNegotiated (B5 regression): a server with several host keys
// (real sshd generates rsa+ecdsa+ed25519) negotiates by the CLIENT's algorithm
// preference. Pinning an ed25519 key while the client's default order prefers
// ecdsa made FixedHostKey reject a legitimate host ("host key mismatch") —
// reproduced live against the PoC VM. The fix constrains HostKeyAlgorithms to
// the pinned key's type.
func TestPinnedKeyTypeNegotiated(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	edSigner, _ := genSigner(t)
	ecSigner := genECDSASigner(t)
	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	remoteSock := shortTempSocket(t)
	serveCannedUnixHTTP(t, remoteSock, "pong")
	// ecdsa first: the server supports both; only HostKeyAlgorithms keeps the
	// negotiation on the pinned ed25519 type.
	ts := newTestServer(t, ecSigner, remoteSock, edSigner)

	reg := &Registry{byName: map[string]*SatelliteConfig{
		"sat": {
			Name:         "sat",
			SSHAddr:      ts.addr(),
			User:         "grovedev",
			HostKey:      authorizedKeyLine(edSigner),
			IdentityFile: idFile,
			SocketPath:   remoteSock,
		},
	}}

	st := store.New()
	cm := newTestConnManager(reg, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm.Start(ctx)

	waitState(t, st, "sat", stateConnected, 5*time.Second)
}

// TestEmptyAgentDoesNotPoisonAuth (B6 regression): a reachable SSH agent with
// zero identities (fresh macOS login) must contribute no auth method —
// otherwise its empty publickey attempt exhausts the server's method
// negotiation and the identity_file signer is never tried.
func TestEmptyAgentDoesNotPoisonAuth(t *testing.T) {
	agentSock := shortTempSocket(t)
	ln, err := net.Listen("unix", agentSock)
	if err != nil {
		t.Fatalf("listen agent sock: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(agent.NewKeyring(), conn)
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", agentSock)

	_, clientPriv := genSigner(t)
	idFile := writeIdentity(t, clientPriv)

	methods, cleanup, err := authMethods(&SatelliteConfig{Name: "sat", IdentityFile: idFile})
	defer cleanup()
	if err != nil {
		t.Fatalf("authMethods: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("empty agent must be skipped: want 1 method (identity only), got %d", len(methods))
	}
}
