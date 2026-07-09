package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
)

// WorkflowEventPayload is the typed payload for the four workflow_* update
// types. It wraps the wire-protocol models.WorkflowEvent; RunName/Phases
// enrich run_discovered events from the daemon's journal watcher (parsed
// script meta), since the per-agent wire event carries no run-level fields.
type WorkflowEventPayload struct {
	Event models.WorkflowEvent `json:"event"`
	// RunName is the workflow name from the persisted script's meta block
	// (journal watcher only).
	RunName string `json:"run_name,omitempty"`
	// Phases lists phase titles from the script meta (journal watcher only).
	Phases []string `json:"phases,omitempty"`
}

// UpdateTypeForWorkflowKind maps a wire event kind to its store update type.
func UpdateTypeForWorkflowKind(k models.WorkflowKind) (UpdateType, bool) {
	switch k {
	case models.WorkflowRunDiscovered:
		return UpdateWorkflowRunDiscovered, true
	case models.WorkflowAgentStarted:
		return UpdateWorkflowAgentStarted, true
	case models.WorkflowAgentCompleted:
		return UpdateWorkflowAgentCompleted, true
	case models.WorkflowRunStale:
		return UpdateWorkflowRunStale, true
	case models.WorkflowRunCompleted:
		return UpdateWorkflowRunCompleted, true
	case models.WorkflowChildrenSnapshot:
		return UpdateWorkflowChildrenSnapshot, true
	case models.WorkflowBashStarted:
		return UpdateWorkflowBashStarted, true
	}
	return "", false
}

// GetWorkflowRuns returns a copy of the aggregated workflow run state,
// keyed by run ID. Agent records are copied so callers cannot mutate
// store state.
func (s *Store) GetWorkflowRuns() map[string]*models.WorkflowRunState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]*models.WorkflowRunState, len(s.state.WorkflowRuns))
	for runID, run := range s.state.WorkflowRuns {
		runCopy := *run
		runCopy.Agents = make(map[string]*models.Subagent, len(run.Agents))
		for agentID, agent := range run.Agents {
			agentCopy := *agent
			runCopy.Agents[agentID] = &agentCopy
		}
		result[runID] = &runCopy
	}
	return result
}

// GetAdhocSubagents returns a copy of the run-less subagent records,
// keyed by session key then agent ID.
func (s *Store) GetAdhocSubagents() map[string]map[string]*models.Subagent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]map[string]*models.Subagent, len(s.state.AdhocSubagents))
	for key, agents := range s.state.AdhocSubagents {
		agentsCopy := make(map[string]*models.Subagent, len(agents))
		for agentID, agent := range agents {
			agentCopy := *agent
			agentsCopy[agentID] = &agentCopy
		}
		result[key] = agentsCopy
	}
	return result
}

