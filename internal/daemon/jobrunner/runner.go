// Package jobrunner provides the daemon's job queue, worker pool, and execution engine.
// It wraps flow's LocalRuntime with concurrency control, persistence, and panic recovery.
package jobrunner

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/flow/pkg/orchestration"
	tuimux "github.com/grovetools/tuimux/api/client"
)

// JobRunner manages the job queue, worker pool, and execution lifecycle.
// It supports DAG-aware scheduling: jobs with unmet dependencies are held in
// a blocked queue and automatically promoted to the run queue when their
// dependencies complete (or reach pending_user for chat→agent edges).
type JobRunner struct {
	queue     chan *models.JobInfo
	workers   int
	running   map[string]context.CancelFunc
	runtime   orchestration.Runtime
	mu        sync.RWMutex
	store     *store.Store
	ulog      *grovelogging.UnifiedLogger
	persister *Persistence

	// tuimuxClient talks to the standalone tuimux daemon that owns agent
	// PTYs out-of-process. Used by AdoptRunningAgents to verify that an
	// adopted job's PtyID still maps to a live PTY after a graceful upgrade.
	// May be nil when the tuimux daemon could not be started.
	tuimuxClient *tuimux.ApiClient

	// blocked holds jobs whose dependencies are not yet satisfied.
	// They are promoted to the run queue by evaluateBlockedJobs().
	blocked   map[string]*models.JobInfo
	blockedMu sync.Mutex

	// transcriptSem limits concurrent appendTranscriptAsync goroutines.
	// Each one spawns external grove/aglogs processes; unbounded concurrency
	// can overwhelm the system.
	transcriptSem chan struct{}

	// onJobDetached is called when an interactive_agent / headless_agent job
	// returns from the executor with a non-terminal "running" status. The
	// log streamer uses this to broadcast "running" to SSE subscribers and
	// close them, so clients like `flow plan run` can return.
	onJobDetached func(jobID string)
}

// SetOnJobDetached registers a callback invoked when a job completes its
// executor with status="running" (interactive_agent / headless_agent launched
// detached). Wired by the daemon to the LogStreamer.NotifyJobDetached method.
func (jr *JobRunner) SetOnJobDetached(fn func(jobID string)) {
	jr.onJobDetached = fn
}

// New creates a new JobRunner with the given store, runtime, and worker count.
// tuimuxClient is the standalone tuimux daemon client used by adoption to
// re-bind out-of-process agent PTYs across a graceful upgrade; it may be nil.
func New(st *store.Store, runtime orchestration.Runtime, workers int, persister *Persistence, tuimuxClient *tuimux.ApiClient) *JobRunner {
	if workers <= 0 {
		workers = 4
	}
	return &JobRunner{
		queue:         make(chan *models.JobInfo, 1000),
		workers:       workers,
		running:       make(map[string]context.CancelFunc),
		runtime:       runtime,
		store:         st,
		ulog:          grovelogging.NewUnifiedLogger("groved.jobrunner"),
		persister:     persister,
		tuimuxClient:  tuimuxClient,
		blocked:       make(map[string]*models.JobInfo),
		transcriptSem: make(chan struct{}, 4),
	}
}

// Start restores persisted queued jobs and launches the worker pool.
func (jr *JobRunner) Start(ctx context.Context) {
	// Restore queued and blocked jobs from persistence
	if jr.persister != nil {
		restored := jr.persister.Load()
		for _, job := range restored {
			if job.Status == "queued" {
				jr.ulog.Info("Restoring queued job").Field("job_id", job.ID).Log(ctx)
				jr.store.ApplyUpdate(store.Update{
					Type:    store.UpdateJobSubmitted,
					Source:  "jobrunner",
					Payload: job,
				})
				jr.queue <- job
			} else if job.Status == "blocked" {
				jr.ulog.Info("Restoring blocked job").Field("job_id", job.ID).Log(ctx)
				jr.blockedMu.Lock()
				jr.blocked[job.ID] = job
				jr.blockedMu.Unlock()
				jr.store.ApplyUpdate(store.Update{
					Type:    store.UpdateJobSubmitted,
					Source:  "jobrunner",
					Payload: job,
				})
			} else if job.Status == "running" {
				// Mark previously-running jobs as failed (daemon restarted)
				job.Status = "failed"
				job.Error = "daemon restarted while job was running"
				now := time.Now()
				job.CompletedAt = &now
				jr.persister.Save(job)
				jr.store.ApplyUpdate(store.Update{
					Type:    store.UpdateJobFailed,
					Source:  "jobrunner",
					Payload: job,
				})
			}
		}
	}

	for i := 0; i < jr.workers; i++ {
		go jr.worker(ctx)
	}

	go jr.watchTransitions(ctx)
}

