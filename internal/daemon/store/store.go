package store

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	grovelogging "github.com/grovetools/core/logging"
	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions/health"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/pathutil"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
	"github.com/grovetools/flow/pkg/orchestration"
)

// Store is the in-memory state store for the daemon.
// It is thread-safe and supports pub/sub for real-time updates.
type Store struct {
	mu                sync.RWMutex
	state             *State
	subscribers       map[chan Update]struct{}
	focus             map[string]focusRegistration // source-owned focus paths with a bounded lease
	now               func() time.Time
	persister         *Persister
	workflowPersister *workflowPersister
	pendingRestore    persistedState // Loaded from disk, applied when workspaces arrive
	ulog              *grovelogging.UnifiedLogger

	// noteIndexDigest fingerprints the note index currently in state, so a
	// rebuilt-but-identical index can be dropped instead of published. Both
	// note producers rebuild the WHOLE index every scan, so without this the
	// replay ring accumulates references to full multi-megabyte maps that
	// differ in nothing. noteIndexSeen distinguishes "digest 0" from "no index
	// applied yet". Guarded by mu.
	noteIndexDigest uint64
	noteIndexSeen   bool

	// satSeenSnapshot marks origins whose first UpdateSatelliteSnapshot of this
	// process has been applied. The first snapshot per origin is a baseline
	// (state transfer, not transitions): jobs already terminal in it synthesize
	// no per-job events (see the UpdateSatelliteSnapshot branch of ApplyUpdate).
	// Guarded by mu.
	satSeenSnapshot map[string]struct{}

	// busMu guards the event bus's sequence counter and replay ring. It is the
	// INNERMOST lock: every publish site already holds s.mu (read or write),
	// so busMu must never be acquired before s.mu.
	busMu sync.Mutex
	// seq is the monotonic sequence stamped on published updates. It starts at
	// 0 and is pre-incremented, so the first published update carries Seq 1.
	seq uint64
	// ring retains the most recent published updates for ?since= replay. It is
	// a fixed-size circular buffer (len(ring) == RingSize); ringPos is the next
	// write slot and ringLen the number of live entries.
	ring    []Update
	ringPos int
	ringLen int
}

// RingSize bounds the in-memory replay ring behind GET /api/stream?since=.
//
// The bus is deliberately in-memory and lossy-by-design at the SUBSCRIPTION
// layer (buffered-100 channels, drop-on-full). The ring is a second, separate
// buffer that lets a client that reconnects quickly close its own gap without
// re-snapshotting: a subscriber that misses events because its channel filled,
// or because it was disconnected, can re-attach with ?since=<last seq it saw>
// and get everything still retained.
//
// What the bound does NOT buy: durability. The ring lives in the daemon
// process, so a restart empties it AND resets the sequence counter. Clients
// must treat a gap signal (Store.Replay's ReplayGap) as "snapshot and
// reconcile", never as "events silently lost".
//
// 1024 entries is roughly a minute of a busy daemon's chatter (git enrichment
// deltas dominate the volume) and a few hundred kilobytes of retained pointers
// — the ring stores the same payload pointers the store already holds, so it
// pins almost nothing the state does not. That holds only because the types it
// does NOT hold in common with the state — the wholesale map replacements, whose
// every superseded generation the ring would otherwise pin — are recorded with
// their payload stripped. See RingDropsPayload.
//
// Payloads are retained by REFERENCE. A replayed update carries whatever the
// pointed-to model looks like now, not a snapshot of what it looked like when
// the event fired. Events are triggers, not a durable log; consumers that need
// point-in-time truth must reconcile against the REST endpoints.
const RingSize = 1024

// New creates a new Store instance, loading any persisted task results and
// workflow events from disk.
func New() *Store {
	s := &Store{
		state: &State{
			Workspaces:     make(map[string]*models.EnrichedWorkspace),
			Sessions:       make(map[string]*models.Session),
			Jobs:           make(map[string]*models.JobInfo),
			NoteIndex:      make(map[string]*models.NoteIndexEntry),
			Plans:          make(map[string][]*orchestration.Plan),
			WorkflowRuns:   make(map[string]*models.WorkflowRunState),
			AdhocSubagents: make(map[string]map[string]*models.Subagent),
			Subjobs:        make(map[string]*models.SubjobState),
			Satellites:     make(map[string]*SatelliteStatusPayload),
		},
		subscribers:       make(map[chan Update]struct{}),
		focus:             make(map[string]focusRegistration),
		ring:              make([]Update, RingSize),
		now:               time.Now,
		satSeenSnapshot:   make(map[string]struct{}),
		persister:         newPersister(),
		workflowPersister: newWorkflowPersister(),
		ulog:              grovelogging.NewUnifiedLogger("groved.store"),
	}
	s.loadPersistedResults()
	s.loadPersistedWorkflowEvents()
	s.loadPersistedSubjobs()
	return s
}

// loadPersistedResults reads task results from disk and stashes them for
// later restoration once workspaces are populated.
func (s *Store) loadPersistedResults() {
	persisted, err := s.persister.load()
	if err != nil || len(persisted) == 0 {
		return
	}
	s.pendingRestore = persisted
}

// Get returns a copy of the current state.
func (s *Store) Get() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return shallow copy
	return *s.state
}

// GetWorkspaces returns a slice of all enriched workspaces.
func (s *Store) GetWorkspaces() []*models.EnrichedWorkspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*models.EnrichedWorkspace, 0, len(s.state.Workspaces))
	for _, ws := range s.state.Workspaces {
		result = append(result, ws)
	}
	return result
}

// WorkspaceNodes returns the discovered workspace node behind every enriched
// workspace, skipping entries that carry none (a persisted row restored before
// discovery has re-seen it).
//
// This is the store's answer to "which workspaces exist", and it is the ONLY
// answer enrichment producers should use. Workspace discovery is a filesystem
// walk that classifies and config-parses every workspace; the workspace
// collector owns it and publishes the result here, so a producer that re-runs
// its own DiscoverAll pays that whole cost again for a set it could have read.
func (s *Store) WorkspaceNodes() []*workspace.WorkspaceNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return WorkspaceNodesOf(s.state.Workspaces)
}

// WorkspaceNodesOf is the pure projection behind WorkspaceNodes, exported so a
// producer that already holds a Get() snapshot computes its node set from the
// SAME snapshot it will diff its results against.
func WorkspaceNodesOf(workspaces map[string]*models.EnrichedWorkspace) []*workspace.WorkspaceNode {
	nodes := make([]*workspace.WorkspaceNode, 0, len(workspaces))
	for _, ws := range workspaces {
		if ws == nil || ws.WorkspaceNode == nil {
			continue
		}
		nodes = append(nodes, ws.WorkspaceNode)
	}
	return nodes
}

// GetSessions returns a slice of all sessions.
func (s *Store) GetSessions() []*models.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*models.Session, 0, len(s.state.Sessions))
	for _, sess := range s.state.Sessions {
		result = append(result, sess)
	}
	return result
}

// GetSession returns a specific session by ID, or nil if not found.
func (s *Store) GetSession(sessionID string) *models.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sess, ok := s.state.Sessions[sessionID]; ok {
		// Return a copy to prevent mutation
		sessCopy := *sess
		return &sessCopy
	}
	return nil
}

// PruneTerminalSessions drops terminal in-memory rows whose best available
// terminal timestamp is strictly before cutoff. The fallback order is EndedAt,
// LastActivity, then StartedAt so legacy/recovered rows remain bounded. It is a
// locked store mutation and publishes one reconciliation event when anything
// changes; callers own provenance logging and metrics.
func (s *Store) PruneTerminalSessions(cutoff time.Time, source string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var ids []string
	for key, sess := range s.state.Sessions {
		if sess == nil || !isTerminalSessionStatus(sess.Status) {
			continue
		}
		stamp := sess.StartedAt
		if !sess.LastActivity.IsZero() {
			stamp = sess.LastActivity
		}
		if sess.EndedAt != nil && !sess.EndedAt.IsZero() {
			stamp = *sess.EndedAt
		}
		if stamp.IsZero() || !stamp.Before(cutoff) {
			continue
		}
		delete(s.state.Sessions, key)
		ids = append(ids, sess.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	s.publishLocked(Update{
		Type: UpdateSessionsPruned, Source: source, Scanned: len(ids),
		Payload: &SessionsPrunedPayload{IDs: append([]string(nil), ids...), Before: cutoff},
	})
	return ids
}

// GetJob returns a specific job by ID, or nil if not found.
func (s *Store) GetJob(jobID string) *models.JobInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if job, ok := s.state.Jobs[jobID]; ok {
		jobCopy := *job
		return &jobCopy
	}
	return nil
}

// GetJobs returns a slice of all jobs.
func (s *Store) GetJobs() []*models.JobInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*models.JobInfo, 0, len(s.state.Jobs))
	for _, job := range s.state.Jobs {
		jobCopy := *job
		result = append(result, &jobCopy)
	}
	return result
}

