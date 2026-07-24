package channels

import (
	"context"
	"testing"
	"time"
)

func TestManagerStopDoesNotDeadlockSavingRoutes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	m := NewManager(nil, SignalConfig{}, HAConfig{}, "test-scope", "test.sock")
	m.routeTable[123] = "job-1"

	done := make(chan struct{})
	go func() {
		m.Stop(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Manager.Stop deadlocked while saving routes")
	}

	state, err := loadChannelState()
	if err != nil {
		t.Fatalf("load persisted channel state: %v", err)
	}
	if got := state.QuoteRoutes[123]; got != "job-1" {
		t.Fatalf("persisted route = %q, want job-1", got)
	}
}
