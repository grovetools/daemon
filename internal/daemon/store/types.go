// Package store provides the in-memory state store for the grove daemon.
package store

import (
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/flow/pkg/orchestration"
)

// State represents the complete world view of the daemon.
type State struct {
	Workspaces  map[string]*models.EnrichedWorkspace `json:"workspaces"`             // Keyed by path
	Sessions    map[string]*models.Session           `json:"sessions"`               // Keyed by ID
	Jobs        map[string]*models.JobInfo           `json:"jobs"`                   // Keyed by job ID
	NoteIndex   map[string]*models.NoteIndexEntry    `json:"note_index,omitempty"`   // Keyed by file path
	NavBindings *models.NavSessionsFile              `json:"nav_bindings,omitempty"` // Nav key binding state
	// Plans caches fully-parsed plan directories keyed by their containing
	// plansDir. Populated by the flow watcher so TUI clients can fetch
	// plan lists over the socket instead of hammering the filesystem
	// every tick.
	Plans map[string][]*orchestration.Plan `json:"plans,omitempty"`

	// WorkflowRuns aggregates subagent/workflow lifecycle events into
	// per-run snapshots, keyed by workflow run ID (the wf_* directory
	// name). Fed by hook-forwarded events (POST /api/workflows/event) and
	// the daemon's journal watcher; deduped by (RunID, AgentID).
	WorkflowRuns map[string]*models.WorkflowRunState `json:"workflow_runs,omitempty"`

	// AdhocSubagents holds run-less subagent records: ad-hoc Agent-tool
	// spawns (which never get a workflow run) and workflow agents whose
	// run attribution hasn't arrived yet (SubagentStart carries no run
	// id; the journal supplies it later, at which point the record
	// migrates into WorkflowRuns). Keyed by session key (job ID, falling
	// back to claude session ID), then agent ID.
	AdhocSubagents map[string]map[string]*models.Subagent `json:"adhoc_subagents,omitempty"`

	// Satellites holds the latest connection-health status per satellite,
	// keyed by registry name. Written by the satellite ConnManager
	// (daemon/internal/daemon/satellite) via UpdateSatelliteStatus and read by
	// P10's `grove status` line and the treemux badge (M2 contract C17). Only
	// the global daemon populates this (the ConnManager is constructed under
	// the scope=="" gate); scoped daemons leave it empty.
	Satellites map[string]*SatelliteStatusPayload `json:"satellites,omitempty"`
}

// UpdateType defines what kind of data changed.
type UpdateType string

