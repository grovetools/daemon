// Package store provides the in-memory state store for the grove daemon.
package store

import (
	"github.com/grovetools/core/pkg/models"
)

// State represents the complete world view of the daemon.
type State struct {
	Workspaces map[string]*models.EnrichedWorkspace `json:"workspaces"` // Keyed by path
	Sessions   map[string]*models.Session           `json:"sessions"`   // Keyed by ID
}

// UpdateType defines what kind of data changed.
type UpdateType string

const (
	UpdateWorkspaces    UpdateType = "workspaces"
	UpdateSessions      UpdateType = "sessions"
	UpdateFocus         UpdateType = "focus"
	UpdateConfigReload  UpdateType = "config_reload"
	UpdateSkillSync     UpdateType = "skill_sync"
)

// SkillSyncPayload contains data broadcasted after a skill sync operation
type SkillSyncPayload struct {
	Workspace    string   `json:"workspace"`
	SyncedSkills []string `json:"synced_skills"`
	DestPaths    []string `json:"dest_paths"`
	Error        string   `json:"error,omitempty"`
}

// Update represents a change to the state.
type Update struct {
	Type    UpdateType
	Source  string      // Which collector sent this update (e.g., "git", "workspace", "session", "plan", "note")
	Scanned int         // Number of items actually scanned (for focused updates)
	Payload interface{}
}
