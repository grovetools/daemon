package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func TestPhase5FixturesProduceOneProjectedDaemonRowPerCurrentAttempt(t *testing.T) {
	cases := []struct {
		name       string
		attempts   []string
		confirm    bool
		terminal   string
		wantStatus string
	}{
		{name: "a claude killed before hook", attempts: []string{"attempt-a"}, wantStatus: "pending"},
		{name: "b pi startup failure", attempts: []string{"attempt-b"}, terminal: "failed", wantStatus: "failed"},
		{name: "c sigkill mid-turn", attempts: []string{"attempt-c"}, confirm: true, terminal: "interrupted", wantStatus: "interrupted"},
		{name: "d daemon restart mid-session", attempts: []string{"attempt-d"}, confirm: true, wantStatus: "running"},
		{name: "e retry reusing job id", attempts: []string{"attempt-e-old", "attempt-e-current"}, confirm: true, wantStatus: "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			for i, attemptID := range tc.attempts {
				// The job watcher is authoritative for which reusable-ID attempt is
				// current before its intent may replace the projected session row.
				st.mu.Lock()
				st.state.Jobs["reused-job"] = &models.JobInfo{ID: "reused-job", AttemptID: attemptID, Type: models.JobType("interactive_agent"), Status: "running"}
				st.mu.Unlock()
				st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
					JobID: "reused-job", AttemptID: attemptID, Provider: "pi", Type: models.SessionTypeInteractiveAgent,
				}})
				if tc.confirm {
					st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
						JobID: "reused-job", AttemptID: attemptID, NativeID: "native-" + attemptID, PID: 100 + i,
					}})
				}
			}
			current := tc.attempts[len(tc.attempts)-1]
			if tc.terminal != "" {
				st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
					JobID: "reused-job", AttemptID: current, Outcome: tc.terminal,
				}})
			}
			rows := st.GetSessions()
			if len(rows) != 1 || rows[0].AttemptID != current || rows[0].Status != tc.wantStatus {
				t.Fatalf("projected rows = %+v, want one current attempt %s status %s", rows, current, tc.wantStatus)
			}
		})
	}
}

func TestSessionLifecycleRejectsStalePriorAttempt(t *testing.T) {
	st := newTestStore(t)
	st.mu.Lock()
	st.state.Jobs["reused-job"] = &models.JobInfo{ID: "reused-job", AttemptID: "attempt-2", Type: models.JobType("interactive_agent"), Status: "running"}
	st.mu.Unlock()

	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "reused-job", AttemptID: "attempt-1", Provider: "claude", Type: models.SessionTypeInteractiveAgent,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "reused-job", AttemptID: "attempt-1", NativeID: "native-1", PID: 101,
	}})
	if got := st.GetJob("reused-job"); got.AttemptID != "attempt-2" {
		t.Fatalf("stale intent rewrote authoritative job attempt: %+v", got)
	}
	if got := st.GetSession("reused-job"); got != nil {
		t.Fatalf("stale intent created projected row: %+v", got)
	}

	// A retry reuses the Flow JobID but establishes a fresh projected attempt.
	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "reused-job", AttemptID: "attempt-2", Provider: "claude", Type: models.SessionTypeHeadlessAgent,
	}})
	if got := len(st.GetSessions()); got != 1 {
		t.Fatalf("projected session rows = %d, want exactly one current row", got)
	}

	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "reused-job", AttemptID: "attempt-1", NativeID: "stale-native", PID: 999,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{
		JobID: "reused-job", AttemptID: "attempt-1", Status: "idle",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
		JobID: "reused-job", AttemptID: "attempt-1", Outcome: "interrupted",
	}})
	// Missing identity is legacy, not a wildcard for the current attempt.
	st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{
		JobID: "reused-job", Status: "idle",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
		JobID: "reused-job", Outcome: "interrupted",
	}})

	got := st.GetSession("reused-job")
	if got.AttemptID != "attempt-2" || got.PID != 0 || got.ClaudeSessionID != "" || got.Status != "pending" || got.EndedAt != nil {
		t.Fatalf("stale prior-attempt callbacks mutated current row: %+v", got)
	}
	if job := st.GetJob("reused-job"); job.Status != "running" || job.PID != 0 || job.CompletedAt != nil {
		t.Fatalf("stale/empty callback mutated identified current job: %+v", job)
	}

	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "reused-job", AttemptID: "attempt-2", NativeID: "native-2", PID: 202,
	}})
	// A duplicate current intent arriving after confirmation is a no-op; it
	// cannot demote running back to pending or discard the provider identity.
	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "reused-job", AttemptID: "attempt-2", Provider: "claude", Type: models.SessionTypeInteractiveAgent,
	}})
	if got := st.GetSession("reused-job"); got.Status != "running" || got.PID != 202 || got.ClaudeSessionID != "native-2" {
		t.Fatalf("duplicate current intent demoted confirmed row: %+v", got)
	}
	st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{
		JobID: "reused-job", AttemptID: "attempt-2", Status: "idle",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
		JobID: "reused-job", AttemptID: "attempt-2", Outcome: "completed",
	}})
	got = st.GetSession("reused-job")
	if got.AttemptID != "attempt-2" || got.PID != 202 || got.Status != "completed" || got.EndedAt == nil {
		t.Fatalf("matching current-attempt lifecycle did not apply: %+v", got)
	}
}