// GetSatelliteStatuses returns a copy of the latest connection-health status
// for every satellite the ConnManager has reported on, keyed by registry name.
// This is the read path P10's `grove status` satellite line consumes (C17).
func (s *Store) GetSatelliteStatuses() map[string]*SatelliteStatusPayload {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]*SatelliteStatusPayload, len(s.state.Satellites))
	for name, status := range s.state.Satellites {
		statusCopy := *status
		result[name] = &statusCopy
	}
	return result
}

// GetAssistantStatus returns a copy of the assistant supervisor's last
// published status, or nil when nothing has published one.
func (s *Store) GetAssistantStatus() *models.AssistantStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.Assistant == nil {
		return nil
	}
	statusCopy := *s.state.Assistant
	return &statusCopy
}

// GetNoteIndex returns note index entries, optionally filtered by workspace.
// If workspace is empty, all entries are returned.
func (s *Store) GetNoteIndex(workspace string) []*models.NoteIndexEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*models.NoteIndexEntry, 0, len(s.state.NoteIndex))
	for _, entry := range s.state.NoteIndex {
		if workspace == "" || entry.Workspace == workspace {
			result = append(result, entry)
		}
	}
	return result
}

// GetNavBindings returns the current nav binding state, or nil if not yet loaded.
func (s *Store) GetNavBindings() *models.NavSessionsFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.NavBindings
}

// GetPlans returns the cached parsed plans for a given plansDir, or nil
// if the watcher has not populated a snapshot for that directory yet.
func (s *Store) GetPlans(planDir string) []*orchestration.Plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Plans[planDir]
}

// GetPlanIndexSnapshot returns a detached portfolio snapshot suitable for an
// HTTP reconciliation response.
func (s *Store) GetPlanIndexSnapshot() *models.PlanIndexSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.PlanIndex == nil {
		return &models.PlanIndexSnapshot{Plans: []models.PlanSummary{}}
	}
	out := *s.state.PlanIndex
	out.Plans = append([]models.PlanSummary(nil), s.state.PlanIndex.Plans...)
	return &out
}

