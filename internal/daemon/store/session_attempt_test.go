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
		{name: "e retry reusing job id", attempts: []string{"018f0000-0002-7000-8000-000000000001", "018f0000-0003-7000-8000-000000000001"}, confirm: true, wantStatus: "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			for i, attemptID := range tc.attempts {
				if i > 0 {
					st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
						JobID: "reused-job", AttemptID: tc.attempts[i-1], Outcome: "interrupted",
					}})
				}
				// Model the collector having observed this attempt; the retry case
				// still exercises replacement of the prior terminal session row.
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

func TestRetriedAttemptSupersedesStaleTerminalProjections(t *testing.T) {
	st := newTestStore(t)
	ended := time.Now().Add(-time.Minute)
	st.mu.Lock()
	st.state.Jobs["retry-job"] = &models.JobInfo{
		ID: "retry-job", AttemptID: "018f0000-0000-7000-8000-000000000001", Type: models.JobType("interactive_agent"),
		Status: "completed", PID: 101, CompletedAt: &ended, Error: "old failure",
	}
	st.state.Sessions["retry-job"] = &models.Session{
		ID: "retry-job", AttemptID: "018f0000-0000-7000-8000-000000000001", Type: models.SessionTypeInteractiveAgent,
		Status: "interrupted", PID: 101, StartedAt: ended.Add(-time.Minute), LastActivity: ended, EndedAt: &ended,
	}
	st.mu.Unlock()

	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "retry-job", AttemptID: "018f0000-0001-7000-8000-000000000001", Provider: "claude", Type: models.SessionTypeInteractiveAgent,
	}})
	if got := st.GetSession("retry-job"); got == nil || got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Status != "pending" || got.PID != 0 {
		t.Fatalf("new intent did not replace terminal session projection: %+v", got)
	}
	if got := st.GetJob("retry-job"); got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Status != "running" || got.PID != 0 || got.CompletedAt != nil || got.Error != "" {
		t.Fatalf("new intent did not repair stale terminal job projection: %+v", got)
	}

	// The collector can lag between every lifecycle edge and restore the old
	// terminal Jobs row. Confirmation must still advance the already-visible
	// retry without treating that stale projection as an active duplicate.
	st.mu.Lock()
	st.state.Jobs["retry-job"] = &models.JobInfo{
		ID: "retry-job", AttemptID: "018f0000-0000-7000-8000-000000000001", Type: models.JobType("interactive_agent"),
		Status: "completed", PID: 101, CompletedAt: &ended, Error: "old failure",
	}
	st.mu.Unlock()
	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "retry-job", AttemptID: "018f0000-0001-7000-8000-000000000001", NativeID: "native-new", PID: 202,
	}})
	if got := st.GetSession("retry-job"); got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Status != "running" || got.PID != 202 || got.ClaudeSessionID != "native-new" {
		t.Fatalf("new confirmation did not advance replacement attempt: %+v", got)
	}
	if got := st.GetJob("retry-job"); got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Status != "running" || got.PID != 202 || got.CompletedAt != nil || got.Error != "" {
		t.Fatalf("new confirmation did not repair stale terminal job projection: %+v", got)
	}

	// Exercise the same lag immediately before the terminal edge.
	st.mu.Lock()
	st.state.Jobs["retry-job"] = &models.JobInfo{
		ID: "retry-job", AttemptID: "018f0000-0000-7000-8000-000000000001", Type: models.JobType("interactive_agent"),
		Status: "completed", PID: 101, CompletedAt: &ended, Error: "old failure",
	}
	st.mu.Unlock()
	st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
		JobID: "retry-job", AttemptID: "018f0000-0001-7000-8000-000000000001", Outcome: "completed",
	}})
	if got := st.GetSession("retry-job"); got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Status != "completed" || got.EndedAt == nil {
		t.Fatalf("new end did not terminate replacement attempt: %+v", got)
	}
	if got := st.GetJob("retry-job"); got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Status != "completed" || got.PID != 0 || got.CompletedAt == nil {
		t.Fatalf("new end did not terminate replacement job projection: %+v", got)
	}
}

