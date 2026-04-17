// Package pty manages daemon-owned PTY sessions that survive client disconnects.
package pty

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/grovetools/core/logging"
	"golang.org/x/sys/unix"
)

// historySize is the maximum number of bytes retained in the per-session
// circular history buffer. New WS clients receive this on attach so they
// can reconstruct recent terminal state.
const historySize = 128 * 1024 // 128 KB

// Session represents a single daemon-owned PTY session.
type Session struct {
	ID        string            `json:"id"`
	Workspace string            `json:"workspace"`
	CWD       string            `json:"cwd"`
	Labels    map[string]string `json:"labels,omitempty"`
	StartedAt time.Time         `json:"started_at"`
	Origin    string            `json:"origin,omitempty"`
	PanelID   string            `json:"panel_id,omitempty"`
	Label     string            `json:"label,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	CreatedBy string            `json:"created_by,omitempty"`

	cmd  *exec.Cmd
	ptmx *os.File

	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
	exited  bool
	exitCh  chan struct{} // closed when the process exits

	// history is a bounded circular buffer of raw PTY output bytes.
	// New WS clients receive the contents on attach so they can
	// reconstruct recent terminal state (scrollback + SIGWINCH redraw).
	historyMu sync.RWMutex
	history   []byte

	ulog   *logging.UnifiedLogger
	onExit func(id string) // callback to manager for cleanup
}

// SessionMetadata is the safe, serializable subset of Session for API responses.
type SessionMetadata struct {
	ID                string            `json:"id"`
	Workspace         string            `json:"workspace"`
	CWD               string            `json:"cwd"`
	Labels            map[string]string `json:"labels,omitempty"`
	PID               int               `json:"pid"`
	StartedAt         time.Time         `json:"started_at"`
	AttachedClients   int               `json:"attached_clients"`
	Origin            string            `json:"origin,omitempty"`
	PanelID           string            `json:"panel_id,omitempty"`
	Label             string            `json:"label,omitempty"`
	SessionID         string            `json:"session_id,omitempty"`
	CreatedBy         string            `json:"created_by,omitempty"`
	ForegroundProcess string            `json:"foreground_process,omitempty"`
}

// ControlMessage is the JSON envelope for text-frame control messages.
type ControlMessage struct {
	Type string `json:"type"` // "resize", "exit"
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Code int    `json:"code,omitempty"`
}

// Metadata returns a safe copy of session metadata.
func (s *Session) Metadata() SessionMetadata {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pid := 0
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}

	return SessionMetadata{
		ID:              s.ID,
		Workspace:       s.Workspace,
		CWD:             s.CWD,
		Labels:          s.Labels,
		PID:             pid,
		StartedAt:       s.StartedAt,
		AttachedClients: len(s.clients),
		Origin:          s.Origin,
		PanelID:         s.PanelID,
		Label:           s.Label,
		SessionID:       s.SessionID,
		CreatedBy:       s.CreatedBy,
	}
}

// ForegroundProcess returns the name of the foreground process running in
// this PTY session, resolved via TIOCGPGRP + ps. Returns empty string on error.
func (s *Session) ForegroundProcess() string {
	if s.ptmx == nil {
		return ""
	}
	pgid, err := unix.IoctlGetInt(int(s.ptmx.Fd()), unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pgid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	comm := strings.TrimSpace(string(out))
	if idx := strings.LastIndex(comm, "/"); idx >= 0 {
		comm = comm[idx+1:]
	}
	return comm
}

// Write sends raw bytes to the PTY master (client → shell).
func (s *Session) Write(data []byte) (int, error) {
	return s.ptmx.Write(data)
}

// Resize sets the PTY window size. Last caller wins.
func (s *Session) Resize(rows, cols uint16) error {
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Kill sends SIGHUP to the child process group, which naturally terminates
// the read loop and triggers cleanup.
func (s *Session) Kill() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	// Send to the process group so child processes also receive the signal.
	return syscall.Kill(-s.cmd.Process.Pid, syscall.SIGHUP)
}

// AddClient registers a WebSocket connection for output broadcast.
func (s *Session) AddClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[conn] = true
	s.ulog.Debug("Client attached").
		Field("session", s.ID).
		Field("clients", len(s.clients)).
		Log(context.Background())
}

// RemoveClient deregisters a WebSocket connection.
func (s *Session) RemoveClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, conn)
	s.ulog.Debug("Client detached").
		Field("session", s.ID).
		Field("clients", len(s.clients)).
		Log(context.Background())
}

// Exited returns true if the child process has exited.
func (s *Session) Exited() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exited
}

// ExitCh returns a channel that is closed when the session exits.
func (s *Session) ExitCh() <-chan struct{} {
	return s.exitCh
}

// readLoop continuously reads from the PTY master and broadcasts to all
// connected WebSocket clients as binary frames. When the read returns an
// error (child exit / fd closed), it broadcasts an exit control message
// and invokes the onExit callback.
func (s *Session) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.appendHistory(buf[:n])
			s.broadcast(buf[:n])
		}
		if err != nil {
			break
		}
	}

	// Wait for the child to fully exit so we can capture the exit code.
	exitCode := 0
	if err := s.cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	s.mu.Lock()
	s.exited = true
	s.mu.Unlock()
	close(s.exitCh)

	// Broadcast exit notification to all attached clients.
	exitMsg := ControlMessage{Type: "exit", Code: exitCode}
	data, _ := json.Marshal(exitMsg)
	s.mu.RLock()
	for conn := range s.clients {
		_ = conn.WriteMessage(websocket.TextMessage, data)
	}
	s.mu.RUnlock()

	s.ulog.Info("PTY session exited").
		Field("session", s.ID).
		Field("exit_code", exitCode).
		Log(context.Background())

	if s.onExit != nil {
		s.onExit(s.ID)
	}
}

// appendHistory appends raw PTY output to the bounded circular history buffer.
func (s *Session) appendHistory(data []byte) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.history = append(s.history, data...)
	if len(s.history) > historySize {
		// Keep the most recent historySize bytes.
		s.history = s.history[len(s.history)-historySize:]
	}
}

// History returns a copy of the current history buffer contents.
func (s *Session) History() []byte {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.history) == 0 {
		return nil
	}
	out := make([]byte, len(s.history))
	copy(out, s.history)
	return out
}

// broadcast sends raw PTY output to all connected clients as binary WS frames.
func (s *Session) broadcast(data []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for conn := range s.clients {
		if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
			s.ulog.Debug("Failed to write to PTY client").
				Err(err).
				Field("session", s.ID).
				Log(context.Background())
		}
	}
}
