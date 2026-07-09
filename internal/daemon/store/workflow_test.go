package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
)

// newTestStore creates a Store backed by a temp state dir so workflow
// persistence never touches (or reads) the developer's real state.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("GROVE_HOME", t.TempDir())
	return New()
}

// seedSession registers and confirms a claude session in the store.
func seedSession(t *testing.T, s *Store, jobID, claudeSessionID string) {
	t.Helper()
	s.ApplyUpdate(Update{
		Type:   UpdateSessionIntent,
		Source: "test",
		Payload: &SessionIntentPayload{
			JobID:    jobID,
			Provider: "claude",
		},
	})
	s.ApplyUpdate(Update{
		Type:   UpdateSessionConfirmation,
		Source: "test",
		Payload: &SessionConfirmationPayload{
			JobID:          jobID,
			NativeID:       claudeSessionID,
			PID:            1234,
			TranscriptPath: "/tmp/transcript.jsonl",
		},
	})
}

func wfUpdate(ev models.WorkflowEvent, extra ...func(*WorkflowEventPayload)) Update {
	updateType, ok := UpdateTypeForWorkflowKind(ev.Kind)
	if !ok {
		panic("unknown kind " + string(ev.Kind))
	}
	payload := &WorkflowEventPayload{Event: ev}
	for _, fn := range extra {
		fn(payload)
	}
	return Update{Type: updateType, Source: ev.Source, Payload: payload}
}

func TestApplySessionConfirmationPersistsTranscriptPath(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	sess := s.GetSession("job-1")
	if sess == nil {
		t.Fatal("session not found")
	}
	if sess.TranscriptPath != "/tmp/transcript.jsonl" {
		t.Errorf("TranscriptPath = %q, want /tmp/transcript.jsonl", sess.TranscriptPath)
	}
	if sess.ClaudeSessionID != "sess-1" {
		t.Errorf("ClaudeSessionID = %q, want sess-1", sess.ClaudeSessionID)
	}
}

func TestApplyWorkflowUpdates(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	t0 := time.Date(2026, 6, 10, 17, 0, 0, 0, time.UTC)

	// Run discovery with script meta enrichment.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowRunDiscovered,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		Timestamp:       t0,
		Source:          models.WorkflowSourceJournal,
	}, func(p *WorkflowEventPayload) {
		p.RunName = "my-workflow"
		p.Phases = []string{"Phase 1", "Phase 2"}
	}))

	// Agent lifecycle.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1",
		Prompt:          "do the thing",
		Timestamp:       t0.Add(time.Second),
		Source:          models.WorkflowSourceJournal,
	}))

	runs := s.GetWorkflowRuns()
	run := runs["wf_1"]
	if run == nil {
		t.Fatal("run wf_1 not found")
	}
	if run.Name != "my-workflow" || len(run.Phases) != 2 {
		t.Errorf("run meta = %q/%v, want my-workflow/[Phase 1 Phase 2]", run.Name, run.Phases)
	}
	if run.JobID != "job-1" || run.ClaudeSessionID != "sess-1" {
		t.Errorf("run attribution = %q/%q", run.JobID, run.ClaudeSessionID)
	}
	agent := run.Agents["a1"]
	if agent == nil {
		t.Fatal("agent a1 not found")
	}
	if agent.Status != "running" || agent.TaskDescription != "do the thing" {
		t.Errorf("agent = status %q desc %q", agent.Status, agent.TaskDescription)
	}
	if run.StartedCount != 1 || run.CompletedCount != 0 {
		t.Errorf("counts = %d/%d, want 1/0", run.StartedCount, run.CompletedCount)
	}

	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1",
		ResultSummary:   "all good",
		Timestamp:       t0.Add(2 * time.Second),
		Source:          models.WorkflowSourceJournal,
	}))

	run = s.GetWorkflowRuns()["wf_1"]
	agent = run.Agents["a1"]
	if agent.Status != "completed" || !agent.Success {
		t.Errorf("agent = status %q success %v, want completed/true", agent.Status, agent.Success)
	}
	if got := agent.ResultSummary["text"]; got != "all good" {
		t.Errorf("result summary = %v, want all good", got)
	}
	if agent.DurationMs != 1000 {
		t.Errorf("DurationMs = %d, want 1000", agent.DurationMs)
	}
	if run.StartedCount != 1 || run.CompletedCount != 1 {
		t.Errorf("counts = %d/%d, want 1/1", run.StartedCount, run.CompletedCount)
	}

	// Session.Subagents mirrors the same record, no duplicates.
	sess := s.GetSession("job-1")
	if len(sess.Subagents) != 1 {
		t.Fatalf("session subagents = %d, want 1", len(sess.Subagents))
	}
	if sess.Subagents[0].ID != "a1" || sess.Subagents[0].Status != "completed" {
		t.Errorf("session subagent = %+v", sess.Subagents[0])
	}

	// Staleness.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowRunStale,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		Timestamp:       t0.Add(10 * time.Minute),
		Source:          models.WorkflowSourceJournal,
	}))
	if !s.GetWorkflowRuns()["wf_1"].Stale {
		t.Error("run should be stale")
	}
}