// applyWorkflowEvent folds one workflow lifecycle event into state. Must be
// called under the store's write lock (or before the store is shared, as in
// startup replay). When persist is true the event is appended to the on-disk
// journal; replayed events pass persist=false.
//
// Dedupe contract (per the workflow-subagent integration spec): events are
// merged by (RunID, AgentID). Hooks are the lifecycle authority and win on
// timestamps; the journal is the attribution/enrichment authority and wins
// on run IDs, prompts, and structured results. Replays are idempotent.
func (s *Store) applyWorkflowEvent(p *WorkflowEventPayload, persist bool) {
	if p == nil {
		return
	}
	ev := p.Event
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}

	switch ev.Kind {
	case models.WorkflowChildrenSnapshot:
		// A session-level live-background-child count, not a per-agent
		// lifecycle delta: write ev.LiveChildren onto the owning session and
		// return BEFORE the persist block below. Snapshot counts must never be
		// journaled — a stale nonzero count replayed by loadPersistedWorkflowEvents
		// at boot would fake idle-busy.
		s.applyChildrenSnapshot(ev)
		// A hook-sourced snapshot's background_tasks[] view is the authoritative
		// live set of background bash jobs; reconcile it into the bash-child
		// bookkeeping (start new, clear absent). Daemon-derived snapshots
		// (Source=="") carry no bash view and never touch it.
		if ev.Source == models.WorkflowSourceHooks {
			s.reconcileBashChildren(ev)
		}
		return

	case models.WorkflowBashStarted:
		// A background bash spawn (PostToolUse backgroundTaskId). Additive only —
		// it starts/refreshes one bash child and never clears others. Ephemeral:
		// return BEFORE the persist block (a replayed start would fake liveness).
		if ev.AgentID == "" {
			return
		}
		s.applyBashStarted(ev)
		return

	case models.WorkflowRunDiscovered:
		if ev.RunID == "" {
			return
		}
		run := s.ensureWorkflowRun(ev)
		if p.RunName != "" {
			run.Name = p.RunName
		}
		if len(p.Phases) > 0 {
			run.Phases = p.Phases
		}
		run.UpdatedAt = ev.Timestamp

	case models.WorkflowAgentStarted, models.WorkflowAgentCompleted:
		if ev.AgentID == "" {
			return
		}
		if !s.applyWorkflowAgentEvent(ev) {
			return // phantom registration dropped; do not persist it
		}

	case models.WorkflowRunStale:
		if ev.RunID == "" {
			return
		}
		// Staleness comes from the journal-quiet + session-gone heuristic
		// (workflowmon.FileSource) — never from the PID reaper. Reserved for
		// session-ended-with-stragglers (RunCompleted handles the clean case).
		run, ok := s.state.WorkflowRuns[ev.RunID]
		if !ok {
			return
		}
		run.Stale = true
		run.UpdatedAt = ev.Timestamp

	case models.WorkflowRunCompleted:
		if ev.RunID == "" {
			return
		}
		// Clean terminal state: the owning session ended with every started
		// agent finished (workflowmon.FileSource's session-end gate, NOT live
		// mid-run count equality).
		run, ok := s.state.WorkflowRuns[ev.RunID]
		if !ok {
			return
		}
		run.Completed = true
		run.UpdatedAt = ev.Timestamp

	default:
		return
	}

	if persist {
		p2 := *p
		p2.Event = ev // carry the stamped timestamp into the journal
		s.workflowPersister.append(&p2)
	}
}

// ensureWorkflowRun returns the run for ev.RunID, creating it if needed and
// backfilling session attribution fields.
func (s *Store) ensureWorkflowRun(ev models.WorkflowEvent) *models.WorkflowRunState {
	if s.state.WorkflowRuns == nil {
		s.state.WorkflowRuns = make(map[string]*models.WorkflowRunState)
	}
	run, ok := s.state.WorkflowRuns[ev.RunID]
	if !ok {
		run = &models.WorkflowRunState{
			RunID:  ev.RunID,
			Agents: make(map[string]*models.Subagent),
		}
		s.state.WorkflowRuns[ev.RunID] = run
	}
	if run.Agents == nil {
		run.Agents = make(map[string]*models.Subagent)
	}
	if run.JobID == "" {
		run.JobID = ev.JobID
	}
	if run.ClaudeSessionID == "" {
		run.ClaudeSessionID = ev.ClaudeSessionID
	}
	// Hook-supplied workflow name backfills; journal RunName still wins
	// (run_discovered overwrites unconditionally above).
	if run.Name == "" && ev.WorkflowName != "" {
		run.Name = ev.WorkflowName
	}
	return run
}

