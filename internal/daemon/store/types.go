// Package store provides the in-memory state store for the grove daemon.
package store

import (
	"sort"
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
	Plans     map[string][]*orchestration.Plan `json:"plans,omitempty"`
	PlanIndex *models.PlanIndexSnapshot        `json:"plan_index,omitempty"`

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

	// Subjobs is the durable materialized report lifecycle index.
	Subjobs map[string]*models.SubjobState `json:"subjobs,omitempty"`

	// Satellites holds the latest connection-health status per satellite,
	// keyed by registry name. Written by the satellite ConnManager
	// (daemon/internal/daemon/satellite) via UpdateSatelliteStatus and read by
	// P10's `grove status` line and the treemux badge (M2 contract C17). Only
	// the global daemon populates this (the ConnManager is constructed under
	// the scope=="" gate); scoped daemons leave it empty.
	Satellites map[string]*SatelliteStatusPayload `json:"satellites,omitempty"`

	// Assistant is the daemon-side assistant supervisor's latest status
	// (assistant-pane spec §3.3), written by the supervisor on every ensure
	// pass. It rides the state stream so the rail's assistant pane can render
	// "assistant stopped: <error>" instead of spinning on a placeholder — a
	// crash-looping assistant must degrade to a VISIBLE state, never a silent
	// loop. Nil until the supervisor publishes (or forever, on an ecosystem
	// with no [assistant] block).
	Assistant *models.AssistantStatus `json:"assistant,omitempty"`
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
	UpdateSessionEnd          UpdateType = "session_end"          // Mark session as completed/interrupted/failed/exited
	UpdateSessionVerdict      UpdateType = "session_verdict"      // Derived health verdict; never session activity
	UpdateSessionActivity     UpdateType = "session_activity"     // Monotonic real activity evidence (hook/transcript/pty)
	UpdateSessionsPruned      UpdateType = "sessions_pruned"      // Retention-bounded deletion of old terminal rows
	UpdateSessionTokens       UpdateType = "session_tokens"       // In-place live token/cost/context fields (daemon-computed)

	// Job lifecycle update types for the daemon's JobRunner.
	UpdateJobSubmitted   UpdateType = "job_submitted"
	UpdateJobStarted     UpdateType = "job_started"
	UpdateJobCompleted   UpdateType = "job_completed"
	UpdateJobFailed      UpdateType = "job_failed"
	UpdateJobCancelled   UpdateType = "job_cancelled"
	UpdateJobPendingUser UpdateType = "job_pending_user"
	// UpdateJobOrphaned marks a job the daemon lost track of across a restart:
	// no live agent process could be found and no exit was recorded. It is
	// deliberately NON-terminal — "the daemon cannot see this job" is a claim
	// about the daemon, not a verdict on the agent's work.
	UpdateJobOrphaned UpdateType = "job_orphaned"

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
	UpdatePlans             UpdateType = "plans"
	UpdatePlanIndexSnapshot UpdateType = "plan_index_snapshot"
	UpdatePlanIndexDelta    UpdateType = "plan_index"

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

	// Forge poller cache change (see store/forge.go). Payload: *ForgeStatePayload.
	UpdateForgeState UpdateType = "forge_state"

	// Satellite connection-health update — emitted by the satellite ConnManager
	// (daemon/internal/daemon/satellite) on every dial/keepalive/backoff state
	// transition. The store records the latest payload into State.Satellites and
	// passes it through convertToAPIUpdate so the treemux badge and (P10) the
	// `grove status` satellite line see it over SSE for free (M2 contract C17).
	// Payload: *SatelliteStatusPayload.
	UpdateSatelliteStatus UpdateType = "satellite_status"

	// Assistant supervisor status — emitted by the assistant supervisor after
	// every ensure pass (assistant-pane spec §3.3). Recorded into
	// State.Assistant and passed through to SSE subscribers so the rail pane
	// and `groved health` read one source of truth.
	// Payload: *models.AssistantStatus.
	UpdateAssistantStatus UpdateType = "assistant_status"

	// Satellite federation snapshot — the reconcile primitive the
	// SatelliteCollector emits on every (re)connect and debounced re-snapshot
	// (M2 contract C7/C16). ApplyUpdate deletes every job/session row whose
	// Origin equals the payload's Origin, then inserts the (already sanitized,
	// origin-stamped) snapshot rows — so a satellite that dropped a job has that
	// row removed on the next snapshot without ever touching other origins or
	// local rows. ApplyUpdate also diffs the old vs new job rows and broadcasts
	// synthesized per-job UpdateJobCompleted/Failed/Cancelled events for remote
	// terminal transitions (B1) — the snapshot is the ONLY federated change
	// signal, so without the diff the lease releaser and ntfy bridge would
	// never fire. Payload: *SatelliteSnapshotPayload.
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

	// Pi Flow subjob report lifecycle updates.
	UpdateSubjobReportReady UpdateType = "subjob_report_ready"
	UpdateSubjobJoined      UpdateType = "subjob_joined"

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

	// Tier-ordered git sweep progress. The boot/refresh/reconcile sweep runs
	// hot workspaces at full concurrency and trickles the cold tail over
	// minutes, so its position is a live fact rather than a completion
	// notification: these let `groved monitor` and a treemux Inspector page
	// render progress without polling /api/system/stats. Payload for all three
	// is *models.GitSweepProgress; none of them mutate state.
	UpdateSweepStarted   UpdateType = "sweep_started"
	UpdateSweepProgress  UpdateType = "sweep_progress"
	UpdateSweepCompleted UpdateType = "sweep_completed"
)