func TestWorkflowDedupeHookThenJournal(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	hookStart := time.Date(2026, 6, 10, 17, 7, 10, 0, time.UTC)

	// 1. Hook SubagentStart: no run attribution yet → ad-hoc bucket.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a1234567890abcdef",
		AgentType:       "workflow-subagent",
		Timestamp:       hookStart,
		Source:          models.WorkflowSourceHooks,
	}))

	adhoc := s.GetAdhocSubagents()
	if adhoc["job-1"]["a1234567890abcdef"] == nil {
		t.Fatal("expected a1234567890abcdef in the ad-hoc bucket")
	}

	// 2. Journal attribution arrives: agent migrates into the run, keeping
	// the hook's timestamp, gaining the journal's prompt.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1234567890abcdef",
		Prompt:          "journal prompt",
		Timestamp:       hookStart.Add(3 * time.Second), // daemon receive time
		Source:          models.WorkflowSourceJournal,
	}))

	if len(s.GetAdhocSubagents()) != 0 {
		t.Error("ad-hoc bucket should be empty after run attribution")
	}
	run := s.GetWorkflowRuns()["wf_1"]
	if run == nil {
		t.Fatal("run wf_1 not found")
	}
	agent := run.Agents["a1234567890abcdef"]
	if agent == nil {
		t.Fatal("agent a1234567890abcdef not migrated into run")
	}
	if !agent.StartedAt.Equal(hookStart) {
		t.Errorf("StartedAt = %v, want hook timestamp %v (hooks win on timestamps)", agent.StartedAt, hookStart)
	}
	if agent.TaskDescription != "journal prompt" {
		t.Errorf("TaskDescription = %q, want journal prompt", agent.TaskDescription)
	}
	if agent.TaskType != "workflow-subagent" {
		t.Errorf("TaskType = %q, want workflow-subagent", agent.TaskType)
	}
	if run.StartedCount != 1 {
		t.Errorf("StartedCount = %d, want 1 (deduped)", run.StartedCount)
	}

	// 3. Journal result, then hook stop: journal wins on results, hooks win
	// on completion timestamps.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1234567890abcdef",
		ResultSummary:   "journal result",
		Timestamp:       hookStart.Add(8 * time.Second),
		Source:          models.WorkflowSourceJournal,
	}))
	hookStop := hookStart.Add(6 * time.Second)
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a1234567890abcdef", // hooks may have no RunID; session lookup attributes it
		AgentType:       "workflow-subagent",
		LastMessage:     "hook last message",
		TranscriptPath:  "/tmp/agent-a1234567890abcdef.jsonl",
		Timestamp:       hookStop,
		Source:          models.WorkflowSourceHooks,
	}))

	run = s.GetWorkflowRuns()["wf_1"]
	agent = run.Agents["a1234567890abcdef"]
	if !agent.CompletedAt.Equal(hookStop) {
		t.Errorf("CompletedAt = %v, want hook timestamp %v", agent.CompletedAt, hookStop)
	}
	if got := agent.ResultSummary["text"]; got != "journal result" {
		t.Errorf("result = %v, want journal result (journal wins on results)", got)
	}
	if run.CompletedCount != 1 {
		t.Errorf("CompletedCount = %d, want 1 (deduped)", run.CompletedCount)
	}
	if len(s.GetAdhocSubagents()) != 0 {
		t.Error("hook completion must not create a duplicate ad-hoc record")
	}
}