// Submit enqueues a new job for execution. If the job's dependencies are not
// yet satisfied, it is placed in the blocked queue and will be automatically
// promoted when its dependencies reach a terminal state.
func (jr *JobRunner) Submit(ctx context.Context, req models.JobSubmitRequest) (*models.JobInfo, error) {
	// Jobs run with no deadline unless the submitter sets an explicit,
	// parseable timeout. Hung jobs are handled by cancellation, not a
	// wall clock.
	timeoutStr := ""
	if req.Timeout != "" {
		if d, err := time.ParseDuration(req.Timeout); err == nil {
			timeoutStr = d.String()
		}
	}

	baseName := strings.TrimSuffix(req.JobFile, ".md")
	jobID := fmt.Sprintf("%s-%s", baseName, uuid.New().String()[:6])

	info := &models.JobInfo{
		ID:          jobID,
		PlanDir:     req.PlanDir,
		JobFile:     req.JobFile,
		Priority:    req.Priority,
		TimeoutStr:  timeoutStr,
		Env:         req.Env,
		AgentTarget: req.AgentTarget,
		Status:      "queued",
		SubmittedAt: time.Now(),
	}

	// Check if dependencies are met; if not, hold the job in the blocked queue.
	if !jr.areDependenciesMet(info) {
		info.Status = "blocked"
		if jr.persister != nil {
			jr.persister.Save(info)
		}
		jr.store.ApplyUpdate(store.Update{
			Type:    store.UpdateJobSubmitted,
			Source:  "jobrunner",
			Payload: info,
		})

		jr.blockedMu.Lock()
		jr.blocked[info.ID] = info
		jr.blockedMu.Unlock()

		jr.ulog.Info("Job blocked (dependencies not met)").
			Field("job_id", info.ID).
			Field("plan_dir", info.PlanDir).
			Field("job_file", info.JobFile).
			Log(ctx)

		return info, nil
	}

	if jr.persister != nil {
		jr.persister.Save(info)
	}
	jr.store.ApplyUpdate(store.Update{
		Type:    store.UpdateJobSubmitted,
		Source:  "jobrunner",
		Payload: info,
	})

	jr.queue <- info
	// Debug: the user-facing lifecycle events (event=job.launched/job.finished)
	// are emitted by flow's orchestrator UpdateJobStatus funnel.
	jr.ulog.Debug("Job submitted").
		Field("job_id", info.ID).
		Field("plan_dir", info.PlanDir).
		Field("job_file", info.JobFile).
		Log(ctx)

	return info, nil
}

// areDependenciesMet loads the plan for the given job and checks whether all
// of its declared dependencies have reached a terminal status. It replicates
// the special-case rule from flow's orchestration engine: an agent job treats
// a chat dependency in pending_user as satisfied.
func (jr *JobRunner) areDependenciesMet(info *models.JobInfo) bool {
	plan, err := orchestration.LoadPlan(info.PlanDir)
	if err != nil {
		jr.ulog.Warn("Could not load plan to check deps; assuming met").
			Err(err).
			Field("job_id", info.ID).
			Log(context.Background())
		return true
	}

	// A held plan's jobs are never started via the daemon: keep them in the
	// blocked queue until the hold is lifted (evaluateBlockedJobs re-checks
	// this on every pass, so unholding promotes them automatically).
	if planOnHold(plan) {
		return false
	}

	job, found := plan.GetJobByFilename(info.JobFile)
	if !found || job == nil {
		return true // Can't find the job definition — let it run
	}

	// If the CLI already transitioned the job to "running" before submitting
	// to the daemon, treat it as runnable — the caller explicitly targeted it.
	if job.Status == orchestration.JobStatusRunning {
		return true
	}

	return job.IsRunnable()
}

