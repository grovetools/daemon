package collector

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// recordingPtyKiller records every PtyID the collector asks to kill, so a test
// can assert which scopes' PTYs were (and were NOT) touched.
type recordingPtyKiller struct {
	mu     sync.Mutex
	killed []string
}

func (k *recordingPtyKiller) KillPty(ptyID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.killed = append(k.killed, ptyID)
	return nil
}

func (k *recordingPtyKiller) wasKilled(ptyID string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, id := range k.killed {
		if id == ptyID {
			return true
		}
	}
	return false
}

// pidZeroSession builds a PID-0 "running" session past the grace period — the
// shape the filesystem job-watcher synthesizes on a daemon that did not itself
// launch the agent. Its real PID is only discoverable via the registry. The
// PtyID mirrors what crash-recovery puts on the store record, so a reap
// exercises the KillPty path. (UpdateSessions replaces the whole set, so all
// seed sessions must be applied in a single update.)
func pidZeroSession(sessionID, ptyID string) *models.Session {
	oldTime := time.Now().Add(-5 * time.Minute)
	return &models.Session{ID: sessionID, PID: 0, PtyID: ptyID, Status: "running", StartedAt: oldTime, LastActivity: oldTime}
}

// TestSessionCollector_ReapsPIDZero_OnlyOwnScope is the core cross-scope
// agent-reaping regression. The reaper recovers a PID-0 session's real PID from
// the GLOBAL crash-recovery registry, but must only do so when the registry
// record's owning scope matches this collector's own scope. A scoped collector
// that adopts+reaps an agent owned by ANOTHER daemon is exactly the leak that
// made killing one daemon interrupt another's agents.
//
// Same-scope: the dead PID is recovered, the session is reaped, and its PTY is
// killed. Foreign-scope: the record is left strictly alone.
func TestSessionCollector_ReapsPIDZero_OnlyOwnScope(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir()) // sandbox the shared global registry

	const ownScope = "/sandbox/worktree-a"

	// Two PID-0 store sessions: one whose registry record is owned by THIS
	// scope, one owned by a DIFFERENT scope. Both registry PIDs are a real proc
	// we then kill, so the only thing deciding "reap vs leave alone" is the scope
	// gate on registry recovery — not liveness.
	reg, err := sessions.NewFileSystemRegistry()
	if err != nil {
		t.Fatalf("NewFileSystemRegistry: %v", err)
	}
	procs := make([]*exec.Cmd, 0, 2)
	register := func(sessionID, ptyID, scope string) {
		p := exec.Command("sleep", "60")
		if err := p.Start(); err != nil {
			t.Fatalf("start proc for %s: %v", sessionID, err)
		}
		procs = append(procs, p)
		if err := reg.Register(sessions.SessionMetadata{
			SessionID:       sessionID,
			JobID:           sessionID,
			ClaudeSessionID: sessionID + "-native",
			PID:             p.Process.Pid,
			PtyID:           ptyID,
			Scope:           scope,
		}); err != nil {
			t.Fatalf("Register %s: %v", sessionID, err)
		}
	}
	register("own-session", "pty-own", ownScope)
	register("foreign-session", "pty-foreign", "/sandbox/worktree-b")
	defer func() {
		for _, p := range procs {
			_ = p.Process.Kill()
			_, _ = p.Process.Wait()
		}
	}()

	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSessions,
		Source:  "test",
		Payload: []*models.Session{pidZeroSession("own-session", "pty-own"), pidZeroSession("foreign-session", "pty-foreign")},
	})

	killer := &recordingPtyKiller{}
	c := NewSessionCollector(50*time.Millisecond, ownScope) // collector for OUR scope
	c.SetPtyKiller(killer)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	updates := make(chan store.Update, 100)
	go func() { _ = c.Run(ctx, st, updates) }()

	// Let the collector recover the own-scope PID and observe it alive, then kill
	// BOTH procs. The foreign one must never be adopted in the first place, so it
	// can never be reaped regardless of liveness.
	time.Sleep(300 * time.Millisecond)
	for _, p := range procs {
		_ = p.Process.Kill()
		_, _ = p.Process.Wait()
	}

	sawOwnEnd := false
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case u := <-updates:
			if u.Type != store.UpdateSessionEnd {
				continue
			}
			p, ok := u.Payload.(*store.SessionEndPayload)
			if !ok {
				continue
			}
			if p.JobID == "foreign-session" {
				t.Fatalf("scoped collector reaped a FOREIGN-scope session — cross-scope leak")
			}
			if p.JobID == "own-session" {
				sawOwnEnd = true
				break loop
			}
		case <-deadline:
			break loop
		}
	}

	if !sawOwnEnd {
		t.Fatal("collector did not reap its own dead PID-0 session via registry recovery")
	}
	if !killer.wasKilled("pty-own") {
		t.Error("collector did not KillPty its own agent's PTY")
	}
	if killer.wasKilled("pty-foreign") {
		t.Fatal("collector called KillPty on a FOREIGN-scope agent's PTY — cross-scope leak")
	}
}

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

	c := NewSessionCollector(50*time.Millisecond, "")

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

	c := NewSessionCollector(50*time.Millisecond, "")
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

