package collector

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/logging"
	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/satellite"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// SocketDialer is the one-method transport seam the SatelliteCollector needs
// (M2 contract C1/C4). It is satisfied by *satellite.ConnManager in production
// and by a fake in tests — the collector never takes the concrete ConnManager,
// so a test can serve a fake remote daemon over a plain unix socket.
type SocketDialer interface {
	DialSatelliteSocket(name string) (net.Conn, error)
}

const (
	// satelliteRetryInterval is how long a per-satellite loop waits before
	// re-checking the connection gate after a disconnect or a failed snapshot.
	satelliteRetryInterval = 5 * time.Second

	// satelliteSnapshotDebounce coalesces a burst of remote job/session SSE
	// events into one re-snapshot. SSE is the change signal; the snapshot is the
	// state transfer (C16).
	satelliteSnapshotDebounce = 1500 * time.Millisecond

	// satelliteReconcileInterval is how often Run re-reads the registry's name
	// set and starts/stops per-satellite loops. The registry is shared mutable
	// state with the ConnManager (its Reload swaps entries in place on `grove
	// satellite up`/`down`), so this poll is what lets a hot-added satellite
	// federate — and a hot-removed one stop — without a daemon restart.
	satelliteReconcileInterval = 5 * time.Second
)

// SatelliteCollector federates remote jobs/sessions into the laptop Store
// (M2 contract C6/C7/C16). One goroutine per registered satellite: gate on the
// ConnManager's connection state (via the satellite_status Store surface),
// snapshot GET /api/jobs + /api/sessions on (re)connect, then tail
// GET /api/stream and treat any job/session-typed event as a dirty signal that
// triggers a debounced re-snapshot. Every row is sanitized and origin-stamped
// before it reaches the Store. On disconnect it emits nothing row-level — rows
// persist and staleness is derived from satellite_status — and reconnect
// reconciles via a fresh snapshot. Constructed GLOBAL-ONLY under the scope==""
// gate in groved.go (C10).
type SatelliteCollector struct {
	dialer SocketDialer
	reg    *satellite.Registry
	ulog   *logging.UnifiedLogger

	// Tunable timings — fields (not consts) so tests can shrink them.
	retryInterval     time.Duration
	snapshotDebounce  time.Duration
	reconcileInterval time.Duration
}

// NewSatelliteCollector builds the collector over a socket dialer (the
// ConnManager in production) and the satellite registry. The Store is NOT taken
// here — it arrives via Run, per the Collector interface (C10/E15).
func NewSatelliteCollector(dialer SocketDialer, reg *satellite.Registry) *SatelliteCollector {
	return &SatelliteCollector{
		dialer:            dialer,
		reg:               reg,
		ulog:              logging.NewUnifiedLogger("groved.collector.satellite"),
		retryInterval:     satelliteRetryInterval,
		snapshotDebounce:  satelliteSnapshotDebounce,
		reconcileInterval: satelliteReconcileInterval,
	}
}

// Name returns the collector's name.
func (c *SatelliteCollector) Name() string { return "satellite" }

// Run reconciles one management goroutine per registry satellite and blocks
// until ctx is cancelled (Collector contract). The registry is re-read every
// reconcileInterval because ConnManager.Reload mutates it in place (satellite
// hot-reload): a name that appears gets a fresh per-satellite loop, a name
// that disappears has its loop cancelled. This is why the collector is
// registered on the global daemon even when the boot registry is empty —
// Engine.Start iterates collectors exactly once (F19), so a collector that
// only existed once satellites did could never pick up the first `up`.
func (c *SatelliteCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	c.ulog.Info("Satellite federation collector started").
		Field("satellites", len(c.reg.Names())).Log(ctx)

	var wg sync.WaitGroup
	running := make(map[string]context.CancelFunc)

	reconcile := func() {
		want := make(map[string]struct{})
		for _, name := range c.reg.Names() {
			want[name] = struct{}{}
		}
		for name, cancel := range running {
			if _, ok := want[name]; !ok {
				// Removed from the registry: stop its loop. Row cleanup is not
				// this loop's job — the ConnManager's "removed" tombstone has
				// the Store drop the origin's federated rows.
				cancel()
				delete(running, name)
			}
		}
		for name := range want {
			if _, ok := running[name]; ok {
				continue
			}
			sctx, cancel := context.WithCancel(ctx)
			running[name] = cancel
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				c.runSatellite(sctx, name, st, updates)
			}(name)
		}
	}

	reconcile()
	ticker := time.NewTicker(c.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Per-satellite loops observe ctx (their contexts are children of
			// it); wait for them to unwind so Run doesn't return early.
			wg.Wait()
			return nil
		case <-ticker.C:
			reconcile()
		}
	}
}

