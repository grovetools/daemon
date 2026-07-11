package satellite

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
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
// ctx is cancelled. Safe to call once; an empty registry launches nothing.
func (cm *ConnManager) Start(ctx context.Context) {
	for _, name := range cm.registry.Names() {
		cfg, ok := cm.registry.Get(name)
		if !ok {
			continue
		}
		cm.mu.Lock()
		cm.conns[name] = &satConn{cfg: cfg, state: stateDisconnected, since: time.Now()}
		cm.mu.Unlock()
		go cm.runSatellite(ctx, cfg)
	}
}

// runSatellite owns one satellite's connection for the whole process lifetime:
// validate-pin → dial → keepalive → backoff-reconnect, forever while ctx lives.
func (cm *ConnManager) runSatellite(ctx context.Context, cfg *SatelliteConfig) {
	// Validate the pinned host key up front (C2). Empty/unparseable → permanent
	// hard-fail: no dial, no retry, never TOFU. Only P10 writing a real host_key
	// can fix it, so spinning a retry loop would be pure noise.
	hostKeyCB, err := fixedHostKeyCallback(cfg)
	if err != nil {
		cm.setState(cfg, stateDisconnected, err)
		cm.ulog.Warn("Satellite host key invalid; connection disabled (no TOFU)").
			Field("satellite", cfg.Name).Err(err).Log(ctx)
		return
	}

	backoff := cm.backoffBase
	for {
		if ctx.Err() != nil {
			return
		}

		client, derr := cm.dial(cfg, hostKeyCB)
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
		cm.setClient(cfg.Name, client)
		cm.setState(cfg, stateConnected, nil)
		cm.ulog.Info("Satellite connected").
			Field("satellite", cfg.Name).Field("addr", cfg.SSHAddr).Log(ctx)

		// Block until the connection dies or ctx is cancelled.
		cm.keepalive(ctx, client)

		cm.setClient(cfg.Name, nil)
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
func (cm *ConnManager) dial(cfg *SatelliteConfig, hostKeyCB ssh.HostKeyCallback) (*ssh.Client, error) {
	auth, cleanup, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: hostKeyCB,
		Timeout:         cm.dialTimeout,
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
func (cm *ConnManager) setClient(name string, client *ssh.Client) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	sc := cm.conns[name]
	if sc == nil {
		sc = &satConn{}
		cm.conns[name] = sc
	}
	sc.client = client
}

// setState records a satellite's connection state and emits a satellite_status
// store update (C17) so the treemux badge and grove status see the transition.
func (cm *ConnManager) setState(cfg *SatelliteConfig, state string, err error) {
	lastErr := ""
	if err != nil {
		lastErr = err.Error()
	}
	now := time.Now()

	cm.mu.Lock()
	sc := cm.conns[cfg.Name]
	if sc == nil {
		sc = &satConn{cfg: cfg}
		cm.conns[cfg.Name] = sc
	}
	sc.state = state
	sc.lastErr = lastErr
	sc.since = now
	cm.mu.Unlock()

	if cm.store != nil {
		cm.store.ApplyUpdate(store.Update{
			Type:   store.UpdateSatelliteStatus,
			Source: "satellite",
			Payload: &store.SatelliteStatusPayload{
				Name:      cfg.Name,
				State:     state,
				Addr:      cfg.SSHAddr,
				LastError: lastErr,
				Since:     now,
			},
		})
	}
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
func fixedHostKeyCallback(cfg *SatelliteConfig) (ssh.HostKeyCallback, error) {
	if cfg.HostKey == "" {
		return nil, fmt.Errorf("satellite %q has no pinned host_key (refusing to TOFU)", cfg.Name)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
	if err != nil {
		return nil, fmt.Errorf("satellite %q host_key unparseable: %w", cfg.Name, err)
	}
	return ssh.FixedHostKey(pub), nil
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
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			cleanup = func() { conn.Close() }
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