// ApplyUpdate modifies the state and notifies subscribers.
func (s *Store) ApplyUpdate(u Update) {
	// Fingerprint a note index BEFORE taking the lock. The digest walks every
	// entry, and s.mu serializes every state mutation in the daemon — the one
	// place a tens-of-thousands-of-entries hash must not run is under it. The
	// comparison itself happens in the UpdateNoteIndex branch below.
	var noteDigest uint64
	if u.Type == UpdateNoteIndex {
		if index, ok := u.Payload.(map[string]*models.NoteIndexEntry); ok {
			noteDigest = noteIndexDigestOf(index)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Follow-up updates derived atomically from this state transition (workspace
	// lifecycle deltas and satellite terminal events); broadcast after u below.
	var synthesized []Update

	switch u.Type {
	case UpdateWorkspaces:
		// Wholesale map replace. Workspaces never federate (a satellite's repos
		// are not the laptop's), so no origin-scoping is needed here (C7).
		if workspaces, ok := u.Payload.(map[string]*models.EnrichedWorkspace); ok {
			s.state.Workspaces = workspaces
			if s.pendingRestore != nil {
				restoreResults(s.state.Workspaces, s.pendingRestore)
				s.pendingRestore = nil
			}
		}
	case UpdateSessions:
		if sessions, ok := u.Payload.([]*models.Session); ok {
			// Wholesale rebuild, but ORIGIN-SCOPED (C7): this replaces only the
			// u.Origin slice of the map. A local SessionCollector snapshot
			// (u.Origin == "") must not evict federated rows, and a remote-origin
			// snapshot must not evict locals. Start by copying forward every
			// entry whose Origin differs from this update's origin, then insert
			// the payload rows (all of which carry Origin == u.Origin).
			//
			// Copy-forward the derived LiveChildren field from the prior map for
			// matching keys: this wholesale replace otherwise wipes it (the count
			// is db:"-" and rebuilt-from-DB sessions carry 0), and unlike
			// LiveTokens there is no ~4s repopulator — LiveChildren only refreshes
			// on the next SubagentStop, minutes away for a long-running background
			// workflow. Zeroing mid-flight would falsely demote idle-busy →
			// truly-idle. Scoped to LiveChildren only (contract D5 / J2 §4);
			// LiveTokens/Subagents are deliberately not copied here.
			newMap := make(map[string]*models.Session)
			for k, sess := range s.state.Sessions {
				if sess.Origin != u.Origin {
					newMap[k] = sess
				}
			}
			for _, sess := range sessions {
				if prev, ok := s.state.Sessions[sessionKey(sess)]; ok {
					if sess.LiveChildren == 0 {
						sess.LiveChildren = prev.LiveChildren
					}
					// Verified is daemon-derived and not persisted. Preserve it across
					// local recovery/discovery snapshots just like other derived state.
					if sess.Verified == "" && health.IsActiveSessionStatus(sess.Status) {
						sess.Verified = prev.Verified
					}
				}
				if !health.IsActiveSessionStatus(sess.Status) {
					sess.Verified = ""
				}
				newMap[sessionKey(sess)] = sess
			}
			s.state.Sessions = newMap
		}

	// Session lifecycle updates
	case UpdateSessionIntent:
		if payload, ok := u.Payload.(*SessionIntentPayload); ok {
			if !s.applySessionIntent(payload, u.Source) {
				return
			}
		}
	case UpdateSessionConfirmation:
		if payload, ok := u.Payload.(*SessionConfirmationPayload); ok {
			if !s.applySessionConfirmation(payload, u.Source) {
				return
			}
		}
	case UpdateSessionStatus:
		if payload, ok := u.Payload.(*SessionStatusPayload); ok {
			if !s.applySessionStatus(payload) {
				return
			}
		}
	case UpdateSessionEnd:
		if payload, ok := u.Payload.(*SessionEndPayload); ok {
			// Terminal session rows are an event-idempotency boundary. Multiple
			// observers may report the same process exit; only the first report
			// mutates state and reaches subscribers.
			if !s.applySessionEnd(payload, u.Source) {
				return
			}
		}
	case UpdateSessionVerdict:
		if payload, ok := u.Payload.(*SessionVerdictPayload); ok {
			if !s.applySessionVerdict(payload) {
				return
			}
		}
	case UpdateSessionActivity:
		if payload, ok := u.Payload.(*SessionActivityPayload); ok {
			if !s.applySessionActivity(payload) {
				return
			}
		}
	case UpdateSessionTokens:
		if payload, ok := u.Payload.(*SessionTokensPayload); ok {
			s.applySessionTokens(payload)
		}

	// Job lifecycle updates
	case UpdateJobSubmitted, UpdateJobStarted, UpdateJobCompleted, UpdateJobFailed, UpdateJobCancelled, UpdateJobPendingUser, UpdateJobOrphaned:
		if job, ok := u.Payload.(*models.JobInfo); ok {
			// jobKey (C7): bare ID for locals, origin-namespaced for federated
			// rows. Local emitters set no Origin, so this is behavior-preserving.
			s.state.Jobs[jobKey(job)] = job
		}

	// Note mutation events from nb
	case UpdateNoteEvent:
		if event, ok := u.Payload.(*models.NoteEvent); ok {
			s.applyNoteEvent(event)
		}

	// Full note index replacement from collector scan. Wholesale map replace;
	// the note index never federates (satellite notes converge home through the
	// Record plane, not this Store), so no origin-scoping is needed here (C7).
	case UpdateNoteIndex:
		if noteIndex, ok := u.Payload.(map[string]*models.NoteIndexEntry); ok {
			// The fence (see note_index_digest.go). An index that fingerprints
			// the same as the one already in state carries no news, so it is
			// neither stored nor published: storing it would swap one live
			// multi-megabyte map for an identical one, and publishing it would
			// hand the replay ring a second copy to pin. Returning here skips
			// publishLocked, so the sequence does not advance either — a no-op
			// scan is invisible to subscribers, which is the point.
			if s.noteIndexSeen && noteDigest == s.noteIndexDigest {
				telemetry.RecordNoteIndexPublish(len(noteIndex), false)
				return
			}
			s.noteIndexDigest, s.noteIndexSeen = noteDigest, true
			s.state.NoteIndex = noteIndex
			telemetry.RecordNoteIndexPublish(len(noteIndex), true)
		}

	// Delta updates for workspace enrichment fields
	case UpdateWorkspacesDelta:
		if deltas, ok := u.Payload.([]*models.WorkspaceDelta); ok {
			for _, d := range deltas {
				ws, exists := s.state.Workspaces[d.Path]
				if !exists {
					continue
				}
				if d.GitStatus != nil {
					ws.GitStatus = d.GitStatus
				}
				if d.GitLanding != nil {
					ws.GitLanding = d.GitLanding
				}
				// *bool: only the git delta builders set ChangedFilesComputed, and
				// they always populate ChangedFiles/BlobHashes alongside it — so
				// when it is present, apply all three unconditionally. A nil-guard
				// per field would keep stale per-file data forever once a repo
				// goes clean (fresh scan yields nil files), making the per-file
				// suppression comparison mismatch on every tick. Non-git deltas
				// leave all three nil and can't touch the cached data.
				if d.ChangedFilesComputed != nil {
					ws.ChangedFiles = d.ChangedFiles
					ws.BlobHashes = d.BlobHashes
					ws.ChangedFilesComputed = *d.ChangedFilesComputed
				} else {
					if d.ChangedFiles != nil {
						ws.ChangedFiles = d.ChangedFiles
					}
					if d.BlobHashes != nil {
						ws.BlobHashes = d.BlobHashes
					}
				}
				if d.NoteCounts != nil {
					ws.NoteCounts = d.NoteCounts
				}
				if d.PlanStats != nil {
					ws.PlanStats = d.PlanStats
				}
				if d.ReleaseInfo != nil {
					ws.ReleaseInfo = d.ReleaseInfo
				}
				if d.ActiveBinary != nil {
					ws.ActiveBinary = d.ActiveBinary
				}
				if d.CxStats != nil {
					ws.CxStats = d.CxStats
				}
				if d.GitRemoteURL != nil {
					ws.GitRemoteURL = *d.GitRemoteURL
				}
				if d.ReviewStats != nil {
					ws.ReviewStats = d.ReviewStats
				}
				if d.MachineSync != nil {
					ws.MachineSync = d.MachineSync
				}
				// *bool: only the tiered sweep sets it — true when it has not
				// reached this workspace yet this daemon lifetime, false on the
				// delta carrying its first scan. Every other producer leaves it
				// nil, so a note or plan delta can never claim git freshness.
				if d.GitStatusPending != nil {
					ws.GitStatusPending = *d.GitStatusPending
				}
			}
		}

	// Bulk discovery of idle jobs from filesystem
	case UpdateJobsDiscovered:
		if jobs, ok := u.Payload.([]*models.JobInfo); ok {
			for _, job := range jobs {
				// Key both the existing lookup and the write through jobKey (C7) so
				// filesystem-discovered local jobs (Origin == "") behave exactly as
				// before while any federated row stays origin-namespaced.
				key := jobKey(job)
				if existing, exists := s.state.Jobs[key]; exists {
					// Prevent stale filesystem reads from reverting active daemon states.
					// If daemon says running/queued, but file says pending/queued/pending_user, ignore.
					// However, if the file says completed/failed/idle, accept the update!
					if (existing.Status == "running" || existing.Status == "queued") &&
						(job.Status == "pending" || job.Status == "queued" || job.Status == "pending_user") {
						continue
					}
				}
				s.state.Jobs[key] = job
			}
		}

	// Channel & Autonomous updates
	case UpdateSessionChannels:
		if payload, ok := u.Payload.(*SessionChannelsPayload); ok {
			if session, exists := s.state.Sessions[payload.JobID]; exists {
				session.Channels = payload.Channels
				session.SignalTarget = payload.SignalTarget
			}
		}
	case UpdateSessionAutonomous:
		if payload, ok := u.Payload.(*SessionAutonomousPayload); ok {
			if session, exists := s.state.Sessions[payload.JobID]; exists {
				session.Autonomous = payload.Autonomous
			}
		}
	case UpdateSessionPing:
		if payload, ok := u.Payload.(*SessionPingPayload); ok {
			if session, exists := s.state.Sessions[payload.JobID]; exists {
				now := time.Now()
				session.LastIdlePingAt = &now
			}
		}
	case UpdateSessionTmuxTarget:
		if payload, ok := u.Payload.(*SessionTmuxTargetPayload); ok {
			if session, exists := s.state.Sessions[payload.JobID]; exists {
				session.TmuxTarget = payload.TmuxTarget
			}
		}
	case UpdateSessionLastSender:
		if payload, ok := u.Payload.(*SessionLastSenderPayload); ok {
			if session, exists := s.state.Sessions[payload.JobID]; exists {
				session.LastSender = payload.LastSender
				session.LastSenderGroup = payload.LastSenderGroup
			}
		}

	case UpdateTaskResult:
		if payload, ok := u.Payload.(*TaskResultPayload); ok {
			s.applyTaskResult(payload)
			s.persistAsync()
		}

	case UpdateTestReport:
		if payload, ok := u.Payload.(*TestReportPayload); ok {
			s.applyTestReport(payload)
			s.persistAsync()
		}

	case UpdateNavBindings:
		if bindings, ok := u.Payload.(*models.NavSessionsFile); ok {
			s.state.NavBindings = bindings
		}

	case UpdatePlans:
		if plansMap, ok := u.Payload.(map[string][]*orchestration.Plan); ok {
			for dir, plans := range plansMap {
				s.state.Plans[dir] = plans
			}
		}

	case UpdatePlanIndexSnapshot:
		if incoming, ok := u.Payload.(*models.PlanIndexSnapshot); ok && incoming != nil {
			previous := make(map[string]models.PlanSummary)
			hadIndex := s.state.PlanIndex != nil
			var revision uint64
			if hadIndex {
				revision = s.state.PlanIndex.Revision
				for _, summary := range s.state.PlanIndex.Plans {
					previous[summary.PlanDir] = summary
				}
			}
			plans := append([]models.PlanSummary(nil), incoming.Plans...)
			seen := make(map[string]struct{}, len(plans))
			for _, summary := range plans {
				seen[summary.PlanDir] = struct{}{}
			}
			removed := make([]string, 0)
			for dir := range previous {
				if _, exists := seen[dir]; !exists {
					removed = append(removed, dir)
				}
			}
			// Deltas carry only rows that materially changed since the stored
			// snapshot; re-scans that observe an identical portfolio must not
			// spend a revision or an SSE frame per subscriber. The very first
			// snapshot is the reconnect/boot baseline and always publishes,
			// even when empty.
			var upserts []models.PlanSummary
			if hadIndex {
				for _, summary := range plans {
					prev, exists := previous[summary.PlanDir]
					if !exists || !planSummaryEquivalent(prev, summary) {
						upserts = append(upserts, summary)
					}
				}
				if len(upserts) == 0 && len(removed) == 0 {
					// Freshness still advances for /api/plan-index consumers;
					// the revision and the wire stay quiet.
					s.state.PlanIndex.ScannedAt = incoming.ScannedAt
					s.state.PlanIndex.Plans = plans
					return
				}
			} else {
				upserts = plans
			}
			revision++
			s.state.PlanIndex = &models.PlanIndexSnapshot{
				Revision: revision, ScannedAt: incoming.ScannedAt, Plans: plans,
			}
			// Subscribers consume deltas, while the durable store retains the full
			// snapshot. Every published scan advances revision, making dropped
			// frames visible.
			u.Type = UpdatePlanIndexDelta
			u.Payload = &models.PlanIndexDelta{
				Revision: revision, ScannedAt: incoming.ScannedAt,
				Upserts: upserts, Removed: removed,
			}

			// The plan index carries the canonical qualified plan↔container binding.
			// Project lifecycle onto only the discovered members of that exact
			// container, never by a bare plan or worktree name. This makes
			// hold/unhold visible to Nav on the same SSE turn while preserving
			// same-named plans and worktrees in other workspaces.
			if deltas := s.applyPlanLifecycleToWorkspaces(plans); len(deltas) > 0 {
				synthesized = append(synthesized, Update{
					Type: UpdateWorkspacesDelta, Source: u.Source, Scanned: len(deltas), Payload: deltas,
				})
			}
		}

	// Workflow/subagent lifecycle events (hooks + journal watcher).
	case UpdateWorkflowRunDiscovered, UpdateWorkflowAgentStarted,
		UpdateWorkflowAgentCompleted, UpdateWorkflowRunStale,
		UpdateWorkflowRunCompleted, UpdateWorkflowChildrenSnapshot,
		UpdateWorkflowBashStarted:
		if payload, ok := u.Payload.(*WorkflowEventPayload); ok {
			// persist=true is neutralized for the children_snapshot and
			// bash_started kinds by a case-local return in applyWorkflowEvent
			// (both are ephemeral and never journaled).
			s.applyWorkflowEvent(payload, true)
		}

	case UpdateSubjobReportReady, UpdateSubjobJoined:
		if event, ok := u.Payload.(*models.SubjobEvent); ok {
			s.applySubjobEvent(event)
		}

	// Satellite connection-health transition from the ConnManager (C17). Record
	// the latest payload per registry name; the broadcast below then carries it
	// to SSE subscribers via convertToAPIUpdate. Not persisted — connection
	// health is live-only and re-emitted on the next dial after a restart.
	//
	// State "removed" is a tombstone from ConnManager.Reload (a `grove
	// satellite down` hot-reload): instead of upserting, drop the status entry
	// AND every federated job/session row for that origin — the satellite is
	// gone from the registry, so no future snapshot would ever reconcile them
	// away. The seen-snapshot baseline marker is cleared too, so a later
	// re-`up` of the same name gets baseline semantics again (its historical
	// terminal jobs must not fire a synthesized terminal-event burst).
	case UpdateSatelliteStatus:
		if payload, ok := u.Payload.(*SatelliteStatusPayload); ok {
			if payload.State == "removed" {
				delete(s.state.Satellites, payload.Name)
				for k, job := range s.state.Jobs {
					if job.Origin == payload.Name {
						delete(s.state.Jobs, k)
					}
				}
				for k, sess := range s.state.Sessions {
					if sess.Origin == payload.Name {
						delete(s.state.Sessions, k)
					}
				}
				delete(s.satSeenSnapshot, payload.Name)
				break
			}
			if s.state.Satellites == nil {
				s.state.Satellites = make(map[string]*SatelliteStatusPayload)
			}
			s.state.Satellites[payload.Name] = payload
		}

	// Assistant supervisor status (spec §3.3). Last-writer-wins on a single
	// slot: there is exactly one assistant per scoped daemon.
	case UpdateAssistantStatus:
		if payload, ok := u.Payload.(*models.AssistantStatus); ok && payload != nil {
			statusCopy := *payload
			s.state.Assistant = &statusCopy
		}

	// Origin-scoped federation snapshot from the SatelliteCollector (C7/C16).
	// The reconcile primitive: drop every job/session row for this origin, then
	// insert the snapshot rows. Rows for other origins and all locals are left
	// untouched, so a satellite that dropped a job has that row removed on the
	// next snapshot without collateral. Rows arrive pre-sanitized and
	// origin-stamped (SanitizeJobInfo/SanitizeSession), so the keys resolve to
	// composite (non-local) keys here.
	case UpdateSatelliteSnapshot:
		if payload, ok := u.Payload.(*SatelliteSnapshotPayload); ok && payload.Origin != "" {
			// Keep the outgoing job rows: remote terminal transitions exist ONLY
			// as snapshot deltas (the satellite's per-job jobrunner events never
			// federate — the collector re-snapshots instead), so the old-vs-new
			// diff below is where the laptop synthesizes them (B1).
			prevJobs := make(map[string]*models.JobInfo)
			for k, job := range s.state.Jobs {
				if job.Origin == payload.Origin {
					prevJobs[k] = job
					delete(s.state.Jobs, k)
				}
			}
			for k, sess := range s.state.Sessions {
				if sess.Origin == payload.Origin {
					delete(s.state.Sessions, k)
				}
			}
			// The first snapshot for an origin this process is a baseline (state
			// transfer, not transitions): a satellite full of historically
			// completed jobs must not fire a terminal-event burst on every boot.
			// Suppressing it costs the lease releaser nothing — a restart empties
			// the in-memory lease map anyway (TTL is the fallback there).
			_, seeded := s.satSeenSnapshot[payload.Origin]
			s.satSeenSnapshot[payload.Origin] = struct{}{}
			for _, job := range payload.Jobs {
				key := jobKey(job)
				s.state.Jobs[key] = job
				// Synthesize the per-job terminal update the local jobrunner would
				// have emitted, so Store subscribers (the P9 lease releaser, the
				// P10 ntfy bridge) see remote terminal transitions at all: on an
				// observed non-terminal→terminal transition, or when a job APPEARS
				// already terminal after the baseline (the laptop may have been
				// disconnected while it finished). Broadcast-only — the row was
				// just written above, and every local-only side-effect consumer
				// (jobrunner.watchTransitions, syncSessionStatusToJobMarkdown)
				// already skips Origin != "" payloads.
				updType, terminal := terminalJobUpdateType(job.Status)
				if !terminal {
					continue
				}
				if prev := prevJobs[key]; prev != nil {
					if _, prevTerminal := terminalJobUpdateType(prev.Status); prevTerminal {
						continue // unchanged terminal row — nothing transitioned
					}
				} else if !seeded {
					continue // baseline snapshot — history, not a transition
				}
				synthesized = append(synthesized, Update{
					Type:    updType,
					Source:  u.Source,
					Origin:  payload.Origin,
					Payload: job,
				})
			}
			for _, sess := range payload.Sessions {
				s.state.Sessions[sessionKey(sess)] = sess
			}
		}
	}

	// Broadcast to subscribers
	s.publishLocked(u)
	// Broadcast derived updates after their source transition so subscribers
	// observe state-then-delta ordering. These go straight to broadcast because
	// their state mutations were already applied under this lock. Publishing
	// them here (rather than inline) is also what gives them their own
	// sequence numbers, so a ?since= replay reproduces the same ordering.
	for _, su := range synthesized {
		s.publishLocked(su)
	}
}

// planSummaryEquivalent reports whether two rows carry the same material
// state. ScannedAt is bookkeeping (every rescan restamps it), so it is
// excluded; UpdatedAt is real data (directory mtime) and compares by instant.
func planSummaryEquivalent(a, b models.PlanSummary) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return false
	}
	a.ScannedAt, b.ScannedAt = time.Time{}, time.Time{}
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// applyPlanLifecycleToWorkspaces projects the plan index's canonical qualified
// binding onto discovered member worktrees. It must be called with s.mu held.
func (s *Store) applyPlanLifecycleToWorkspaces(plans []models.PlanSummary) []*models.WorkspaceDelta {
	type association struct {
		planName, planDir, status string
	}
	byWorktreeRoot := make(map[string]association, len(plans))
	for _, summary := range plans {
		root, err := pathutil.NormalizeForLookup(summary.WorktreePath)
		if err != nil || summary.WorktreePath == "" {
			continue
		}
		status := summary.Lifecycle
		if status == "live" {
			status = ""
		}
		byWorktreeRoot[root] = association{summary.PlanName, summary.PlanDir, status}
	}

	var deltas []*models.WorkspaceDelta
	for path, ws := range s.state.Workspaces {
		if ws == nil || ws.WorkspaceNode == nil {
			continue
		}
		// Membership is structural, never kind-based: a member checkout inside
		// a bound container (<owner>/.grove-worktrees/<plan>/<repo>) has no
		// grove.yml and discovers as NonGroveRepo, so IsWorktree() is false —
		// yet that leaf is exactly the row Nav renders for the plan.
		// WorktreeRootForPath returns false for anything outside a container,
		// which keeps parent and standalone workspaces untouched.
		root, ok := workspace.WorktreeRootForPath(ws.Path)
		if !ok {
			continue
		}
		normalizedRoot, err := pathutil.NormalizeForLookup(root)
		if err != nil {
			continue
		}
		associated, ok := byWorktreeRoot[normalizedRoot]
		if !ok {
			continue
		}
		var stats models.PlanStats
		if ws.PlanStats != nil {
			stats = *ws.PlanStats
		}
		if stats.AssociatedPlan == associated.planName && stats.AssociatedPlanDir == associated.planDir && stats.PlanStatus == associated.status {
			continue
		}
		stats.AssociatedPlan = associated.planName
		stats.AssociatedPlanDir = associated.planDir
		stats.PlanStatus = associated.status
		ws.PlanStats = &stats
		deltas = append(deltas, &models.WorkspaceDelta{Path: path, PlanStats: &stats})
	}
	return deltas
}

// terminalJobUpdateType maps a federated job's terminal status to the per-job
// update type the local jobrunner emits for it (see jobrunner markDone). The
// other jobrunner-terminal statuses deliberately do NOT map: "idle" marks a
// discovered-but-never-run job (a job_completed for it would release a dispatch
// lease for work that never happened), and rare interrupted/abandoned rows are
// left to the lease TTL rather than guessed into a lifecycle event.
func terminalJobUpdateType(status string) (UpdateType, bool) {
	switch status {
	case "completed":
		return UpdateJobCompleted, true
	case "failed":
		return UpdateJobFailed, true
	case "cancelled":
		return UpdateJobCancelled, true
	default:
		return "", false
	}
}

// applySessionIntent creates a new session entry from an intent (before agent launch).
//
// The recorded Type is the launcher's, not a constant: stamping every
// registration "interactive_agent" told treemux a headless job had a terminal
// to attach, which is what opened an empty shell on click instead of the job's
// transcript stream.
func lifecycleReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "unknown"
	}
	return reason
}