// applyWorkflowAgentEvent upserts a per-agent record for a started/completed
// event, handling run attribution:
//   - RunID set → the run's Agents map; a record parked in the ad-hoc bucket
//     (hook event arrived before journal attribution) migrates into the run.
//   - RunID empty → an existing run-attributed record for the same session
//     wins (the journal already attributed this agent); otherwise the
//     event lands in the per-session ad-hoc bucket.
//
// Returns false (applying nothing) when the event is a phantom
// type-registration: a started event with no run attribution and a non-spawn
// agent id that would otherwise create a brand-new ad-hoc record. The harness
// fires SubagentStart once per registered agent definition (Explore, Plan) at
// session init with a short, transcript-less agent_id; this is the daemon's
// belt-and-suspenders guard so an older hooks binary that still forwards them
// (or already-persisted phantoms replayed at startup) can't repopulate the
// ad-hoc bucket. Events that match an existing record (run migration, the
// real-spawn lifecycle) are never affected.
func (s *Store) applyWorkflowAgentEvent(ev models.WorkflowEvent) bool {
	var agent *models.Subagent
	var run *models.WorkflowRunState

	if ev.RunID != "" {
		run = s.ensureWorkflowRun(ev)
		agent = run.Agents[ev.AgentID]
		if agent == nil {
			if moved := s.takeAdhocSubagent(ev); moved != nil {
				agent = moved
			} else {
				agent = &models.Subagent{ID: ev.AgentID}
			}
			run.Agents[ev.AgentID] = agent
		}
	} else {
		run, agent = s.findRunAgent(ev)
		if agent == nil {
			if ev.Kind == models.WorkflowAgentStarted && !isSpawnAgentID(ev.AgentID) {
				return false
			}
			agent = s.ensureAdhocSubagent(ev)
		}
	}

	mergeWorkflowAgentEvent(agent, ev)

	if run != nil {
		run.StartedCount, run.CompletedCount = countWorkflowAgents(run.Agents)
		if ev.Timestamp.After(run.UpdatedAt) {
			run.UpdatedAt = ev.Timestamp
		}
	}

	s.upsertSessionSubagent(ev.JobID, agent)
	// Non-flow (raw claude) sessions carry no GROVE_FLOW_JOB_ID, so the
	// job-keyed fold above is a no-op for them. Fold by claude session id too
	// so their Session.Subagents populates — this is what lets treemux render
	// per-child titles for a raw interactive agent's background subagents (F4b).
	// Skipped when a job id was present (the job-keyed fold already handled the
	// flow session; avoids a redundant scan).
	if ev.JobID == "" && ev.ClaudeSessionID != "" {
		s.upsertSessionSubagentByClaudeID(ev.ClaudeSessionID, agent)
	}
	return true
}

// spawnAgentIDRe matches a genuine subagent spawn id: the literal 'a'
// followed by exactly 16 hex digits (17 chars total), e.g.
// "a62124203bfeb94f0". Claude Code mints this id for every real Task/Agent
// spawn and writes its transcript at <session>/subagents/agent-<id>.jsonl.
// Phantom type-registration events (one per .claude/agents/*.md definition,
// fired at session init) carry a short 'a' + ~6 hex id and no transcript.
var spawnAgentIDRe = regexp.MustCompile(`^a[0-9a-f]{16}$`)

// isSpawnAgentID reports whether agentID is a genuine spawn id rather than a
// phantom type-registration id (see spawnAgentIDRe).
func isSpawnAgentID(agentID string) bool {
	return spawnAgentIDRe.MatchString(agentID)
}

// workflowSessionKey identifies the owning session for run-less bookkeeping:
// job ID when stamped (GROVE_FLOW_JOB_ID), else the claude session ID.
func workflowSessionKey(ev models.WorkflowEvent) string {
	if ev.JobID != "" {
		return ev.JobID
	}
	return ev.ClaudeSessionID
}

// findRunAgent locates an existing run-attributed agent record for a
// run-less event by matching the event's session to each run.
func (s *Store) findRunAgent(ev models.WorkflowEvent) (*models.WorkflowRunState, *models.Subagent) {
	for _, run := range s.state.WorkflowRuns {
		if ev.JobID != "" && run.JobID == ev.JobID ||
			ev.ClaudeSessionID != "" && run.ClaudeSessionID == ev.ClaudeSessionID {
			if agent, ok := run.Agents[ev.AgentID]; ok {
				return run, agent
			}
		}
	}
	return nil, nil
}

// takeAdhocSubagent removes and returns the agent's record from the
// session's ad-hoc bucket, if present (migration into a run).
func (s *Store) takeAdhocSubagent(ev models.WorkflowEvent) *models.Subagent {
	key := workflowSessionKey(ev)
	agents, ok := s.state.AdhocSubagents[key]
	if !ok {
		return nil
	}
	agent, ok := agents[ev.AgentID]
	if !ok {
		return nil
	}
	delete(agents, ev.AgentID)
	if len(agents) == 0 {
		delete(s.state.AdhocSubagents, key)
	}
	return agent
}

