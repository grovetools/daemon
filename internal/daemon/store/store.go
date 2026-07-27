package store

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"

	grovelogging "github.com/grovetools/core/logging"
	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/pathutil"
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

	// satSeenSnapshot marks origins whose first UpdateSatelliteSnapshot of this
	// process has been applied. The first snapshot per origin is a baseline
	// (state transfer, not transitions): jobs already terminal in it synthesize
	// no per-job events (see the UpdateSatelliteSnapshot branch of ApplyUpdate).
	// Guarded by mu.
	satSeenSnapshot map[string]struct{}
}

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
				if prev, ok := s.state.Sessions[sessionKey(sess)]; ok && sess.LiveChildren == 0 {
					sess.LiveChildren = prev.LiveChildren
				}
				newMap[sessionKey(sess)] = sess
			}
			s.state.Sessions = newMap
		}

	// Session lifecycle updates
	case UpdateSessionIntent:
		if payload, ok := u.Payload.(*SessionIntentPayload); ok {
			s.applySessionIntent(payload)
		}
	case UpdateSessionConfirmation:
		if payload, ok := u.Payload.(*SessionConfirmationPayload); ok {
			s.applySessionConfirmation(payload)
		}
	case UpdateSessionStatus:
		if payload, ok := u.Payload.(*SessionStatusPayload); ok {
			s.applySessionStatus(payload)
		}
	case UpdateSessionEnd:
		if payload, ok := u.Payload.(*SessionEndPayload); ok {
			s.applySessionEnd(payload)
		}
	case UpdateSessionTokens:
		if payload, ok := u.Payload.(*SessionTokensPayload); ok {
			s.applySessionTokens(payload)
		}

	// Job lifecycle updates
	case UpdateJobSubmitted, UpdateJobStarted, UpdateJobCompleted, UpdateJobFailed, UpdateJobCancelled, UpdateJobPendingUser:
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
			s.state.NoteIndex = noteIndex
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
	for ch := range s.subscribers {
		select {
		case ch <- u:
		default:
			// Non-blocking send to prevent slow clients from stalling the daemon
		}
	}
	// Broadcast derived updates after their source transition so subscribers
	// observe state-then-delta ordering. These go straight to broadcast because
	// their state mutations were already applied under this lock.
	for _, su := range synthesized {
		for ch := range s.subscribers {
			select {
			case ch <- su:
			default:
			}
		}
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
func (s *Store) applySessionIntent(payload *SessionIntentPayload) {
	session := &models.Session{
		ID:               payload.JobID,
		Type:             "interactive_agent",
		Provider:         payload.Provider,
		PID:              0, // Not yet known
		WorkingDirectory: payload.WorkDir,
		Status:           "pending", // Waiting for confirmation
		StartedAt:        time.Now(),
		LastActivity:     time.Now(),
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
}

// applySessionConfirmation updates a pending session with actual process info.
func (s *Store) applySessionConfirmation(payload *SessionConfirmationPayload) {
	session, exists := s.state.Sessions[payload.JobID]
	if !exists {
		// Create a new session if intent was missed
		session = &models.Session{
			ID:        payload.JobID,
			Type:      "interactive_agent",
			StartedAt: time.Now(),
		}
		s.state.Sessions[payload.JobID] = session
	}

	// Update with confirmation data
	session.ClaudeSessionID = payload.NativeID
	session.PID = payload.PID
	session.Status = "running"
	session.LastActivity = time.Now()
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
	if jobExists {
		job.PID = payload.PID
	}

	// Diagnostic (permanent, Debug): proves the PID arrived non-zero and
	// whether the JobInfo was present in the store at confirm time. Observe via
	// `core logs --component groved.store --level debug -f`.
	s.ulog.Debug("Session confirmation: propagating PID to JobInfo").
		Field("job_id", payload.JobID).
		Field("pid", payload.PID).
		Field("job_exists_in_store", jobExists).
		StructuredOnly().
		Log(context.Background())
}

// applySessionStatus updates the status of an active session.
// If the session doesn't exist, creates a minimal record so status transitions
// (e.g., idle→running from hooks PreToolUse) work even without prior registration.
func (s *Store) applySessionStatus(payload *SessionStatusPayload) {
	session, exists := s.state.Sessions[payload.JobID]
	if !exists {
		// Create a minimal session record — hooks may be calling UpdateSessionStatus
		// before flow has registered the session via RegisterSessionIntent.
		session = &models.Session{
			ID:           payload.JobID,
			Status:       payload.Status,
			StartedAt:    time.Now(),
			LastActivity: time.Now(),
		}
		s.state.Sessions[payload.JobID] = session
		return
	}

	prevStatus := session.Status
	session.Status = payload.Status
	session.LastActivity = time.Now()

	// Interactive (tmux-detached) agents have no foreground runtime loop to
	// persist a mid-session status change into their job markdown — unlike chat
	// jobs, whose foreground DaemonRuntime stream already does this. Mirror the
	// pending_user/running transition into the job front-matter here so the
	// existing SSE "session" broadcast (fired by Apply's subscriber loop after
	// this returns) → flow TUI refreshPlan chain surfaces the blocked state with
	// zero flow-side changes.
	s.syncSessionStatusToJobMarkdown(session, prevStatus, payload.Status)
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

// applySessionEnd marks a session as ended.
func (s *Store) applySessionEnd(payload *SessionEndPayload) {
	now := time.Now()

	if session, exists := s.state.Sessions[payload.JobID]; exists {
		session.Status = payload.Outcome
		session.EndedAt = &now
		session.LastActivity = now
	}

	// Also update the job if it exists in the Jobs map (from JobCollector discovery)
	if job, exists := s.state.Jobs[payload.JobID]; exists {
		job.Status = payload.Outcome
		job.CompletedAt = &now
	}
}

// applySessionTokens overlays daemon-computed live token usage onto existing
// session records in place. It only stamps sessions that already exist (a token
// snapshot for a since-ended session is simply dropped) and never touches
// LastActivity — a token refresh is not agent activity.
func (s *Store) applySessionTokens(payload *SessionTokensPayload) {
	for _, u := range payload.Updates {
		session, exists := s.state.Sessions[u.JobID]
		if !exists {
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
func (s *Store) SetSessionPtyID(jobID, ptyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, exists := s.state.Sessions[jobID]; exists {
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
		if ws.WorkspaceNode == nil || ws.Name != event.Workspace {
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
	if event.Event == models.NoteEventMoved && event.PrevWorkspace != "" {
		for _, ws := range s.state.Workspaces {
			if ws.WorkspaceNode == nil || ws.Name != event.PrevWorkspace {
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
	update := Update{Type: UpdateFocus, Source: "client", Scanned: len(agg), Payload: agg}
	for ch := range s.subscribers {
		select {
		case ch <- update:
		default:
		}
	}
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

	update := Update{
		Type:    UpdateConfigReload,
		Source:  "config",
		Payload: file, // The file that changed
	}
	for ch := range s.subscribers {
		select {
		case ch <- update:
		default:
		}
	}
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

	update := Update{
		Type:    UpdateThemeChanged,
		Source:  "config",
		Payload: palette,
	}
	for ch := range s.subscribers {
		select {
		case ch <- update:
		default:
		}
	}
}

// BroadcastBootPhase fans a daemon boot-progress transition out to all
// subscribers. status is *daemon.BootStatus (core/pkg/daemon); it is passed
// opaquely so the store keeps no dependency on the core client package —
// convertToAPIUpdate type-asserts it back on the server side. Modeled on
// BroadcastConfigReload: best-effort, non-blocking, drops on a full buffer.
func (s *Store) BroadcastBootPhase(status interface{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	update := Update{
		Type:    UpdateBootPhase,
		Source:  "boot",
		Payload: status,
	}
	for ch := range s.subscribers {
		select {
		case ch <- update:
		default:
		}
	}
}