const (
	UpdateWorkspaces    UpdateType = "workspaces"
	UpdateSessions      UpdateType = "sessions"
	UpdateFocus         UpdateType = "focus"
	UpdateConfigReload  UpdateType = "config_reload"
	UpdateThemeChanged  UpdateType = "theme_changed"
	UpdateSkillSync     UpdateType = "skill_sync"
	UpdateWatcherStatus UpdateType = "watcher_status"

	// Session lifecycle update types for the consolidated session tracking system.
	// These enable race-free session management by the daemon.
	UpdateSessionIntent       UpdateType = "session_intent"       // Pre-register session before agent launch
	UpdateSessionConfirmation UpdateType = "session_confirmation" // Link intent with actual PID
	UpdateSessionStatus       UpdateType = "session_status"       // Update session status (running/idle/pending_user)
	UpdateSessionEnd          UpdateType = "session_end"          // Mark session as completed/interrupted/failed
	UpdateSessionTokens       UpdateType = "session_tokens"       // In-place live token/cost/context fields (daemon-computed)

	// Job lifecycle update types for the daemon's JobRunner.
	UpdateJobSubmitted   UpdateType = "job_submitted"
	UpdateJobStarted     UpdateType = "job_started"
	UpdateJobCompleted   UpdateType = "job_completed"
	UpdateJobFailed      UpdateType = "job_failed"
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
	UpdateSessionChannels   UpdateType = "session_channels"    // Update channels for a session
	UpdateSessionAutonomous UpdateType = "session_autonomous"  // Update autonomous config for a session
	UpdateSessionPing       UpdateType = "session_ping"        // Record idle ping timestamp
	UpdateSessionTmuxTarget UpdateType = "session_tmux_target" // Update tmux target after detach/attach
	UpdateSessionLastSender UpdateType = "session_last_sender" // Track last Signal sender for routing

	// Nav bindings update — full replacement of nav binding state.
	UpdateNavBindings UpdateType = "nav_bindings"

	// Plans update — full replacement of the cached plan list for one or
	// more plansDir keys. Payload is map[string][]*orchestration.Plan.
	UpdatePlans UpdateType = "plans"

	// Memory index mutation — broadcast after the memory watcher upserts or
	// deletes a document. Payload is MemoryIndexPayload. The TUI uses this
	// to render a transient [Index Syncing…] indicator.
	UpdateMemoryIndex UpdateType = "memory_index"

	// Memory reindex trigger — the HTTP endpoint emits this so the
	// MemoryHandler watcher can pick it up in HandleStoreUpdate and
	// queue the requested re-indexing work asynchronously.
	UpdateMemoryReindex UpdateType = "memory_reindex" // Payload: *MemoryReindexPayload

	// Task result reporting — stores the outcome of developer hygiene tasks
	// (build, check, fmt, lint) per workspace, enabling cache-based skipping.
	UpdateTaskResult UpdateType = "task_result" // Payload: *TaskResultPayload
	UpdateTestReport UpdateType = "test_report" // Payload: *TestReportPayload

	// Native agent pane lifecycle — these are pass-through events that the
	// daemon relays from Flow (or the HTTP API) to groveterm via SSE.
	// The daemon does NOT apply them to its own state; they exist purely
	// as a control-plane channel between Flow and the terminal.
	UpdateSpawnAgentPane  UpdateType = "spawn_agent_pane"  // Payload: *SpawnAgentPayload
	UpdateAttachAgentPane UpdateType = "attach_agent_pane" // Payload: *AttachAgentPayload
	UpdateAgentInput      UpdateType = "agent_input"       // Payload: *AgentInputPayload
	UpdateCaptureRequest  UpdateType = "capture_request"   // Payload: *CaptureRequestPayload

	// Sync lifecycle update — emitted by the SyncHandler watcher when a
	// local document is quarantined (secret heuristics) or, in Phase 1+,
	// when a pulled change conflicts with local edits.
	// Payload: *SyncConflictPayload.
	UpdateSyncConflict UpdateType = "sync_conflict"

	// Satellite connection-health update — emitted by the satellite ConnManager
	// (daemon/internal/daemon/satellite) on every dial/keepalive/backoff state
	// transition. The store records the latest payload into State.Satellites and
	// passes it through convertToAPIUpdate so the treemux badge and (P10) the
	// `grove status` satellite line see it over SSE for free (M2 contract C17).
	// Payload: *SatelliteStatusPayload.
	UpdateSatelliteStatus UpdateType = "satellite_status"

	// Satellite federation snapshot — the reconcile primitive the
	// SatelliteCollector emits on every (re)connect and debounced re-snapshot
	// (M2 contract C7/C16). ApplyUpdate deletes every job/session row whose
	// Origin equals the payload's Origin, then inserts the (already sanitized,
	// origin-stamped) snapshot rows — so a satellite that dropped a job has that
	// row removed on the next snapshot without ever touching other origins or
	// local rows. Payload: *SatelliteSnapshotPayload.
	UpdateSatelliteSnapshot UpdateType = "satellite_snapshot"

	// Workflow/subagent lifecycle update types. Each maps to a DISTINCT
	// SSE update_type string in convertToAPIUpdate (mirroring the job_*
	// lifecycle pattern, never the collapsed "session" pattern) — a
	// missed wire layer makes events silently invisible to consumers.
	// Payload: *WorkflowEventPayload.
	UpdateWorkflowRunDiscovered  UpdateType = "workflow_run_discovered"
	UpdateWorkflowAgentStarted   UpdateType = "workflow_agent_started"
	UpdateWorkflowAgentCompleted UpdateType = "workflow_agent_completed"
	UpdateWorkflowRunStale       UpdateType = "workflow_run_stale"
	UpdateWorkflowRunCompleted   UpdateType = "workflow_run_completed"
	// UpdateWorkflowChildrenSnapshot carries a live-background-child count
	// (WorkflowChildrenSnapshot) that the store writes onto the owning
	// session's LiveChildren field. Unlike the other workflow_* types it
	// mints no run/agent rows and is never persisted (no boot replay).
	UpdateWorkflowChildrenSnapshot UpdateType = "workflow_children_snapshot"
	// UpdateWorkflowBashStarted announces a background bash spawn
	// (WorkflowBashStarted) that the store records as a live bash child (F6).
	// Like the snapshot it mints no run rows and is never persisted — bash
	// liveness is ephemeral and TTL-bounded.
	UpdateWorkflowBashStarted UpdateType = "workflow_bash_started"

	// Build queue lifecycle update types for the daemon's machine-wide
	// build scheduler (buildqueue). Each maps to a DISTINCT SSE
	// update_type string in convertToAPIUpdate (same wire rule as the
	// workflow_* types above). Per-job build OUTPUT never goes through
	// the store broadcast — it streams over the dedicated per-job SSE
	// endpoint GET /api/build/jobs/{id}/stream.
	// Payload: *BuildEventPayload.
	UpdateBuildQueued   UpdateType = "build_queued"
	UpdateBuildStarted  UpdateType = "build_started"
	UpdateBuildFinished UpdateType = "build_finished"

	// Daemon boot progress. Broadcast by the early-bind boot goroutine at
	// each phase boundary so connected clients (treemux's cold-start splash)
	// can render a progress bar while the daemon finishes booting. Payload is
	// *daemon.BootStatus (core/pkg/daemon); the terminal event carries
	// Done=true. Only fires under --ready-at=bind; the default bind-last
	// ordering never broadcasts because no client can connect until boot ends.
	UpdateBootPhase UpdateType = "boot_phase"
)

