// Package store provides the in-memory state store for the grove daemon.
package store

import (
	"github.com/grovetools/core/pkg/models"
)

// State represents the complete world view of the daemon.
type State struct {
	Workspaces map[string]*models.EnrichedWorkspace `json:"workspaces"` // Keyed by path
	Sessions   map[string]*models.Session           `json:"sessions"`   // Keyed by ID
	Jobs       map[string]*models.JobInfo           `json:"jobs"`       // Keyed by job ID
}

// UpdateType defines what kind of data changed.
type UpdateType string

const (
	UpdateWorkspaces    UpdateType = "workspaces"
	UpdateSessions      UpdateType = "sessions"
	UpdateFocus         UpdateType = "focus"
	UpdateConfigReload  UpdateType = "config_reload"
	UpdateSkillSync     UpdateType = "skill_sync"
	UpdateWatcherStatus UpdateType = "watcher_status"

	// Session lifecycle update types for the consolidated session tracking system.
	// These enable race-free session management by the daemon.
	UpdateSessionIntent       UpdateType = "session_intent"       // Pre-register session before agent launch
	UpdateSessionConfirmation UpdateType = "session_confirmation" // Link intent with actual PID
	UpdateSessionStatus       UpdateType = "session_status"       // Update session status (running/idle/pending_user)
	UpdateSessionEnd          UpdateType = "session_end"          // Mark session as completed/interrupted/failed

	// Job lifecycle update types for the daemon's JobRunner.
	UpdateJobSubmitted UpdateType = "job_submitted"
	UpdateJobStarted   UpdateType = "job_started"
	UpdateJobCompleted UpdateType = "job_completed"
	UpdateJobFailed    UpdateType = "job_failed"
	UpdateJobCancelled   UpdateType = "job_cancelled"
	UpdateJobPendingUser UpdateType = "job_pending_user"

	// Bulk discovery of idle jobs from filesystem scanning.
	UpdateJobsDiscovered UpdateType = "jobs_discovered"
)

// SkillSyncPayload contains data broadcasted after a skill sync operation
type SkillSyncPayload struct {
	Workspace    string   `json:"workspace"`
	SyncedSkills []string `json:"synced_skills"`
	DestPaths    []string `json:"dest_paths"`
	Error        string   `json:"error,omitempty"`
}

// SessionIntentPayload contains data for pre-registering a session.
type SessionIntentPayload struct {
	JobID       string `json:"job_id"`
	Provider    string `json:"provider"`
	JobFilePath string `json:"job_file_path"`
	PlanName    string `json:"plan_name"`
	Title       string `json:"title"`
	WorkDir     string `json:"work_dir"`
}

// SessionConfirmationPayload contains data for confirming a session after agent startup.
type SessionConfirmationPayload struct {
	JobID          string `json:"job_id"`
	NativeID       string `json:"native_id"`
	PID            int    `json:"pid"`
	TranscriptPath string `json:"transcript_path"`
}

// SessionStatusPayload contains data for updating a session's status.
type SessionStatusPayload struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"` // "running", "idle", "pending_user"
}

// SessionEndPayload contains data for ending a session.
type SessionEndPayload struct {
	JobID   string `json:"job_id"`
	Outcome string `json:"outcome"` // "completed", "interrupted", "failed"
}

// Update represents a change to the state.
type Update struct {
	Type    UpdateType
	Source  string      // Which collector sent this update (e.g., "git", "workspace", "session", "plan", "note")
	Scanned int         // Number of items actually scanned (for focused updates)
	Payload interface{}
}