func TestWorkflowDedupeJournalThenHook(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	journalStart := time.Date(2026, 6, 10, 17, 7, 12, 0, time.UTC)
	hookStart := journalStart.Add(-2 * time.Second) // hooks fire before the journal flushes

	// 1. Journal first: agent lands directly in the run.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_2",
		AgentID:         "a2",
		Prompt:          "journal prompt",
		Timestamp:       journalStart,
		Source:          models.WorkflowSourceJournal,
	}))

	// 2. Hook start (no RunID): must find the run-attributed record and
	// override the timestamp, not create an ad-hoc duplicate.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a2",
		AgentType:       "workflow-subagent",
		Timestamp:       hookStart,
		Source:          models.WorkflowSourceHooks,
	}))

	run := s.GetWorkflowRuns()["wf_2"]
	agent := run.Agents["a2"]
	if agent == nil {
		t.Fatal("agent a2 not found in run")
	}
	if !agent.StartedAt.Equal(hookStart) {
		t.Errorf("StartedAt = %v, want hook timestamp %v", agent.StartedAt, hookStart)
	}
	if agent.TaskDescription != "journal prompt" {
		t.Errorf("TaskDescription = %q, journal prompt must survive the hook merge", agent.TaskDescription)
	}
	if len(s.GetAdhocSubagents()) != 0 {
		t.Error("hook event must merge into the run, not the ad-hoc bucket")
	}
	if run.StartedCount != 1 {
		t.Errorf("StartedCount = %d, want 1", run.StartedCount)
	}
}

func TestAdhocAgentToolSubagent(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	ts := time.Date(2026, 6, 10, 17, 7, 21, 0, time.UTC)
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a4567890abcdef123",
		AgentType:       "Explore",
		Prompt:          "find the handler",
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	}))
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a4567890abcdef123",
		AgentType:       "Explore",
		LastMessage:     "found it",
		Timestamp:       ts.Add(4 * time.Second),
		Source:          models.WorkflowSourceHooks,
	}))

	adhoc := s.GetAdhocSubagents()
	agent := adhoc["job-1"]["a4567890abcdef123"]
	if agent == nil {
		t.Fatal("ad-hoc agent not found")
	}
	if agent.Status != "completed" || agent.TaskType != "Explore" {
		t.Errorf("agent = status %q type %q", agent.Status, agent.TaskType)
	}
	if got := agent.ResultSummary["text"]; got != "found it" {
		t.Errorf("result = %v, want found it (LastMessage fallback)", got)
	}
	if len(s.GetWorkflowRuns()) != 0 {
		t.Error("ad-hoc spawns must not create workflow runs")
	}

	// Session mirror covers ad-hoc agents too.
	sess := s.GetSession("job-1")
	if len(sess.Subagents) != 1 || sess.Subagents[0].ID != "a4567890abcdef123" {
		t.Errorf("session subagents = %+v", sess.Subagents)
	}
}

