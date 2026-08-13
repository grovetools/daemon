package cmd

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/daemon"
)

// ---------------------------------------------------------------------------
// Scoped self-yield
// ---------------------------------------------------------------------------
//
// A scoped groved that was auto-started because no host daemon was registered
// yet has nothing to do once one is. It has no UI, no clients, and no work; it
// exists only because a client asked a question during the seconds of a fleet
// restart when the registry was empty. Left alone it runs a full set of
// watchers, tailers and collectors until the auto-shutdown timer reaps it —
// minutes of duplicated load, and it lands exactly on the boot-sweep spike.
//
// So it hands the scope back. The daemon watches for a live, reachable host
// covering its scope and, when it finds one while owning nothing, drains
// through the ordinary stop path (PTY reaping, scoped tuimux teardown, pidfile
// release) as if it had been sent SIGTERM.
//
// Every condition below is a guard against yielding a daemon somebody wants:
//
//   - scope != "": the global daemon is the fallback everyone else yields TO.
//   - --auto-shutdown: the flag the auto-start factory adds and a human running
//     `groved start --scope ...` does not. An explicitly started daemon is
//     never second-guessed.
//   - no --pair-with-pid: a paired daemon belongs to a named process (a tend
//     fixture, a treemux) and dies with it, not on our judgment.
//   - a REACHABLE host on a DIFFERENT socket: a registration alone is not
//     enough — yielding to a socket that answers nobody would strand the
//     scope — and a host whose socket is ours is a treemux that spawned us,
//     which we must not exit out from under.
//   - idle: no attached clients, no sessions, no jobs in flight. This is the
//     load-bearing guard: a scoped daemon's stop path kills the agent PTYs it
//     owns, so yielding while it owns any would kill live agents.

const (
	// hostYieldInitialDelay is how long a fresh scoped daemon runs before it
	// will consider yielding. It covers the gap between our socket binding and
	// the client that spawned us actually attaching: a spawning client connects
	// inside its own factory call (sub-second), and a treemux additionally
	// registers itself as the host for this very scope, which the socket
	// comparison then rules out on its own. Ten seconds is generous against
	// both, and it is what bounds a fleet-restart straggler's life.
	hostYieldInitialDelay = 10 * time.Second

	// hostYieldInterval is the re-check cadence. A host that appears later (a
	// treemux started after us, or a global groved that finished booting after
	// we did) still reclaims the scope, at a poll rate that costs a directory
	// read and one dial.
	hostYieldInterval = 10 * time.Second

	// HostYieldDelayEnv compresses both timings for fixtures, as a duration
	// applied to the initial delay; the interval becomes the same value.
	// Unset in normal operation.
	HostYieldDelayEnv = "GROVE_HOST_YIELD_DELAY"

	// HostYieldEnv set to "0" or "false" disables self-yield. It is the control
	// arm of the acceptance measurement — with it and GROVE_HOST_BOOT_GRACE=0,
	// one binary reproduces the pre-fix behavior exactly, so the fixture can
	// show the stragglers appearing and then not appearing without comparing
	// two builds on a machine whose load drifts.
	HostYieldEnv = "GROVE_HOST_YIELD"
)

// hostYieldEnabled reports whether the self-yield watcher should run.
func hostYieldEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(HostYieldEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// hostYieldParams collects what the yield watcher needs. idle and the clock
// timings are injected so the decision can be tested without a daemon.
type hostYieldParams struct {
	scope      string
	socketPath string
	// idle reports whether this daemon currently owns nothing that would die
	// with it: no attached clients, no sessions, no jobs in flight.
	idle func() bool
	// yield is closed exactly once, when the daemon should stand down.
	yield chan struct{}
	ulog  *logging.UnifiedLogger

	// lookupHost and reachable default to the real registry and a real dial.
	lookupHost func(dir string) (daemon.HostRecord, bool)
	reachable  func(socketPath string) bool
	// initialDelay and interval default to the constants above.
	initialDelay time.Duration
	interval     time.Duration
}

func yieldToHostDaemon(ctx context.Context, p hostYieldParams) {
	if p.lookupHost == nil {
		p.lookupHost = daemon.LookupHost
	}
	if p.reachable == nil {
		p.reachable = daemon.SocketReachable
	}
	if p.initialDelay <= 0 {
		p.initialDelay = hostYieldInitialDelay
	}
	if p.interval <= 0 {
		p.interval = hostYieldInterval
	}
	if raw := strings.TrimSpace(os.Getenv(HostYieldDelayEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			p.initialDelay, p.interval = d, d
		}
	}

	timer := time.NewTimer(p.initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer.Reset(p.interval)

		host, ok := p.lookupHost(p.scope)
		switch {
		case !ok, host.Starting, host.SocketPath == p.socketPath:
			continue
		case !p.idle():
			continue
		case !p.reachable(host.SocketPath):
			continue
		}

		p.ulog.Info("Scoped daemon yielding to host daemon").
			Field("scope", p.scope).
			Field("host_socket", host.SocketPath).
			Field("host_program", host.Program).
			Field("host_pid", host.PID).
			Log(ctx)
		close(p.yield)
		return
	}
}