func TestSessionLifecyclePreservesLegacyEmptyAttempt(t *testing.T) {
	st := newTestStore(t)
	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "legacy", Provider: "claude", Type: models.SessionTypeInteractiveAgent,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "legacy", NativeID: "legacy-native", PID: 303,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{JobID: "legacy", Status: "idle"}})
	st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{JobID: "legacy", Outcome: "interrupted"}})

	got := st.GetSession("legacy")
	if got == nil || got.AttemptID != "" || got.PID != 303 || got.Status != "interrupted" || got.EndedAt == nil {
		t.Fatalf("legacy empty-attempt lifecycle changed semantics: %+v", got)
	}
}

func TestDerivedSessionWritersRejectStaleAttempt(t *testing.T) {
	st := newTestStore(t)
	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "job", AttemptID: "attempt-current", Type: models.SessionTypeInteractiveAgent,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "job", AttemptID: "attempt-current", NativeID: "native", PID: 42,
	}})
	before := st.GetSession("job").LastActivity
	later := before.Add(time.Minute)

	st.ApplyUpdate(Update{Type: UpdateSessionActivity, Payload: &SessionActivityPayload{
		JobID: "job", AttemptID: "attempt-old", ObservedAt: later, Source: "transcript",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionVerdict, Payload: &SessionVerdictPayload{
		JobID: "job", AttemptID: "attempt-old", Verified: "stale",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionTokens, Payload: &SessionTokensPayload{Updates: []SessionTokenUpdate{{
		JobID: "job", AttemptID: "attempt-old", LiveTokens: 99,
	}}}})
	st.SetSessionPtyID("job", "attempt-old", "pty-old")

	got := st.GetSession("job")
	if !got.LastActivity.Equal(before) || got.Verified != "" || got.LiveTokens != 0 || got.PtyID != "" {
		t.Fatalf("stale derived writers mutated current attempt: %+v", got)
	}

	st.ApplyUpdate(Update{Type: UpdateSessionActivity, Payload: &SessionActivityPayload{
		JobID: "job", AttemptID: "attempt-current", ObservedAt: later, Source: "transcript",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionVerdict, Payload: &SessionVerdictPayload{
		JobID: "job", AttemptID: "attempt-current", Verified: "alive",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionTokens, Payload: &SessionTokensPayload{Updates: []SessionTokenUpdate{{
		JobID: "job", AttemptID: "attempt-current", LiveTokens: 100,
	}}}})
	st.SetSessionPtyID("job", "attempt-current", "pty-current")
	got = st.GetSession("job")
	if !got.LastActivity.After(before) || got.Verified != "alive" || got.LiveTokens != 100 || got.PtyID != "pty-current" {
		t.Fatalf("exact derived writers did not update current attempt: %+v", got)
	}
}

func TestSessionStatusNeverCreatesKindlessRow(t *testing.T) {
	st := newTestStore(t)
	st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{
		JobID: "unknown", AttemptID: "attempt-x", Status: "running",
	}})
	if got := st.GetSession("unknown"); got != nil {
		t.Fatalf("status-only update manufactured kind-less row: %+v", got)
	}

	st.mu.Lock()
	st.state.Jobs["known"] = &models.JobInfo{ID: "known", AttemptID: "attempt-y", Type: models.SessionTypeHeadlessAgent, Status: "running"}
	st.mu.Unlock()
	st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{
		JobID: "known", AttemptID: "stale-attempt", Status: "running",
	}})
	if got := st.GetSession("known"); got != nil {
		t.Fatalf("stale status manufactured a row for current job: %+v", got)
	}
	st.ApplyUpdate(Update{Type: UpdateSessionStatus, Payload: &SessionStatusPayload{
		JobID: "known", AttemptID: "attempt-y", Status: "running",
	}})
	got := st.GetSession("known")
	if got == nil || got.Type != models.SessionTypeHeadlessAgent || got.AttemptID != "attempt-y" {
		t.Fatalf("status did not inherit known job type/attempt: %+v", got)
	}
}