// allUpdateTypes is the canonical roster of the event vocabulary — every
// UpdateType constant declared above, in declaration order.
//
// It exists because "the switch statement is the spec" is exactly the problem
// this bus had: the SSE converter's allowlist, the `[[daemon.hooks.on_event]]`
// matcher and the config reference all need to agree on what an event name
// can be, and each of them inventing its own list is how they drift. A test
// (TestAllUpdateTypesCoversEveryConstant) AST-parses this file and fails if a
// constant is added without being listed here.
var allUpdateTypes = []UpdateType{
	UpdateWorkspaces, UpdateSessions, UpdateFocus, UpdateConfigReload,
	UpdateThemeChanged, UpdateSkillSync, UpdateWatcherStatus,
	UpdateSessionIntent, UpdateSessionConfirmation, UpdateSessionStatus,
	UpdateSessionEnd, UpdateSessionVerdict, UpdateSessionActivity, UpdateSessionsPruned, UpdateSessionTokens,
	UpdateJobSubmitted, UpdateJobStarted, UpdateJobCompleted, UpdateJobFailed,
	UpdateJobCancelled, UpdateJobPendingUser, UpdateJobOrphaned,
	UpdateJobsDiscovered,
	UpdateNoteEvent, UpdateNoteIndex, UpdateWorkspacesDelta,
	UpdateSessionChannels, UpdateSessionAutonomous, UpdateSessionPing,
	UpdateSessionTmuxTarget, UpdateSessionLastSender,
	UpdateNavBindings,
	UpdatePlans, UpdatePlanIndexSnapshot, UpdatePlanIndexDelta,
	UpdateMemoryIndex, UpdateMemoryReindex,
	UpdateTaskResult, UpdateTestReport,
	UpdateSpawnAgentPane, UpdateAttachAgentPane, UpdateAgentInput, UpdateCaptureRequest,
	UpdateSyncConflict, UpdateForgeState,
	UpdateSatelliteStatus, UpdateSatelliteSnapshot,
	UpdateWorkflowRunDiscovered, UpdateWorkflowAgentStarted,
	UpdateWorkflowAgentCompleted, UpdateWorkflowRunStale,
	UpdateWorkflowRunCompleted, UpdateWorkflowChildrenSnapshot,
	UpdateWorkflowBashStarted,
	UpdateSubjobReportReady, UpdateSubjobJoined,
	UpdateBuildQueued, UpdateBuildStarted, UpdateBuildFinished,
	UpdateBootPhase,
	UpdateSweepStarted, UpdateSweepProgress, UpdateSweepCompleted,
}

