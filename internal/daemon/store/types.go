// Package store provides the in-memory state store for the grove daemon.
package store

import (
	"github.com/grovetools/core/pkg/models"
)

// State represents the complete world view of the daemon.
type State struct {
	Workspaces  map[string]*models.EnrichedWorkspace `json:"workspaces"`              // Keyed by path
	Sessions    map[string]*models.Session           `json:"sessions"`                // Keyed by ID
	Jobs        map[string]*models.JobInfo           `json:"jobs"`                    // Keyed by job ID
	NoteIndex   map[string]*models.NoteIndexEntry    `json:"note_index,omitempty"`    // Keyed by file path
	NavBindings *models.NavSessionsFile              `json:"nav_bindings,omitempty"`  // Nav key binding state
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

	// Note mutation event from nb for incremental count updates.
	UpdateNoteEvent UpdateType = "note_event"

	// Full note index replacement from note collector scan.
	UpdateNoteIndex UpdateType = "note_index"

	// Delta update for workspace enrichment fields (git, plan, note).
	// Payload is []*models.WorkspaceDelta — only changed fields on changed workspaces.
	UpdateWorkspacesDelta UpdateType = "workspaces_delta"

	// Channel & Autonomous update types.
	UpdateSessionChannels   UpdateType = "session_channels"   // Update channels for a session
	UpdateSessionAutonomous UpdateType = "session_autonomous" // Update autonomous config for a session
	UpdateSessionPing       UpdateType = "session_ping"       // Record idle ping timestamp
	UpdateSessionTmuxTarget UpdateType = "session_tmux_target" // Update tmux target after detach/attach
	UpdateSessionLastSender UpdateType = "session_last_sender" // Track last Signal sender for routing

	// Nav bindings update — full replacement of nav binding state.
	UpdateNavBindings UpdateType = "nav_bindings"

	// Memory index mutation — broadcast after the memory watcher upserts or
	// deletes a document. Payload is MemoryIndexPayload. The TUI uses this
	// to render a transient [Index Syncing…] indicator.
	UpdateMemoryIndex UpdateType = "memory_index"

	// Native agent pane lifecycle — these are pass-through events that the
	// daemon relays from Flow (or the HTTP API) to groveterm via SSE.
	// The daemon does NOT apply them to its own state; they exist purely
	// as a control-plane channel between Flow and the terminal.
	UpdateSpawnAgentPane  UpdateType = "spawn_agent_pane"  // Payload: *SpawnAgentPayload
	UpdateAttachAgentPane UpdateType = "attach_agent_pane" // Payload: *AttachAgentPayload
	UpdateAgentInput      UpdateType = "agent_input"       // Payload: *AgentInputPayload
	UpdateCaptureRequest  UpdateType = "capture_request"   // Payload: *CaptureRequestPayload
)

// MemoryIndexPayload describes a single memory store mutation for SSE subscribers.
type MemoryIndexPayload struct {
	Op   string `json:"op"`   // "upsert" | "delete"
	Path string `json:"path"` // File path that was indexed / removed
}

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

	// Channel & Autonomous support
	Channels   []string                 `json:"channels,omitempty"`
	Autonomous *models.AutonomousConfig `json:"autonomous,omitempty"`
	TmuxTarget string                   `json:"tmux_target,omitempty"`
}

// SessionChannelsPayload contains data for updating session channels.
type SessionChannelsPayload struct {
	JobID    string   `json:"job_id"`
	Channels []string `json:"channels"`
}

// SessionAutonomousPayload contains data for updating session autonomous config.
type SessionAutonomousPayload struct {
	JobID      string                   `json:"job_id"`
	Autonomous *models.AutonomousConfig `json:"autonomous"`
}

// SessionPingPayload records an idle ping timestamp.
type SessionPingPayload struct {
	JobID string `json:"job_id"`
}

// SessionTmuxTargetPayload contains data for updating a session's tmux target.
type SessionTmuxTargetPayload struct {
	JobID      string `json:"job_id"`
	TmuxTarget string `json:"tmux_target"`
}

// SessionLastSenderPayload tracks the last Signal sender for a session.
type SessionLastSenderPayload struct {
	JobID      string `json:"job_id"`
	LastSender string `json:"last_sender"`
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

// SpawnAgentPayload requests groveterm to spawn a native agent pane.
type SpawnAgentPayload struct {
	JobID     string            `json:"job_id"`
	PlanName  string            `json:"plan_name"`
	JobTitle  string            `json:"job_title"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	WorkDir   string            `json:"work_dir"`
	Env       map[string]string `json:"env,omitempty"`
	AutoSplit bool              `json:"auto_split"`
}

// AttachAgentPayload tells groveterm to attach to a daemon-owned agent PTY.
type AttachAgentPayload struct {
	JobID     string            `json:"job_id"`
	PlanName  string            `json:"plan_name"`
	JobTitle  string            `json:"job_title"`
	PtyID     string            `json:"pty_id"`
	WorkDir   string            `json:"work_dir"`
	Env       map[string]string `json:"env,omitempty"`
	AutoSplit bool              `json:"auto_split"`
}

// AgentInputPayload delivers input text to a native agent pane.
type AgentInputPayload struct {
	JobID string `json:"job_id"`
	Input string `json:"input"`
}

// CaptureRequestPayload requests a screen capture from a native agent pane.
type CaptureRequestPayload struct {
	JobID string `json:"job_id"`
}

// Update represents a change to the state.
type Update struct {
	Type    UpdateType
	Source  string      // Which collector sent this update (e.g., "git", "workspace", "session", "plan", "note")
	Scanned int         // Number of items actually scanned (for focused updates)
	Payload interface{}
}
