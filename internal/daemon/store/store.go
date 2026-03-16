package store

import (
	"sync"

	"github.com/grovetools/core/pkg/enrichment"
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
			Workspaces: make(map[string]*enrichment.EnrichedWorkspace),
			Sessions:   make(map[string]*models.Session),
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
func (s *Store) GetWorkspaces() []*enrichment.EnrichedWorkspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*enrichment.EnrichedWorkspace, 0, len(s.state.Workspaces))
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

// ApplyUpdate modifies the state and notifies subscribers.
func (s *Store) ApplyUpdate(u Update) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch u.Type {
	case UpdateWorkspaces:
		if workspaces, ok := u.Payload.(map[string]*enrichment.EnrichedWorkspace); ok {
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
