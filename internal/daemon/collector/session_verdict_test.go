package collector

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions/health"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func staticHealthProbe(state health.State, evidence health.Evidence) func(context.Context, []*models.Session, time.Time) []*health.Probe {
	return func(_ context.Context, rows []*models.Session, now time.Time) []*health.Probe {
		out := make([]*health.Probe, 0, len(rows))
		for _, row := range rows {
			out = append(out, &health.Probe{Session: row, ProbedAt: now, Evidence: evidence, Verdict: health.Verdict{State: state}})
		}
		return out
	}
}

func TestSessionCollectorFlagsDeathAfterGraceWithoutSeenAlive(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	started := time.Unix(1_000, 0)
	var nowUnix atomic.Int64
	nowUnix.Store(started.Add(10 * time.Second).UnixNano()) // initially inside grace
	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateSessions, Payload: []*models.Session{{
		ID: "dies-in-grace", PID: 99999999, Type: "headless_agent", Status: "running",
		StartedAt: started, LastActivity: started,
	}}})
	c := NewSessionCollector(10*time.Millisecond, "")
	c.now = func() time.Time { return time.Unix(0, nowUnix.Load()) }
	c.probeHealth = staticHealthProbe(health.Stale, health.Evidence{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan store.Update, 100)
	go func() { _ = c.Run(ctx, st, updates) }()
	time.Sleep(40 * time.Millisecond)
	nowUnix.Store(started.Add(sessionReapGracePeriod + time.Second).UnixNano())

	deadline := time.After(time.Second)
	for {
		select {
		case u := <-updates:
			switch u.Type {
			case store.UpdateSessionEnd:
				t.Fatal("never-seen-alive death was reaped")
			case store.UpdateSessionVerdict:
				p := u.Payload.(*store.SessionVerdictPayload)
				if p.JobID == "dies-in-grace" && p.Verified == "stale" {
					return
				}
			}
		case <-deadline:
			t.Fatal("death inside grace was not flagged stale after grace elapsed")
		}
	}
}

func TestSessionCollectorFlagsPIDZeroDeadRegistry(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	// Past classifier grace but still inside the headless activity lease: this
	// isolates the Phase-2 stale verdict from Phase-4 expiry (which intentionally
	// retracts to unverified).
	old := time.Now().Add(-time.Minute)
	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateSessions, Payload: []*models.Session{{
		ID: "pid-zero", PID: 0, Type: "headless_agent", Status: "running",
		StartedAt: old, LastActivity: old,
	}}})
	c := NewSessionCollector(10*time.Millisecond, "")
	c.probeHealth = staticHealthProbe(health.Stale, health.Evidence{
		RegistryFound: true, HasMetadata: true, MetaScope: "", RegistryPID: 99999999,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	updates := make(chan store.Update, 100)
	go func() { _ = c.Run(ctx, st, updates) }()
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionVerdict {
				p := u.Payload.(*store.SessionVerdictPayload)
				if p.JobID == "pid-zero" && p.Verified == "stale" {
					return
				}
			}
		case <-ctx.Done():
			t.Fatal("PID-0 dead registry row was not flagged stale")
		}
	}
}

func TestSessionCollectorVerdictWriterSkipsTerminalRows(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	old := time.Now().Add(-time.Hour)
	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateSessions, Payload: []*models.Session{{
		ID: "terminal", PID: 99999999, Status: "interrupted", StartedAt: old, LastActivity: old,
	}}})
	c := NewSessionCollector(10*time.Millisecond, "")
	c.probeHealth = staticHealthProbe(health.Stale, health.Evidence{})

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	updates := make(chan store.Update, 20)
	go func() { _ = c.Run(ctx, st, updates) }()
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionVerdict {
				t.Fatal("terminal row received verdict update")
			}
		case <-ctx.Done():
			return
		}
	}
}