func (s *Store) applySessionIntent(payload *SessionIntentPayload, source string) bool {
	if payload == nil || payload.JobID == "" {
		return false
	}

	job := s.state.Jobs[payload.JobID]
	if job != nil && job.AttemptID != "" && job.AttemptID != payload.AttemptID {
		s.ulog.Warn("Ignored session intent for stale attempt").
			Field("event", "session.lifecycle.intent_mismatch").
			Field("job_id", payload.JobID).
			Field("current_attempt_id", job.AttemptID).
			Field("incoming_attempt_id", payload.AttemptID).
			StructuredOnly().
			Log(context.Background())
		return false
	}
	if current := s.state.Sessions[payload.JobID]; current != nil {
		if current.AttemptID == payload.AttemptID {
			// Intent is an at-most-once edge. A delayed duplicate must not demote
			// an already confirmed current attempt back to pending.
			return false
		}
		// Replacing the projected row requires the job collector's authoritative
		// current attempt. Without it, two identified callbacks cannot decide
		// which attempt is newer merely from arrival order.
		if payload.AttemptID == "" || job == nil || job.AttemptID != payload.AttemptID {
			return false
		}
	}

	now := s.now()
	session := &models.Session{
		ID:               payload.JobID,
		AttemptID:        payload.AttemptID,
		Type:             models.SessionTypeOrDefault(payload.Type),
		Provider:         payload.Provider,
		PID:              0, // Not yet known
		WorkingDirectory: payload.WorkDir,
		Status:           "pending", // Waiting for confirmation
		StartedAt:        now,
		LastActivity:     now,
		PlanName:         payload.PlanName,
		JobTitle:         payload.Title,
		ParentJobID:      payload.ParentJobID,
		JobFilePath:      payload.JobFilePath,
		Channels:         payload.Channels,
		Autonomous:       payload.Autonomous,
		TmuxTarget:       payload.TmuxTarget,
		SignalTarget:     payload.SignalTarget,
		Mux:              payload.Mux,
	}
	s.state.Sessions[payload.JobID] = session
	if job != nil {
		if job.AttemptID != payload.AttemptID {
			job.PID = 0
		}
		job.AttemptID = payload.AttemptID
	}
	s.ulog.Info("Session lifecycle: intent registered").
		Field("event", "session.lifecycle.intent").
		Field("job_id", payload.JobID).
		Field("attempt_id", payload.AttemptID).
		Field("legacy_attempt", payload.AttemptID == "").
		Field("reason", lifecycleReason(source)).
		StructuredOnly().
		Log(context.Background())
	return true
}

