package cmd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/daemon"
)

// yieldFixture drives yieldToHostDaemon with an injected registry and dialer,
// on timings short enough for a unit test. Everything the real watcher would
// learn from the filesystem and the network is a field here.
type yieldFixture struct {
	// mu guards the fields the watcher goroutine reads while a test mutates
	// them mid-run (TestYieldWaitsForConditionsToBecomeSafe does exactly that).
	mu        sync.Mutex
	host      daemon.HostRecord
	hostFound bool
	reachable bool
	idle      bool
	yield     chan struct{}
}

func (f *yieldFixture) set(mut func(*yieldFixture)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mut(f)
}

func (f *yieldFixture) run(t *testing.T, scope, sock string) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.yield = make(chan struct{})
	go yieldToHostDaemon(ctx, hostYieldParams{
		scope:      scope,
		socketPath: sock,
		idle: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.idle
		},
		yield: f.yield,
		ulog:  logging.NewUnifiedLogger("test.yield"),
		lookupHost: func(string) (daemon.HostRecord, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.host, f.hostFound
		},
		reachable: func(string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.reachable
		},
		initialDelay: 10 * time.Millisecond,
		interval:     10 * time.Millisecond,
	})
	t.Cleanup(cancel)
	return cancel
}

// yielded reports whether the watcher decided to stand down within a window
// generous enough to cover several poll intervals.
func (f *yieldFixture) yielded() bool {
	select {
	case <-f.yield:
		return true
	case <-time.After(300 * time.Millisecond):
		return false
	}
}

// TestYieldsToReachableHostDaemon is the behavior itself: an idle scoped daemon
// with a live, reachable host covering its scope hands the scope back rather
// than idling out the auto-shutdown timer.
func TestYieldsToReachableHostDaemon(t *testing.T) {
	f := &yieldFixture{
		host:      daemon.HostRecord{SocketPath: "/run/global.sock", Program: "groved", PID: 42, Daemon: true},
		hostFound: true,
		reachable: true,
		idle:      true,
	}
	f.run(t, "/wt/perf-audit", "/run/scoped.sock")

	if !f.yielded() {
		t.Fatal("an idle scoped daemon did not yield to a reachable host daemon")
	}
}

// TestYieldHoldsWhenNotSafe walks every guard. Each row is a way a scoped
// daemon can be wanted; none of them may end in an exit.
func TestYieldHoldsWhenNotSafe(t *testing.T) {
	cases := []struct {
		name string
		why  string
		mut  func(*yieldFixture)
	}{
		{
			"no host registered", "there is nothing to yield to",
			func(f *yieldFixture) { f.hostFound = false },
		},
		{
			"host still starting", "its socket cannot serve the scope yet",
			func(f *yieldFixture) { f.host.Starting = true },
		},
		{
			"host is us", "a treemux that spawned this daemon registered our own socket",
			func(f *yieldFixture) { f.host.SocketPath = "/run/scoped.sock" },
		},
		{
			"host unreachable", "yielding to a socket nobody answers would strand the scope",
			func(f *yieldFixture) { f.reachable = false },
		},
		{
			"daemon busy", "the stop path would kill the sessions and jobs it owns",
			func(f *yieldFixture) { f.idle = false },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &yieldFixture{
				host:      daemon.HostRecord{SocketPath: "/run/global.sock", Daemon: true},
				hostFound: true,
				reachable: true,
				idle:      true,
			}
			tc.mut(f)
			f.run(t, "/wt/perf-audit", "/run/scoped.sock")

			if f.yielded() {
				t.Fatalf("yielded although %s", tc.why)
			}
		})
	}
}

// TestYieldWaitsForConditionsToBecomeSafe: the watcher keeps looking. A host
// that finishes booting, or a daemon that goes idle after its client detaches,
// still reclaims the scope on a later pass.
func TestYieldWaitsForConditionsToBecomeSafe(t *testing.T) {
	f := &yieldFixture{
		host:      daemon.HostRecord{SocketPath: "/run/global.sock", Daemon: true, Starting: true},
		hostFound: true,
		reachable: true,
		idle:      false,
	}
	f.run(t, "/wt/perf-audit", "/run/scoped.sock")

	if f.yielded() {
		t.Fatal("yielded while the host was starting and the daemon was busy")
	}
	f.set(func(f *yieldFixture) {
		f.host.Starting = false
		f.idle = true
	})
	if !f.yielded() {
		t.Fatal("did not yield once the host was ready and the daemon went idle")
	}
}

// TestYieldStopsWithContext: the watcher is a boot-time goroutine and must not
// outlive the daemon's context (nor fire into a shutdown already under way).
func TestYieldStopsWithContext(t *testing.T) {
	f := &yieldFixture{
		host:      daemon.HostRecord{SocketPath: "/run/global.sock", Daemon: true},
		hostFound: true,
		reachable: true,
		idle:      false,
	}
	cancel := f.run(t, "/wt/perf-audit", "/run/scoped.sock")
	cancel()
	f.set(func(f *yieldFixture) { f.idle = true })

	if f.yielded() {
		t.Fatal("a cancelled watcher still asked the daemon to stand down")
	}
}
