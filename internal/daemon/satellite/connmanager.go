package satellite

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/daemon/internal/daemon/store"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Connection states reported through satellite_status (C17).
const (
	stateConnected    = "connected"
	stateBackoff      = "backoff"
	stateDisconnected = "disconnected"

	// stateRemoved is emitted exactly once when Reload drops a satellite from
	// the registry. The Store treats it as a tombstone: it deletes the status
	// entry (and the origin's federated rows) instead of upserting, so `grove
	// satellite status` stops listing the satellite without a daemon restart.
	stateRemoved = "removed"
)

// errKeepaliveLost marks a connection dropped by a failed keepalive or a dead
// underlying transport, as opposed to a fresh dial failure.
var errKeepaliveLost = errors.New("satellite connection lost (keepalive failed)")

// satConn is the ConnManager's per-satellite live state.
type satConn struct {
	cfg     *SatelliteConfig
	client  *ssh.Client
	state   string
	lastErr string
	since   time.Time

	// cancel stops this satellite's runSatellite goroutine (a per-satellite
	// child of the Start ctx). Reload uses it to tear down removed/changed
	// entries without touching the rest.
	cancel context.CancelFunc

	// Sync-forward state (syncforward.go). forward is the human-readable
	// status string surfaced through SatelliteStatusPayload.Forward; forwardLn
	// is the bound local listener (nil = not bound); forwardConns tracks live
	// forwarded connections for teardown.
	forward      string
	forwardLn    net.Listener
	forwardConns map[net.Conn]struct{}
}

// ConnManager owns one SSH connection per registered satellite. Each satellite
// gets a goroutine (spawned by Start) that dials, runs keepalive@openssh.com
// pings, and reconnects with exponential backoff for the satellite's whole
// lifetime. It exposes exactly one downstream primitive, DialSatelliteSocket
// (C1). Constructed GLOBAL-ONLY under the scope=="" gate in groved.go (C10).
type ConnManager struct {
	registry *Registry
	store    *store.Store
	ulog     *grovelogging.UnifiedLogger

	mu    sync.Mutex
	conns map[string]*satConn

	// runCtx is the ctx Start was called with; Reload derives per-satellite
	// contexts from it when starting entries after boot. Nil until Start —
	// a Reload before Start only swaps the registry (Start spawns from the
	// registry, so nothing is lost). Guarded by mu.
	runCtx context.Context

	// reloadMu serializes Reload calls (two concurrent POSTs to the reload
	// endpoint must not interleave their stop/start sequences). It is NOT mu:
	// Reload takes mu briefly and repeatedly, and must never hold it across
	// goroutine teardown.
	reloadMu sync.Mutex

	// Tunable timings — fields (not consts) so tests can shrink them.
	dialTimeout       time.Duration
	keepaliveInterval time.Duration
	backoffBase       time.Duration
	backoffCap        time.Duration
}

// NewConnManager builds a ConnManager over the registry, emitting connection
// health to the store. Call Start to spin up the per-satellite goroutines.
func NewConnManager(reg *Registry, st *store.Store) *ConnManager {
	return &ConnManager{
		registry:          reg,
		store:             st,
		ulog:              grovelogging.NewUnifiedLogger("groved.satellite"),
		conns:             make(map[string]*satConn),
		dialTimeout:       10 * time.Second,
		keepaliveInterval: 30 * time.Second,
		backoffBase:       1 * time.Second,
		backoffCap:        60 * time.Second,
	}
}

// Start launches one management goroutine per registry entry. Each runs until
// ctx is cancelled or Reload drops its entry. Safe to call once; an empty
// registry launches nothing (the manager stays cheap and inert until a Reload
// adds entries).
func (cm *ConnManager) Start(ctx context.Context) {
	cm.mu.Lock()
	cm.runCtx = ctx
	cm.mu.Unlock()
	for _, name := range cm.registry.Names() {
		if cfg, ok := cm.registry.Get(name); ok {
			cm.startSatellite(cfg)
		}
	}
}