// planOnHold reports whether the plan is on hold — plan-level
// `status: hold` in .grove-plan.yml — mirroring the CLI guard in
// flow/cmd/plan_run.go. Hold prevents NEW runs only: a held plan's jobs
// must never be started by the daemon, but jobs and agents that are
// already executing continue unaffected.
func planOnHold(plan *orchestration.Plan) bool {
	return plan != nil && plan.Config != nil && plan.Config.Status == "hold"
}

// evaluateBlockedJobs re-checks every blocked job's dependencies and promotes
// those that are now satisfied to the run queue.
func (jr *JobRunner) evaluateBlockedJobs() {
	jr.blockedMu.Lock()
	defer jr.blockedMu.Unlock()

	for id, info := range jr.blocked {
		if jr.areDependenciesMet(info) {
			delete(jr.blocked, id)

			info.Status = "queued"
			if jr.persister != nil {
				jr.persister.Save(info)
			}
			jr.store.ApplyUpdate(store.Update{
				Type:    store.UpdateJobSubmitted,
				Source:  "jobrunner",
				Payload: info,
			})

			jr.ulog.Info("Blocked job promoted to queue").
				Field("job_id", info.ID).
				Field("job_file", info.JobFile).
				Log(context.Background())

			jr.queue <- info
		}
	}
}

// Cancel stops a running or queued job.
func (jr *JobRunner) Cancel(jobID string) error {
	jr.mu.Lock()
	cancel, exists := jr.running[jobID]
	jr.mu.Unlock()

	if exists {
		cancel()
		return nil
	}

	// If it's queued, mark it cancelled so the worker skips it
	info := jr.store.GetJob(jobID)
	if info != nil && info.Status == "queued" {
		info.Status = "cancelled"
		now := time.Now()
		info.CompletedAt = &now
		if jr.persister != nil {
			jr.persister.Save(info)
		}
		jr.store.ApplyUpdate(store.Update{
			Type:    store.UpdateJobCancelled,
			Source:  "jobrunner",
			Payload: info,
		})
		return nil
	}

	// If it's blocked, remove from blocked queue and mark cancelled
	jr.blockedMu.Lock()
	if blockedInfo, ok := jr.blocked[jobID]; ok {
		delete(jr.blocked, jobID)
		jr.blockedMu.Unlock()
		blockedInfo.Status = "cancelled"
		now := time.Now()
		blockedInfo.CompletedAt = &now
		if jr.persister != nil {
			jr.persister.Save(blockedInfo)
		}
		jr.store.ApplyUpdate(store.Update{
			Type:    store.UpdateJobCancelled,
			Source:  "jobrunner",
			Payload: blockedInfo,
		})
		return nil
	}
	jr.blockedMu.Unlock()

	return fmt.Errorf("job %s is not running, queued, or blocked", jobID)
}

func (jr *JobRunner) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case jobInfo := <-jr.queue:
			if jobInfo.Status == "cancelled" {
				continue
			}
			jr.executeJob(ctx, jobInfo)
		}
	}
}

