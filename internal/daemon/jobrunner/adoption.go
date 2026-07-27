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
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/flow/pkg/orchestration"
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
//  1. Resolves whether an agent process is alive — live PTY, session-registry
//     PID, then job.PID (see jobAgentAlive)
//  2. If alive, spawns a poller to detect when it exits
//  3. If not, reconciles from the durable .status file, or records the
//     non-terminal "orphaned" when the agent left no exit behind
func (jr *JobRunner) AdoptRunningAgents(ctx context.Context) {
	if jr.persister == nil {
		return
	}

	// Load all persisted jobs
	jobs := jr.persister.Load()
	adopted := 0
	// Counts jobs whose agent was not found alive. They land on a terminal
	// status only when a .status file says so; otherwise they are orphaned.
	reconciled := 0

	// Health-gate the out-of-process PTY list BEFORE any fail decision. PTYs
	// now live in the standalone tuimux daemon; an adopted interactive job is
	// only truly alive if its PtyID still maps to a live PTY there. But a
	// transient/too-early query that returns an error must NOT cause us to
	// fail live sessions, so we retry a few times and, if it still fails, set
	// tuimuxAvailable=false and fall back to the PID-only path (fail-open).
	livePtys := map[string]bool{}
	// Same list indexed by the job_id tag: a live PTY carrying this job's id is
	// the most direct evidence its agent survived the restart.
	livePtyJobs := map[string]bool{}
	var liveMetas []tuimuxpty.SessionMetadata
	tuimuxAvailable := false
	if jr.tuimuxClient != nil {
		for attempt := 0; attempt < 3; attempt++ {
			metas, err := jr.listLivePtys()
			if err == nil {
				liveMetas = metas
				for _, m := range metas {
					livePtys[m.ID] = true
					if jobID := m.Tags["job_id"]; jobID != "" {
						livePtyJobs[jobID] = true
					}
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
			// Already-stranded headless jobs (A4): the JobInfo is already
			// terminal (a prior boot reconciled it, or it pre-dates the
			// finalize fix), but the job-file frontmatter may still sit at
			// running/idle — the exact strand this feature fixes.
			// FinalizeHeadlessJob re-reads frontmatter from disk and no-ops if
			// it is already terminal, so this is safe to call unconditionally
			// for headless jobs and converts pre-fix strands at the next boot.
			if job.Type == models.JobType("headless_agent") {
				jr.finalizeHeadlessFrontmatter(ctx, job)
			}
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
				reconciled++
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

		// A job is adoptable when an agent process for it is alive — which is
		// not the same question as "is job.PID alive". See jobAgentAlive: for
		// an interactive agent job.PID is the launcher, which is expected to be
		// gone while the agent works on under its PTY. Jobs with no live agent
		// fall through to the .status reconcile below before any verdict is
		// recorded, and absence of a .status file yields the non-terminal
		// "orphaned", never "failed".
		alive := jr.jobAgentAlive(job, livePtyJobs)
		if !alive {
			status, errMsg := jr.reconcileLostJob(job)
			jr.applyReconciledStatus(job, status, errMsg, "adoption")
			reconciled++
			jr.ulog.Info("Adoption: no live agent process; reconciled from .status").
				Field("job_id", job.ID).
				Field("pid", job.PID).
				Field("status", job.Status).
				Log(ctx)
			// markDone-equivalent above owns JobInfo; the finalizer owns
			// frontmatter. For headless jobs, drive the job-file frontmatter to
			// the same terminal state from the same .status. An orphaned job is
			// not terminal, so it is left out — there is nothing truthful to
			// finalize to.
			if isJobTerminal(job.Status) {
				jr.finalizeHeadlessFrontmatter(ctx, job)
			}
			continue
		}

		// Process is still alive — spawn a poller to wait for it
		go jr.adoptedPIDPoller(ctx, job)
		adopted++
		jr.ulog.Info("Adoption: agent alive, polling for completion").
			Field("job_id", job.ID).
			Field("pid", job.PID).
			Log(ctx)
	}

	if adopted > 0 || reconciled > 0 {
		jr.ulog.Info("Agent adoption complete").
			Field("adopted", adopted).
			Field("reconciled", reconciled).
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

// jobAgentAlive reports whether an agent process for this job is still running.
//
// job.PID alone cannot answer that. For an interactive agent the persisted PID
// is the launcher/orchestrator that spawned the pane — it exits as soon as the
// agent is mounted, so a dead job.PID is the normal, healthy steady state while
// the agent works on under the PTY. Deciding liveness from it is answering a
// question about the wrong process, and answering "failed" for a job that is
// still writing its transcript.
//
// The signals, strongest first:
//  1. a live out-of-process PTY tagged with this job id — the mux owns the
//     agent's life, so this is direct evidence;
//  2. a live PID in the session registry, which the agent itself confirmed;
//  3. job.PID, which is only meaningful for direct children (headless agents).
//
// livePtyJobs may be nil, in which case the PTY list is queried on demand;
// callers that already hold the list pass it to avoid re-querying per job.
func (jr *JobRunner) jobAgentAlive(job *models.JobInfo, livePtyJobs map[string]bool) bool {
	if job == nil {
		return false
	}
	if livePtyJobs == nil {
		livePtyJobs = jr.livePtyJobIDs()
	}
	if livePtyJobs[job.ID] {
		return true
	}
	if pid := jr.registryAgentPID(job.ID); pid > 0 && jr.isPIDAlive(pid) {
		return true
	}
	return job.PID > 0 && jr.isPIDAlive(job.PID)
}

// registryAgentPID returns the PID the agent recorded for itself in the session
// registry, or 0 when there is none. PIDs of 1 or less are ignored: they are
// placeholders written before the real PID was known, and PID 1 is always
// "alive", which would make every such job permanently unreconcilable.
func (jr *JobRunner) registryAgentPID(jobID string) int {
	if jobID == "" {
		return 0
	}
	registry, err := sessions.NewFileSystemRegistry()
	if err != nil || registry == nil {
		return 0
	}
	metadata, err := registry.Find(jobID)
	if err != nil || metadata == nil || metadata.PID <= 1 {
		return 0
	}
	return metadata.PID
}

// livePtyJobIDs returns the set of job ids that still own a live PTY in the
// standalone tuimux daemon. An unavailable daemon yields an empty set, which
// makes this signal absent rather than negative — callers must treat a miss as
// "no evidence", never as "the agent is dead".
func (jr *JobRunner) livePtyJobIDs() map[string]bool {
	live := map[string]bool{}
	metas, err := jr.listLivePtys()
	if err != nil {
		return live
	}
	for _, meta := range metas {
		if jobID := meta.Tags["job_id"]; jobID != "" {
			live[jobID] = true
		}
	}
	return live
}

// reconcileLostJob decides what a job whose agent is not alive should become.
// It never returns "failed" on absence of evidence: a job with a .status file
// reconciles to that file's verdict, and a job without one is orphaned — a
// non-terminal state that says the daemon lost track of the process without
// claiming the work failed. Terminal "failed" stays reserved for a recorded
// non-zero exit.
func (jr *JobRunner) reconcileLostJob(job *models.JobInfo) (status, errMsg string) {
	statusContent, err := jr.readStatusFile(jr.getStatusFilePath(job))
	if err != nil {
		return "orphaned", "daemon lost track of this job across a restart: no live agent process and no exit status recorded. The agent may still be running; check `aglogs read` for its transcript."
	}
	if statusContent.ExitCode == 0 {
		return "completed", ""
	}
	return "failed", "agent exited with code: " + strconv.Itoa(statusContent.ExitCode)
}

// applyReconciledStatus persists a restart-recovery verdict and broadcasts it.
// CompletedAt is stamped only for genuinely terminal states — an orphaned job
// has not completed, and stamping it would let every downstream consumer read
// the daemon's uncertainty as a finished run.
func (jr *JobRunner) applyReconciledStatus(job *models.JobInfo, status, errMsg, source string) {
	job.Status = status
	job.Error = errMsg
	if isJobTerminal(status) {
		now := time.Now()
		job.CompletedAt = &now
	}
	if jr.persister != nil {
		jr.persister.Save(job)
	}

	updateType := store.UpdateJobCompleted
	switch status {
	case "failed":
		updateType = store.UpdateJobFailed
	case "orphaned":
		updateType = store.UpdateJobOrphaned
	}
	jr.store.ApplyUpdate(store.Update{
		Type:    updateType,
		Source:  source,
		Payload: job,
	})
}

// listLivePtys queries the standalone tuimux daemon's GET /api/pty/list and
// returns the live PTY session metadata. It dials the tuimux unix socket
// directly (the ApiClient does not expose a PTY-list method) so adoption can
// re-bind adopted jobs to their surviving out-of-process PTYs. A short timeout
// keeps a slow/dead daemon from stalling boot; the caller treats any error as
// "list unavailable" and falls open to PID-only adoption.
func (jr *JobRunner) listLivePtys() ([]tuimuxpty.SessionMetadata, error) {
	// Prefer the socket of the wired tuimux client (this daemon's own
	// scope-keyed socket). Fall back to DefaultSocketPath(), which reads the
	// GROVE_SCOPE this daemon exported at start, so it still resolves to the
	// same scoped socket.
	sock := tuimux.DefaultSocketPath()
	if jr.tuimuxClient != nil && jr.tuimuxClient.SocketPath != "" {
		sock = jr.tuimuxClient.SocketPath
	}
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

// adoptedPIDPoller polls for the completion of an adopted agent and records its
// outcome. The cheap kill(pid, 0) probe runs every 2 seconds; it is only ever
// evidence of life, never of death, because job.PID may name a launcher that
// exited long before the agent will. A negative probe is therefore escalated to
// the full jobAgentAlive check — PTY list plus session registry — which is rate
// limited because it talks to the tuimux daemon over its socket.
func (jr *JobRunner) adoptedPIDPoller(ctx context.Context, job *models.JobInfo) {
	const fullCheckInterval = 10 * time.Second

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastFullCheck := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if job.PID > 0 && jr.isPIDAlive(job.PID) {
				continue
			}
			if time.Since(lastFullCheck) < fullCheckInterval {
				continue
			}
			lastFullCheck = time.Now()
			if jr.jobAgentAlive(job, nil) {
				continue
			}

			// No agent process anywhere — reconcile from the durable .status
			// file, or record the non-terminal orphaned state when the agent
			// left no exit record.
			status, errMsg := jr.reconcileLostJob(job)
			jr.markDone(job, status, errMsg)

			// markDone owns JobInfo; the finalizer owns frontmatter. For
			// headless jobs, drive the job-file frontmatter to the same
			// terminal state from the same .status. Skipped for orphaned:
			// there is no terminal verdict to propagate.
			if isJobTerminal(job.Status) {
				jr.finalizeHeadlessFrontmatter(ctx, job)
			}

			jr.ulog.Info("Adoption poller: agent no longer running").
				Field("job_id", job.ID).
				Field("pid", job.PID).
				Field("status", job.Status).
				Log(ctx)
			return
		}
	}
}

// getStatusFilePath returns the path where a job's .status file should be written.
// Format: .artifacts/<job-id>/.status
func (jr *JobRunner) getStatusFilePath(job *models.JobInfo) string {
	return filepath.Join(job.PlanDir, ".artifacts", job.ID, ".status")
}

// finalizeHeadlessFrontmatter reconciles a headless job's job-file frontmatter
// to a terminal status via flow's FinalizeHeadlessJob. Adoption's markDone owns
// the daemon-side JobInfo; this owns the frontmatter side (they read the same
// .status so they never diverge). No-op for non-headless jobs.
// FinalizeHeadlessJob re-reads the frontmatter from disk and is idempotent, so
// this is safe to call from every adoption branch.
func (jr *JobRunner) finalizeHeadlessFrontmatter(ctx context.Context, job *models.JobInfo) {
	if job == nil || job.Type != models.JobType("headless_agent") {
		return
	}

	plan, err := orchestration.LoadPlan(job.PlanDir)
	if err != nil {
		jr.ulog.Warn("Adoption: failed to load plan for headless finalize").
			Field("job_id", job.ID).
			Field("plan_dir", job.PlanDir).
			Err(err).
			Log(ctx)
		return
	}

	fjob, found := plan.GetJobByFilename(job.JobFile)
	if !found {
		jr.ulog.Warn("Adoption: headless job not found in plan for finalize").
			Field("job_id", job.ID).
			Field("job_file", job.JobFile).
			Log(ctx)
		return
	}

	if err := orchestration.FinalizeHeadlessJob(fjob, plan); err != nil {
		jr.ulog.Warn("Adoption: FinalizeHeadlessJob failed").
			Field("job_id", job.ID).
			Err(err).
			Log(ctx)
		return
	}

	jr.ulog.Info("Adoption: finalized headless job frontmatter").
		Field("job_id", job.ID).
		Field("status", fjob.Status).
		Log(ctx)
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