// startSatellite creates the satConn entry and spawns runSatellite under a
// per-satellite child context, so Reload can stop this one satellite without
// disturbing the rest. A start before Start (early-bind: the HTTP socket can
// accept a reload POST before the watcher stage runs cm.Start) is a no-op —
// the entry is already in the registry and Start will spawn it.
func (cm *ConnManager) startSatellite(cfg *SatelliteConfig) {
	cm.mu.Lock()
	if cm.runCtx == nil {
		cm.mu.Unlock()
		return
	}
	sctx, cancel := context.WithCancel(cm.runCtx)
	cm.conns[cfg.Name] = &satConn{cfg: cfg, state: stateDisconnected, since: time.Now(), cancel: cancel}
	cm.mu.Unlock()
	go cm.runSatellite(sctx, cfg)
}

// stopSatellite tears one satellite down: remove its satConn (so the stale
// goroutine's setState/setClient calls become no-ops via the cfg-identity
// guards), cancel its context, and close its client + sync-forward listener
// so the keepalive and accept loops unwind promptly instead of waiting out a
// backoff sleep. Idempotent; a never-started name is a clean no-op.
func (cm *ConnManager) stopSatellite(name string) {
	cm.mu.Lock()
	sc := cm.conns[name]
	delete(cm.conns, name)
	cm.mu.Unlock()
	if sc == nil {
		return
	}
	if sc.cancel != nil {
		sc.cancel()
	}
	if sc.client != nil {
		// Close makes client.Wait return, which unblocks keepalive without
		// waiting for the next ping tick.
		sc.client.Close()
	}
	if sc.forwardLn != nil {
		sc.forwardLn.Close()
	}
	for c := range sc.forwardConns {
		c.Close()
	}
}

// ReloadSummary reports what a Reload did, by satellite name. It is the JSON
// body of POST /api/satellites/reload.
type ReloadSummary struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Changed   []string `json:"changed"`
	Unchanged []string `json:"unchanged"`
}

// Reload diffs the freshly-loaded registry against the live one and applies
// the delta: removed or changed satellites have their goroutine, SSH client,
// and sync-forward listener torn down (removed ones additionally emit a
// stateRemoved tombstone so the Store drops their status and federated rows);
// added or changed satellites get a fresh runSatellite. Unchanged entries —
// every SatelliteConfig field equal — keep their live connection untouched.
//
// "Changed" is whole-struct inequality: every field (ssh_addr, host_key,
// user, identity_file, socket_path, sync_local_port, sync_remote_addr)
// shapes the connection, its auth, or its forward, and a reconnect through
// runSatellite re-derives all of them — including the host-key pin, which
// stays validate-before-dial (never TOFU, C2).
//
// The shared Registry is updated in place (replace) so the collector's
// reconcile loop sees the same entry set. Safe under concurrent
// DialSatelliteSocket/HasSatellite: the registry swap is atomic under its
// own lock, and conns mutations go through cm.mu as everywhere else.
func (cm *ConnManager) Reload(newReg *Registry) *ReloadSummary {
	cm.reloadMu.Lock()
	defer cm.reloadMu.Unlock()

	oldEntries := cm.registry.snapshot()
	newEntries := newReg.snapshot()

	summary := &ReloadSummary{
		Added:     []string{},
		Removed:   []string{},
		Changed:   []string{},
		Unchanged: []string{},
	}
	for name, ncfg := range newEntries {
		ocfg, ok := oldEntries[name]
		switch {
		case !ok:
			summary.Added = append(summary.Added, name)
		case *ocfg != *ncfg:
			summary.Changed = append(summary.Changed, name)
		default:
			summary.Unchanged = append(summary.Unchanged, name)
		}
	}
	for name := range oldEntries {
		if _, ok := newEntries[name]; !ok {
			summary.Removed = append(summary.Removed, name)
		}
	}
	sort.Strings(summary.Added)
	sort.Strings(summary.Removed)
	sort.Strings(summary.Changed)
	sort.Strings(summary.Unchanged)

	// Stop BEFORE the registry swap so a concurrent DialSatelliteSocket never
	// pairs a new registry entry with a stale satConn's client.
	for _, name := range summary.Removed {
		cm.stopSatellite(name)
	}
	for _, name := range summary.Changed {
		cm.stopSatellite(name)
	}

	cm.registry.replace(newReg)

	// Start from the registry's own pointers (not newEntries') so satConn.cfg
	// identity matches what Get hands out from here on.
	for _, name := range append(append([]string{}, summary.Added...), summary.Changed...) {
		if cfg, ok := cm.registry.Get(name); ok {
			cm.startSatellite(cfg)
		}
	}

	// Tombstone the removed satellites' status (see stateRemoved). Emitted
	// after teardown so the stale goroutine can no longer overwrite it.
	for _, name := range summary.Removed {
		if ocfg := oldEntries[name]; ocfg != nil {
			cm.emitStatus(ocfg, stateRemoved, "", "", time.Now())
		}
	}

	cm.ulog.Info("Satellite registry reloaded").
		Field("added", len(summary.Added)).
		Field("removed", len(summary.Removed)).
		Field("changed", len(summary.Changed)).
		Field("unchanged", len(summary.Unchanged)).
		Log(context.Background())
	return summary
}