func (jr *JobRunner) executeJob(ctx context.Context, info *models.JobInfo) {
	// Panic recovery — prevents executor panics from crashing the daemon
	defer func() {
		if r := recover(); r != nil {
			jr.ulog.Error("Job panicked").
				Field("job_id", info.ID).
				Field("panic", fmt.Sprintf("%v", r)).
				Log(ctx)
			jr.markDone(info, "failed", fmt.Sprintf("panic: %v", r))
			jr.cleanupRunning(info.ID)
		}
	}()

	// No deadline by default — only an explicit per-job timeout creates one.
	var jobCtx context.Context
	var cancel context.CancelFunc
	if info.TimeoutStr != "" {
		if d, err := time.ParseDuration(info.TimeoutStr); err == nil && d > 0 {
			jobCtx, cancel = context.WithTimeout(ctx, d)
		}
	}
	if jobCtx == nil {
		jobCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	jr.mu.Lock()
	jr.running[info.ID] = cancel
	jr.mu.Unlock()

	defer jr.cleanupRunning(info.ID)

	// Mark as running
	now := time.Now()
	info.StartedAt = &now
	info.Status = "running"
	if jr.persister != nil {
		jr.persister.Save(info)
	}
	jr.store.ApplyUpdate(store.Update{
		Type:    store.UpdateJobStarted,
		Source:  "jobrunner",
		Payload: info,
	})

	jr.ulog.Debug("Job started").
		Field("job_id", info.ID).
		Field("plan_dir", info.PlanDir).
		Field("job_file", info.JobFile).
		Log(ctx)

	// Load plan and execute
	plan, err := orchestration.LoadPlan(info.PlanDir)
	if err != nil {
		jr.markDone(info, "failed", fmt.Sprintf("load plan: %v", err))
		return
	}

	// Plan-level hold gate: never start a job whose plan is on hold. This
	// catches jobs that were already queued when the hold was set. The job
	// is returned to the blocked queue (not failed) and will be promoted by
	// evaluateBlockedJobs once the hold is lifted.
	if planOnHold(plan) {
		info.Status = "blocked"
		info.StartedAt = nil
		if jr.persister != nil {
			jr.persister.Save(info)
		}
		jr.store.ApplyUpdate(store.Update{
			Type:    store.UpdateJobSubmitted,
			Source:  "jobrunner",
			Payload: info,
		})

		jr.blockedMu.Lock()
		jr.blocked[info.ID] = info
		jr.blockedMu.Unlock()

		jr.ulog.Info("Job blocked (plan on hold)").
			Field("job_id", info.ID).
			Field("plan_dir", info.PlanDir).
			Field("job_file", info.JobFile).
			Log(ctx)
		return
	}

	// Inject agent_target from the submission request into the plan so
	// executors can route without consulting env vars or daemon state.
	if info.AgentTarget != "" {
		if plan.Orchestration == nil {
			plan.Orchestration = &orchestration.Config{}
		}
		plan.Orchestration.AgentTarget = info.AgentTarget
	}

	// Stash the job log path on info so the server's log-stream handler
	// tails the same file the runtime writes to.
	if job, ok := plan.GetJobByFilename(info.JobFile); ok {
		if logPath, pathErr := orchestration.GetJobLogPath(plan, job); pathErr == nil {
			info.LogFilePath = logPath
			if jr.persister != nil {
				jr.persister.Save(info)
			}
			jr.store.ApplyUpdate(store.Update{
				Type:    store.UpdateJobStarted,
				Source:  "jobrunner",
				Payload: info,
			})
		}
	}

	// Create an orchestrator to utilize flow's dependency logic
	orch, err := orchestration.NewOrchestrator(plan, &orchestration.OrchestratorConfig{
		Runtime: jr.runtime,
	})
	if err != nil {
		jr.markDone(info, "failed", fmt.Sprintf("new orchestrator: %v", err))
		return
	}

	// Discard stdout — job output is captured in job.log by the runtime.
	// Without this, non-agent job output leaks to the daemon's terminal.
	jobCtx = grovelogging.WithWriter(jobCtx, io.Discard)

	// Execute via Orchestrator using the provided job file
	err = orch.RunJob(jobCtx, info.JobFile)

	if err != nil {
		if jobCtx.Err() == context.Canceled {
			jr.markDone(info, "cancelled", "job was cancelled")
		} else if jobCtx.Err() == context.DeadlineExceeded {
			jr.markDone(info, "failed", "job timed out")
		} else {
			jr.markDone(info, "failed", err.Error())
		}
	} else {
		// Check the job's actual final status — some job types manage their own status:
		// - chat jobs set pending_user (waiting for user input)
		// - interactive_agent jobs set running (launched in tmux, user interacts)
		finalStatus := "completed"
		if job, _ := plan.GetJobByFilename(info.JobFile); job != nil {
			if job.Status == orchestration.JobStatusPendingUser || job.Status == orchestration.JobStatusRunning {
				finalStatus = string(job.Status)
			}
		}
		jr.markDone(info, finalStatus, "")
	}
}

func (jr *JobRunner) markDone(info *models.JobInfo, status, errMsg string) {
	info.Status = status
	info.Error = errMsg

	// Only set CompletedAt for terminal states
	if status != "pending_user" && status != "running" {
		now := time.Now()
		info.CompletedAt = &now
	}

	if jr.persister != nil {
		jr.persister.Save(info)
	}

	updateType := store.UpdateJobCompleted
	switch status {
	case "failed":
		updateType = store.UpdateJobFailed
	case "cancelled":
		updateType = store.UpdateJobCancelled
	case "pending_user":
		updateType = store.UpdateJobPendingUser
	}
	jr.store.ApplyUpdate(store.Update{
		Type:    updateType,
		Source:  "jobrunner",
		Payload: info,
	})

	// Debug: misleading as a lifecycle event — this also fires when an
	// interactive/headless agent detaches to "running" (see below). The real
	// event=job.finished line comes from flow's UpdateJobStatus funnel.
	jr.ulog.Debug("Job finished").
		Field("job_id", info.ID).
		Field("status", status).
		Field("error", errMsg).
		Log(context.Background())

	// For interactive/headless agents that finish their launch with "running",
	// notify the log streamer so it can broadcast and close SSE subscribers.
	// "running" is not in isTerminalStatus (the log file keeps growing), so
	// the tail loop on its own would never release the streaming client.
	if status == "running" && jr.onJobDetached != nil {
		jr.onJobDetached(info.ID)
	}

	// Re-evaluate blocked jobs — this job's completion may unblock dependents.
	jr.evaluateBlockedJobs()
}

func (jr *JobRunner) cleanupRunning(jobID string) {
	jr.mu.Lock()
	if cancel, ok := jr.running[jobID]; ok {
		cancel()
		delete(jr.running, jobID)
	}
	jr.mu.Unlock()
}

func isJobTerminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "idle", "interrupted", "abandoned":
		return true
	default:
		return false
	}
}