func TestWorkflowRestartRebuildFromPersistedEvents(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())

	t0 := time.Date(2026, 6, 10, 17, 0, 0, 0, time.UTC)

	s1 := New()
	s1.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowRunDiscovered,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		Timestamp:       t0,
		Source:          models.WorkflowSourceJournal,
	}, func(p *WorkflowEventPayload) {
		p.RunName = "persisted-flow"
		p.Phases = []string{"P1"}
	}))
	s1.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1",
		Prompt:          "task",
		Timestamp:       t0.Add(time.Second),
		Source:          models.WorkflowSourceHooks,
	}))
	s1.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1",
		ResultSummary:   "done",
		Timestamp:       t0.Add(2 * time.Second),
		Source:          models.WorkflowSourceJournal,
	}))
	// A run-less ad-hoc agent persists too.
	s1.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-2",
		ClaudeSessionID: "sess-2",
		AgentID:         "a234567890abcdef1",
		AgentType:       "Explore",
		Timestamp:       t0.Add(3 * time.Second),
		Source:          models.WorkflowSourceHooks,
	}))

	// "Restart": a fresh store on the same state dir rebuilds from jsonl.
	s2 := New()

	runs := s2.GetWorkflowRuns()
	run := runs["wf_1"]
	if run == nil {
		t.Fatal("run wf_1 not rebuilt after restart")
	}
	if run.Name != "persisted-flow" || len(run.Phases) != 1 {
		t.Errorf("rebuilt run meta = %q/%v", run.Name, run.Phases)
	}
	agent := run.Agents["a1"]
	if agent == nil {
		t.Fatal("agent a1 not rebuilt")
	}
	if agent.Status != "completed" {
		t.Errorf("rebuilt agent status = %q, want completed", agent.Status)
	}
	if !agent.StartedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("rebuilt StartedAt = %v, want %v", agent.StartedAt, t0.Add(time.Second))
	}
	if got := agent.ResultSummary["text"]; got != "done" {
		t.Errorf("rebuilt result = %v, want done", got)
	}
	if run.StartedCount != 1 || run.CompletedCount != 1 {
		t.Errorf("rebuilt counts = %d/%d, want 1/1", run.StartedCount, run.CompletedCount)
	}

	adhoc := s2.GetAdhocSubagents()
	if adhoc["job-2"]["a234567890abcdef1"] == nil {
		t.Error("ad-hoc agent x1 not rebuilt after restart")
	}

	// Replay is idempotent: a third load yields identical counts.
	s3 := New()
	run3 := s3.GetWorkflowRuns()["wf_1"]
	if run3 == nil || run3.StartedCount != 1 || run3.CompletedCount != 1 {
		t.Errorf("second replay diverged: %+v", run3)
	}
}

func TestWorkflowNamePassthrough(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	ts := time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC)

	// 1. Hook event with Name arrives for a run-attributed agent.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_name_test",
		AgentID:         "a1",
		AgentType:       "Explore",
		Name:            "explore codebase",
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	}))

	run := s.GetWorkflowRuns()["wf_name_test"]
	if run == nil {
		t.Fatal("run not found")
	}
	agent := run.Agents["a1"]
	if agent == nil {
		t.Fatal("agent a1 not found")
	}
	if agent.Name != "explore codebase" {
		t.Errorf("agent.Name = %q, want 'explore codebase'", agent.Name)
	}

	// 2. Journal event arrives without a name — must not overwrite.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_name_test",
		AgentID:         "a1",
		Prompt:          "find the handler",
		Timestamp:       ts.Add(time.Second),
		Source:          models.WorkflowSourceJournal,
	}))

	agent = s.GetWorkflowRuns()["wf_name_test"].Agents["a1"]
	if agent.Name != "explore codebase" {
		t.Errorf("journal must not overwrite hook name: got %q", agent.Name)
	}
	if agent.TaskDescription != "find the handler" {
		t.Errorf("TaskDescription = %q, want journal prompt", agent.TaskDescription)
	}

	// 3. Hook completion with the same name: should survive.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_name_test",
		AgentID:         "a1",
		Name:            "explore codebase",
		LastMessage:     "done",
		Timestamp:       ts.Add(2 * time.Second),
		Source:          models.WorkflowSourceHooks,
	}))

	agent = s.GetWorkflowRuns()["wf_name_test"].Agents["a1"]
	if agent.Name != "explore codebase" {
		t.Errorf("name must survive completion: got %q", agent.Name)
	}

	// 4. Name is mirrored in session.Subagents.
	sess := s.GetSession("job-1")
	if len(sess.Subagents) != 1 {
		t.Fatalf("session.Subagents = %d, want 1", len(sess.Subagents))
	}
	if sess.Subagents[0].Name != "explore codebase" {
		t.Errorf("session subagent Name = %q, want 'explore codebase'", sess.Subagents[0].Name)
	}
}