// runSatellite owns one satellite's connection for the whole process lifetime:
// validate-pin → dial → keepalive → backoff-reconnect, forever while ctx lives.
func (cm *ConnManager) runSatellite(ctx context.Context, cfg *SatelliteConfig) {
	// Validate the pinned host key up front (C2). Empty/unparseable → permanent
	// hard-fail: no dial, no retry, never TOFU. Only P10 writing a real host_key
	// can fix it, so spinning a retry loop would be pure noise.
	hostKeyCB, hostKeyAlgos, err := fixedHostKeyCallback(cfg)
	if err != nil {
		cm.setState(cfg, stateDisconnected, err)
		cm.ulog.Warn("Satellite host key invalid; connection disabled (no TOFU)").
			Field("satellite", cfg.Name).Err(err).Log(ctx)
		return
	}

	// Bind the local sync forward up front (feature-gated on sync_local_port,
	// see syncforward.go). A busy port is surfaced via status and retried on
	// each successful connect below; the listener itself stays bound across
	// reconnects — accepted conns just fail fast while disconnected.
	cm.ensureSyncForward(ctx, cfg)

	backoff := cm.backoffBase
	for {
		if ctx.Err() != nil {
			return
		}

		client, derr := cm.dial(cfg, hostKeyCB, hostKeyAlgos)
		if derr != nil {
			cm.setState(cfg, stateBackoff, derr)
			cm.ulog.Warn("Satellite dial failed; backing off").
				Field("satellite", cfg.Name).Field("backoff", backoff.String()).Err(derr).Log(ctx)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = cm.nextBackoff(backoff)
			continue
		}

		// Connected: reset backoff and report health.
		backoff = cm.backoffBase
		cm.setClient(cfg, client)
		cm.setState(cfg, stateConnected, nil)
		cm.ulog.Info("Satellite connected").
			Field("satellite", cfg.Name).Field("addr", cfg.SSHAddr).Log(ctx)

		// Retry the sync-forward bind if the port was busy earlier (no-op when
		// already bound or the feature is off).
		cm.ensureSyncForward(ctx, cfg)

		// Block until the connection dies or ctx is cancelled.
		cm.keepalive(ctx, client)

		cm.setClient(cfg, nil)
		client.Close()

		if ctx.Err() != nil {
			cm.setState(cfg, stateDisconnected, ctx.Err())
			return
		}

		cm.setState(cfg, stateBackoff, errKeepaliveLost)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = cm.nextBackoff(backoff)
	}
}

// dial establishes one SSH connection with the pinned host-key callback and
// agent-based (plus optional identity-file) auth.
func (cm *ConnManager) dial(cfg *SatelliteConfig, hostKeyCB ssh.HostKeyCallback, hostKeyAlgos []string) (*ssh.Client, error) {
	auth, cleanup, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	clientCfg := &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              auth,
		HostKeyCallback:   hostKeyCB,
		HostKeyAlgorithms: hostKeyAlgos,
		Timeout:           cm.dialTimeout,
	}

	client, err := ssh.Dial("tcp", cfg.SSHAddr, clientCfg)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// keepalive pings the satellite every keepaliveInterval and returns as soon as