func TestCurrentAttemptConfirmationRepairsLaggingActiveJobProjection(t *testing.T) {
	st := newTestStore(t)
	const olderAttempt = "018f0000-0000-7000-8000-000000000001"
	const newerAttempt = "018f0000-0001-7000-8000-000000000001"
	ended := time.Now().Add(-time.Minute)
	st.mu.Lock()
	st.state.Jobs["retry-job"] = &models.JobInfo{
		ID: "retry-job", AttemptID: olderAttempt, Type: models.JobType("interactive_agent"),
		Status: "completed", PID: 101, CompletedAt: &ended,
	}
	st.state.Sessions["retry-job"] = &models.Session{
		ID: "retry-job", AttemptID: olderAttempt, Type: models.SessionTypeInteractiveAgent,
		Status: "completed", PID: 101, StartedAt: ended.Add(-time.Minute), LastActivity: ended, EndedAt: &ended,
	}
	st.mu.Unlock()

	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "retry-job", AttemptID: newerAttempt, Type: models.SessionTypeInteractiveAgent,
	}})
	if got := st.GetSession("retry-job"); got == nil || got.AttemptID != newerAttempt || got.Status != "pending" {
		t.Fatalf("new intent was not accepted before projection lag: %+v", got)
	}

	// Model the collector restoring a still-running row from the older attempt
	// after intent but before process confirmation.
	st.mu.Lock()
	st.state.Jobs["retry-job"] = &models.JobInfo{
		ID: "retry-job", AttemptID: olderAttempt, Type: models.JobType("interactive_agent"), Status: "running", PID: 101,
	}
	st.mu.Unlock()

	// Even though the stale callback agrees with Jobs, the current Session is
	// authoritative and must keep the older attempt from being revived.
	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "retry-job", AttemptID: olderAttempt, NativeID: "native-old", PID: 303,
	}})
	if got := st.GetSession("retry-job"); got.AttemptID != newerAttempt || got.Status != "pending" || got.PID != 0 || got.ClaudeSessionID != "" {
		t.Fatalf("older confirmation mutated current session before repair: %+v", got)
	}
	if got := st.GetJob("retry-job"); got.AttemptID != olderAttempt || got.Status != "running" || got.PID != 101 {
		t.Fatalf("rejected older confirmation mutated lagging job projection: %+v", got)
	}

	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "retry-job", AttemptID: newerAttempt, NativeID: "native-new", PID: 202,
	}})
	if got := st.GetSession("retry-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 202 || got.ClaudeSessionID != "native-new" {
		t.Fatalf("matching confirmation did not advance current session: %+v", got)
	}
	if got := st.GetJob("retry-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 202 || got.CompletedAt != nil {
		t.Fatalf("matching confirmation did not repair lagging active job projection: %+v", got)
	}

	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "retry-job", AttemptID: olderAttempt, NativeID: "native-old", PID: 303,
	}})
	if got := st.GetSession("retry-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 202 || got.ClaudeSessionID != "native-new" {
		t.Fatalf("older confirmation mutated current session: %+v", got)
	}
	if got := st.GetJob("retry-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 202 {
		t.Fatalf("older confirmation mutated repaired job projection: %+v", got)
	}
}

