package store

import (
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

// bashStarted builds a WorkflowBashStarted update (PostToolUse backgroundTaskId).
func bashStarted(jobID, claudeID, bashID, command string, ts time.Time) Update {
	return wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowBashStarted,
		JobID:           jobID,
		ClaudeSessionID: claudeID,
		AgentID:         bashID,
		Name:            command,
		Timestamp:       ts,
		Source:          models.WorkflowSourceHooks,
	})
}

// bashSnapshot builds a hook-sourced children_snapshot carrying an authoritative
// live-bash set (the SubagentStop background_tasks[] shell entries).
func bashSnapshot(jobID, claudeID string, ts time.Time, live ...models.BashChildRef) Update {
	return wfUpdate(models.WorkflowEvent{
		Kind:             models.WorkflowChildrenSnapshot,
		JobID:            jobID,
		ClaudeSessionID:  claudeID,
		LiveBashChildren: live,
		Timestamp:        ts,
		Source:           models.WorkflowSourceHooks,
	})
}

func liveBashTitles(t *testing.T, s *Store, jobID string) []string {
	t.Helper()
	sess := s.GetSession(jobID)
	if sess == nil {
		t.Fatalf("session %q not found", jobID)
	}
	var titles []string
	for _, sa := range sess.Subagents {
		switch sa.Status {
		case "completed", "failed":
			continue
		}
		titles = append(titles, sa.Name)
	}
	return titles
}

// TestBashStartedCountsAndTitles: a background bash spawn (PostToolUse) is
// counted by LiveChildCounts and folds onto Session.Subagents with the command
// as its render title — the F6 "N bg" + indented-line coverage for bash.
func TestBashStartedCountsAndTitles(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	s.ApplyUpdate(bashStarted("job-1", "sess-1", "bt3yezzj6", "sleep 120", ts))

	if got := s.LiveChildCounts()["job-1"]; got != 1 {
		t.Fatalf("LiveChildCounts = %d, want 1 (one live bash)", got)
	}
	if titles := liveBashTitles(t, s, "job-1"); len(titles) != 1 || titles[0] != "sleep 120" {
		t.Fatalf("live titles = %v, want [\"sleep 120\"]", titles)
	}
}

// TestBashSelfClearsOnSnapshotDrop: once a hook-sourced snapshot no longer
// lists a tracked bash id, it is marked completed and drops from both the count
// and the rendered titles — the accurate clear path for sessions that fire
// SubagentStop.
func TestBashSelfClearsOnSnapshotDrop(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	s.ApplyUpdate(bashStarted("job-1", "sess-1", "bt3yezzj6", "sleep 120", ts))
	if got := s.LiveChildCounts()["job-1"]; got != 1 {
		t.Fatalf("LiveChildCounts before drop = %d, want 1", got)
	}

	// A later snapshot whose live-bash set omits bt3yezzj6 clears it.
	s.ApplyUpdate(bashSnapshot("job-1", "sess-1", ts.Add(time.Second)))
	if got := s.LiveChildCounts()["job-1"]; got != 0 {
		t.Fatalf("LiveChildCounts after drop = %d, want 0 (self-cleared)", got)
	}
	if titles := liveBashTitles(t, s, "job-1"); len(titles) != 0 {
		t.Fatalf("live titles after drop = %v, want none", titles)
	}
}

// TestBashSnapshotStartsAndKeeps: a hook snapshot both starts a listed bash and
// keeps still-listed ones live; a subsequent snapshot dropping one clears only
// that one.
func TestBashSnapshotStartsAndKeeps(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	s.ApplyUpdate(bashSnapshot("job-1", "sess-1", ts,
		models.BashChildRef{ID: "b1", Command: "poll loop"},
		models.BashChildRef{ID: "b2", Command: "tail log"},
	))
	if got := s.LiveChildCounts()["job-1"]; got != 2 {
		t.Fatalf("LiveChildCounts = %d, want 2", got)
	}

	// b1 finishes; b2 still live.
	s.ApplyUpdate(bashSnapshot("job-1", "sess-1", ts.Add(time.Second),
		models.BashChildRef{ID: "b2", Command: "tail log"},
	))
	if got := s.LiveChildCounts()["job-1"]; got != 1 {
		t.Fatalf("LiveChildCounts after one drop = %d, want 1", got)
	}
	if titles := liveBashTitles(t, s, "job-1"); len(titles) != 1 || titles[0] != "tail log" {
		t.Fatalf("live titles = %v, want [\"tail log\"]", titles)
	}
}

