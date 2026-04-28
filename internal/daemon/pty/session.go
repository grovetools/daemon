// Package pty is a shim that wraps tuimux/pty with Grove-specific metadata fields.
package pty

import (
	"time"

	tuimuxpty "github.com/grovetools/tuimux/pty"
)

const historySize = 128 * 1024 // re-exported for any internal consumers

// Session wraps a tuimux PTY session with Grove-specific metadata.
type Session struct {
	*tuimuxpty.Session

	Workspace string `json:"workspace"`
	Origin    string `json:"origin,omitempty"`
	PanelID   string `json:"panel_id,omitempty"`
	Label     string `json:"label,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

// SessionMetadata is the Grove-specific API response type.
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

// ControlMessage is re-exported from tuimux/pty.
type ControlMessage = tuimuxpty.ControlMessage

// Metadata returns Grove-enriched session metadata.
func (s *Session) Metadata() SessionMetadata {
	inner := s.Session.Metadata()
	return SessionMetadata{
		ID:              inner.ID,
		Workspace:       s.Workspace,
		CWD:             inner.CWD,
		Labels:          inner.Tags,
		PID:             inner.PID,
		StartedAt:       inner.StartedAt,
		AttachedClients: inner.AttachedClients,
		Origin:          s.Origin,
		PanelID:         s.PanelID,
		Label:           s.Label,
		SessionID:       s.SessionID,
		CreatedBy:       s.CreatedBy,
	}
}