func TestWorkflowNameAdhocSubagent(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	ts := time.Date(2026, 6, 17, 10, 5, 0, 0, time.UTC)

	// Ad-hoc Agent-tool spawn with a name (no RunID).
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a34567890abcdef12",
		AgentType:       "general-purpose",
		Name:            "investigate auth flow",
		Prompt:          "investigate the auth flow",
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	}))

	adhoc := s.GetAdhocSubagents()
	agent := adhoc["job-1"]["a34567890abcdef12"]
	if agent == nil {
		t.Fatal("adhoc agent not found")
	}
	if agent.Name != "investigate auth flow" {
		t.Errorf("adhoc agent.Name = %q, want 'investigate auth flow'", agent.Name)
	}

	// Complete the ad-hoc agent.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a34567890abcdef12",
		Name:            "investigate auth flow",
		LastMessage:     "found the issue",
		Timestamp:       ts.Add(5 * time.Second),
		Source:          models.WorkflowSourceHooks,
	}))

	agent = s.GetAdhocSubagents()["job-1"]["a34567890abcdef12"]
	if agent.Name != "investigate auth flow" {
		t.Errorf("adhoc name after completion = %q", agent.Name)
	}
	if agent.Status != "completed" {
		t.Errorf("adhoc status = %q, want completed", agent.Status)
	}

	// Name is mirrored in session.Subagents.
	sess := s.GetSession("job-1")
	found := false
	for _, sa := range sess.Subagents {
		if sa.ID == "a34567890abcdef12" {
			found = true
			if sa.Name != "investigate auth flow" {
				t.Errorf("session adhoc subagent Name = %q", sa.Name)
			}
		}
	}
	if !found {
		t.Error("adhoc agent not found in session.Subagents")
	}
}

func TestWorkflowNameHookOverridesJournalName(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	ts := time.Date(2026, 6, 17, 10, 10, 0, 0, time.UTC)

	// Journal arrives first with a name (unusual but possible).
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_override",
		AgentID:         "a2",
		Name:            "journal-name",
		Prompt:          "do something",
		Timestamp:       ts,
		Source:          models.WorkflowSourceJournal,
	}))

	agent := s.GetWorkflowRuns()["wf_override"].Agents["a2"]
	if agent.Name != "journal-name" {
		t.Errorf("initial Name = %q, want 'journal-name'", agent.Name)
	}

	// Hook arrives later with a different name — hooks win.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_override",
		AgentID:         "a2",
		Name:            "hook-name",
		Timestamp:       ts.Add(time.Second),
		Source:          models.WorkflowSourceHooks,
	}))

	agent = s.GetWorkflowRuns()["wf_override"].Agents["a2"]
	if agent.Name != "hook-name" {
		t.Errorf("hook must override journal name: got %q, want 'hook-name'", agent.Name)
	}
}

func TestWorkflowUpdateBroadcast(t *testing.T) {
	s := newTestStore(t)
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	u := wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1",
		Timestamp:       time.Now(),
		Source:          models.WorkflowSourceHooks,
	})
	s.ApplyUpdate(u)

	select {
	case got := <-ch:
		if got.Type != UpdateWorkflowAgentStarted {
			t.Errorf("broadcast type = %q, want %q", got.Type, UpdateWorkflowAgentStarted)
		}
	case <-time.After(time.Second):
		t.Fatal("no broadcast received")
	}
}

