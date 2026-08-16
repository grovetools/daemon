package cmd

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/daemon/internal/daemon/pidfile"
)

// ---------------------------------------------------------------------------
// Post-bind self-check
// ---------------------------------------------------------------------------
//
// The pidfile lock elects one starter, and that election is what actually stops
// two cold `groved start` invocations from both becoming daemons. This watchdog
// is the belt-and-suspenders behind it, because two of the daemon's own
// mechanisms are destructive rather than exclusive:
//
//   - Server.Listen unlinks whatever sits at the socket path before binding, so
//     a process that binds after us takes every future client while our
//     listener keeps accepting on an inode with no name.
//   - the pidfile is a plain file that any process can rewrite, and once it
//     names somebody else we are invisible to `groved status`, `groved stats`,
//     `groved stop` and every auto-start client.
//
// A daemon in either state is a shadow: it serves nobody, but it still runs a
// full set of collectors, fsnotify watchers, log tailers and its own signal-cli
// subprocess against the same account as the real daemon. That is the 2h22m,
// 120%-CPU process the 2026-08-15 audit found — visible only in `ps`.
//
// So the daemon polices its own identity and stands down when it loses it. The
// check is two stats and one small file read on a slow cadence, and it starts
// only after the socket is bound (the earliest point at which either artifact
// can be stolen).

const (
	// shadowCheckInitialDelay lets boot settle before the first check. Nothing
	// can steal the socket before it exists, and the very first moments after
	// bind are when a legitimate handoff (an upgrade successor that beat our
	// predecessor's teardown) is most likely to be mid-flight.
	shadowCheckInitialDelay = 15 * time.Second

	// shadowCheckInterval is the steady-state cadence. A steal is permanent
	// once it happens, so this only decides how long a shadow survives, and
	// paying two stats a minute is the right price for that.
	shadowCheckInterval = 60 * time.Second

	// shadowCheckConfirmDelay is how long a suspected loss must persist before
	// the daemon acts on it. A socket path is replaced non-atomically (unlink,
	// then bind), so a single stat can catch an unrelated tool mid-replace.
	shadowCheckConfirmDelay = 2 * time.Second

	// ShadowCheckEnv set to "0"/"false" disables the self-check, and any Go
	// duration compresses all three timings above for fixtures.
	ShadowCheckEnv = "GROVE_SHADOW_CHECK"
)

// shadowCheckParams is what the watchdog needs. Both probes are injected so the
// decision logic can be tested without a bound socket or a real pidfile.
type shadowCheckParams struct {
	// socketLost reports that the socket path holds a different file than the
	// one this daemon bound, with a detail line for the log.
	socketLost func() (bool, string)
	// pidfileLost reports that the pidfile no longer names this process.
	pidfileLost func() (bool, string)
	// shadowed is closed exactly once, when this daemon should stand down.
	shadowed chan struct{}
	ulog     *logging.UnifiedLogger

	initialDelay time.Duration
	interval     time.Duration
	confirmDelay time.Duration
}

// shadowCheckEnabled reports whether the self-check should run.
func shadowCheckEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ShadowCheckEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// watchForShadowing polls this daemon's two identity artifacts and closes
// p.shadowed when either has been taken over.
//
// Losing EITHER is sufficient. They fail independently — a rebind steals the
// socket while leaving the pidfile alone, an old binary's Release deletes a
// pidfile while leaving the socket alone — and either one on its own means no
// client will ever reach this process again.
func watchForShadowing(ctx context.Context, p shadowCheckParams) {
	if p.initialDelay <= 0 {
		p.initialDelay = shadowCheckInitialDelay
	}
	if p.interval <= 0 {
		p.interval = shadowCheckInterval
	}
	if p.confirmDelay <= 0 {
		p.confirmDelay = shadowCheckConfirmDelay
	}
	if raw := strings.TrimSpace(os.Getenv(ShadowCheckEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			p.initialDelay, p.interval, p.confirmDelay = d, d, d
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

		if lost, _, _ := probeShadowed(p); !lost {
			continue
		}

		// Confirm before acting: an unlink-then-create by any other tool is
		// two syscalls, and catching the gap between them is not a steal. The
		// confirming probe is the one whose verdict is logged, so the detail
		// describes the state that is actually being acted on.
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.confirmDelay):
		}
		lost, artifact, detail := probeShadowed(p)
		if !lost {
			continue
		}

		p.ulog.Error("Daemon identity lost; standing down as a shadow daemon").
			Field("event", "daemon.shadowed").
			Field("artifact", artifact).
			Field("detail", detail).
			Field("pid", os.Getpid()).
			Log(ctx)
		close(p.shadowed)
		return
	}
}

// pidfileLost adapts pidfile.Owns to the watchdog's probe shape.
//
// Missing and replaced pidfiles both count as lost election metadata. The
// stable sibling lock still prevents a second workload during confirmation,
// while retiring here ensures the surviving daemon never remains invisible.
func pidfileLost(path string) (bool, string) {
	owns, why := pidfile.Owns(path)
	return !owns, why
}

// probeShadowed runs both probes, reporting which artifact was lost.
func probeShadowed(p shadowCheckParams) (lost bool, artifact, detail string) {
	if p.socketLost != nil {
		if lost, detail := p.socketLost(); lost {
			return true, "socket", detail
		}
	}
	if p.pidfileLost != nil {
		if lost, detail := p.pidfileLost(); lost {
			return true, "pidfile", detail
		}
	}
	return false, "", ""
}