// lifecycleAttemptMatches preserves pre-AttemptID compatibility while ensuring
// two identified attempts for the same reusable JobID can never mutate each
// other. Only an unidentified current row accepts an identified upgrade; an
// unidentified incoming callback can never target an identified current row.
func lifecycleAttemptMatches(current, incoming string) bool {
	// An unidentified legacy row may be upgraded by an identified callback.
	// The reverse is forbidden: an empty callback is legacy, not an alias for
	// whichever current attempt happens to share its reusable JobID.
	return current == "" || current == incoming
}

func (s *Store) logLegacyLifecycle(event, jobID, currentAttempt, incomingAttempt string) {
	if currentAttempt != "" && incomingAttempt != "" {
		return
	}
	s.ulog.Warn("Applying legacy session lifecycle update without exact attempt identity").
		Field("event", event).
		Field("job_id", jobID).
		Field("current_attempt_id", currentAttempt).
		Field("incoming_attempt_id", incomingAttempt).
		StructuredOnly().
		Log(context.Background())
}

// applySessionConfirmation updates a pending session with actual process info.
func (s *Store) applySessionConfirmation(payload *SessionConfirmationPayload, source string) bool {
	now := s.now()
	session, exists := s.state.Sessions[payload.JobID]
	if exists && !lifecycleAttemptMatches(session.AttemptID, payload.AttemptID) {
		s.ulog.Warn("Ignored session confirmation for stale attempt").
			Field("event", "session.lifecycle.confirmation_mismatch").
			Field("job_id", payload.JobID).
			Field("current_attempt_id", session.AttemptID).
			Field("incoming_attempt_id", payload.AttemptID).
			StructuredOnly().
			Log(context.Background())
		return false
	}
	if !exists {
		// Confirmation is allowed to recover a missed intent only when it also
		// targets the current identified Flow attempt.
		typ := models.SessionTypeInteractiveAgent
		if job := s.state.Jobs[payload.JobID]; job != nil {
			if !lifecycleAttemptMatches(job.AttemptID, payload.AttemptID) {
				return false
			}
			if job.Type != "" {
				typ = string(job.Type)
			}
		}
		session = &models.Session{
			ID:        payload.JobID,
			AttemptID: payload.AttemptID,
			Type:      typ,
			StartedAt: now,
		}
		s.state.Sessions[payload.JobID] = session
	} else {
		s.logLegacyLifecycle("session.lifecycle.confirmation_legacy", payload.JobID, session.AttemptID, payload.AttemptID)
		if session.AttemptID == "" {
			session.AttemptID = payload.AttemptID
		}
	}

	// Update with confirmation data
	session.ClaudeSessionID = payload.NativeID
	session.PID = payload.PID
	session.Status = "running"
	maxSessionActivity(session, now)
	if payload.TranscriptPath != "" {
		session.TranscriptPath = payload.TranscriptPath
	}

	// Propagate the confirmed PID onto the matching JobInfo record. Adoption
	// (jobrunner/adoption.go) reads JobInfo.PID to re-attach detached agents
	// across a daemon restart; without this copy the field stays 0 forever and
	// every running job is marked "failed (no PID)" on a graceful upgrade.
	// The jobrunner persists this to disk via its UpdateSessionConfirmation
	// listener, which runs after this broadcast.
	job, jobExists := s.state.Jobs[payload.JobID]
	jobAttemptMatched := jobExists && lifecycleAttemptMatches(job.AttemptID, payload.AttemptID)
	if jobAttemptMatched {
		job.PID = payload.PID
	}

	// Diagnostic (permanent, Debug): proves the PID arrived non-zero and
	// whether the JobInfo was present in the store at confirm time. Observe via
	// `core logs --component groved.store --level debug -f`.
	s.ulog.Info("Session lifecycle: process confirmed").
		Field("event", "session.lifecycle.confirmed").
		Field("job_id", payload.JobID).
		Field("attempt_id", session.AttemptID).
		Field("native_id", payload.NativeID).
		Field("pid", payload.PID).
		Field("reason", lifecycleReason(source)).
		Field("job_exists_in_store", jobExists).
		Field("job_attempt_matched", jobAttemptMatched).
		StructuredOnly().
		Log(context.Background())
	return true
}

// applySessionStatus updates the status of an active session. A missed intent
// may be recovered only when the Job collector supplies a real type; otherwise
// the hook update is rejected rather than manufacturing a kind-less row.
func (s *Store) applySessionStatus(payload *SessionStatusPayload) bool {
	now := s.now()
	session, exists := s.state.Sessions[payload.JobID]
	if !exists {
		job := s.state.Jobs[payload.JobID]
		if job == nil || job.Type == "" || !lifecycleAttemptMatches(job.AttemptID, payload.AttemptID) {
			s.ulog.Warn("Ignored session status without matching typed intent or job").
				Field("event", "session.lifecycle.status_missing_type").
				Field("job_id", payload.JobID).
				Field("attempt_id", payload.AttemptID).
				StructuredOnly().
				Log(context.Background())
			return false
		}
		session = &models.Session{
			ID:           payload.JobID,
			AttemptID:    payload.AttemptID,
			Type:         string(job.Type),
			Status:       payload.Status,
			StartedAt:    now,
			LastActivity: now,
		}
		s.state.Sessions[payload.JobID] = session
		return true
	}
	if !lifecycleAttemptMatches(session.AttemptID, payload.AttemptID) {
		s.ulog.Warn("Ignored session status for stale attempt").
			Field("event", "session.lifecycle.status_mismatch").
			Field("job_id", payload.JobID).
			Field("current_attempt_id", session.AttemptID).
			Field("incoming_attempt_id", payload.AttemptID).
			StructuredOnly().
			Log(context.Background())
		return false
	}
	s.logLegacyLifecycle("session.lifecycle.status_legacy", payload.JobID, session.AttemptID, payload.AttemptID)
	if session.AttemptID == "" {
		session.AttemptID = payload.AttemptID
	}

	prevStatus := session.Status
	session.Status = payload.Status
	if isTerminalSessionStatus(payload.Status) {
		session.Verified = ""
	}
	// Session status updates originate at hook arrivals. Even when the status is
	// unchanged they are genuine activity, but the clock is monotonic so a late
	// callback can never move the lease backwards.
	maxSessionActivity(session, now)

	// Interactive (tmux-detached) agents have no foreground runtime loop to
	// persist a mid-session status change into their job markdown — unlike chat
	// jobs, whose foreground DaemonRuntime stream already does this. Mirror the
	// pending_user/running transition into the job front-matter here so the
	// existing SSE "session" broadcast (fired by Apply's subscriber loop after
	// this returns) → flow TUI refreshPlan chain surfaces the blocked state with
	// zero flow-side changes.
	s.syncSessionStatusToJobMarkdown(session, prevStatus, payload.Status)
	return true
}