// MemoryIndexPayload describes a single memory store mutation for SSE subscribers.
type MemoryIndexPayload struct {
	Op   string `json:"op"`   // "upsert" | "delete"
	Path string `json:"path"` // File path that was indexed / removed
}

// MemoryReindexPayload describes a reindex request triggered via the HTTP API.
type MemoryReindexPayload struct {
	Mode   string `json:"mode"`             // "stale", "all", "path"
	Target string `json:"target,omitempty"` // File path (only for mode "path")
}

// SyncConflictPayload describes a sync conflict or quarantine event for SSE
// subscribers. In Phase 0 only secret quarantine fires; pull conflicts
// arrive with the Phase 1 sync server.
type SyncConflictPayload struct {
	Kind       string `json:"kind"` // "secret_quarantine" | "conflict" | "oversize_skipped" | "diverged"
	Workspace  string `json:"workspace"`
	Path       string `json:"path"` // slash-normalized workspace-relative path
	DocumentID string `json:"document_id,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// SatelliteStatusPayload describes a satellite's connection health for SSE
// subscribers and State.Satellites. Emitted by the ConnManager on every state
// transition (M2 contract C17). State is one of "connected", "backoff",
// "disconnected". LastError carries the most recent dial/keepalive failure
// (empty when connected). Since marks when the current State was entered.
type SatelliteStatusPayload struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Addr      string    `json:"addr,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Since     time.Time `json:"since"`
}

// SatelliteSnapshotPayload carries a full jobs+sessions snapshot for a single
// satellite origin (M2 contract C7). Origin is the satellite's registry name
// (already stamped onto every row by the collector via SanitizeJobInfo/
// SanitizeSession). ApplyUpdate treats it as an origin-scoped replacement:
// remove that origin's rows, insert these. It never touches locals or other
// origins, which is what makes reconnect-reconcile safe (C16).
type SatelliteSnapshotPayload struct {
	Origin   string            `json:"origin"`
	Jobs     []*models.JobInfo `json:"jobs"`
	Sessions []*models.Session `json:"sessions"`
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
	Channels     []string                 `json:"channels,omitempty"`
	Autonomous   *models.AutonomousConfig `json:"autonomous,omitempty"`
	TmuxTarget   string                   `json:"tmux_target,omitempty"`
	SignalTarget string                   `json:"signal_target,omitempty"`

	// Mux identifies the multiplexer backing the session's PTY.
	Mux string `json:"mux,omitempty"`
}

// SessionChannelsPayload contains data for updating session channels.
type SessionChannelsPayload struct {
	JobID        string   `json:"job_id"`
	Channels     []string `json:"channels"`
	SignalTarget string   `json:"signal_target,omitempty"`
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
	JobID           string `json:"job_id"`
	LastSender      string `json:"last_sender"`
	LastSenderGroup string `json:"last_sender_group"`
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

// SessionTokenUpdate carries daemon-computed live token usage for one session.
// The fields mirror the derived models.Session token fields.
type SessionTokenUpdate struct {
	JobID       string  `json:"job_id"`
	LiveTokens  int64   `json:"live_tokens"`
	LiveCostUSD float64 `json:"live_cost_usd"`
	ContextSize int64   `json:"context_size"`
	Model       string  `json:"model,omitempty"`
}

// SessionTokensPayload batches live token updates for one or more sessions.
// Applied in-place so it never clobbers concurrent session lifecycle mutations
// (a full-set UpdateSessions replace, built from a snapshot, could drop a
// session added/removed by another goroutine between read and apply).
type SessionTokensPayload struct {
	Updates []SessionTokenUpdate `json:"updates"`
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

// BuildEventPayload describes a build queue lifecycle transition for SSE
// subscribers. Output lines are NOT included — they stream over the
// dedicated per-job endpoint.
type BuildEventPayload struct {
	JobID      string `json:"job_id"`
	GroupID    string `json:"group_id"`
	Workspace  string `json:"workspace"`
	Dir        string `json:"dir"`
	Verb       string `json:"verb"`
	Status     string `json:"status"` // "queued", "running", "succeeded", "failed", "cancelled"
	ExitCode   int    `json:"exit_code,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// TaskResultPayload contains data for reporting a task execution result.
type TaskResultPayload struct {
	Workspace string             `json:"workspace"`
	Verb      string             `json:"verb"`
	Result    *models.TaskResult `json:"result"`
}

// TestReportPayload contains data for reporting structured test results.
type TestReportPayload struct {
	Workspace string             `json:"workspace"`
	Report    *models.TestReport `json:"report"`
}

// Update represents a change to the state.
type Update struct {
	Type    UpdateType
	Source  string // Which collector sent this update (e.g., "git", "workspace", "session", "plan", "note")
	Scanned int    // Number of items actually scanned (for focused updates)
	// Origin scopes a wholesale-replacement update to one satellite's rows (M2
	// contract C7). Empty == local: a local snapshot (SessionCollector) must not
	// evict federated rows, and a remote-origin snapshot must not evict locals.
	// Only UpdateSessions consults it today; other update types ignore it.
	Origin  string
	Payload interface{}
}