// ensureAdhocSubagent returns (creating if needed) the run-less record for
// the event's agent in its session bucket.
func (s *Store) ensureAdhocSubagent(ev models.WorkflowEvent) *models.Subagent {
	key := workflowSessionKey(ev)
	if s.state.AdhocSubagents == nil {
		s.state.AdhocSubagents = make(map[string]map[string]*models.Subagent)
	}
	agents, ok := s.state.AdhocSubagents[key]
	if !ok {
		agents = make(map[string]*models.Subagent)
		s.state.AdhocSubagents[key] = agents
	}
	agent, ok := agents[ev.AgentID]
	if !ok {
		agent = &models.Subagent{ID: ev.AgentID}
		agents[ev.AgentID] = agent
	}
	return agent
}

// mergeWorkflowAgentEvent folds one event into the durable per-agent record.
// Field authority: hooks win on timestamps (the journal has none — its
// timestamps are daemon receive times); the journal wins on prompts and
// structured results (hooks can't see workflow spawn prompts, and its
// last_assistant_message is only a fallback result). Hooks win on names
// (the hooks forwarder reads agent-<id>.meta.json; the journal has no name).
func mergeWorkflowAgentEvent(agent *models.Subagent, ev models.WorkflowEvent) {
	fromHooks := ev.Source == models.WorkflowSourceHooks

	if agent.ParentSessionID == "" {
		agent.ParentSessionID = ev.ClaudeSessionID
	}
	if ev.AgentType != "" && agent.TaskType == "" {
		agent.TaskType = ev.AgentType
	}
	// Carry the spawn-source discriminator onto the durable record so the live
	// drawer can fall back to it as a title when a still-running child has no
	// meta.json-derived Name/TaskDescription yet (F5). Never clobber a good
	// value with a later empty one.
	if ev.AgentType != "" && agent.AgentType == "" {
		agent.AgentType = ev.AgentType
	}
	if ev.Prompt != "" && (agent.TaskDescription == "" || !fromHooks) {
		agent.TaskDescription = ev.Prompt
	}
	// Hooks win on names: prefer non-empty hook-sourced values; do not
	// overwrite an existing name with empty.
	if ev.Name != "" && (agent.Name == "" || fromHooks) {
		agent.Name = ev.Name
	}

	switch ev.Kind {
	case models.WorkflowAgentStarted:
		if agent.StartedAt.IsZero() || fromHooks {
			agent.StartedAt = ev.Timestamp
		}
		if agent.Status == "" {
			agent.Status = "running"
		}
	case models.WorkflowAgentCompleted:
		if agent.CompletedAt.IsZero() || fromHooks {
			agent.CompletedAt = ev.Timestamp
		}
		agent.Status = "completed"
		agent.Success = true
		if ev.ResultSummary != "" && (!fromHooks || agent.ResultSummary == nil) {
			agent.ResultSummary = map[string]any{"text": ev.ResultSummary}
		} else if ev.LastMessage != "" && agent.ResultSummary == nil {
			agent.ResultSummary = map[string]any{"text": ev.LastMessage}
		}
	}

	if !agent.StartedAt.IsZero() && !agent.CompletedAt.IsZero() && !agent.CompletedAt.Before(agent.StartedAt) {
		agent.DurationMs = agent.CompletedAt.Sub(agent.StartedAt).Milliseconds()
		agent.DurationSeconds = int(agent.DurationMs / 1000)
	}
}

// countWorkflowAgents recomputes the run scoreboard. An agent whose start
// event was missed but that completed still counts as started.
func countWorkflowAgents(agents map[string]*models.Subagent) (started, completed int) {
	for _, a := range agents {
		if !a.StartedAt.IsZero() || a.Status == "completed" {
			started++
		}
		if a.Status == "completed" {
			completed++
		}
	}
	return started, completed
}

// upsertSessionSubagent mirrors the per-agent record onto the owning
// session's Subagents slice (keyed by session/job ID), populating the
// dormant models.Session.Subagents so GET /api/sessions surfaces subagent
// activity with no new endpoints.
func (s *Store) upsertSessionSubagent(jobID string, agent *models.Subagent) {
	if jobID == "" || agent == nil {
		return
	}
	session, ok := s.state.Sessions[jobID]
	if !ok {
		return
	}
	for i := range session.Subagents {
		if session.Subagents[i].ID == agent.ID {
			session.Subagents[i] = *agent
			return
		}
	}
	session.Subagents = append(session.Subagents, *agent)
}