// TestExpireBashChildrenTTL: the guaranteed-clear floor. A bash on a session
// that never fires another snapshot still clears once older than the TTL.
func TestExpireBashChildrenTTL(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	ttl := 10 * time.Minute

	s.ApplyUpdate(bashStarted("job-1", "sess-1", "bt3yezzj6", "sleep 120", ts))

	// Before the TTL: still counted, ExpireBashChildren is a no-op.
	if changed := s.ExpireBashChildren(ts.Add(ttl-time.Second), ttl); changed {
		t.Fatalf("ExpireBashChildren before TTL reported a change")
	}
	if got := s.LiveChildCounts()["job-1"]; got != 1 {
		t.Fatalf("LiveChildCounts before TTL = %d, want 1", got)
	}

	// Past the TTL: expired, cleared from count and titles.
	if changed := s.ExpireBashChildren(ts.Add(ttl+time.Second), ttl); !changed {
		t.Fatalf("ExpireBashChildren past TTL reported no change")
	}
	if got := s.LiveChildCounts()["job-1"]; got != 0 {
		t.Fatalf("LiveChildCounts past TTL = %d, want 0 (guaranteed clear)", got)
	}
	if titles := liveBashTitles(t, s, "job-1"); len(titles) != 0 {
		t.Fatalf("live titles past TTL = %v, want none", titles)
	}
}

// TestDaemonSnapshotDoesNotReconcileBash: a daemon-derived children_snapshot
// (Source != hooks, no bash view) must never clear tracked bash — only the
// authoritative hook snapshot may.
func TestDaemonSnapshotDoesNotReconcileBash(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	s.ApplyUpdate(bashStarted("job-1", "sess-1", "bt3yezzj6", "sleep 120", ts))

	// The collector emits this shape each tick with an empty Source.
	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:         models.WorkflowChildrenSnapshot,
		JobID:        "job-1",
		LiveChildren: 1,
		Timestamp:    ts.Add(time.Second),
	}))
	if got := s.LiveChildCounts()["job-1"]; got != 1 {
		t.Fatalf("daemon snapshot cleared bash: LiveChildCounts = %d, want 1", got)
	}
}

// TestBashStartedNotPersisted: bash liveness is ephemeral — a restart (fresh
// store over the same state dir) must not replay a stuck bash child.
func TestBashStartedNotPersisted(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	s1 := New()
	s1.ApplyUpdate(Update{
		Type:    UpdateSessionIntent,
		Source:  "test",
		Payload: &SessionIntentPayload{JobID: "job-1", Provider: "claude"},
	})
	s1.ApplyUpdate(bashStarted("job-1", "", "bt3yezzj6", "sleep 120", ts))

	s2 := New() // reloads persisted workflow events
	if got := s2.GetAdhocSubagents(); len(got) != 0 {
		t.Fatalf("bash child was persisted and replayed: %+v", got)
	}
}

// TestAgentTypeFlowsToSubagent (F5): a live subagent with an empty Name still
// carries its AgentType onto the durable Session.Subagents record, so the
// renderer's floor can title it.
func TestAgentTypeFlowsToSubagent(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "job-1", "sess-1")
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	s.ApplyUpdate(wfUpdate(models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		AgentID:         "a1234567890abcdef",
		AgentType:       "Explore",
		// Name intentionally empty — meta.json not written yet at SubagentStart.
		Timestamp: ts,
		Source:    models.WorkflowSourceHooks,
	}))

	sess := s.GetSession("job-1")
	if sess == nil || len(sess.Subagents) != 1 {
		t.Fatalf("Subagents = %+v, want one record", sess)
	}
	if sess.Subagents[0].AgentType != "Explore" {
		t.Fatalf("Subagents[0].AgentType = %q, want Explore", sess.Subagents[0].AgentType)
	}
	if sess.Subagents[0].Name != "" {
		t.Fatalf("Subagents[0].Name = %q, want empty", sess.Subagents[0].Name)
	}
}