// the connection dies (ping error or transport close) or ctx is cancelled. It
// returns promptly on server death via client.Wait rather than waiting a full
// ping interval — this is what makes reconnect fast.
func (cm *ConnManager) keepalive(ctx context.Context, client *ssh.Client) {
	ticker := time.NewTicker(cm.keepaliveInterval)
	defer ticker.Stop()

	closed := make(chan struct{})
	go func() {
		client.Wait()
		close(closed)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-closed:
			return
		case <-ticker.C:
			if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}

// HasSatellite reports whether name is a registered satellite. Callers use it
// to fail a dispatch fast with a clear error before attempting a dial.
func (cm *ConnManager) HasSatellite(name string) bool {
	_, ok := cm.registry.Get(name)
	return ok
}

// DialSatelliteSocket is the ONLY downstream primitive (C1). It opens a
// direct-streamlocal@openssh.com channel to the satellite's remote groved unix
// socket and returns it as a net.Conn. It errors immediately if the satellite
// is unknown or not currently connected — callers (P8's collector) retry on
// their own tick rather than blocking here.
func (cm *ConnManager) DialSatelliteSocket(name string) (net.Conn, error) {
	cfg, ok := cm.registry.Get(name)
	if !ok {
		return nil, fmt.Errorf("satellite %q not in registry", name)
	}

	cm.mu.Lock()
	sc := cm.conns[name]
	var client *ssh.Client
	if sc != nil {
		client = sc.client
	}
	cm.mu.Unlock()

	if client == nil {
		return nil, fmt.Errorf("satellite %q not connected", name)
	}

	socketPath := remoteSocketPath(cfg)
	// x/crypto/ssh implements direct-streamlocal@openssh.com inside Client.Dial
	// for the "unix" network (see ssh/tcpip.go Dial + ssh/streamlocal.go), so
	// this returns a net.Conn tunneled to the remote socket directly.
	conn, err := client.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial satellite %q socket %s: %w", name, socketPath, err)
	}
	return conn, nil
}

// setClient stores (or clears, on nil) the live ssh.Client for a satellite.
// The cfg pointer-identity guard makes calls from a stale goroutine (one whose
// entry Reload already stopped or replaced) silent no-ops — every satConn is
// created by startSatellite with the exact cfg pointer its goroutine captured,
// so a mismatch can only mean "this goroutine no longer owns the entry".
func (cm *ConnManager) setClient(cfg *SatelliteConfig, client *ssh.Client) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	sc := cm.conns[cfg.Name]
	if sc == nil || sc.cfg != cfg {
		return // stale goroutine (removed/replaced by Reload)
	}
	sc.client = client
}

// setState records a satellite's connection state and emits a satellite_status
// store update (C17) so the treemux badge and grove status see the transition.
// Stale-goroutine calls neither mutate nor emit (see setClient) — a removed
// satellite's tombstone must not be overwritten by its unwinding goroutine.
func (cm *ConnManager) setState(cfg *SatelliteConfig, state string, err error) {
	lastErr := ""
	if err != nil {
		lastErr = err.Error()
	}
	now := time.Now()

	cm.mu.Lock()
	sc := cm.conns[cfg.Name]
	if sc == nil || sc.cfg != cfg {
		cm.mu.Unlock()
		return // stale goroutine (removed/replaced by Reload)
	}
	sc.state = state
	sc.lastErr = lastErr
	sc.since = now
	forward := sc.forward
	cm.mu.Unlock()

	cm.emitStatus(cfg, state, lastErr, forward, now)
}

// setForward records the sync-forward status string (syncforward.go) and
// re-emits satellite_status so /api/satellites and SSE subscribers see it
// alongside the connection state. Since is left at the connection state's
// entry time — the Forward field is orthogonal to the state machine.
// Stale-goroutine calls neither mutate nor emit (see setClient).
func (cm *ConnManager) setForward(cfg *SatelliteConfig, forward string) {
	cm.mu.Lock()
	sc := cm.conns[cfg.Name]
	if sc == nil || sc.cfg != cfg {
		cm.mu.Unlock()
		return // stale goroutine (removed/replaced by Reload)
	}
	sc.forward = forward
	state, lastErr, since := sc.state, sc.lastErr, sc.since
	cm.mu.Unlock()

	cm.emitStatus(cfg, state, lastErr, forward, since)
}