func TestIsSpawnAgentID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"a62124203bfeb94f0", true}, // real spawn: a + 16 hex
		{"a1234567890abcdef", true},
		{"a03e225", false},            // phantom: a + 6 hex (Explore/Plan registration)
		{"ac81b9b", false},            // phantom
		{"", false},                   // empty
		{"a62124203bfeb94f0a", false}, // 17 hex (too long)
		{"a62124203bfeb94g0", false},  // non-hex digit
		{"explore-1", false},          // synthetic / not spawn form
		{"b62124203bfeb94f0", false},  // wrong prefix
	}
	for _, tt := range tests {
		if got := isSpawnAgentID(tt.id); got != tt.want {
			t.Errorf("isSpawnAgentID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

// TestPhantomSubagentStartDropped verifies the daemon's belt-and-suspenders
// guard: an agent_started event with no run attribution and a short, non-spawn
// agent_id (the phantom Explore/Plan type-registration shape) never creates an
// ad-hoc subagent, while a real full-spawn-id event still does.
func TestPhantomSubagentStartDropped(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")

	ts := time.Date(2026, 6, 17, 6, 30, 14, 0, time.UTC)

	// Phantom: short id, empty RunID — must be dropped.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a03e225",
		AgentType:       "Explore",
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	}))
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "ac81b9b",
		AgentType:       "Plan",
		Timestamp:       ts.Add(time.Millisecond),
		Source:          models.WorkflowSourceHooks,
	}))

	if got := s.GetAdhocSubagents(); len(got) != 0 {
		t.Errorf("phantom registration events must not create ad-hoc subagents, got %+v", got)
	}
	if got := s.GetWorkflowRuns(); len(got) != 0 {
		t.Errorf("phantom events must not create runs, got %+v", got)
	}
	if sess := s.GetSession("job-1"); len(sess.Subagents) != 0 {
		t.Errorf("phantom events must not mirror onto the session, got %+v", sess.Subagents)
	}

	// Real spawn: full id, empty RunID — must be kept in the ad-hoc bucket.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a62124203bfeb94f0",
		AgentType:       "general-purpose",
		Timestamp:       ts.Add(time.Second),
		Source:          models.WorkflowSourceHooks,
	}))

	adhoc := s.GetAdhocSubagents()
	if adhoc["job-1"]["a62124203bfeb94f0"] == nil {
		t.Fatalf("real spawn must create an ad-hoc subagent, got %+v", adhoc)
	}
	if len(adhoc["job-1"]) != 1 {
		t.Errorf("only the real spawn should be present, got %+v", adhoc["job-1"])
	}
}

func childrenSnapshot(jobID, claudeSessionID string, count int, now time.Time) Update {
	return wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowChildrenSnapshot,
		JobID:           jobID,
		ClaudeSessionID: claudeSessionID,
		LiveChildren:    count,
		Timestamp:       now,
		Source:          models.WorkflowSourceHooks,
	})
}

func TestChildrenSnapshotSetsLiveChildren(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	now := time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)

	s.ApplyUpdate(childrenSnapshot("job-1", "sess-1", 3, now))
	if got := s.GetSession("job-1").LiveChildren; got != 3 {
		t.Fatalf("LiveChildren = %d, want 3", got)
	}

	// A zero snapshot clears idle-busy (assignment is unconditional).
	s.ApplyUpdate(childrenSnapshot("job-1", "sess-1", 0, now.Add(time.Second)))
	if got := s.GetSession("job-1").LiveChildren; got != 0 {
		t.Fatalf("LiveChildren after zero snapshot = %d, want 0", got)
	}
}

func TestChildrenSnapshotClaudeSessionFallback(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	now := time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)

	// Empty JobID: the lookup must fall back to matching ClaudeSessionID (works
	// post-applySessionConfirmation, which seedSession performs).
	s.ApplyUpdate(childrenSnapshot("", "sess-1", 2, now))
	if got := s.GetSession("job-1").LiveChildren; got != 2 {
		t.Fatalf("LiveChildren via ClaudeSessionID fallback = %d, want 2", got)
	}
}

func TestChildrenSnapshotUnknownSessionNoop(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)

	// No session seeded: must be a silent no-op — no session created, no
	// runs/adhoc buckets touched.
	s.ApplyUpdate(childrenSnapshot("ghost-job", "ghost-sess", 5, now))

	if sess := s.GetSession("ghost-job"); sess != nil {
		t.Errorf("snapshot must not create a session, got %+v", sess)
	}
	if got := s.GetWorkflowRuns(); len(got) != 0 {
		t.Errorf("snapshot must not create runs, got %+v", got)
	}
	if got := s.GetAdhocSubagents(); len(got) != 0 {
		t.Errorf("snapshot must not create ad-hoc subagents, got %+v", got)
	}
}

