package pty

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	tuimuxpty "github.com/grovetools/tuimux/pty"
)

// Manager is a shim around tuimux/pty.Manager that translates Grove-specific fields.
type Manager struct {
	inner *tuimuxpty.Manager
	mu    sync.RWMutex
	grove map[string]*Session // Grove wrappers keyed by session ID
}

// CreateRequest holds Grove-specific PTY creation parameters.
type CreateRequest struct {
	CWD       string            `json:"cwd"`
	Env       []string          `json:"env,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Rows      uint16            `json:"rows,omitempty"`
	Cols      uint16            `json:"cols,omitempty"`
	Origin    string            `json:"origin,omitempty"`
	PanelID   string            `json:"panel_id,omitempty"`
	Label     string            `json:"label,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	CreatedBy string            `json:"created_by,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
}

// NewManager creates a new PTY session manager.
func NewManager() *Manager {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Manager{
		inner: tuimuxpty.NewManager(logger.With(slog.String("component", "groved.pty"))),
		grove: make(map[string]*Session),
	}
}

// Create spawns a new PTY session, mapping Grove fields to tuimux Tags.
func (m *Manager) Create(req CreateRequest) (*Session, error) {
	name := req.Workspace
	if name == "" {
		name = filepath.Base(req.CWD)
	}

	// Merge Grove-specific fields into tags
	tags := make(map[string]string)
	for k, v := range req.Labels {
		tags[k] = v
	}
	if req.Origin != "" {
		tags["origin"] = req.Origin
	}
	if req.PanelID != "" {
		tags["panel_id"] = req.PanelID
	}
	if req.Label != "" {
		tags["label"] = req.Label
	}
	if req.SessionID != "" {
		tags["session_id"] = req.SessionID
	}
	if req.CreatedBy != "" {
		tags["created_by"] = req.CreatedBy
	}

	// Also inject Grove-specific env vars alongside tuimux's TUIMUX_PTY=1
	env := append(req.Env, "GROVE_PTY=1", "GROVE_TERMINAL=1")

	inner, err := m.inner.Create(tuimuxpty.CreateRequest{
		CWD:     req.CWD,
		Env:     env,
		Tags:    tags,
		Rows:    req.Rows,
		Cols:    req.Cols,
		Name:    name,
		Command: req.Command,
		Args:    req.Args,
	})
	if err != nil {
		return nil, err
	}

	sess := &Session{
		Session:   inner,
		Workspace: name,
		Origin:    req.Origin,
		PanelID:   req.PanelID,
		Label:     req.Label,
		SessionID: req.SessionID,
		CreatedBy: req.CreatedBy,
	}

	m.mu.Lock()
	m.grove[inner.ID] = sess
	m.mu.Unlock()

	// Clean up wrapper when session exits
	go func() {
		<-inner.ExitCh()
		m.mu.Lock()
		delete(m.grove, inner.ID)
		m.mu.Unlock()
	}()

	return sess, nil
}

// Get returns a Grove-wrapped session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.grove[id]
	return s, ok
}

// List returns Grove-enriched metadata for all active sessions.
func (m *Manager) List() []SessionMetadata {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]SessionMetadata, 0, len(m.grove))
	for _, s := range m.grove {
		md := s.Metadata()
		md.ForegroundProcess = s.ForegroundProcess()
		result = append(result, md)
	}
	return result
}

// Kill terminates a session by ID.
func (m *Manager) Kill(id string) error {
	m.mu.RLock()
	s, ok := m.grove[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	return s.Kill()
}

// Shutdown kills all active PTY sessions.
func (m *Manager) Shutdown() {
	m.inner.Shutdown()
}
