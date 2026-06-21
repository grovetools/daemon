package collector

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// drainForSessionEnd runs the collector against st and reports whether it
// emitted an UpdateSessionEnd for sessionID before the deadline.
func drainForSessionEnd(t *testing.T, c *SessionCollector, st *store.Store, sessionID string, within time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()

	updates := make(chan store.Update, 100)
	go func() { _ = c.Run(ctx, st, updates) }()

	timeout := time.After(within)
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionEnd {
				payload, ok := u.Payload.(*store.SessionEndPayload)
				if !ok {
					t.Fatalf("expected SessionEndPayload, got %T", u.Payload)
				}
				if payload.JobID == sessionID {
					if payload.Outcome != "interrupted" {
						t.Errorf("expected outcome 'interrupted', got %q", payload.Outcome)
					}
					return true
				}
			}
		case <-timeout:
			return false
		}
	}
}

// TestSessionCollector_ReapsDeadPIDAfterSeenAlive verifies the alive→dead
// transition is reaped: a PID observed alive at least once and then dead (for
// reapDeadStrikes consecutive polls) is reaped.
func TestSessionCollector_ReapsDeadPIDAfterSeenAlive(t *testing.T) {
	// A real child process gives us a PID that is genuinely alive, then dead.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child process: %v", err)
	}
	pid := cmd.Process.Pid
	sessionID := "test-alive-then-dead"

	st := store.New()
	oldTime := time.Now().Add(-5 * time.Minute) // past the grace period
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			{ID: sessionID, PID: pid, Status: "running", StartedAt: oldTime, LastActivity: oldTime},
		},
	})

	c := NewSessionCollector(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	updates := make(chan store.Update, 100)
	go func() { _ = c.Run(ctx, st, updates) }()

	// Let the collector observe the PID alive at least once (seenAlive=true),
	// then kill the process so subsequent polls read it dead.
	time.Sleep(300 * time.Millisecond)
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait() // reap the zombie so IsProcessAlive reports dead

	timeout := time.After(2 * time.Second)
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionEnd {
				if p, ok := u.Payload.(*store.SessionEndPayload); ok && p.JobID == sessionID {
					return // reaped as expected
				}
			}
		case <-timeout:
			t.Fatal("collector did not reap the dead PID after it was seen alive")
		}
	}
}

// TestSessionCollector_ReapsPIDZeroViaRegistry is the orphaned-agent regression:
// a store session with PID 0 (synthesized by the filesystem job-watcher on a
// daemon that did not launch the agent) must still be reaped when the agent has
// died, by recovering its real — now dead — PID from the global crash-recovery
// registry. Before the fix the collector skipped every PID-0 session, so such
// orphans lingered "running" forever.
func TestSessionCollector_ReapsPIDZeroViaRegistry(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir()) // isolate the global registry

	// A real child gives a genuinely-alive PID that we later kill, mirroring an
	// agent confirmed alive (registry written) that subsequently exits.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child process: %v", err)
	}
	pid := cmd.Process.Pid

	reg, err := sessions.NewFileSystemRegistry()
	if err != nil {
		t.Fatalf("NewFileSystemRegistry: %v", err)
	}
	sessionID := "coordinate-orphan-zero-pid"
	// The registry holds the confirmed identity + real PID (as written at confirm
	// time by whichever scoped daemon launched the agent).
	if err := reg.Register(sessions.SessionMetadata{
		SessionID:       sessionID,
		JobID:           sessionID,
		ClaudeSessionID: "00000000-aaaa-bbbb-cccc-000000000000",
		PID:             pid,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	st := store.New()
	oldTime := time.Now().Add(-5 * time.Minute) // past the grace period
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			// PID 0 — the watcher-synthesized record with no native ID. The real
			// PID is only discoverable via the registry.
			{ID: sessionID, PID: 0, Status: "running", StartedAt: oldTime, LastActivity: oldTime},
		},
	})

	c := NewSessionCollector(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	updates := make(chan store.Update, 100)
	go func() { _ = c.Run(ctx, st, updates) }()

	// Let the collector recover the PID and observe it alive, then kill it.
	time.Sleep(300 * time.Millisecond)
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	timeout := time.After(2 * time.Second)
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionEnd {
				if p, ok := u.Payload.(*store.SessionEndPayload); ok && p.JobID == sessionID {
					return // reaped via registry-recovered PID, as intended
				}
			}
		case <-timeout:
			t.Fatal("collector did not reap a PID-0 session via its registry-recovered PID")
		}
	}
}

// TestSessionCollector_SkipsPIDZeroWithoutRegistry verifies the guard is not
// over-broad: a PID-0 session with NO registry entry is a genuinely unstarted
// intent and must be left alone (no PID to judge), not reaped.
func TestSessionCollector_SkipsPIDZeroWithoutRegistry(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir()) // empty registry

	st := store.New()
	sessionID := "test-unstarted-intent"
	oldTime := time.Now().Add(-5 * time.Minute)
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			{ID: sessionID, PID: 0, Status: "running", StartedAt: oldTime, LastActivity: oldTime},
		},
	})

	c := NewSessionCollector(50 * time.Millisecond)
	if drainForSessionEnd(t, c, st, sessionID, 600*time.Millisecond) {
		t.Fatal("collector reaped a PID-0 session with no confirmed PID (unstarted-intent regression)")
	}
}

// TestSessionCollector_SkipsGracePeriod verifies sessions within the startup
// grace window are never reaped, even with a dead PID.
func TestSessionCollector_SkipsGracePeriod(t *testing.T) {
	st := store.New()
	sessionID := "test-grace-session"
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			{
				ID:           sessionID,
				PID:          99999999, // dead PID
				Status:       "running",
				StartedAt:    time.Now(), // within the grace period
				LastActivity: time.Now(),
			},
		},
	})

	c := NewSessionCollector(50 * time.Millisecond)
	if drainForSessionEnd(t, c, st, sessionID, 500*time.Millisecond) {
		t.Fatal("collector reaped a session within the grace period")
	}
}

// TestSessionCollector_SkipsNeverAliveDeadPID verifies the startup-race guard:
// a session past the grace period whose PID is dead but was NEVER observed
// alive (e.g. a slow startup whose real agent PID hasn't registered yet) must
// NOT be reaped. This is the case the user was worried about.
func TestSessionCollector_SkipsNeverAliveDeadPID(t *testing.T) {
	st := store.New()
	sessionID := "test-never-alive"
	oldTime := time.Now().Add(-5 * time.Minute) // past the grace period
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			{ID: sessionID, PID: 99999999, Status: "running", StartedAt: oldTime, LastActivity: oldTime},
		},
	})

	c := NewSessionCollector(50 * time.Millisecond)
	// Note: this session was injected via the store (not crash recovery), so it
	// is not seeded as seenAlive — the collector must leave it alone.
	if drainForSessionEnd(t, c, st, sessionID, 600*time.Millisecond) {
		t.Fatal("collector reaped a never-confirmed-alive PID (startup-race regression)")
	}
}