// isTerminalJobStatus reports whether a job markdown status represents a
// finished job that must never be downgraded to an in-flight status.
func isTerminalJobStatus(status orchestration.JobStatus) bool {
	switch status {
	case orchestration.JobStatusCompleted,
		orchestration.JobStatusFailed,
		orchestration.JobStatusAbandoned:
		return true
	default:
		return false
	}
}

// syncSessionStatusToJobMarkdown mirrors a session's pending_user/running status
// into its job markdown front-matter (reusing flow's atomic front-matter writer)
// so detached interactive agents surface their blocked state in flow's TUI. Any
// load/write failure is logged and swallowed: a markdown-write problem must never
// break session-status application.
func (s *Store) syncSessionStatusToJobMarkdown(session *models.Session, prevStatus, newStatus string) {
	// Never drive a local-disk write off a remote session (C8/B9). A federated
	// session's JobFilePath is a satellite-side path: LoadJob would error-spam at
	// best and, on a coincidental path collision, clobber an unrelated local job
	// markdown. Remote job status converges home via the Record plane, not here.
	if session.Origin != "" {
		return
	}
	// Only flow jobs carry a JobFilePath; skip raw, non-flow Claude sessions.
	if session.JobFilePath == "" {
		return
	}
	// Only the blocked/resumed transitions are interesting here.
	if newStatus != string(orchestration.JobStatusPendingUser) &&
		newStatus != string(orchestration.JobStatusRunning) {
		return
	}
	// Avoid markdown churn on repeated identical session notifications.
	if newStatus == prevStatus {
		return
	}

	job, err := orchestration.LoadJob(session.JobFilePath)
	if err != nil {
		s.ulog.Warn("Failed to load job markdown for session status sync").
			Field("job_id", session.ID).
			Field("job_file", session.JobFilePath).
			Field("error", err.Error()).
			StructuredOnly().
			Log(context.Background())
		return
	}
	// LoadJob leaves FilePath empty (its callers set it); UpdateJobStatus needs it.
	job.FilePath = session.JobFilePath
	if job.AttemptID != "" && job.AttemptID != session.AttemptID {
		return
	}

	// Never downgrade a terminal status (completed/failed/abandoned).
	if isTerminalJobStatus(job.Status) {
		return
	}
	// Already matches the desired status — nothing to write.
	oldStatus := string(job.Status)
	if oldStatus == newStatus {
		return
	}

	sp := orchestration.NewStatePersister()
	if err := sp.UpdateJobStatus(job, orchestration.JobStatus(newStatus)); err != nil {
		s.ulog.Warn("Failed to write job markdown status for session").
			Field("job_id", session.ID).
			Field("job_file", session.JobFilePath).
			Field("new_status", newStatus).
			Field("error", err.Error()).
			StructuredOnly().
			Log(context.Background())
		return
	}

	s.ulog.Debug("Synced session status to job markdown").
		Field("job_id", session.ID).
		Field("job_file", session.JobFilePath).
		Field("old_status", oldStatus).
		Field("new_status", newStatus).
		StructuredOnly().
		Log(context.Background())
}

// applySessionVerdict changes only the derived verdict for an existing active
// local session. It deliberately does not touch LastActivity: observation by a
// health poller is not evidence of agent activity.
func maxSessionActivity(session *models.Session, observedAt time.Time) bool {
	if session == nil || observedAt.IsZero() || !observedAt.After(session.LastActivity) {
		return false
	}
	session.LastActivity = observedAt
	return true
}

// applySessionActivity renews a lease only from the closed set of authoritative
// real-activity writers. It is monotonic and never changes status or verdict.
func (s *Store) applySessionActivity(payload *SessionActivityPayload) bool {
	if payload == nil {
		return false
	}
	switch payload.Source {
	case "hook", "transcript", "pty":
	default:
		return false
	}
	session, exists := s.state.Sessions[payload.JobID]
	if !exists || session.Origin != "" || !health.IsActiveSessionStatus(session.Status) ||
		!lifecycleAttemptMatches(session.AttemptID, payload.AttemptID) {
		return false
	}
	if !payload.ExpectedStartedAt.IsZero() && !payload.ExpectedStartedAt.Equal(session.StartedAt) {
		return false
	}
	observedAt := payload.ObservedAt
	if now := s.now(); observedAt.After(now) {
		observedAt = now
	}
	return maxSessionActivity(session, observedAt)
}

func (s *Store) applySessionVerdict(payload *SessionVerdictPayload) bool {
	if payload == nil {
		return false
	}
	session, exists := s.state.Sessions[payload.JobID]
	if !exists || session.Origin != "" || !health.IsActiveSessionStatus(session.Status) ||
		!lifecycleAttemptMatches(session.AttemptID, payload.AttemptID) {
		return false
	}
	switch payload.Verified {
	case "alive", "unverified", "stale":
	default:
		return false
	}
	if session.Verified == payload.Verified {
		return false
	}
	session.Verified = payload.Verified
	return true
}

// applySessionEnd marks a session as ended. It returns false when the session
// was already terminal so callers can suppress the duplicate lifecycle update.
func (s *Store) applySessionEnd(payload *SessionEndPayload, source string) bool {
	now := s.now()
	nativeID := ""
	attemptID := payload.AttemptID
	session, sessionExists := s.state.Sessions[payload.JobID]

	if sessionExists {
		if !lifecycleAttemptMatches(session.AttemptID, payload.AttemptID) {
			s.ulog.Warn("Ignored session end for stale attempt").
				Field("event", "session.lifecycle.end_mismatch").
				Field("job_id", payload.JobID).
				Field("current_attempt_id", session.AttemptID).
				Field("incoming_attempt_id", payload.AttemptID).
				StructuredOnly().
				Log(context.Background())
			return false
		}
		if session.EndedAt != nil || isTerminalSessionStatus(session.Status) {
			return false
		}
		s.logLegacyLifecycle("session.lifecycle.end_legacy", payload.JobID, session.AttemptID, payload.AttemptID)
		if session.AttemptID == "" {
			session.AttemptID = payload.AttemptID
		}
		attemptID = session.AttemptID
		nativeID = session.ClaudeSessionID
		session.Status = payload.Outcome
		session.Verified = ""
		session.EndedAt = &now
		session.LastActivity = now
	}

	// Also update the job if it exists in the Jobs map (from JobCollector
	// discovery). "exited" is deliberately session-only: a supervised process
	// can exit successfully before Flow's completion gate decides the job result.
	job, jobExists := s.state.Jobs[payload.JobID]
	if !sessionExists && jobExists && !lifecycleAttemptMatches(job.AttemptID, payload.AttemptID) {
		return false
	}
	if jobExists && payload.Outcome != "exited" && lifecycleAttemptMatches(job.AttemptID, payload.AttemptID) {
		job.Status = payload.Outcome
		job.CompletedAt = &now
	}

	reason := payload.Reason
	if strings.TrimSpace(reason) == "" {
		reason = source
	}
	reason = lifecycleReason(reason)
	s.ulog.Info("Session lifecycle: terminal").
		Field("event", "session.lifecycle.terminal").
		Field("job_id", payload.JobID).
		Field("attempt_id", attemptID).
		Field("native_id", nativeID).
		Field("outcome", payload.Outcome).
		Field("reason", reason).
		StructuredOnly().
		Log(context.Background())
	return true
}

func isTerminalSessionStatus(status string) bool {
	switch status {
	case "completed", "interrupted", "failed", "exited", "stopped", "error", "abandoned":
		return true
	default:
		return false
	}
}

// applySessionTokens overlays daemon-computed live token usage onto existing
// session records in place. It only stamps sessions that already exist (a token
// snapshot for a since-ended session is simply dropped) and never touches
// LastActivity — a token refresh is not agent activity.
func (s *Store) applySessionTokens(payload *SessionTokensPayload) {
	for _, u := range payload.Updates {
		session, exists := s.state.Sessions[u.JobID]
		if !exists || !lifecycleAttemptMatches(session.AttemptID, u.AttemptID) {
			continue
		}
		session.LiveTokens = u.LiveTokens
		session.LiveCostUSD = u.LiveCostUSD
		session.ContextSize = u.ContextSize
		session.Model = u.Model
	}
}

func (s *Store) applyTaskResult(payload *TaskResultPayload) {
	normalizedInput, err := pathutil.NormalizeForLookup(payload.Workspace)
	if err != nil {
		return
	}
	for _, ws := range s.state.Workspaces {
		if ws.WorkspaceNode == nil {
			continue
		}
		normalizedPath, err := pathutil.NormalizeForLookup(ws.Path)
		if err != nil || normalizedPath != normalizedInput {
			continue
		}
		if ws.TaskResults == nil {
			ws.TaskResults = make(map[string]*models.TaskResult)
		}
		ws.TaskResults[payload.Verb] = payload.Result
		return
	}
}

