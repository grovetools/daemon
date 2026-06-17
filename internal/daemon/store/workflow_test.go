package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
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
		AgentID:         "a1",
		AgentType:       "workflow-subagent",
		Timestamp:       hookStart,
		Source:          models.WorkflowSourceHooks,
	}))

	adhoc := s.GetAdhocSubagents()
	if adhoc["job-1"]["a1"] == nil {
		t.Fatal("expected a1 in the ad-hoc bucket")
	}

	// 2. Journal attribution arrives: agent migrates into the run, keeping
	// the hook's timestamp, gaining the journal's prompt.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1",
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
	agent := run.Agents["a1"]
	if agent == nil {
		t.Fatal("agent a1 not migrated into run")
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
		AgentID:         "a1",
		ResultSummary:   "journal result",
		Timestamp:       hookStart.Add(8 * time.Second),
		Source:          models.WorkflowSourceJournal,
	}))
	hookStop := hookStart.Add(6 * time.Second)
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a1", // hooks may have no RunID; session lookup attributes it
		AgentType:       "workflow-subagent",
		LastMessage:     "hook last message",
		TranscriptPath:  "/tmp/agent-a1.jsonl",
		Timestamp:       hookStop,
		Source:          models.WorkflowSourceHooks,
	}))

	run = s.GetWorkflowRuns()["wf_1"]
	agent = run.Agents["a1"]
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
		AgentID:         "explore-1",
		AgentType:       "Explore",
		Prompt:          "find the handler",
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	}))
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentCompleted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "explore-1",
		AgentType:       "Explore",
		LastMessage:     "found it",
		Timestamp:       ts.Add(4 * time.Second),
		Source:          models.WorkflowSourceHooks,
	}))

	adhoc := s.GetAdhocSubagents()
	agent := adhoc["job-1"]["explore-1"]
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
	if len(sess.Subagents) != 1 || sess.Subagents[0].ID != "explore-1" {
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
		AgentID:         "x1",
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
	if adhoc["job-2"]["x1"] == nil {
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
		AgentID:         "adhoc-1",
		AgentType:       "general-purpose",
		Name:            "investigate auth flow",
		Prompt:          "investigate the auth flow",
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	}))

	adhoc := s.GetAdhocSubagents()
	agent := adhoc["job-1"]["adhoc-1"]
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
		AgentID:         "adhoc-1",
		Name:            "investigate auth flow",
		LastMessage:     "found the issue",
		Timestamp:       ts.Add(5 * time.Second),
		Source:          models.WorkflowSourceHooks,
	}))

	agent = s.GetAdhocSubagents()["job-1"]["adhoc-1"]
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
		if sa.ID == "adhoc-1" {
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
