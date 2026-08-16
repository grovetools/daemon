package collector

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions/health"
	"github.com/grovetools/daemon/internal/daemon/store"
	tuimuxpty "github.com/grovetools/tuimux/pty"
)

type fakePtyActivitySource struct {
	metas []tuimuxpty.SessionMetadata
	calls atomic.Int32
}

func (f *fakePtyActivitySource) ListPtys() ([]tuimuxpty.SessionMetadata, error) {
	f.calls.Add(1)
	return f.metas, nil
}

func TestIngestActivityUsesAdvancingTranscriptAndExactPtyID(t *testing.T) {
	now := time.Now().Round(0)
	transcript := t.TempDir() + "/transcript.jsonl"
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptAt := now.Add(-time.Minute)
	if err := os.Chtimes(transcript, transcriptAt, transcriptAt); err != nil {
		t.Fatal(err)
	}
	ptyAt := now.Add(-30 * time.Second)
	source := &fakePtyActivitySource{metas: []tuimuxpty.SessionMetadata{
		{ID: "wrong", Tags: map[string]string{"job_id": "job"}, LastActivity: now},
		{ID: "pty-exact", LastActivity: ptyAt},
	}}
	c := NewSessionCollector(time.Second, "")
	c.SetPtyActivitySource(source)
	rows := []*models.Session{{
		ID: "job", Type: "interactive_agent", Status: "running", PtyID: "pty-exact",
		StartedAt: now.Add(-time.Hour), LastActivity: now.Add(-2 * time.Minute), TranscriptPath: transcript,
	}}
	updates := make(chan store.Update, 4)
	effective := c.ingestActivity(context.Background(), rows, now, updates)
	if source.calls.Load() != 1 {
		t.Fatalf("PTY source called %d times", source.calls.Load())
	}
	if !effective[0].LastActivity.Equal(ptyAt) {
		t.Fatalf("effective activity = %v, want exact PTY activity %v", effective[0].LastActivity, ptyAt)
	}
	if !rows[0].LastActivity.Equal(now.Add(-2 * time.Minute)) {
		t.Fatal("collector mutated live store snapshot")
	}
	seen := map[string]time.Time{}
	for len(updates) > 0 {
		p := (<-updates).Payload.(*store.SessionActivityPayload)
		seen[p.Source] = p.ObservedAt
	}
	if !seen["transcript"].Equal(transcriptAt) || !seen["pty"].Equal(ptyAt) {
		t.Fatalf("activity updates = %#v", seen)
	}

	// Polling unchanged evidence is observational. A strict transcript mtime
	// advance emits again even when token values would remain unchanged.
	_ = c.ingestActivity(context.Background(), rows, now, updates)
	if len(updates) != 1 { // PTY remains newer than the unchanged store snapshot
		t.Fatalf("unchanged transcript emitted again; updates=%d", len(updates))
	}
	<-updates
	advanced := now.Add(-10 * time.Second)
	if err := os.Chtimes(transcript, advanced, advanced); err != nil {
		t.Fatal(err)
	}
	_ = c.ingestActivity(context.Background(), rows, now, updates)
	foundTranscript := false
	for len(updates) > 0 {
		p := (<-updates).Payload.(*store.SessionActivityPayload)
		foundTranscript = foundTranscript || p.Source == "transcript" && p.ObservedAt.Equal(advanced)
	}
	if !foundTranscript {
		t.Fatal("strict transcript mtime advance did not renew activity")
	}
}

func TestKilledProviderBecomesUnverifiedWithinLeaseWithReaperDisabled(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // reap the child so process liveness is unambiguously false

	started := time.Now().Add(-time.Hour).Round(0)
	last := started.Add(time.Minute)
	var clock atomic.Int64
	clock.Store(last.Add(50 * time.Millisecond).UnixNano())
	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateSessions, Payload: []*models.Session{{
		ID: "killed", PID: pid, Type: "headless_agent", Status: "running",
		StartedAt: started, LastActivity: last, Verified: "alive",
	}}})
	c := NewSessionCollector(5*time.Millisecond, "")
	c.now = func() time.Time { return time.Unix(0, clock.Load()) }
	c.SetLeasePolicy(health.LeasePolicy{Interactive: time.Second, Headless: 100 * time.Millisecond, TurnBased: time.Second})
	c.reapEnabled = false
	c.probeHealth = func(context.Context, []*models.Session, time.Time) []*health.Probe { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan store.Update, 100)
	var ended atomic.Bool
	go func() { _ = c.Run(ctx, st, updates) }()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case u := <-updates:
				if u.Type == store.UpdateSessionEnd {
					ended.Store(true)
				}
				st.ApplyUpdate(u)
			}
		}
	}()

	clock.Store(last.Add(101 * time.Millisecond).UnixNano())
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got := st.GetSession("killed")
		if got.Verified == "unverified" {
			if ended.Load() {
				t.Fatal("reaper-disabled collector emitted session end")
			}
			if got.Status != "running" || got.EndedAt != nil {
				t.Fatalf("lease expiry changed lifecycle: %+v", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("killed provider did not become unverified: %+v", st.GetSession("killed"))
}