// runSatellite owns one satellite's federation loop for the process lifetime.
func (c *SatelliteCollector) runSatellite(ctx context.Context, name string, st *store.Store, updates chan<- store.Update) {
	// Build the client once. The dial closure runs per request/stream, so a dead
	// connection just errors per-call (DialSatelliteSocket fails fast when the
	// satellite is not connected); there is no stale client to rebuild.
	client, err := coredaemon.NewRemoteClientWithDialer(func(context.Context) (net.Conn, error) {
		return c.dialer.DialSatelliteSocket(name)
	})
	if err != nil {
		c.ulog.Warn("Failed to build satellite client; federation disabled for this satellite").
			Field("satellite", name).Err(err).Log(ctx)
		return
	}
	defer client.Close()

	for {
		if ctx.Err() != nil {
			return
		}

		// Gate on connection state (C16/E16). The ConnManager exposes no direct
		// state getter — the satellite_status Store surface IS the getter.
		if !c.isConnected(st, name) {
			if !sleepCtx(ctx, c.retryInterval) {
				return
			}
			continue
		}

		// (Re)connect: snapshot reconciles this origin's rows (B7).
		if err := c.snapshot(ctx, name, client, updates); err != nil {
			c.ulog.Debug("Satellite snapshot failed; will retry").
				Field("satellite", name).Err(err).Log(ctx)
			if !sleepCtx(ctx, c.retryInterval) {
				return
			}
			continue
		}

		// Tail the remote stream, re-snapshotting on dirty signals until the
		// stream drops or ctx is cancelled. On return we loop back to the gate;
		// we emit nothing row-level on disconnect (staleness is derived).
		c.tail(ctx, name, client, st, updates)
		if ctx.Err() != nil {
			return
		}
		if !sleepCtx(ctx, c.retryInterval) {
			return
		}
	}
}

// tail subscribes to the remote SSE stream and re-snapshots (debounced) on any
// job/session-typed event. It returns when the stream closes/errors or ctx is
// cancelled. It deliberately does NOT parse the untyped SSE payloads
// (StateUpdate.Payload is interface{} → an untyped map after JSON): the event
// is only a change signal, the snapshot is the state transfer. Robust against
// payload-shape drift; remote row volume is small.
func (c *SatelliteCollector) tail(ctx context.Context, name string, client *coredaemon.RemoteClient, st *store.Store, updates chan<- store.Update) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := client.StreamState(streamCtx)
	if err != nil {
		c.ulog.Debug("Satellite stream connect failed").
			Field("satellite", name).Err(err).Log(ctx)
		return
	}

	var debounce *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case u, ok := <-ch:
			if !ok {
				// Stream closed — disconnect. Emit nothing (B8); reconnect
				// snapshot will reconcile.
				return
			}
			if isDirtyStateEvent(u.UpdateType) {
				// Replace-not-reset: abandon the old timer's channel entirely so
				// there is no drained/undrained footgun.
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.NewTimer(c.snapshotDebounce)
				debounceC = debounce.C
			}
		case <-debounceC:
			debounceC = nil
			debounce = nil
			if err := c.snapshot(ctx, name, client, updates); err != nil {
				c.ulog.Debug("Satellite re-snapshot failed; next event retries").
					Field("satellite", name).Err(err).Log(ctx)
			}
		}
	}
}

// snapshot fetches the remote jobs+sessions, sanitizes + origin-stamps every
// row, and emits one UpdateSatelliteSnapshot (the origin-scoped reconcile
// primitive). A fetch error returns without emitting so the caller can retry.
func (c *SatelliteCollector) snapshot(ctx context.Context, name string, client *coredaemon.RemoteClient, updates chan<- store.Update) error {
	jobs, err := client.ListJobs(ctx, models.JobFilter{})
	if err != nil {
		return err
	}
	sessions, err := client.GetSessions(ctx)
	if err != nil {
		return err
	}

	for _, j := range jobs {
		satellite.SanitizeJobInfo(j, name)
	}
	for _, s := range sessions {
		satellite.SanitizeSession(s, name)
	}

	select {
	case updates <- store.Update{
		Type:    store.UpdateSatelliteSnapshot,
		Source:  "satellite",
		Origin:  name,
		Scanned: len(jobs) + len(sessions),
		Payload: &store.SatelliteSnapshotPayload{
			Origin:   name,
			Jobs:     jobs,
			Sessions: sessions,
		},
	}:
	case <-ctx.Done():
	}
	return nil
}

// isConnected reports whether the ConnManager currently has the satellite
// connected, read from the satellite_status Store surface (C17).
func (c *SatelliteCollector) isConnected(st *store.Store, name string) bool {
	status, ok := st.GetSatelliteStatuses()[name]
	return ok && status != nil && status.State == "connected"
}

// isDirtyStateEvent reports whether a remote SSE update_type should trigger a
// re-snapshot. It matches the job/session-typed events plus the initial/full
// snapshots — never parsing payloads (C16/E16).
func isDirtyStateEvent(updateType string) bool {
	switch updateType {
	case "sessions", "session", "initial", "full":
		return true
	}
	return strings.HasPrefix(updateType, "job_")
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