// watchTransitions listens for job state changes in the daemon store.
// When an interactive_agent or headless_agent reaches a terminal state,
// it automatically appends the transcript and unblocks downstream dependencies.
func (jr *JobRunner) watchTransitions(ctx context.Context) {
	sub := jr.store.Subscribe()
	defer jr.store.Unsubscribe(sub)

	processed := make(map[string]struct{})
	seenActive := make(map[string]struct{})

	for {
		select {
		case <-ctx.Done():
			return
		case update := <-sub:
			switch update.Type {
			case store.UpdateJobsDiscovered:
				// Jobs discovered from filesystem scan. On the first scan,
				// already-terminal jobs only trigger dependency evaluation.
				// On subsequent scans, if a previously-active job is now
				// terminal, treat it as a real transition and append transcript.
				if jobs, ok := update.Payload.([]*models.JobInfo); ok {
					needsEval := false
					for _, job := range jobs {
						// Federated jobs (C8/C12) belong to a satellite: their PlanDir
						// is a satellite-side path, so appendTranscriptAsync's
						// LoadPlan(info.PlanDir) would fail/mis-resolve. The laptop
						// jobrunner never drives a remote job's transitions.
						if job.Origin != "" {
							continue
						}
						if isJobTerminal(job.Status) {
							if _, ok := processed[job.ID]; !ok {
								processed[job.ID] = struct{}{}
								_, wasActive := seenActive[job.ID]
								delete(seenActive, job.ID)
								if wasActive && (job.Type == "interactive_agent" || job.Type == "headless_agent") {
									jr.appendTranscriptAsync(job)
								} else {
									needsEval = true
								}
							}
						} else {
							delete(processed, job.ID)
							seenActive[job.ID] = struct{}{}
						}
					}
					if needsEval {
						jr.evaluateBlockedJobs()
					}
				}
			case store.UpdateJobCompleted, store.UpdateJobFailed, store.UpdateJobCancelled:
				if job, ok := update.Payload.(*models.JobInfo); ok {
					// Skip federated jobs (C8/C12): appendTranscriptAsync /
					// evaluateBlockedJobs LoadPlan a satellite-side PlanDir.
					if job.Origin != "" {
						continue
					}
					if isJobTerminal(job.Status) {
						if _, ok := processed[job.ID]; !ok {
							processed[job.ID] = struct{}{}
							if job.Type == "interactive_agent" || job.Type == "headless_agent" {
								jr.appendTranscriptAsync(job)
							} else {
								jr.evaluateBlockedJobs()
							}
						}
					}
				}
			case store.UpdateSessionEnd:
				if payload, ok := update.Payload.(*store.SessionEndPayload); ok {
					job := jr.store.GetJob(payload.JobID)
					if job != nil {
						if _, ok := processed[job.ID]; !ok {
							processed[job.ID] = struct{}{}
							if job.Type == "interactive_agent" || job.Type == "headless_agent" {
								jr.appendTranscriptAsync(job)
							} else {
								jr.evaluateBlockedJobs()
							}
						}
					}
				}
			case store.UpdateSessionConfirmation:
				// The store's applySessionConfirmation already copied the
				// confirmed PID onto JobInfo.PID before this broadcast. The
				// confirmation arrives asynchronously, after the job was first
				// marked "running" and saved, so re-persist the job to flush the
				// PID to disk — otherwise jobs/<id>.json keeps pid:null and
				// adoption can never re-attach the agent on a graceful upgrade.
				if payload, ok := update.Payload.(*store.SessionConfirmationPayload); ok {
					job := jr.store.GetJob(payload.JobID)
					pid := 0
					if job != nil {
						pid = job.PID
					}
					// Diagnostic (permanent, Debug): proves the event reaches the
					// jobrunner and shows the PID read back from the store just
					// before the persist. Observe via
					// `core logs --component groved.jobrunner --level debug -f`.
					jr.ulog.Debug("Session confirmation: persisting JobInfo with PID").
						Field("job_id", payload.JobID).
						Field("job_pid", pid).
						StructuredOnly().
						Log(ctx)
					if jr.persister != nil && job != nil {
						jr.persister.Save(job)
					}
				}
			}
		}
	}
}

