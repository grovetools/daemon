package pty

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	creackpty "github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

// Manager is a thread-safe registry of daemon-owned PTY sessions.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	logger   *logrus.Entry
}

// NewManager creates a new PTY session manager.
func NewManager(logger *logrus.Entry) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		logger:   logger.WithField("component", "pty-manager"),
	}
}

// CreateRequest holds the parameters for creating a new PTY session.
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

// Create spawns a new shell in a PTY, registers it, and starts the read loop.
func (m *Manager) Create(req CreateRequest) (*Session, error) {
	id := uuid.New().String()

	name := req.Workspace
	if name == "" {
		name = filepath.Base(req.CWD)
	}

	var cmd *exec.Cmd
	if req.Command != "" {
		cmd = exec.Command(req.Command, req.Args...)
	} else {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.Command(shell)
	}
	cmd.Dir = req.CWD
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "GROVE_PTY=1")
	if len(req.Env) > 0 {
		cmd.Env = append(cmd.Env, req.Env...)
	}

	// NOTE: Do NOT set Setpgid here. creack/pty v1.1.20+ has a regression
	// where Setpgid combined with pty.Start causes EPERM on macOS.
	// The shell will still get its own session via the PTY's Setsid.

	rows := req.Rows
	cols := req.Cols
	if rows == 0 {
		rows = 40
	}
	if cols == 0 {
		cols = 120
	}

	ptmx, err := creackpty.StartWithSize(cmd, &creackpty.Winsize{
		Rows: rows,
		Cols: cols,
	})
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}

	sess := &Session{
		ID:        id,
		Workspace: name,
		CWD:       req.CWD,
		Labels:    req.Labels,
		Origin:    req.Origin,
		PanelID:   req.PanelID,
		Label:     req.Label,
		SessionID: req.SessionID,
		CreatedBy: req.CreatedBy,
		cmd:       cmd,
		ptmx:      ptmx,
		clients:   make(map[*websocket.Conn]bool),
		exitCh:    make(chan struct{}),
		logger:    m.logger.WithField("session", id),
		onExit:    m.remove,
	}
	sess.StartedAt = time.Now()

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	go sess.readLoop()

	m.logger.WithField("session", id).WithField("cwd", req.CWD).Info("PTY session created")
	return sess, nil
}

// Get returns a session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// List returns metadata for all active sessions, including live foreground
// process resolution via TIOCGPGRP.
func (m *Manager) List() []SessionMetadata {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]SessionMetadata, 0, len(m.sessions))
	for _, s := range m.sessions {
		md := s.Metadata()
		md.ForegroundProcess = s.ForegroundProcess()
		result = append(result, md)
	}
	return result
}

// Kill terminates a session by ID.
func (m *Manager) Kill(id string) error {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	return s.Kill()
}

// Shutdown kills all active PTY sessions. Called during daemon graceful shutdown.
func (m *Manager) Shutdown() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	for _, id := range ids {
		if err := m.Kill(id); err != nil {
			m.logger.WithField("session", id).WithError(err).Warn("Failed to kill PTY session during shutdown")
		}
	}
}

// remove is the onExit callback invoked by a Session when its process exits.
func (m *Manager) remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	m.logger.WithField("session", id).Debug("PTY session removed from registry")
}