// upsertSessionSubagentByClaudeID mirrors a per-agent record onto the owning
// session's Subagents slice, locating the session by its claude session id
// (Session.ClaudeSessionID, or Session.ID for raw sessions whose map key IS
// the claude UUID). Used for non-flow sessions the job-ID fold can't serve.
// Silent no-op when no session matches (invariant 5).
func (s *Store) upsertSessionSubagentByClaudeID(claudeID string, agent *models.Subagent) {
	if claudeID == "" || agent == nil {
		return
	}
	for _, session := range s.state.Sessions {
		if session.ClaudeSessionID != claudeID && session.ID != claudeID {
			continue
		}
		for i := range session.Subagents {
			if session.Subagents[i].ID == agent.ID {
				session.Subagents[i] = *agent
				return
			}
		}
		session.Subagents = append(session.Subagents, *agent)
		return
	}
}

// liveSubagentCount counts the still-running agents in a bucket keyed by
// session/claude id: an agent is live iff its Status is not a terminal value
// (completed/failed). Started-but-unfinished agents carry Status "running"
// (mergeWorkflowAgentEvent); the ones that have finished carry "completed".
func liveSubagentCount(agents map[string]*models.Subagent) int {
	n := 0
	for _, a := range agents {
		if a == nil {
			continue
		}
		switch a.Status {
		case "completed", "failed":
			continue
		}
		n++
	}
	return n
}

// LiveChildCounts returns the daemon-derived live background-child count for
// every tracked session, computed from the store's OWN workflow-run and ad-hoc
// subagent bookkeeping (not the hook snapshot). Unlike the hook's
// children_snapshot this is self-clearing: once a child's agent_completed lands
// (Status=="completed") it drops out, so a session whose children have all
// finished derives 0 even when no further SubagentStop ever fires — the F3
// clearing guarantee. Per session the count is (live ad-hoc subagents keyed to
// it) + (live workflow-run agents = StartedCount−CompletedCount for runs it
// owns). Live background *bash* jobs (F6) ride the ad-hoc bucket as
// bashChildTaskType records, so they are counted here too; their clear paths
// are drop-reconciliation (reconcileBashChildren) and the ExpireBashChildren
// TTL floor rather than a completion hook. Read-locked; returns a fresh map keyed by
// session ID (== the s.state.Sessions key), so the caller can key a
// children_snapshot update by JobID directly.
func (s *Store) LiveChildCounts() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := make(map[string]int, len(s.state.Sessions))
	for id, session := range s.state.Sessions {
		if session == nil {
			continue
		}
		n := 0
		// Ad-hoc subagents are bucketed by workflowSessionKey (job id when set,
		// else claude session id). A raw session's map key IS its claude UUID,
		// so check both the session id and ClaudeSessionID; a given agent lives
		// under exactly one key, so there is no double counting.
		n += liveSubagentCount(s.state.AdhocSubagents[id])
		if session.ClaudeSessionID != "" && session.ClaudeSessionID != id {
			n += liveSubagentCount(s.state.AdhocSubagents[session.ClaudeSessionID])
		}
		// Live workflow-run agents for runs this session owns.
		for _, run := range s.state.WorkflowRuns {
			if run == nil || run.Completed {
				continue
			}
			owned := (run.JobID != "" && run.JobID == id) ||
				(session.ClaudeSessionID != "" && run.ClaudeSessionID == session.ClaudeSessionID) ||
				run.ClaudeSessionID == id
			if !owned {
				continue
			}
			if live := run.StartedCount - run.CompletedCount; live > 0 {
				n += live
			}
		}
		counts[id] = n
	}
	return counts
}

// applyChildrenSnapshot writes a live-background-child count onto the owning
// session's derived LiveChildren field (models.Session, db:"-", never
// persisted). Lookup mirrors upsertSessionSubagent: by ev.JobID first, then a
// linear scan matching Session.ClaudeSessionID for non-flow interactive
// sessions (which the job-ID key can't serve — the fallback works only
// post-applySessionConfirmation, when ClaudeSessionID is set, which is fine:
// no children exist before the agent starts). Silent no-op when no session
// matches (invariant 5), so a snapshot for an unknown/ended session neither
// creates a session nor errors. The assignment is unconditional — a zero count
// clears a previously-nonzero LiveChildren (idle-busy → idle). Must be called
// under the store write lock (ApplyUpdate holds it).
func (s *Store) applyChildrenSnapshot(ev models.WorkflowEvent) {
	if ev.JobID != "" {
		if session, ok := s.state.Sessions[ev.JobID]; ok {
			session.LiveChildren = ev.LiveChildren
			return
		}
	}
	if ev.ClaudeSessionID != "" {
		for _, session := range s.state.Sessions {
			if session.ClaudeSessionID == ev.ClaudeSessionID {
				session.LiveChildren = ev.LiveChildren
				return
			}
		}
	}
	// No matching session: silent no-op (mirrors upsertSessionSubagent).
}