// emitStatus publishes one satellite_status update to the store (C17).
func (cm *ConnManager) emitStatus(cfg *SatelliteConfig, state, lastErr, forward string, since time.Time) {
	if cm.store == nil {
		return
	}
	cm.store.ApplyUpdate(store.Update{
		Type:   store.UpdateSatelliteStatus,
		Source: "satellite",
		Payload: &store.SatelliteStatusPayload{
			Name:      cfg.Name,
			State:     state,
			Addr:      cfg.SSHAddr,
			LastError: lastErr,
			Forward:   forward,
			Since:     since,
		},
	})
}

// nextBackoff doubles the delay, capped at backoffCap.
func (cm *ConnManager) nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > cm.backoffCap {
		return cm.backoffCap
	}
	return d
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// fixedHostKeyCallback builds a pin-only HostKeyCallback from the registry's
// authorized_keys-format HostKey. Empty or unparseable keys are a hard error
// (C2) — the caller must NOT fall back to an accept-any callback.
//
// It also returns the HostKeyAlgorithms the client must offer: a server with
// several host keys (sshd generates rsa+ecdsa+ed25519) negotiates by the
// client's algorithm preference, so without this constraint it may present a
// key of a different type than the pinned one and FixedHostKey rejects a
// legitimate host ("host key mismatch").
func fixedHostKeyCallback(cfg *SatelliteConfig) (ssh.HostKeyCallback, []string, error) {
	if cfg.HostKey == "" {
		return nil, nil, fmt.Errorf("satellite %q has no pinned host_key (refusing to TOFU)", cfg.Name)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
	if err != nil {
		return nil, nil, fmt.Errorf("satellite %q host_key unparseable: %w", cfg.Name, err)
	}
	var algos []string
	switch pub.Type() {
	case ssh.KeyAlgoRSA:
		// An RSA *key* is offered under the SHA-2 signature algorithm names.
		algos = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	default:
		algos = []string{pub.Type()}
	}
	return ssh.FixedHostKey(pub), algos, nil
}

// authMethods assembles the SSH auth methods: agent signers from
// $SSH_AUTH_SOCK, plus an optional identity file. The returned cleanup closes
// the agent connection (its signers are captured during the handshake, so it is
// safe to close right after ssh.Dial returns). At least one method must be
// available or dialing is refused.
func authMethods(cfg *SatelliteConfig) ([]ssh.AuthMethod, func(), error) {
	var methods []ssh.AuthMethod
	cleanup := func() {}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			// Probe eagerly: a reachable agent holding zero keys (fresh macOS
			// login shells) must not contribute a method — an empty publickey
			// attempt burns the server's method negotiation and the
			// identity_file signer below never gets tried.
			if sgs, serr := ag.Signers(); serr == nil && len(sgs) > 0 {
				methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
				cleanup = func() { conn.Close() }
			} else {
				conn.Close()
			}
		}
	}

	if cfg.IdentityFile != "" {
		signer, err := loadIdentityFile(cfg.IdentityFile)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("satellite %q identity_file: %w", cfg.Name, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if len(methods) == 0 {
		cleanup()
		return nil, func() {}, fmt.Errorf("satellite %q: no ssh auth available (SSH_AUTH_SOCK unset and no identity_file)", cfg.Name)
	}
	return methods, cleanup, nil
}

// loadIdentityFile parses a private key from disk into an ssh.Signer.
func loadIdentityFile(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(pem)
}

// remoteSocketPath resolves the remote GLOBAL groved socket (paths.SocketPath("")
// as evaluated on the satellite — C1). The laptop cannot call paths.SocketPath
// for a foreign $HOME, so it uses the cluster-harness convention with an
// explicit per-satellite override.
func remoteSocketPath(cfg *SatelliteConfig) string {
	if cfg.SocketPath != "" {
		return cfg.SocketPath
	}
	return "/home/" + cfg.User + "/.local/state/grove/groved.sock"
}