func TestRetriedAttemptSupersedesOrphanedJobProjection(t *testing.T) {
	st := newTestStore(t)
	const olderAttempt = "018f0000-0000-7000-8000-000000000001"
	const newerAttempt = "018f0000-0001-7000-8000-000000000001"
	st.mu.Lock()
	st.state.Jobs["orphaned-job"] = &models.JobInfo{
		ID: "orphaned-job", AttemptID: olderAttempt, Type: models.JobType("interactive_agent"),
		Status: "orphaned", PID: 101, Error: "daemon lost track of this job",
	}
	st.mu.Unlock()

	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "orphaned-job", AttemptID: newerAttempt, Type: models.SessionTypeInteractiveAgent,
	}})
	if got := st.GetSession("orphaned-job"); got == nil || got.AttemptID != newerAttempt || got.Status != "pending" || got.PID != 0 {
		t.Fatalf("new intent did not replace orphaned job projection: %+v", got)
	}
	if got := st.GetJob("orphaned-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 0 || got.Error != "" {
		t.Fatalf("new intent did not repair orphaned job projection: %+v", got)
	}

	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "orphaned-job", AttemptID: newerAttempt, NativeID: "native-new", PID: 202,
	}})
	if got := st.GetSession("orphaned-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 202 || got.ClaudeSessionID != "native-new" {
		t.Fatalf("new confirmation did not advance retry: %+v", got)
	}
	if got := st.GetJob("orphaned-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 202 {
		t.Fatalf("new confirmation did not update job projection: %+v", got)
	}

	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "orphaned-job", AttemptID: olderAttempt, NativeID: "native-old", PID: 303,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
		JobID: "orphaned-job", AttemptID: olderAttempt, Outcome: "failed",
	}})
	if got := st.GetSession("orphaned-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 202 || got.ClaudeSessionID != "native-new" || got.EndedAt != nil {
		t.Fatalf("older callbacks mutated active retry session: %+v", got)
	}
	if got := st.GetJob("orphaned-job"); got.AttemptID != newerAttempt || got.Status != "running" || got.PID != 202 || got.CompletedAt != nil {
		t.Fatalf("older callbacks mutated active retry job: %+v", got)
	}
}

func TestRetriedAttemptCannotTakeOverActiveProjection(t *testing.T) {
	st := newTestStore(t)
	st.mu.Lock()
	st.state.Jobs["active-job"] = &models.JobInfo{
		ID: "active-job", AttemptID: "attempt-active", Type: models.JobType("interactive_agent"), Status: "running", PID: 101,
	}
	st.state.Sessions["active-job"] = &models.Session{
		ID: "active-job", AttemptID: "attempt-active", Type: models.SessionTypeInteractiveAgent,
		Status: "running", PID: 101, ClaudeSessionID: "native-active", StartedAt: time.Now(), LastActivity: time.Now(),
	}
	st.mu.Unlock()

	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "active-job", AttemptID: "018f0000-0001-7000-8000-000000000001", Type: models.SessionTypeInteractiveAgent,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "active-job", AttemptID: "018f0000-0001-7000-8000-000000000001", NativeID: "native-new", PID: 202,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
		JobID: "active-job", AttemptID: "018f0000-0001-7000-8000-000000000001", Outcome: "completed",
	}})

	if got := st.GetSession("active-job"); got.AttemptID != "attempt-active" || got.Status != "running" || got.PID != 101 || got.ClaudeSessionID != "native-active" || got.EndedAt != nil {
		t.Fatalf("new attempt took over active session projection: %+v", got)
	}
	if got := st.GetJob("active-job"); got.AttemptID != "attempt-active" || got.Status != "running" || got.PID != 101 || got.CompletedAt != nil {
		t.Fatalf("new attempt took over active job projection: %+v", got)
	}
}

func TestOlderAttemptCannotRetakeTerminalProjection(t *testing.T) {
	st := newTestStore(t)
	ended := time.Now().Add(-time.Minute)
	const newerAttempt = "018f0000-0001-7000-8000-000000000001"
	const olderAttempt = "018f0000-0000-7000-8000-000000000001"
	st.mu.Lock()
	st.state.Jobs["terminal-job"] = &models.JobInfo{
		ID: "terminal-job", AttemptID: newerAttempt, Type: models.JobType("interactive_agent"), Status: "completed", CompletedAt: &ended,
	}
	st.state.Sessions["terminal-job"] = &models.Session{
		ID: "terminal-job", AttemptID: newerAttempt, Type: models.SessionTypeInteractiveAgent,
		Status: "completed", StartedAt: ended.Add(-time.Minute), LastActivity: ended, EndedAt: &ended,
	}
	st.mu.Unlock()

	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "terminal-job", AttemptID: olderAttempt, Type: models.SessionTypeInteractiveAgent,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{
		JobID: "terminal-job", AttemptID: olderAttempt, NativeID: "old-native", PID: 101,
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{
		JobID: "terminal-job", AttemptID: olderAttempt, Outcome: "failed",
	}})

	if got := st.GetSession("terminal-job"); got.AttemptID != newerAttempt || got.Status != "completed" || got.PID != 0 || got.EndedAt == nil {
		t.Fatalf("older callbacks retook terminal session projection: %+v", got)
	}
	if got := st.GetJob("terminal-job"); got.AttemptID != newerAttempt || got.Status != "completed" || got.PID != 0 || got.CompletedAt == nil {
		t.Fatalf("older callbacks retook terminal job projection: %+v", got)
	}
}