// bashChildTaskType marks an ad-hoc subagent record as a live background bash
// job rather than a real subagent. Background bash reuses the AdhocSubagents /
// Session.Subagents machinery so the existing count (liveSubagentCount →
// LiveChildCounts) and indented-title render (liveChildItems, Name first) cover
// it for free; the marker is how the bash-specific clear paths (drop
// reconciliation, TTL) find their own records without disturbing subagents.
const bashChildTaskType = "background-bash"

// mirrorBashChildToSession folds a bash-child record onto the owning session's
// Subagents slice. The bash bucket is keyed by workflowSessionKey (job id when
// set, else claude id); a job id lands via upsertSessionSubagent and a claude
// id via upsertSessionSubagentByClaudeID, so calling both mirrors the record
// regardless of which keying built it (the non-matching call is a no-op).
func (s *Store) mirrorBashChildToSession(sessionKey string, agent *models.Subagent) {
	s.upsertSessionSubagent(sessionKey, agent)
	s.upsertSessionSubagentByClaudeID(sessionKey, agent)
}

// upsertBashChild starts or refreshes one live background bash child in the
// session's ad-hoc bucket and mirrors it onto the session. A new record is
// stamped running with StartedAt=now (the TTL clock); an existing one keeps its
// original StartedAt (so a refreshing snapshot never resets the TTL) and is
// re-marked running only if it had not already gone terminal. Name backfills
// from the command when the record has none yet.
func (s *Store) upsertBashChild(sessionKey, bashID, command string, now time.Time) {
	if sessionKey == "" || bashID == "" {
		return
	}
	if s.state.AdhocSubagents == nil {
		s.state.AdhocSubagents = make(map[string]map[string]*models.Subagent)
	}
	agents, ok := s.state.AdhocSubagents[sessionKey]
	if !ok {
		agents = make(map[string]*models.Subagent)
		s.state.AdhocSubagents[sessionKey] = agents
	}
	agent, ok := agents[bashID]
	if !ok {
		agent = &models.Subagent{ID: bashID, TaskType: bashChildTaskType, StartedAt: now}
		agents[bashID] = agent
	}
	if agent.Name == "" && command != "" {
		agent.Name = command
	}
	switch agent.Status {
	case "completed", "failed":
		// A prior clear won; do not resurrect it.
	default:
		agent.Status = "running"
	}
	s.mirrorBashChildToSession(sessionKey, agent)
}

// completeBashChild marks one bash child terminal and re-mirrors it so it drops
// from both the live count (liveSubagentCount) and the rendered child list
// (liveChildItems skips completed).
func (s *Store) completeBashChild(sessionKey string, agent *models.Subagent) {
	if agent.Status == "completed" {
		return
	}
	agent.Status = "completed"
	s.mirrorBashChildToSession(sessionKey, agent)
}

// applyBashStarted records a background bash spawn (WorkflowBashStarted). It is
// additive: it never clears sibling bash children (the PostToolUse hook sees
// only the one new job, not the full live set). This is the sole liveness
// signal for a session with no subagent — such a session never fires
// SubagentStop, so its bash is cleared only by ExpireBashChildren's TTL.
func (s *Store) applyBashStarted(ev models.WorkflowEvent) {
	sessionKey := workflowSessionKey(ev)
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	s.upsertBashChild(sessionKey, ev.AgentID, ev.Name, ts)
}

