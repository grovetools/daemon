// Package jobrunner provides adoption logic for recovering orphaned agent processes.
// PHASE 2: When the daemon restarts, it scans for agents that were running and adopts them.
package jobrunner

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	tuimux "github.com/grovetools/tuimux/api/client"
	tuimuxpty "github.com/grovetools/tuimux/pty"
)

// statusFileContent represents the JSON structure written to .status files by agents.
type statusFileContent struct {
	ExitCode  int    `json:"exit_code"`
	Timestamp string `json:"timestamp"`
	JobID     string `json:"job_id"`
}

// AdoptRunningAgents scans persisted job state for "running" agents and attempts to
// adopt those that are still alive. For each running job, it:
// 1. Checks if the PID is still alive via kill(pid, 0)
// 2. If alive, spawns a poller to detect when it exits
// 3. If dead or no .status file, marks it as failed
func (jr *JobRunner) AdoptRunningAgents(ctx context.Context) {
	if jr.persister == nil {
		return
	}

	// Load all persisted jobs
	jobs := jr.persister.Load()
	adopted := 0
	failed := 0

	// Health-gate the out-of-process PTY list BEFORE any fail decision. PTYs
	// now live in the standalone tuimux daemon; an adopted interactive job is
	// only truly alive if its PtyID still maps to a live PTY there. But a
	// transient/too-early query that returns an error must NOT cause us to
	// fail live sessions, so we retry a few times and, if it still fails, set
	// tuimuxAvailable=false and fall back to the PID-only path (fail-open).
	livePtys := map[string]bool{}
	var liveMetas []tuimuxpty.SessionMetadata
	tuimuxAvailable := false
	if jr.tuimuxClient != nil {
		for attempt := 0; attempt < 3; attempt++ {
			metas, err := jr.listLivePtys()
			if err == nil {
				liveMetas = metas
				for _, m := range metas {
					livePtys[m.ID] = true
				}
				tuimuxAvailable = true
				break
			}
			jr.ulog.Warn("Adoption: tuimux PTY list query failed; retrying").
				Err(err).Field("attempt", attempt+1).Log(ctx)
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !tuimuxAvailable {
		jr.ulog.Warn("Adoption: tuimux PTY list unavailable; falling back to PID-only adoption (no PTY split-brain check)").Log(ctx)
	}

	// Rebuild the agent-SESSION store from the live PTY list before anything
	// consumes it. After a `groved upgrade` the agent PTYs survive in the
	// standalone tuimux daemon (each tagged with job_id/plan_name/type/label)
	// but the daemon's session store comes back empty: RecoverSessions only
	// reads on-disk Claude-Code sessions, not the agent panes. Without this,
	// sessions carry no PtyID, so the stop-path KillPty can't close panes,
	// treemux restart falls back to a read-only log view instead of the live
	// pane, and post-upgrade spawns fail to mount. The tuimux PTY list is the
	// authoritative source of truth; reconstruct a models.Session per surviving
	// agent PTY. Runs before the split-brain check below (which reads
	// session.PtyID) and before treemux's first GetSessions.
	if tuimuxAvailable {
		jr.rebuildAgentSessions(ctx, liveMetas)
	}

	for _, job := range jobs {
		if job.Status != "running" {
			continue
		}

		// PTY split-brain check (only when the tuimux daemon answered and the
		// job actually owns a PTY). Headless jobs (no session / empty PtyID)
		// bypass this entirely and keep the existing PID-only path below, which
		// already survives upgrades. If the job's PtyID is gone from the live
		// list, its out-of-process PTY died during the drain window: the agent
		// process is an unreachable orphan, so reap the process group and mark
		// it failed. We only do this when tuimuxAvailable — never on a flaky
		// query. The .status reconcile below is preserved for every other case.
		if tuimuxAvailable {
			session := jr.store.GetSession(job.ID)
			if session != nil && session.PtyID != "" && !livePtys[session.PtyID] {
				if job.PID > 0 {
					// Negative PID targets the whole process group.
					_ = syscall.Kill(-job.PID, syscall.SIGKILL)
				}
				job.Status = "failed"
				job.Error = "PTY lost during daemon upgrade"
				now := time.Now()
				job.CompletedAt = &now
				jr.persister.Save(job)
				jr.store.ApplyUpdate(store.Update{
					Type:    store.UpdateJobFailed,
					Source:  "adoption",
					Payload: job,
				})
				failed++
				jr.ulog.Info("Adoption: PTY lost during upgrade; reaped orphan and marked failed").
					Field("job_id", job.ID).
					Field("pid", job.PID).
					Field("pty_id", session.PtyID).
					Log(ctx)
				continue
			}
		}

		statusPath := jr.getStatusFilePath(job)
		_, statErr := os.Stat(statusPath)

		// Diagnostic (permanent, Debug): logs the exact state adoption sees for
		// each running job — including the persisted ID, the PID, the computed
		// .status path, and whether that file exists. This both confirms the
		// PID-propagation fix took effect and settles the job.ID identity
		// question (does the persisted JobInfo.ID match the .status writer's
		// key?). Observe via
		// `core logs --component groved.jobrunner --level debug -f`.
		jr.ulog.Debug("Adoption loop: evaluating running job").
			Field("job_id", job.ID).
			Field("job_status", job.Status).
			Field("job_pid", job.PID).
			Field("status_path", statusPath).
			Field("status_file_exists", statErr == nil).
			StructuredOnly().
			Log(ctx)

		// A job is adoptable only when it has a live PID. Missing or dead PIDs
		// fall through to the .status reconcile below before we declare failure
		// — this is the core fix: never mark a job "failed (no PID)" without
		// first consulting the durable .status file the agent wrote on exit.
		alive := job.PID > 0 && jr.isPIDAlive(job.PID)
		if !alive {
			// Process is missing or dead — try to read .status for exit code.
			if statusContent, err := jr.readStatusFile(statusPath); err == nil {
				// .status file exists — reconcile to the true terminal state.
				if statusContent.ExitCode == 0 {
					job.Status = "completed"
				} else {
					job.Status = "failed"
					job.Error = "agent exited with code: " + strconv.Itoa(statusContent.ExitCode)
				}
			} else {
				// No .status file — agent vanished without recording an exit.
				job.Status = "failed"
				job.Error = "daemon restarted; agent process exited without status file"
			}

			now := time.Now()
			job.CompletedAt = &now
			jr.persister.Save(job)

			updateType := store.UpdateJobCompleted
			if job.Status == "failed" {
				updateType = store.UpdateJobFailed
			}
			jr.store.ApplyUpdate(store.Update{
				Type:    updateType,
				Source:  "adoption",
				Payload: job,
			})
			failed++
			jr.ulog.Info("Adoption: process not alive, reconciled from .status").
				Field("job_id", job.ID).
				Field("pid", job.PID).
				Field("status", job.Status).
				Log(ctx)
			continue
		}

		// Process is still alive — spawn a poller to wait for it
		go jr.adoptedPIDPoller(ctx, job)
		adopted++
		jr.ulog.Info("Adoption: process alive, polling for completion").
			Field("job_id", job.ID).
			Field("pid", job.PID).
			Log(ctx)
	}

	if adopted > 0 || failed > 0 {
		jr.ulog.Info("Agent adoption complete").
			Field("adopted", adopted).
			Field("failed", failed).
			Log(ctx)
	}
}

// rebuildAgentSessions reconstructs the agent-session store from the live tuimux
// PTY list. For each surviving PTY tagged type=="agent" it either updates the
// PtyID of an already-recovered session (keyed by the job_id tag) or synthesizes
// a fresh models.Session from the PTY's tags. It merges with the sessions
// currently in the store (disk-recovered Claude-Code sessions from
// RecoverSessions, which has already run) and applies the union exactly once via
// UpdateSessions — never just the agent PTYs, because that update REPLACES the
// whole session map and would otherwise clobber the recovered sessions. The
// operation is idempotent: re-running it neither duplicates nor wipes sessions.
func (jr *JobRunner) rebuildAgentSessions(ctx context.Context, metas []tuimuxpty.SessionMetadata) {
	// Start from what's already in the store (disk-recovered sessions) so we
	// don't clobber them when we re-apply the full set.
	merged := map[string]*models.Session{}
	for _, sess := range jr.store.GetSessions() {
		merged[sess.ID] = sess
	}

	rebuilt := 0
	updated := 0
	for _, m := range metas {
		if m.Tags["type"] != "agent" {
			continue
		}
		jobID := m.Tags["job_id"]
		if jobID == "" {
			continue
		}
		if sess, ok := merged[jobID]; ok {
			if sess.PtyID != m.ID {
				sess.PtyID = m.ID
				updated++
			}
			continue
		}
		merged[jobID] = &models.Session{
			ID:   jobID,
			Type: "interactive_agent",
			// WorkingDirectory must be set from the PTY's CWD: scoped surfaces
			// (treemux's rail rehydrate and the agents drawer) filter sessions
			// by workspace via IsSessionInWorkspace(WorkingDirectory, ...). An
			// empty WD drops the synthesized session from those views after an
			// upgrade even though its PTY is alive.
			WorkingDirectory: m.CWD,
			PtyID:            m.ID,
			Mux:              models.MuxTreemux,
			Status:           "running",
			PlanName:         m.Tags["plan_name"],
			JobTitle:         m.Tags["label"],
			StartedAt:        m.StartedAt,
			LastActivity:     time.Now(),
		}
		rebuilt++
	}

	if rebuilt == 0 && updated == 0 {
		return
	}

	sessions := make([]*models.Session, 0, len(merged))
	for _, sess := range merged {
		sessions = append(sessions, sess)
	}
	jr.store.ApplyUpdate(store.Update{
		Type:    store.UpdateSessions,
		Source:  "adoption_rebuild",
		Payload: sessions,
	})
	jr.ulog.Info("Adoption: rebuilt agent session store from live PTY list").
		Field("rebuilt", rebuilt).
		Field("updated", updated).
		Field("total_sessions", len(sessions)).
		StructuredOnly().
		Log(ctx)
}

// isPIDAlive checks if a process ID is still running via kill(pid, 0).
// Returns true if the process exists, false otherwise.
func (jr *JobRunner) isPIDAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// listLivePtys queries the standalone tuimux daemon's GET /api/pty/list and
// returns the live PTY session metadata. It dials the tuimux unix socket
// directly (the ApiClient does not expose a PTY-list method) so adoption can
// re-bind adopted jobs to their surviving out-of-process PTYs. A short timeout
// keeps a slow/dead daemon from stalling boot; the caller treats any error as
// "list unavailable" and falls open to PID-only adoption.
func (jr *JobRunner) listLivePtys() ([]tuimuxpty.SessionMetadata, error) {
	sock := tuimux.DefaultSocketPath()
	httpClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	resp, err := httpClient.Get("http://localhost/api/pty/list")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &listError{status: resp.StatusCode}
	}
	var metas []tuimuxpty.SessionMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metas); err != nil {
		return nil, err
	}
	return metas, nil
}