func TestRetriedAttemptSupersedesStaleSessionAndLaggingActiveJob(t *testing.T) {
	st := newTestStore(t)
	st.mu.Lock()
	st.state.Jobs["stale-job"] = &models.JobInfo{ID: "stale-job", AttemptID: "018f0000-0000-7000-8000-000000000001", Type: models.JobType("interactive_agent"), Status: "running", PID: 101}
	st.state.Sessions["stale-job"] = &models.Session{
		ID: "stale-job", AttemptID: "018f0000-0000-7000-8000-000000000001", Type: models.SessionTypeInteractiveAgent,
		Status: "running", Verified: "stale", PID: 101, StartedAt: time.Now(), LastActivity: time.Now(),
	}
	st.mu.Unlock()

	st.ApplyUpdate(Update{Type: UpdateSessionIntent, Payload: &SessionIntentPayload{
		JobID: "stale-job", AttemptID: "018f0000-0001-7000-8000-000000000001", Type: models.SessionTypeInteractiveAgent,
	}})
	if got := st.GetSession("stale-job"); got == nil || got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Status != "pending" || got.Verified != "" {
		t.Fatalf("new intent did not replace stale session projection: %+v", got)
	}
	if got := st.GetJob("stale-job"); got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Status != "running" || got.PID != 0 {
		t.Fatalf("new intent did not repair lagging active job projection: %+v", got)
	}
}

func TestTerminalProjectionAcceptsMissedRetryEdges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update Update
		status string
		pid    int
	}{
		{name: "confirmation", update: Update{Type: UpdateSessionConfirmation, Payload: &SessionConfirmationPayload{JobID: "job", AttemptID: "018f0000-0001-7000-8000-000000000001", NativeID: "native-new", PID: 202}}, status: "running", pid: 202},
		{name: "end", update: Update{Type: UpdateSessionEnd, Payload: &SessionEndPayload{JobID: "job", AttemptID: "018f0000-0001-7000-8000-000000000001", Outcome: "failed"}}, status: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ended := time.Now().Add(-time.Minute)
			st.mu.Lock()
			st.state.Jobs["job"] = &models.JobInfo{ID: "job", AttemptID: "018f0000-0000-7000-8000-000000000001", Type: models.JobType("headless_agent"), Status: "completed", CompletedAt: &ended}
			st.state.Sessions["job"] = &models.Session{ID: "job", AttemptID: "018f0000-0000-7000-8000-000000000001", Type: models.SessionTypeHeadlessAgent, Status: "completed", StartedAt: ended, LastActivity: ended, EndedAt: &ended}
			st.mu.Unlock()

			st.ApplyUpdate(tc.update)
			got := st.GetSession("job")
			if got == nil || got.AttemptID != "018f0000-0001-7000-8000-000000000001" || got.Type != models.SessionTypeHeadlessAgent || got.Status != tc.status || got.PID != tc.pid {
				t.Fatalf("missed %s edge did not replace terminal projection: %+v", tc.name, got)
			}
			if job := st.GetJob("job"); job.AttemptID != "018f0000-0001-7000-8000-000000000001" || job.Status != tc.status {
				t.Fatalf("missed %s edge did not repair job projection: %+v", tc.name, job)
			}
		})
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
		JobID: "job", AttemptID: "018f0000-0000-7000-8000-000000000001", ObservedAt: later, Source: "transcript",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionVerdict, Payload: &SessionVerdictPayload{
		JobID: "job", AttemptID: "018f0000-0000-7000-8000-000000000001", Verified: "stale",
	}})
	st.ApplyUpdate(Update{Type: UpdateSessionTokens, Payload: &SessionTokensPayload{Updates: []SessionTokenUpdate{{
		JobID: "job", AttemptID: "018f0000-0000-7000-8000-000000000001", LiveTokens: 99,
	}}}})
	st.SetSessionPtyID("job", "018f0000-0000-7000-8000-000000000001", "pty-old")

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
