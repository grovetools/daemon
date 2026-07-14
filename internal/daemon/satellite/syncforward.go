package satellite

// syncforward.go is the daemon-owned local forward for satellite sync traffic.
// The sync client speaks plain HTTP to http://127.0.0.1:<sync_local_port>;
// syncd on the satellite binds 127.0.0.1:8788 and refuses non-loopback without
// TLS, so the intended transport is loopback→loopback over the satellite's
// existing pinned, auto-reconnecting SSH connection (a direct-tcpip channel per
// local connection). This replaces the manually-run `ssh -fN -L` tunnel that
// died silently.
//
// Lifecycle: the listener is bound once per satellite goroutine (runSatellite)
// and kept across reconnects — while the satellite is disconnected, accepted
// connections are closed immediately (fail fast; the sync client retries on
// its own). If the port is busy at bind time (e.g. a stale manual tunnel or
// another satellite entry), the failure is surfaced through the satellite's
// status Forward field and the bind is retried on each successful reconnect.
// Daemon shutdown (ctx cancel) closes the listener and all active forwards.

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"
)

// defaultSyncRemoteAddr is syncd's loopback bind on the satellite, dialed when
// sync_remote_addr is not set.
const defaultSyncRemoteAddr = "127.0.0.1:8788"

// syncRemoteAddr resolves the satellite-side loopback address the forward
// dials over the SSH connection.
func syncRemoteAddr(cfg *SatelliteConfig) string {
	if cfg.SyncRemoteAddr != "" {
		return cfg.SyncRemoteAddr
	}
	return defaultSyncRemoteAddr
}

// ensureSyncForward binds the local sync listener for a satellite with
// sync_local_port > 0, unless it is already bound. Called from runSatellite
// before the dial loop and again on every successful connect, so a port that
// was busy at boot (a stale manual tunnel, typically) is retaken once it frees
// up. A bind failure is logged and surfaced via the Forward status field — it
// never crashes or stalls the ConnManager.
func (cm *ConnManager) ensureSyncForward(ctx context.Context, cfg *SatelliteConfig) {
	if cfg.SyncLocalPort <= 0 {
		return
	}

	cm.mu.Lock()
	sc := cm.conns[cfg.Name]
	if sc == nil || sc.cfg != cfg || sc.forwardLn != nil {
		cm.mu.Unlock()
		return // already bound, or a stale goroutine (removed/replaced by Reload)
	}
	cm.mu.Unlock()

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.SyncLocalPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cm.setForward(cfg, "port busy on "+addr+": "+err.Error())
		cm.ulog.Warn("Satellite sync forward: local port unavailable").
			Field("satellite", cfg.Name).Field("addr", addr).Err(err).Log(ctx)
		return
	}

	cm.mu.Lock()
	sc = cm.conns[cfg.Name]
	if sc == nil || sc.cfg != cfg {
		// Reload tore this satellite down between the bind and here; a
		// replacement goroutine (if any) owns its own forward now.
		cm.mu.Unlock()
		ln.Close()
		return
	}
	sc.forwardLn = ln
	if sc.forwardConns == nil {
		sc.forwardConns = make(map[net.Conn]struct{})
	}
	cm.mu.Unlock()

	cm.setForward(cfg, "active on "+addr)
	cm.ulog.Info("Satellite sync forward listening").
		Field("satellite", cfg.Name).Field("addr", addr).
		Field("remote", syncRemoteAddr(cfg)).Log(ctx)

	// Teardown: ctx is the satellite goroutine's lifetime (the daemon's, or
	// shorter when Reload cancels this one satellite — stopSatellite also
	// closes the listener directly; closeSyncForward's cfg-identity guard
	// keeps this watcher from ever touching a successor's fresh forward).
	go func() {
		<-ctx.Done()
		cm.closeSyncForward(cfg)
	}()
	go cm.acceptSyncForward(cfg, ln)
}