func TestChildrenSnapshotNotPersisted(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	now := time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)

	s1 := New()
	// Seed a session so the snapshot actually lands (not just a miss no-op).
	s1.ApplyUpdate(Update{
		Type:    UpdateSessionIntent,
		Source:  "test",
		Payload: &SessionIntentPayload{JobID: "job-1", Provider: "claude"},
	})
	s1.ApplyUpdate(Update{
		Type:   UpdateSessionConfirmation,
		Source: "test",
		Payload: &SessionConfirmationPayload{
			JobID: "job-1", NativeID: "sess-1", PID: 1234,
		},
	})
	s1.ApplyUpdate(childrenSnapshot("job-1", "sess-1", 4, now))
	if got := s1.GetSession("job-1").LiveChildren; got != 4 {
		t.Fatalf("precondition: LiveChildren = %d, want 4", got)
	}

	// The snapshot must not have been journaled: the workflows state dir has no
	// .jsonl files, and a restart store rebuilds no runs/adhoc.
	wfDir := filepath.Join(paths.StateDir(), "daemon", "workflows")
	if entries, err := os.ReadDir(wfDir); err == nil {
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".jsonl" {
				t.Errorf("snapshot must not persist a jsonl, found %q", e.Name())
			}
		}
	}

	s2 := New()
	if got := s2.GetWorkflowRuns(); len(got) != 0 {
		t.Errorf("restart must rebuild no runs from a snapshot, got %+v", got)
	}
	if got := s2.GetAdhocSubagents(); len(got) != 0 {
		t.Errorf("restart must rebuild no ad-hoc subagents from a snapshot, got %+v", got)
	}
	// LiveChildren is derived (db:"-"): it does not survive a restart. Session
	// only exists post-restart if it was persisted through another path; here
	// no UpdateSessions was emitted, so the restart store has no session at all.
	if sess := s2.GetSession("job-1"); sess != nil && sess.LiveChildren != 0 {
		t.Errorf("LiveChildren must not survive restart, got %d", sess.LiveChildren)
	}
}

func TestChildrenSnapshotBroadcast(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	s.ApplyUpdate(childrenSnapshot("job-1", "sess-1", 1, time.Now()))

	select {
	case got := <-ch:
		if got.Type != UpdateWorkflowChildrenSnapshot {
			t.Errorf("broadcast type = %q, want %q", got.Type, UpdateWorkflowChildrenSnapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("no broadcast received")
	}
}

func TestUpdateSessionsPreservesLiveChildren(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	now := time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)
	s.ApplyUpdate(childrenSnapshot("job-1", "sess-1", 3, now))

	// A bulk UpdateSessions replace whose incoming session carries LiveChildren=0
	// (rebuilt from DB) must copy-forward the prior nonzero value for matching IDs.
	s.ApplyUpdate(Update{
		Type:   UpdateSessions,
		Source: "session_recovery",
		Payload: []*models.Session{
			{ID: "job-1", ClaudeSessionID: "sess-1", Status: "idle"},
		},
	})
	if got := s.GetSession("job-1").LiveChildren; got != 3 {
		t.Errorf("LiveChildren after UpdateSessions = %d, want 3 (copy-forward)", got)
	}

	// A session absent from the prior map gets no copy-forward: it stays zero.
	s.ApplyUpdate(Update{
		Type:   UpdateSessions,
		Source: "session_recovery",
		Payload: []*models.Session{
			{ID: "job-2", ClaudeSessionID: "sess-2", Status: "idle"},
		},
	})
	if got := s.GetSession("job-2").LiveChildren; got != 0 {
		t.Errorf("new session LiveChildren = %d, want 0", got)
	}
}

// startAdhoc / completeAdhoc fire agent lifecycle events for a run-less
// (Agent-tool) subagent, the shape the collector's LiveChildCounts derivation
// keys on.
func startAdhoc(t *testing.T, s *Store, jobID, claudeID, agentID string, ts time.Time) {
	t.Helper()
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           jobID,
		ClaudeSessionID: claudeID,
		AgentID:         agentID,
		AgentType:       "Explore",
		Name:            "explore the handler",
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	}))
}

func completeAdhoc(t *testing.T, s *Store, jobID, claudeID, agentID string, ts time.Time) {
	t.Helper()
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           jobID,
		ClaudeSessionID: claudeID,
		AgentID:         agentID,
		AgentType:       "Explore",
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	}))
}

