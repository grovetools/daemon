package store

import (
	"sync"
	"time"

	"github.com/grovetools/core/pkg/models"
)

// Store is the in-memory state store for the daemon.
// It is thread-safe and supports pub/sub for real-time updates.
type Store struct {
	mu          sync.RWMutex
	state       *State
	subscribers map[chan Update]struct{}
	focus       map[string]struct{} // Focused workspace paths for priority scanning
}

// New creates a new Store instance.
func New() *Store {
	return &Store{
		state: &State{
			Workspaces: make(map[string]*models.EnrichedWorkspace),
			Sessions:   make(map[string]*models.Session),
			Jobs:       make(map[string]*models.JobInfo),
		},
		subscribers: make(map[chan Update]struct{}),
		focus:       make(map[string]struct{}),
	}
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

// ApplyUpdate modifies the state and notifies subscribers.
func (s *Store) ApplyUpdate(u Update) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch u.Type {
	case UpdateWorkspaces:
		if workspaces, ok := u.Payload.(map[string]*models.EnrichedWorkspace); ok {
			s.state.Workspaces = workspaces
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
	// Note: TranscriptPath is not currently in models.Session but could be added
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

	session.Status = payload.Status
	session.LastActivity = time.Now()
}

// applySessionEnd marks a session as ended.
func (s *Store) applySessionEnd(payload *SessionEndPayload) {
	session, exists := s.state.Sessions[payload.JobID]
	if !exists {
		return // Session not found
	}

	session.Status = payload.Outcome
	now := time.Now()
	session.EndedAt = &now
	session.LastActivity = now
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

// SetFocus updates the set of focused workspace paths.
// Focused workspaces get priority scanning by collectors.
func (s *Store) SetFocus(paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.focus = make(map[string]struct{}, len(paths))
	for _, p := range paths {
		s.focus[p] = struct{}{}
	}

	// Broadcast focus change to subscribers
	update := Update{
		Type:    UpdateFocus,
		Source:  "client",
		Scanned: len(paths),
		Payload: paths,
	}
	for ch := range s.subscribers {
		select {
		case ch <- update:
		default:
		}
	}
}

// GetFocus returns the set of focused workspace paths.
func (s *Store) GetFocus() map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy
	result := make(map[string]struct{}, len(s.focus))
	for k := range s.focus {
		result[k] = struct{}{}
	}
	return result
}

// IsFocused returns true if the given path is in the focus set.
func (s *Store) IsFocused(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.focus[path]
	return ok
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