// acceptSyncForward serves the bound listener until it is closed. Each
// accepted connection is forwarded over the satellite's CURRENT ssh.Client;
// while disconnected there is none, so the connection is closed immediately —
// fail fast, never queue, the sync client's retry loop owns the waiting.
func (cm *ConnManager) acceptSyncForward(cfg *SatelliteConfig, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed (shutdown via closeSyncForward) — nothing to
			// clean here; closeSyncForward owns the tracked connections.
			return
		}

		client := cm.currentClient(cfg.Name)
		if client == nil {
			conn.Close()
			continue
		}
		go cm.forwardSyncConn(cfg, conn, client)
	}
}

// forwardSyncConn opens a direct-tcpip channel to the satellite-side sync
// address and pipes bidirectionally. A dial failure (connection died between
// Accept and here, or syncd is down) closes the local connection immediately —
// same fail-fast contract as a disconnected satellite.
func (cm *ConnManager) forwardSyncConn(cfg *SatelliteConfig, local net.Conn, client *ssh.Client) {
	// x/crypto/ssh implements direct-tcpip inside Client.Dial for "tcp"
	// networks (ssh/tcpip.go), so this is a net.Conn tunneled to the
	// satellite's loopback syncd.
	remote, err := client.Dial("tcp", syncRemoteAddr(cfg))
	if err != nil {
		local.Close()
		return
	}

	if !cm.trackForwardConn(cfg, local, true) {
		// Forward torn down between Accept and here.
		remote.Close()
		local.Close()
		return
	}
	defer cm.trackForwardConn(cfg, local, false)

	pipeBidirectional(local, remote)
}

// closeSyncForward closes a satellite's sync listener and every active
// forwarded connection. Idempotent; called on ctx cancellation (daemon
// shutdown / satellite goroutine teardown). The cfg-identity guard matters
// on the Reload path: the old goroutine's ctx-done watcher fires AFTER
// stopSatellite may have already installed a successor satConn under the
// same name, and it must not tear down that successor's fresh forward.
func (cm *ConnManager) closeSyncForward(cfg *SatelliteConfig) {
	cm.mu.Lock()
	sc := cm.conns[cfg.Name]
	var ln net.Listener
	var conns []net.Conn
	if sc != nil && sc.cfg == cfg {
		ln = sc.forwardLn
		sc.forwardLn = nil
		for c := range sc.forwardConns {
			conns = append(conns, c)
		}
		sc.forwardConns = nil
	}
	cm.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
}

// currentClient returns the satellite's live ssh.Client, or nil while
// disconnected.
func (cm *ConnManager) currentClient(name string) *ssh.Client {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if sc := cm.conns[name]; sc != nil {
		return sc.client
	}
	return nil
}

// trackForwardConn adds/removes a live forwarded connection in the
// satellite's set so closeSyncForward can tear them down. Returns false when
// the forward is not (or no longer) active — including when a Reload replaced
// the satConn under this name (cfg-identity mismatch), so a lingering old
// forward never leaks a connection into the successor's set.
func (cm *ConnManager) trackForwardConn(cfg *SatelliteConfig, c net.Conn, add bool) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	sc := cm.conns[cfg.Name]
	if sc == nil || sc.cfg != cfg || sc.forwardConns == nil {
		return false
	}
	if add {
		sc.forwardConns[c] = struct{}{}
	} else {
		delete(sc.forwardConns, c)
	}
	return true
}

// pipeBidirectional copies both directions between a and b, half-close aware:
// when one direction hits EOF the peer's write side is shut down (CloseWrite —
// both *net.TCPConn and the ssh channel conn implement it) so the other
// direction can still drain. Both connections are fully closed once both
// copies finish; neither goroutine outlives the connection pair.
func pipeBidirectional(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		halfCloseWrite(b)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		halfCloseWrite(a)
	}()
	wg.Wait()
	a.Close()
	b.Close()
}

// halfCloseWrite signals write-side EOF to the peer, falling back to a full
// close for conns without CloseWrite.
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