// TestLiveChildCountsSelfClears is the F3 regression guard: a session with two
// live ad-hoc subagents derives LiveChildren==2 from the daemon's own
// bookkeeping, and after both complete the derivation returns 0 on the next
// pass — WITHOUT any further children_snapshot. This is what returns an
// idle-busy agent to truly-idle when no more SubagentStop hooks fire.
func TestLiveChildCountsSelfClears(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	startAdhoc(t, s, "job-1", "sess-1", "a1111111111111111", ts)
	startAdhoc(t, s, "job-1", "sess-1", "a2222222222222222", ts)

	if got := s.LiveChildCounts()["job-1"]; got != 2 {
		t.Fatalf("LiveChildCounts = %d, want 2 (two running subagents)", got)
	}

	completeAdhoc(t, s, "job-1", "sess-1", "a1111111111111111", ts.Add(time.Second))
	if got := s.LiveChildCounts()["job-1"]; got != 1 {
		t.Fatalf("LiveChildCounts after one completion = %d, want 1", got)
	}

	completeAdhoc(t, s, "job-1", "sess-1", "a2222222222222222", ts.Add(2*time.Second))
	if got := s.LiveChildCounts()["job-1"]; got != 0 {
		t.Fatalf("LiveChildCounts after both complete = %d, want 0 (self-cleared)", got)
	}
}

// TestLiveChildCountsRawSessionByClaudeID covers a raw (non-flow) interactive
// session: its subagents forward with an EMPTY job id, keyed on the claude
// session id. The derivation must still count them (F3 for raw sessions), and
// the ClaudeSessionID fold must populate Session.Subagents so treemux can
// render per-child titles (F4b).
func TestLiveChildCountsRawSessionByClaudeID(t *testing.T) {
	s := newTestStore(t)
	// Raw session: job id == claude UUID (its store key), as hooks register it.
	seedSession(t, s, "sess-raw", "sess-raw")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	startAdhoc(t, s, "" /* no GROVE_FLOW_JOB_ID */, "sess-raw", "a3333333333333333", ts)

	if got := s.LiveChildCounts()["sess-raw"]; got != 1 {
		t.Fatalf("raw-session LiveChildCounts = %d, want 1", got)
	}
	// The ClaudeSessionID fold must have populated the session mirror so the
	// child's title is available to the renderer.
	sess := s.GetSession("sess-raw")
	if len(sess.Subagents) != 1 || sess.Subagents[0].Name != "explore the handler" {
		t.Fatalf("raw-session Subagents = %+v, want one titled child", sess.Subagents)
	}

	completeAdhoc(t, s, "", "sess-raw", "a3333333333333333", ts.Add(time.Second))
	if got := s.LiveChildCounts()["sess-raw"]; got != 0 {
		t.Fatalf("raw-session LiveChildCounts after completion = %d, want 0", got)
	}
}

// TestLiveChildCountsWorkflowRun covers the workflow-run arm of the derivation:
// live agents = StartedCount − CompletedCount for a run the session owns.
func TestLiveChildCountsWorkflowRun(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	// Two workflow agents start, one completes → one still live.
	for _, id := range []string{"a1111111111111111", "a2222222222222222"} {
		s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
			Kind: models.WorkflowAgentStarted, RunID: "wf_1", JobID: "job-1",
			ClaudeSessionID: "sess-1", AgentID: id, Timestamp: ts,
			Source: models.WorkflowSourceHooks,
		}))
	}
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind: models.WorkflowAgentCompleted, RunID: "wf_1", JobID: "job-1",
		ClaudeSessionID: "sess-1", AgentID: "a1111111111111111", Timestamp: ts.Add(time.Second),
		Source: models.WorkflowSourceHooks,
	}))

	if got := s.LiveChildCounts()["job-1"]; got != 1 {
		t.Fatalf("LiveChildCounts (workflow run) = %d, want 1", got)
	}

	// Completing the run marks it Completed → excluded → 0.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind: models.WorkflowAgentCompleted, RunID: "wf_1", JobID: "job-1",
		ClaudeSessionID: "sess-1", AgentID: "a2222222222222222", Timestamp: ts.Add(2 * time.Second),
		Source: models.WorkflowSourceHooks,
	}))
	if got := s.LiveChildCounts()["job-1"]; got != 0 {
		t.Fatalf("LiveChildCounts after all workflow agents complete = %d, want 0", got)
	}
}