// listError is returned when GET /api/pty/list responds with a non-200 status.
type listError struct{ status int }

func (e *listError) Error() string {
	return "pty list returned status " + strconv.Itoa(e.status)
}

// adoptedPIDPoller polls for the completion of an adopted process and marks it done.
// It checks the PID every 2 seconds until it disappears.
func (jr *JobRunner) adoptedPIDPoller(ctx context.Context, job *models.JobInfo) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !jr.isPIDAlive(job.PID) {
				// Process is dead — read .status file and mark job
				statusPath := jr.getStatusFilePath(job)
				if statusContent, err := jr.readStatusFile(statusPath); err == nil {
					if statusContent.ExitCode == 0 {
						jr.markDone(job, "completed", "")
					} else {
						jr.markDone(job, "failed", "agent exited with code: "+strconv.Itoa(statusContent.ExitCode))
					}
				} else {
					jr.markDone(job, "failed", "agent process exited without status file")
				}

				jr.ulog.Info("Adoption poller: process exited").
					Field("job_id", job.ID).
					Field("pid", job.PID).
					Field("status", job.Status).
					Log(ctx)
				return
			}
		}
	}
}

// getStatusFilePath returns the path where a job's .status file should be written.
// Format: .artifacts/<job-id>/.status
func (jr *JobRunner) getStatusFilePath(job *models.JobInfo) string {
	return filepath.Join(job.PlanDir, ".artifacts", job.ID, ".status")
}

// readStatusFile reads and parses a .status file written by an agent.
func (jr *JobRunner) readStatusFile(statusPath string) (*statusFileContent, error) {
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return nil, err
	}

	var content statusFileContent
	if err := json.Unmarshal(data, &content); err != nil {
		return nil, err
	}

	return &content, nil
}