// reconcileBashChildren folds a hook-sourced children snapshot's authoritative
// live-bash set into the bookkeeping: every listed job is started/refreshed,
// and every bash child currently tracked for the session that is ABSENT from
// the list is marked completed. The SubagentStop background_tasks[] view lists
// exactly the still-live children, so absence is a reliable "gone" signal — the
// accurate clear path for any session that fires SubagentStop (TTL is the floor
// for those that never do). Only ever called for hook-sourced snapshots.
func (s *Store) reconcileBashChildren(ev models.WorkflowEvent) {
	sessionKey := workflowSessionKey(ev)
	if sessionKey == "" {
		return
	}
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	live := make(map[string]bool, len(ev.LiveBashChildren))
	for _, b := range ev.LiveBashChildren {
		if b.ID == "" {
			continue
		}
		live[b.ID] = true
		s.upsertBashChild(sessionKey, b.ID, b.Command, ts)
	}
	for id, agent := range s.state.AdhocSubagents[sessionKey] {
		if agent == nil || agent.TaskType != bashChildTaskType {
			continue
		}
		if !live[id] {
			s.completeBashChild(sessionKey, agent)
		}
	}
}

// ExpireBashChildren is the guaranteed-clear floor for background bash: it marks
// every still-running bash child older than ttl completed (re-mirroring so the
// count and title drop) and reports whether anything changed. Bash liveness has
// no reliable per-job completion hook (background_tasks[] entries only ever
// carry status "running"; a bash-only session never fires SubagentStop at all),
// so without this a finished bash on such a session would show forever. The
// collector calls it each tick; the resulting count change propagates through
// the normal LiveChildCounts → children_snapshot diff. This bounds the display
// to at most ttl past a bash's true finish — the accepted F6 reliability bound.
func (s *Store) ExpireBashChildren(now time.Time, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for sessionKey, agents := range s.state.AdhocSubagents {
		for _, agent := range agents {
			if agent == nil || agent.TaskType != bashChildTaskType {
				continue
			}
			switch agent.Status {
			case "completed", "failed":
				continue
			}
			if now.Sub(agent.StartedAt) < ttl {
				continue
			}
			s.completeBashChild(sessionKey, agent)
			changed = true
		}
	}
	return changed
}

// workflowPersister appends enriched workflow events to
// StateDir()/daemon/workflows/<runId>.jsonl (run-less events go to
// adhoc-<sessionKey>.jsonl) so run state survives daemon restarts. The
// fold's dedupe maps tolerate replay, so duplicate lines (e.g. journal
// re-reads after restart) are harmless.
type workflowPersister struct {
	mu  sync.Mutex
	dir string
}

func newWorkflowPersister() *workflowPersister {
	base := paths.StateDir()
	if base == "" {
		return &workflowPersister{}
	}
	return &workflowPersister{dir: filepath.Join(base, "daemon", "workflows")}
}

func (p *workflowPersister) append(payload *WorkflowEventPayload) {
	if p.dir == "" {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.MkdirAll(p.dir, 0o755); err != nil { //nolint:gosec // G301: daemon state dir
		return
	}
	f, err := os.OpenFile(filepath.Join(p.dir, workflowEventFileName(payload.Event)),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G302/G306: non-secret state
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// load reads every persisted workflow event, ordered by event timestamp so
// the replayed fold reproduces merge precedence deterministically.
func (p *workflowPersister) load() []*WorkflowEventPayload {
	if p.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return nil
	}
	var payloads []*WorkflowEventPayload
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(p.dir, entry.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var payload WorkflowEventPayload
			if err := json.Unmarshal([]byte(line), &payload); err != nil {
				continue // tolerate corrupt lines
			}
			payloads = append(payloads, &payload)
		}
		_ = f.Close()
	}
	sort.SliceStable(payloads, func(i, j int) bool {
		return payloads[i].Event.Timestamp.Before(payloads[j].Event.Timestamp)
	})
	return payloads
}

func workflowEventFileName(ev models.WorkflowEvent) string {
	name := ev.RunID
	if name == "" {
		name = "adhoc-" + workflowSessionKey(ev)
	}
	return sanitizeWorkflowFileName(name) + ".jsonl"
}

// sanitizeWorkflowFileName keeps run/session-derived file names path-safe.
func sanitizeWorkflowFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}

// loadPersistedWorkflowEvents rebuilds WorkflowRuns/AdhocSubagents from the
// persisted event journal. Called from New() before the store is shared, so
// no locking is needed; events replay with persist=false.
func (s *Store) loadPersistedWorkflowEvents() {
	for _, payload := range s.workflowPersister.load() {
		s.applyWorkflowEvent(payload, false)
	}
}
