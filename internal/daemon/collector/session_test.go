package collector

import (
	"context"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func TestSessionCollector_ReapsDeadPID(t *testing.T) {
	// Create a store with a "running" session that has a dead PID.
	// PID 99999999 is almost certainly not alive on any system.
	st := store.New()
	deadPID := 99999999
	sessionID := "test-dead-session"

	// Seed the store with a session whose PID is dead and whose timestamps
	// are old enough to be past the 30-second grace period.
	oldTime := time.Now().Add(-5 * time.Minute)
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			{
				ID:           sessionID,
				PID:          deadPID,
				Status:       "running",
				StartedAt:    oldTime,
				LastActivity: oldTime,
			},
		},
	})

	// Verify session is in the store
	sessions := st.GetSessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "running" {
		t.Fatalf("expected status 'running', got %q", sessions[0].Status)
	}

	// Create the collector with a very fast interval
	collector := NewSessionCollector(100 * time.Millisecond)

	// Run the collector in a goroutine with a context that we'll cancel
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	updates := make(chan store.Update, 100)

	go func() {
		_ = collector.Run(ctx, st, updates)
	}()

	// Wait for the reaper to detect the dead PID and send an update
	var gotEndUpdate bool
	timeout := time.After(2 * time.Second)
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionEnd {
				payload, ok := u.Payload.(*store.SessionEndPayload)
				if !ok {
					t.Fatalf("expected SessionEndPayload, got %T", u.Payload)
				}
				if payload.JobID != sessionID {
					t.Errorf("expected job ID %q, got %q", sessionID, payload.JobID)
				}
				if payload.Outcome != "interrupted" {
					t.Errorf("expected outcome 'interrupted', got %q", payload.Outcome)
				}
				gotEndUpdate = true
				cancel() // Stop the collector
				goto done
			}
			// Skip non-end updates (e.g., crash recovery)
		case <-timeout:
			goto done
		}
	}

done:
	if !gotEndUpdate {
		t.Fatal("session collector did not reap the dead PID within the timeout")
	}
}

func TestSessionCollector_SkipsGracePeriod(t *testing.T) {
	// Sessions within the 30-second grace period should NOT be reaped.
	st := store.New()
	sessionID := "test-grace-session"

	// Session started just now — within the grace period
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			{
				ID:           sessionID,
				PID:          99999999, // dead PID
				Status:       "running",
				StartedAt:    time.Now(),
				LastActivity: time.Now(),
			},
		},
	})

	collector := NewSessionCollector(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	updates := make(chan store.Update, 100)
	go func() {
		_ = collector.Run(ctx, st, updates)
	}()

	// Wait for context to expire — should NOT see an UpdateSessionEnd
	<-ctx.Done()

	// Drain the channel
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionEnd {
				t.Fatal("session collector reaped a session within the grace period")
			}
		default:
			return
		}
	}
}