// AllUpdateTypes returns the event vocabulary, sorted, as a fresh slice.
// Consumers: the on_event hook matcher's config validation, and anything
// documenting or completing event names.
func AllUpdateTypes() []UpdateType {
	out := make([]UpdateType, len(allUpdateTypes))
	copy(out, allUpdateTypes)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsKnownUpdateType reports whether t names a declared update type.
func IsKnownUpdateType(t UpdateType) bool {
	for _, known := range allUpdateTypes {
		if known == t {
			return true
		}
	}
	return false
}

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
	// Kind is the event class. Not all of these are document conflicts — this
	// is the sync system's general "something needs a human" feed:
	// "secret_quarantine" | "conflict" | "oversize_skipped" | "diverged" |
	// "registry_foreign_write" | "registration" | "auth_failed". The last is transport-wide
	// rather than per-document, so it carries no notespace or path.
	Kind          string `json:"kind"`
	NotespaceID   string `json:"notespace_id,omitempty"`
	NotespaceName string `json:"notespace_name,omitempty"`
	Path          string `json:"path"` // slash-normalized notespace-relative path
	DocumentID    string `json:"document_id,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// SatelliteStatusPayload describes a satellite's connection health for SSE
// subscribers and State.Satellites. Emitted by the ConnManager on every state
// transition (M2 contract C17). State is one of "connected", "backoff",
// "disconnected", "exec-only" (a kind=exec satellite the ConnManager never
// dials). LastError carries the most recent dial/keepalive failure
// (empty when connected). Since marks when the current State was entered.
// Forward describes the daemon-owned local sync forward when the satellite
// has sync_local_port configured (e.g. "active on 127.0.0.1:8788" or
// "port busy on 127.0.0.1:8788: <err>"); empty when the feature is off.
type SatelliteStatusPayload struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Addr      string    `json:"addr,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Forward   string    `json:"forward,omitempty"`
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
	AttemptID   string `json:"attempt_id,omitempty"`
	ParentJobID string `json:"parent_job_id,omitempty"`
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

	// Type is the session shape being registered ("interactive_agent" or
	// "headless_agent"); see daemon.SessionIntent.Type. Empty means
	// "interactive_agent" — what every launcher meant before the field existed.
	Type string `json:"type,omitempty"`
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
	AttemptID      string `json:"attempt_id,omitempty"`
	NativeID       string `json:"native_id"`
	PID            int    `json:"pid"`
	TranscriptPath string `json:"transcript_path"`
}

// SessionStatusPayload contains data for updating a session's status.
type SessionStatusPayload struct {
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id,omitempty"`
	Status    string `json:"status"` // "running", "idle", "pending_user"
}

// SessionEndPayload contains data for ending a session.
type SessionEndPayload struct {
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id,omitempty"`
	Outcome   string `json:"outcome"`          // "completed", "interrupted", "failed", "exited" (neutral; job gate remains authoritative)
	Reason    string `json:"reason,omitempty"` // lifecycle evidence, e.g. process_dead or api_kill
}

// SessionVerdictPayload carries the daemon's derived health classification.
// Applying it changes only Session.Verified; it must never renew LastActivity.
type SessionVerdictPayload struct {
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id,omitempty"`
	Verified  string `json:"verified"` // alive|unverified|stale
}

// SessionActivityPayload carries genuine observed activity. ObservedAt is
// applied monotonically and Source is a closed vocabulary; idle pings, token
// recomputation, focus, and attachment are intentionally not accepted.
type SessionActivityPayload struct {
	JobID             string    `json:"job_id"`
	AttemptID         string    `json:"attempt_id,omitempty"`
	ExpectedStartedAt time.Time `json:"expected_started_at,omitempty"` // legacy Phase-4 optimistic guard
	ObservedAt        time.Time `json:"observed_at"`
	Source            string    `json:"source"` // hook|transcript|pty
}

// SessionsPrunedPayload announces retention-based removal of terminal rows.
// IDs are included for audit/debug consumers; active rows are never eligible.
type SessionsPrunedPayload struct {
	IDs    []string  `json:"ids"`
	Before time.Time `json:"before"`
}

// SessionTokenUpdate carries daemon-computed live token usage for one session.
// The fields mirror the derived models.Session token fields.
type SessionTokenUpdate struct {
	JobID       string  `json:"job_id"`
	AttemptID   string  `json:"attempt_id,omitempty"`
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
	Type UpdateType
	// Seq is the monotonic sequence number the store stamps on every update
	// as it is published. It starts at 1 for the first update of a daemon
	// process and never repeats within that process; it RESETS across daemon
	// restarts (the bus is in-memory), so a client holding a cursor across a
	// restart sees a lower Current than its Since and must reconcile — see
	// Store.Replay. Zero means "not published yet" (or, on the wire, "this
	// daemon predates sequencing").
	Seq     uint64
	Source  string // Which collector sent this update (e.g., "git", "workspace", "session", "plan", "note")
	Scanned int    // Number of items actually scanned (for focused updates)
	// Origin scopes a wholesale-replacement update to one satellite's rows (M2
	// contract C7). Empty == local: a local snapshot (SessionCollector) must not
	// evict federated rows, and a remote-origin snapshot must not evict locals.
	// Only UpdateSessions consults it today; other update types ignore it.
	Origin  string
	Payload interface{}
}
