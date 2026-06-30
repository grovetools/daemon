package store

import (
	"context"
	"sync"
	"time"

	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/util/pathutil"
	"github.com/grovetools/flow/pkg/orchestration"
)

// Store is the in-memory state store for the daemon.
// It is thread-safe and supports pub/sub for real-time updates.
type Store struct {
	mu                sync.RWMutex
	state             *State
	subscribers       map[chan Update]struct{}
	focus             map[string]map[string]struct{} // [source][path] focused workspace paths for priority scanning
	persister         *Persister
	workflowPersister *workflowPersister
	pendingRestore    persistedState // Loaded from disk, applied when workspaces arrive
	ulog              *grovelogging.UnifiedLogger
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
		},
		subscribers:       make(map[chan Update]struct{}),
		focus:             make(map[string]map[string]struct{}),
		persister:         newPersister(),
		workflowPersister: newWorkflowPersister(),
		ulog:              grovelogging.NewUnifiedLogger("groved.store"),
	}
	s.loadPersistedResults()
	s.loadPersistedWorkflowEvents()
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

// ApplyUpdate modifies the state and notifies subscribers.
func (s *Store) ApplyUpdate(u Update) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch u.Type {
	case UpdateWorkspaces:
		if workspaces, ok := u.Payload.(map[string]*models.EnrichedWorkspace); ok {
			s.state.Workspaces = workspaces
			if s.pendingRestore != nil {
				restoreResults(s.state.Workspaces, s.pendingRestore)
				s.pendingRestore = nil
			}
		}
	case UpdateSessions:
		if sessions, ok := u.Payload.([]*models.Session); ok {
			// Rebuild map
			newMap := make(map[string]*models.Session)
			for _, sess := range sessions {
				newMap[sess.ID] = sess
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

	// Job lifecycle updates
	case UpdateJobSubmitted, UpdateJobStarted, UpdateJobCompleted, UpdateJobFailed, UpdateJobCancelled, UpdateJobPendingUser:
		if job, ok := u.Payload.(*models.JobInfo); ok {
			s.state.Jobs[job.ID] = job
		}

	// Note mutation events from nb
	case UpdateNoteEvent:
		if event, ok := u.Payload.(*models.NoteEvent); ok {
			s.applyNoteEvent(event)
		}

	// Full note index replacement from collector scan
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
				if existing, exists := s.state.Jobs[job.ID]; exists {
					// Prevent stale filesystem reads from reverting active daemon states.
					// If daemon says running/queued, but file says pending/queued/pending_user, ignore.
					// However, if the file says completed/failed/idle, accept the update!
					if (existing.Status == "running" || existing.Status == "queued") &&
						(job.Status == "pending" || job.Status == "queued" || job.Status == "pending_user") {
						continue
					}
				}
				s.state.Jobs[job.ID] = job
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

	// Workflow/subagent lifecycle events (hooks + journal watcher).
	case UpdateWorkflowRunDiscovered, UpdateWorkflowAgentStarted,
		UpdateWorkflowAgentCompleted, UpdateWorkflowRunStale,
		UpdateWorkflowRunCompleted:
		if payload, ok := u.Payload.(*WorkflowEventPayload); ok {
			s.applyWorkflowEvent(payload, true)
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

// SetFocus replaces the set of focused workspace paths for a single source.
// Each source (e.g. "nav", "treemux_git") owns its own path set; multiple
// sources are aggregated across IsFocused/GetFocus so they don't clobber each
// other. Focused workspaces get priority scanning by collectors.
func (s *Store) SetFocus(source string, paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(paths) == 0 {
		delete(s.focus, source)
	} else {
		set := make(map[string]struct{}, len(paths))
		for _, p := range paths {
			set[p] = struct{}{}
		}
		s.focus[source] = set
	}

	// Aggregate the full focus set across all sources for the broadcast payload.
	seen := make(map[string]struct{})
	agg := make([]string, 0)
	for _, set := range s.focus {
		for p := range set {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			agg = append(agg, p)
		}
	}

	// Broadcast focus change to subscribers
	update := Update{
		Type:    UpdateFocus,
		Source:  "client",
		Scanned: len(agg),
		Payload: agg,
	}
	for ch := range s.subscribers {
		select {
		case ch <- update:
		default:
		}
	}
}

// GetFocus returns the aggregated set of focused workspace paths across all sources.
func (s *Store) GetFocus() map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy aggregated across all sources.
	result := make(map[string]struct{})
	for _, set := range s.focus {
		for p := range set {
			result[p] = struct{}{}
		}
	}
	return result
}

// IsFocused returns true if the given path is focused by any source.
func (s *Store) IsFocused(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, set := range s.focus {
		if _, ok := set[path]; ok {
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