func (s *Store) applyTestReport(payload *TestReportPayload) {
	normalizedInput, err := pathutil.NormalizeForLookup(payload.Workspace)
	if err != nil {
		return
	}
	for _, ws := range s.state.Workspaces {
		if ws.WorkspaceNode == nil {
			continue
		}
		normalizedPath, err := pathutil.NormalizeForLookup(ws.Path)
		if err != nil || normalizedPath != normalizedInput {
			continue
		}
		if ws.TestReports == nil {
			ws.TestReports = make(map[string]*models.TestReport)
		}
		ws.TestReports[payload.Report.Verb] = payload.Report
		return
	}
}

// persistAsync snapshots task/test results and writes to disk in a goroutine.
// Must be called under the store's write lock.
func (s *Store) persistAsync() {
	snap := snapshot(s.state.Workspaces)
	go s.persister.save(snap)
}

// SetSessionPtyID associates a daemon PTY session ID with a session entry.
// Called by handleAgentSpawn after creating the daemon-owned PTY.
func (s *Store) SetSessionPtyID(jobID, attemptID, ptyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, exists := s.state.Sessions[jobID]; exists && lifecycleAttemptMatches(session.AttemptID, attemptID) {
		session.PtyID = ptyID
	}
}

// applyNoteEvent incrementally adjusts note counts and the note index based on a mutation event.
// The event carries workspace name and note type, allowing precise count updates
// without a full filesystem rescan.
func (s *Store) applyNoteEvent(event *models.NoteEvent) {
	// Update note index
	switch event.Event {
	case models.NoteEventCreated, models.NoteEventUpdated:
		if event.IndexEntry != nil {
			s.state.NoteIndex[event.Path] = event.IndexEntry
		}
	case models.NoteEventDeleted, models.NoteEventArchived:
		// Typed archive events carry Path = new .archive location and
		// PrevPath = original location; evict both so the source entry
		// doesn't go stale. Legacy events (PrevPath empty) keep working.
		delete(s.state.NoteIndex, event.Path)
		if event.PrevPath != "" {
			delete(s.state.NoteIndex, event.PrevPath)
		}
	case models.NoteEventMoved:
		if event.PrevPath != "" {
			delete(s.state.NoteIndex, event.PrevPath)
		}
		if event.IndexEntry != nil {
			s.state.NoteIndex[event.Path] = event.IndexEntry
		}
	case models.NoteEventRenamed:
		if event.PrevPath != "" {
			delete(s.state.NoteIndex, event.PrevPath)
		}
		if event.IndexEntry != nil {
			s.state.NoteIndex[event.Path] = event.IndexEntry
		}
	}

	// Find workspace by name (state.Workspaces is keyed by path)
	for _, ws := range s.state.Workspaces {
		if ws.WorkspaceNode == nil || ws.Name != event.NotespaceName {
			continue
		}

		if ws.NoteCounts == nil {
			ws.NoteCounts = &models.NoteCounts{}
		}

		switch event.Event {
		case models.NoteEventCreated:
			adjustNoteCount(ws.NoteCounts, event.NoteType, 1)
		case models.NoteEventDeleted, models.NoteEventArchived:
			adjustNoteCount(ws.NoteCounts, event.NoteType, -1)
		case models.NoteEventMoved:
			adjustNoteCount(ws.NoteCounts, event.NoteType, 1)
		case models.NoteEventUpdated:
			// No count change
		case models.NoteEventRenamed:
			// No count change — same workspace and type
		}
		break
	}

	// For moved/archived events, also decrement the source workspace
	if event.Event == models.NoteEventMoved && event.PrevNotespaceName != "" {
		for _, ws := range s.state.Workspaces {
			if ws.WorkspaceNode == nil || ws.Name != event.PrevNotespaceName {
				continue
			}
			if ws.NoteCounts == nil {
				ws.NoteCounts = &models.NoteCounts{}
			}
			prevType := event.PrevNoteType
			if prevType == "" {
				prevType = event.NoteType
			}
			adjustNoteCount(ws.NoteCounts, prevType, -1)
			break
		}
	}
}

// adjustNoteCount modifies a specific count field by delta.
func adjustNoteCount(counts *models.NoteCounts, noteType string, delta int) {
	switch noteType {
	case "current":
		counts.Current = max(0, counts.Current+delta)
	case "issues":
		counts.Issues = max(0, counts.Issues+delta)
	case "inbox":
		counts.Inbox = max(0, counts.Inbox+delta)
	case "docs":
		counts.Docs = max(0, counts.Docs+delta)
	case "completed":
		counts.Completed = max(0, counts.Completed+delta)
	case "review":
		counts.Review = max(0, counts.Review+delta)
	case "in-progress", "in_progress":
		counts.InProgress = max(0, counts.InProgress+delta)
	default:
		counts.Other = max(0, counts.Other+delta)
	}
}

// publishLocked stamps u with the next sequence number, records it in the
// replay ring, and fans it out to every subscriber. Sends stay non-blocking:
// a slow client drops frames rather than stalling the daemon (the historical
// contract — the ring is what lets it recover).
//
// The caller must hold s.mu, read or write. Every broadcast in this package
// goes through here; adding a second fan-out loop would mint updates the ring
// never saw, so ?since= would silently skip them.
func (s *Store) publishLocked(u Update) {
	u.Seq = s.recordLocked(u)
	for ch := range s.subscribers {
		select {
		case ch <- u:
		default:
			// Non-blocking send to prevent slow clients from stalling the daemon
		}
	}
}

// RingDropsPayload reports whether an update's payload must be left out of the
// replay ring.
//
// RingSize's bound reasons that the ring "stores the same payload pointers the
// state already holds, so it pins almost nothing the state does not". That is
// true of the deltas and per-entity events that dominate the ring's volume, and
// FALSE of the handful of types whose payload is a WHOLESALE REPLACEMENT of a
// large map: ApplyUpdate swaps state's pointer to the new generation, and the
// ring keeps every SUPERSEDED generation alive until 1024 further updates evict
// it. Measured on the live daemon (2026-08-13, 691 workspaces, 19h uptime): one
// note-index generation retains 20 MB across 38k entries, the collector rebuilds
// and republishes it whole every 5 minutes, and the ring was pinning ~12 of them
// — 244 MB, 38% of the post-GC live heap.
//
// Dropping the payload is unobservable rather than merely cheap. Store.Replay is
// the ring's ONLY reader; it feeds server.convertUpdatePayload, and these types
// are exactly the ones that reach the wire without their payload ("note_index"
// is a re-fetch signal carrying no data) or do not reach the wire at all
// (declared in server.apiUpdateSkipList, which points consumers at GET /api/plans
// and GET /api/plans/index). The sequence number, type and source still replay,
// so a ?since= client sees the same frames it always did. Nothing in-process
// reads these payloads off a subscription channel either — the store is their
// only consumer, via ApplyUpdate, which has already run by the time the update
// is recorded.
//
// TestRingDropsAreUnreachableOnTheWire pins that correspondence: a type may only
// be listed here while its wire shape carries no payload.
func RingDropsPayload(t UpdateType) bool {
	switch t {
	case UpdateNoteIndex, UpdatePlans, UpdatePlanIndexSnapshot:
		return true
	default:
		return false
	}
}

// recordLocked assigns the next sequence number and writes the stamped update
// into the replay ring, returning the assigned sequence.
//
// u is taken BY VALUE: clearing the payload here bounds what the ring retains
// without touching the copy publishLocked broadcasts to live subscribers.
func (s *Store) recordLocked(u Update) uint64 {
	s.busMu.Lock()
	defer s.busMu.Unlock()
	s.seq++
	u.Seq = s.seq
	if RingDropsPayload(u.Type) {
		u.Payload = nil
	}
	if len(s.ring) > 0 {
		s.ring[s.ringPos] = u
		s.ringPos = (s.ringPos + 1) % len(s.ring)
		if s.ringLen < len(s.ring) {
			s.ringLen++
		}
	}
	return s.seq
}

// CurrentSeq returns the sequence number of the most recently published
// update (0 when nothing has been published by this daemon process).
func (s *Store) CurrentSeq() uint64 {
	s.busMu.Lock()
	defer s.busMu.Unlock()
	return s.seq
}

// ReplayGap reports that a ?since= cursor could not be honored exactly. A zero
// Reason means the replay was complete: the caller saw every update after its
// cursor.
type ReplayGap struct {
	// Reason is "too_old" when the ring had already evicted the updates the
	// cursor asked for, and "reset" when the cursor is ahead of the daemon's
	// current sequence — which means the daemon restarted (sequences restart
	// at 1) or the client invented a cursor.
	Reason string
	// Since is the cursor the caller passed.
	Since uint64
	// Oldest is the lowest sequence still retained (0 when the ring is empty).
	Oldest uint64
	// Current is the daemon's sequence at the time of the call.
	Current uint64
}

