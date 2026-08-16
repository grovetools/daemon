package store

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
)

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