func (jr *JobRunner) appendTranscriptAsync(info *models.JobInfo) {
	go func() {
		// Limit concurrent transcript appends — each spawns external processes
		jr.transcriptSem <- struct{}{}
		defer func() { <-jr.transcriptSem }()

		// Allow time for final logs to flush to disk before appending
		time.Sleep(1 * time.Second)

		// Ensure we evaluate downstream jobs even if transcript fails or skips
		defer jr.evaluateBlockedJobs()

		ctx := context.Background()
		plan, err := orchestration.LoadPlan(info.PlanDir)
		if err != nil {
			jr.ulog.Warn("Failed to load plan for auto-transcript").Err(err).Log(ctx)
			return
		}

		job, found := plan.GetJobByFilename(info.JobFile)
		if !found {
			return
		}

		// AppendAgentTranscript is idempotent; it splices the transcript
		// section under the job-file lock via StatePersister.UpdateJobTranscript,
		// so it is safe to call concurrently with flow-side writers.
		if err := orchestration.AppendAgentTranscript(job, plan); err != nil {
			jr.ulog.Warn("Failed to auto-append agent transcript").Err(err).Log(ctx)
		} else {
			jr.ulog.Debug("Auto-appended agent transcript").Field("job_id", job.ID).Log(ctx)
		}

		// Archive workflow run artifacts alongside the transcript:
		// daemon-observed completions never pass through flow's CompleteJob
		// or the headless executor, so without this call workflow runs from
		// daemon-run jobs were silently lost. Idempotent overwrite;
		// warn-and-continue like the transcript above.
		if err := orchestration.ArchiveWorkflowRuns(job, plan); err != nil {
			jr.ulog.Warn("Failed to archive workflow runs").Err(err).Log(ctx)
		} else {
			jr.ulog.Debug("Archived workflow runs").Field("job_id", job.ID).Log(ctx)
		}
	}()
}
