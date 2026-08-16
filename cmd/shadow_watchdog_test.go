package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/daemon/internal/daemon/pidfile"
)

func testWatchParams(shadowed chan struct{}) shadowCheckParams {
	return shadowCheckParams{
		shadowed:     shadowed,
		ulog:         logging.NewUnifiedLogger("test"),
		initialDelay: 5 * time.Millisecond,
		interval:     5 * time.Millisecond,
		confirmDelay: 5 * time.Millisecond,
	}
}

func TestWatchForShadowingStandsDownOnASocketSteal(t *testing.T) {
	shadowed := make(chan struct{})
	p := testWatchParams(shadowed)
	p.socketLost = func() (bool, string) { return true, "socket replaced" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchForShadowing(ctx, p)

	select {
	case <-shadowed:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never fired on a stolen socket")
	}
}

func TestWatchForShadowingStandsDownOnAPidfileTakeover(t *testing.T) {
	shadowed := make(chan struct{})
	p := testWatchParams(shadowed)
	p.pidfileLost = func() (bool, string) { return true, "pidfile names pid 999999" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchForShadowing(ctx, p)

	select {
	case <-shadowed:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never fired on a pidfile takeover")
	}
}

// TestWatchForShadowingIgnoresATransientMiss is the whole reason for the
// confirm step: a socket path is replaced with an unlink followed by a bind, so
// one unlucky stat must not take a healthy daemon down.
func TestWatchForShadowingIgnoresATransientMiss(t *testing.T) {
	var calls atomic.Int32
	shadowed := make(chan struct{})
	p := testWatchParams(shadowed)
	p.socketLost = func() (bool, string) {
		// Lost on the first probe only.
		return calls.Add(1) == 1, "momentary"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchForShadowing(ctx, p)

	select {
	case <-shadowed:
		t.Fatal("watchdog fired on an unconfirmed miss")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatchForShadowingStopsWithItsContext(t *testing.T) {
	shadowed := make(chan struct{})
	p := testWatchParams(shadowed)
	p.initialDelay = time.Hour
	p.socketLost = func() (bool, string) { return true, "never reached" }

	ctx, cancel := context.WithCancel(context.Background())
	go watchForShadowing(ctx, p)
	cancel()

	select {
	case <-shadowed:
		t.Fatal("watchdog fired after its context was cancelled")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPidfileLost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groved.pid")

	// Ours.
	if err := pidfile.AcquireWait(path, 0); err != nil {
		t.Fatal(err)
	}
	if lost, why := pidfileLost(path); lost {
		t.Fatalf("our own pidfile reported lost: %s", why)
	}

	// Taken over by another process.
	if err := os.WriteFile(path, []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lost, _ := pidfileLost(path); !lost {
		t.Error("a pidfile naming another process should count as lost")
	}

	// Simply deleted: not a takeover, and not worth taking a live daemon down.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if lost, why := pidfileLost(path); lost {
		t.Errorf("a missing pidfile should not count as lost, got %q", why)
	}
	_ = pidfile.Release(path)
}

func TestPidfileLostIgnoresOurOwnPidWrittenByAnotherPath(t *testing.T) {
	// A pidfile this process never acquired but which names it (an upgrade
	// successor re-reading its own path, a test fixture) is still ours.
	path := filepath.Join(t.TempDir(), "groved.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if lost, why := pidfileLost(path); lost {
		t.Errorf("pidfile naming us reported lost: %s", why)
	}
}

func TestShadowCheckEnabled(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", true},
		{"0", false},
		{"false", false},
		{"off", false},
		{"no", false},
		{"1", true},
		{"200ms", true}, // a duration compresses the timings, it does not disable
	} {
		t.Setenv(ShadowCheckEnv, tc.env)
		if got := shadowCheckEnabled(); got != tc.want {
			t.Errorf("%s=%q: got %v, want %v", ShadowCheckEnv, tc.env, got, tc.want)
		}
	}
}