// Gapped reports whether the replay was lossy.
func (g ReplayGap) Gapped() bool { return g.Reason != "" }

const (
	// ReplayGapTooOld means the ring evicted what the cursor asked for.
	ReplayGapTooOld = "too_old"
	// ReplayGapReset means the cursor is ahead of the daemon — a restart.
	ReplayGapReset = "reset"
)

// Replay returns every retained update with Seq > since, oldest first, plus a
// gap verdict. A gapped result still resumes correctly (the caller gets
// whatever IS retained, or nothing on a reset), but the caller MUST
// snapshot-reconcile rather than assume continuity: see RingSize.
func (s *Store) Replay(since uint64) ([]Update, ReplayGap) {
	s.busMu.Lock()
	defer s.busMu.Unlock()

	gap := ReplayGap{Since: since, Current: s.seq}
	if s.ringLen > 0 {
		gap.Oldest = s.seq - uint64(s.ringLen) + 1
	}

	switch {
	case since > s.seq:
		// The client's cursor predates this daemon process. Replaying nothing
		// is the only honest answer; the gap tells it to re-snapshot.
		gap.Reason = ReplayGapReset
		return nil, gap
	case since == s.seq:
		return nil, gap
	case s.ringLen == 0 || since+1 < gap.Oldest:
		gap.Reason = ReplayGapTooOld
	}

	out := make([]Update, 0, s.ringLen)
	// Walk the ring oldest-first: ringPos is the next write slot, so it is
	// also the oldest entry once the buffer has wrapped.
	start := (s.ringPos - s.ringLen + len(s.ring)) % len(s.ring)
	for i := 0; i < s.ringLen; i++ {
		u := s.ring[(start+i)%len(s.ring)]
		if u.Seq > since {
			out = append(out, u)
		}
	}
	return out, gap
}

// Subscribe creates a new subscription channel for state updates.
func (s *Store) Subscribe() chan Update {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Update, 100) // Buffered
	s.subscribers[ch] = struct{}{}
	return ch
}

// Unsubscribe removes a subscription and closes its channel.
func (s *Store) Unsubscribe(ch chan Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscribers, ch)
	close(ch)
}

// normFocusPath canonicalizes a focus path so SetFocus and IsFocused key off the
// same normalized string regardless of case/symlink differences between the
// path a client pushes (e.g. treemux's ScopeReposForFocus / a raw nav peek path)
// and the daemon's discovered ws.Path. Without this the focus set matched a repo
// case-insensitively for scan-selection but case-SENSITIVELY for the per-file
// ChangedFiles attachment, so a focused repo was scanned yet silently emitted
// no per-file data — the git-viewer cache-missed and fell back to live git.
// Falls back to a lowercased raw path when canonicalization fails (path not on
// disk yet); NormalizeForLookup is memoized so this is cheap on the hot path.
func normFocusPath(p string) string {
	if n, err := pathutil.NormalizeForLookup(p); err == nil {
		return n
	}
	return strings.ToLower(p)
}

// NormalizePathKey canonicalizes a path with the same rules the focus set uses
// (see normFocusPath). Collectors that select workspaces by path — e.g. the
// git collector's scope-bounded full sweeps — must go through this rather than
// comparing raw spellings, or a case/symlink difference between the configured
// scope and the daemon's discovered ws.Path silently drops workspaces.
func NormalizePathKey(p string) string {
	return normFocusPath(p)
}

const defaultFocusTTL = 5 * time.Minute

type focusRegistration struct {
	paths     map[string]struct{}
	expiresAt time.Time
}

// SetFocus replaces a source's focused paths with the default bounded lease.
// Existing clients keep the same API, but must periodically re-assert visibility;
// abandoned focus no longer leaves the daemon on its fast polling path forever.
func (s *Store) SetFocus(source string, paths []string) {
	s.SetFocusTTL(source, paths, defaultFocusTTL)
}

// SetFocusTTL replaces a source's focused paths with a bounded lease. Empty
// paths clear immediately; non-positive or overlong TTLs use the five-minute
// daemon maximum.
func (s *Store) SetFocusTTL(source string, paths []string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(paths) == 0 {
		delete(s.focus, source)
	} else {
		if ttl <= 0 || ttl > defaultFocusTTL {
			ttl = defaultFocusTTL
		}
		set := make(map[string]struct{}, len(paths))
		for _, p := range paths {
			set[normFocusPath(p)] = struct{}{}
		}
		s.focus[source] = focusRegistration{paths: set, expiresAt: s.now().Add(ttl)}
	}
	s.broadcastFocusLocked()
}

// pruneExpiredFocusLocked removes expired source leases. The caller holds s.mu.
func (s *Store) pruneExpiredFocusLocked() bool {
	now := s.now()
	changed := false
	for source, reg := range s.focus {
		if !now.Before(reg.expiresAt) {
			delete(s.focus, source)
			changed = true
		}
	}
	return changed
}

func (s *Store) aggregateFocusLocked() map[string]struct{} {
	result := make(map[string]struct{})
	for _, reg := range s.focus {
		for p := range reg.paths {
			result[p] = struct{}{}
		}
	}
	return result
}

func (s *Store) broadcastFocusLocked() {
	aggSet := s.aggregateFocusLocked()
	agg := make([]string, 0, len(aggSet))
	for p := range aggSet {
		agg = append(agg, p)
	}
	s.publishLocked(Update{Type: UpdateFocus, Source: "client", Scanned: len(agg), Payload: agg})
}

// GetFocus returns the aggregated, non-expired focused workspace paths.
func (s *Store) GetFocus() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pruneExpiredFocusLocked() {
		s.broadcastFocusLocked()
	}
	return s.aggregateFocusLocked()
}

// ResolveWorkspacePaths maps request paths onto the store's known workspaces
// using the same normalization as the focus set (see normFocusPath), so the
// scoped /api/refresh accepts the same path spellings clients already push as
// focus. Unknown paths are silently skipped; duplicates resolve once.
func (s *Store) ResolveWorkspacePaths(paths []string) []*models.EnrichedWorkspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byNorm := make(map[string]*models.EnrichedWorkspace, len(s.state.Workspaces))
	for _, ws := range s.state.Workspaces {
		byNorm[normFocusPath(ws.Path)] = ws
	}
	seen := make(map[string]struct{}, len(paths))
	resolved := make([]*models.EnrichedWorkspace, 0, len(paths))
	for _, p := range paths {
		key := normFocusPath(p)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if ws, ok := byNorm[key]; ok {
			resolved = append(resolved, ws)
		}
	}
	return resolved
}

// IsFocused returns true if the given path is focused by any source. The lookup
// normalizes the query path the same way SetFocus normalized the stored keys, so
// a case/symlink difference between the pushed focus path and the daemon's
// ws.Path can no longer make a focused repo miss its per-file enrichment.
func (s *Store) IsFocused(path string) bool {
	key := normFocusPath(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pruneExpiredFocusLocked() {
		s.broadcastFocusLocked()
	}
	for _, reg := range s.focus {
		if _, ok := reg.paths[key]; ok {
			return true
		}
	}
	return false
}

// BroadcastConfigReload sends a config reload notification to all subscribers.
// This is used by the ConfigWatcher to notify clients when config files change.
func (s *Store) BroadcastConfigReload(file string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	s.publishLocked(Update{
		Type:    UpdateConfigReload,
		Source:  "config",
		Payload: file, // The file that changed
	})
}

// BroadcastThemeChanged sends a theme change notification to all subscribers.
// It is emitted by the groved ConfigWatcher wiring when the resolved global
// tui.theme value actually changes (the coarse config_reload event still
// fires for every config write). The palette payload carries fully resolved
// role colors for both appearances so consumers never re-parse layered
// config.
func (s *Store) BroadcastThemeChanged(name string, palette *coredaemon.ThemeChangedPayload) {
	if palette == nil {
		return
	}
	if palette.Name == "" {
		palette.Name = name
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	s.publishLocked(Update{
		Type:    UpdateThemeChanged,
		Source:  "config",
		Payload: palette,
	})
}

// BroadcastBootPhase fans a daemon boot-progress transition out to all
// subscribers. status is *daemon.BootStatus (core/pkg/daemon); it is passed
// opaquely so the store keeps no dependency on the core client package —
// convertToAPIUpdate type-asserts it back on the server side. Modeled on
// BroadcastConfigReload: best-effort, non-blocking, drops on a full buffer.
func (s *Store) BroadcastBootPhase(status interface{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	s.publishLocked(Update{
		Type:    UpdateBootPhase,
		Source:  "boot",
		Payload: status,
	})
}