// TestSessionCollector_ReapsPendingViaRegistryScoped is the pending-strand
// regression: a session stuck at Status "pending" (claude exited before
// completing its first turn) must still be reaped, not linger forever. It
// exercises the full path a hook-registered pending session takes — PID 0 in
// the store, real PID (and owning scope) only in the crash-recovery registry —
// so it also covers the scope gate on registry recovery. Before pending was
// added to the status gate the collector skipped it every tick.
func TestSessionCollector_ReapsPendingViaRegistryScoped(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir()) // isolate the global registry

	const ownScope = "/sandbox/worktree-a"

	// A real child gives a genuinely-alive PID we later kill, mirroring a hook-
	// registered agent that stalled at "pending" and then exited. It must be
	// alive when the collector starts so startup crash-recovery does not
	// unregister the record before the liveness loop can recover its PID.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child process: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	reg, err := sessions.NewFileSystemRegistry()
	if err != nil {
		t.Fatalf("NewFileSystemRegistry: %v", err)
	}
	sessionID := "pending-strand-scoped"
	// A hook-registered pending session always has a registry record with a real
	// PID and its owning scope stamped (the fix in hooks/context.go).
	if err := reg.Register(sessions.SessionMetadata{
		SessionID:       sessionID,
		JobID:           sessionID,
		ClaudeSessionID: sessionID + "-native",
		PID:             pid,
		Scope:           ownScope,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	st := store.New()
	oldTime := time.Now().Add(-5 * time.Minute) // past the grace period
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			// PID 0 + Status "pending" — the shape a pending strand presents. The
			// real PID is only discoverable via the registry, gated on scope.
			{ID: sessionID, PID: 0, Status: "pending", StartedAt: oldTime, LastActivity: oldTime},
		},
	})

	c := NewSessionCollector(50*time.Millisecond, ownScope) // collector for OUR scope
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	updates := make(chan store.Update, 100)
	go func() { _ = c.Run(ctx, st, updates) }()

	// Let the collector recover the scoped PID and observe it alive, then kill it
	// so subsequent polls read it dead and reap the pending strand.
	time.Sleep(300 * time.Millisecond)
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	timeout := time.After(2 * time.Second)
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionEnd {
				if p, ok := u.Payload.(*store.SessionEndPayload); ok && p.JobID == sessionID {
					return // pending strand reaped via registry-recovered scoped PID
				}
			}
		case <-timeout:
			t.Fatal("collector did not reap a dead pending session via its registry-recovered scoped PID")
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

	c := NewSessionCollector(50*time.Millisecond, "")
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

	c := NewSessionCollector(50*time.Millisecond, "")
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

	c := NewSessionCollector(50*time.Millisecond, "")
	// Note: this session was injected via the store (not crash recovery), so it
	// is not seeded as seenAlive — the collector must leave it alone.
	if drainForSessionEnd(t, c, st, sessionID, 600*time.Millisecond) {
		t.Fatal("collector reaped a never-confirmed-alive PID (startup-race regression)")
	}
}
