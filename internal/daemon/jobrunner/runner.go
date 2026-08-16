// Package jobrunner provides the daemon's job queue, worker pool, and execution engine.
// It wraps flow's LocalRuntime with concurrency control, persistence, and panic recovery.
package jobrunner

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/grovetools/core/config"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/jobattr"
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

	// adoptOnce makes AdoptRunningAgents a boot-time singleton. The sweep
	// loads every job record this machine has ever persisted (the store is a
	// file per job in a shared state dir, thousands deep on a working laptop),
	// starts a poller goroutine per live agent, and reconciles frontmatter —
	// none of which is idempotent-cheap enough to run twice.
	adoptOnce sync.Once

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

// Busy reports whether this runner has work in flight — a job executing, or one
// queued or blocked waiting to. A nil runner (jobs disabled) is never busy.
//
// Used by the scoped self-yield: a daemon may hand its scope to a host daemon
// only when nothing it owns would die with it.
func (jr *JobRunner) Busy() bool {
	if jr == nil {
		return false
	}
	jr.mu.RLock()
	running := len(jr.running)
	jr.mu.RUnlock()
	if running > 0 || len(jr.queue) > 0 {
		return true
	}
	jr.blockedMu.Lock()
	defer jr.blockedMu.Unlock()
	return len(jr.blocked) > 0
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

		// Resolve the live PTY set once, and only when a restored job actually
		// claims to be running — the query costs a round trip to the tuimux
		// daemon and most boots restore nothing running.
		var livePtyJobs map[string]bool
		for _, job := range restored {
			if job.Status == "running" {
				livePtyJobs = jr.livePtyJobIDs()
				break
			}
		}

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
				// A daemon restart is not an agent failure. Agent processes
				// live outside groved — under a PTY the mux owns, or as
				// detached process groups — so surviving a restart is their
				// designed behavior. This used to mark every such job failed
				// with no liveness check at all, which contradicted the agent
				// that went on working (and writing its transcript) for the
				// next hour, and clobbered the verdict adoption had just
				// reached for the same job.
				if jr.jobAgentAlive(job, livePtyJobs) {
					jr.ulog.Info("Restart recovery: agent still alive; job stays running").
						Field("job_id", job.ID).
						Field("pid", job.PID).
						Log(ctx)
					jr.store.ApplyUpdate(store.Update{
						Type:    store.UpdateJobStarted,
						Source:  "jobrunner",
						Payload: job,
					})
					continue
				}
				status, errMsg := jr.reconcileLostJob(job)
				jr.applyReconciledStatus(job, status, errMsg, "jobrunner")
				jr.ulog.Info("Restart recovery: no live agent process").
					Field("job_id", job.ID).
					Field("pid", job.PID).
					Field("status", status).
					Log(ctx)
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
	// Materialize a shipped plan bundle onto this node's replica plan dir
	// (M2 C12) before anything reads the plan from disk. The written files are
	// ordinary replica-notebook content from here on and converge home via M1
	// sync (C13 — no new sync machinery). Sets req.PlanDir to the local path.
	if req.PlanBundle != nil {
		planDir, err := jr.materializeBundle(ctx, req.PlanBundle)
		if err != nil {
			return nil, fmt.Errorf("materializing plan bundle: %w", err)
		}
		req.PlanDir = planDir
	}

	// F1 (C15): normalize the plan dir to an absolute path before it is stored
	// on the job and re-stat'd by orchestration.LoadPlan. A relative or
	// non-canonical PlanDir otherwise resolves against the daemon's cwd, so
	// LoadPlan opens the wrong (or a nonexistent) directory and dispatch fails
	// with "job not found". Covers both local dispatch and bundle materialize.
	if req.PlanDir != "" {
		if abs, err := filepath.Abs(req.PlanDir); err == nil {
			req.PlanDir = abs
		}
	}

	// Jobs run with no deadline unless the submitter sets an explicit,
	// parseable timeout. Hung jobs are handled by cancellation, not a
	// wall clock.
	timeoutStr := ""
	if req.Timeout != "" {
		if d, err := time.ParseDuration(req.Timeout); err == nil {
			timeoutStr = d.String()
		}
	}

	// One job, one record: the Flow job ID from the job file's frontmatter is
	// the identity. Minting a second key from the filename used to give a job
	// two daemon records — a typed one keyed by the Flow ID (which also keys
	// every artifact path, including the .status file adoption reads) and an
	// untyped one keyed by filename that owned nothing on disk yet still won
	// lookups. Fall back to a synthesized key only when the job file can't be
	// read, and carry the type either way so no record is shaped like the
	// duplicates were.
	jobID, jobType, jobTitle, jobWorktree, attemptID := jr.flowJobIdentity(req.PlanDir, req.JobFile)
	if jobID == "" {
		baseName := strings.TrimSuffix(req.JobFile, ".md")
		jobID = fmt.Sprintf("%s-%s", baseName, uuid.New().String()[:6])
		jr.ulog.Warn("Job frontmatter has no Flow ID; falling back to a filename-derived key").
			Field("job_id", jobID).
			Field("plan_dir", req.PlanDir).
			Field("job_file", req.JobFile).
			Log(ctx)
	}

	info := &models.JobInfo{
		ID:          jobID,
		AttemptID:   attemptID,
		Title:       jobTitle,
		Type:        jobType,
		PlanDir:     req.PlanDir,
		PlanName:    filepath.Base(req.PlanDir),
		JobFile:     req.JobFile,
		Priority:    req.Priority,
		TimeoutStr:  timeoutStr,
		Env:         req.Env,
		AgentTarget: req.AgentTarget,
		Status:      "queued",
		SubmittedAt: time.Now(),
	}

	// Now that submissions share the Flow job's identity, this record and the
	// one the filesystem scan discovered are the same record. Carry over the
	// workspace fields only the scan knows, so submitting a job doesn't blank
	// the repo/branch columns until the next scan restores them.
	if existing := jr.store.GetJob(jobID); existing != nil {
		if info.AttemptID == "" {
			info.AttemptID = existing.AttemptID
		}
		if info.Title == "" {
			info.Title = existing.Title
		}
		if info.Type == "" {
			info.Type = existing.Type
		}
		info.WorkDir = existing.WorkDir
		info.Repo = existing.Repo
		info.Branch = existing.Branch
		info.Channels = existing.Channels
	} else {
		// Nothing to carry over: this submission IS the row. Compute the trio
		// through the same helper the other producers use, so it cannot
		// disagree with the sweep that follows.
		info.WorkDir, info.Repo, info.Branch = jr.resolveWorkspace(req.PlanDir, jobWorktree)
	}

	// Same reasoning for routing, one step further out: a resubmission of a job
	// this daemon has already run (a retry, or a re-run after an interrupted
	// agent) must not lose the agent_target the first submission established.
	// Belt and braces only — every submitter is still expected to tag its own
	// jobs, and a genuinely untagged FIRST submission still fails hard in the
	// executor, because there is nothing to recover and guessing a mux from the
	// daemon's own environment would route agents into whatever terminal
	// happened to start groved.
	if info.AgentTarget == "" {
		if recovered := jr.lastKnownAgentTarget(jobID); recovered != "" {
			info.AgentTarget = recovered
			jr.ulog.Info("Recovered agent_target from this job's previous submission").
				Field("job_id", info.ID).
				Field("agent_target", recovered).
				Field("job_file", req.JobFile).
				Log(ctx)
		}
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

// lastKnownAgentTarget returns the agent_target a previous submission of this
// job recorded, or "" when this daemon has never seen one.
//
// It consults both of the daemon's records because each is lossy in a different
// way. The in-memory store row is authoritative immediately after a submit, but
// both filesystem producers — the periodic JobCollector sweep and the flow
// watcher's fsnotify path — republish jobs as JobInfo built from frontmatter
// alone, which knows nothing of agent_target, and UpdateJobsDiscovered replaces
// the row wholesale. The field is therefore gone from the store moments after
// submission, which is exactly the window a retry lands in. The on-disk record
// is written only by this runner, so it keeps the field until the next
// submission overwrites it — which is also why this lookup must happen here, in
// Submit, before Save() replaces it with the new record.
func (jr *JobRunner) lastKnownAgentTarget(jobID string) string {
	if existing := jr.store.GetJob(jobID); existing != nil && existing.AgentTarget != "" {
		return existing.AgentTarget
	}
	if jr.persister != nil {
		if saved := jr.persister.Get(jobID); saved != nil {
			return saved.AgentTarget
		}
	}
	return ""
}

// flowJobIdentity reads the Flow job's own identity — ID, type, title,
// worktree, and current execution attempt — from the plan.
func (jr *JobRunner) flowJobIdentity(planDir, jobFile string) (id string, jobType models.JobType, title, worktree, attemptID string) {
	plan, err := orchestration.LoadPlan(planDir)
	if err != nil {
		return "", "", "", "", ""
	}
	job, found := plan.GetJobByFilename(jobFile)
	if !found || job == nil {
		return "", "", "", "", ""
	}
	return job.ID, models.JobType(job.Type), job.Title, job.Worktree, job.AttemptID
}

// resolveWorkspace computes the WorkDir/Repo/Branch trio for a job the store
// has never seen, so a FIRST submission is attributed at submit time rather
// than waiting for a filesystem producer. Without it jobrunner is a third
// producer of the shared job row (see the jobattr package doc) that publishes
// those three fields empty — and not just briefly: UpdateJobsDiscovered SKIPS
// a row whose daemon status is "queued", so a blank-attributed queued job is
// passed over by every later sweep and persisted blank across restarts.
//
// The workspace set comes from the store's cache, not a fresh DiscoverAll:
// discovery is the multi-minute sweep this bug's window is made of. A cold
// store yields no answer, leaving the pre-existing scan-heals-it behavior.
func (jr *JobRunner) resolveWorkspace(planDir, worktreeName string) (workDir, repo, branch string) {
	if planDir == "" {
		return "", "", ""
	}
	enriched := jr.store.GetWorkspaces()
	if len(enriched) == 0 {
		return "", "", ""
	}
	nodes := make([]*workspace.WorkspaceNode, 0, len(enriched))
	for _, ws := range enriched {
		if ws != nil && ws.WorkspaceNode != nil {
			nodes = append(nodes, ws.WorkspaceNode)
		}
	}

	coreCfg, err := config.LoadDefault()
	if err != nil {
		return "", "", ""
	}
	locator := workspace.NewNotebookLocator(coreCfg)

	// planDir is "<plans root>/<plan name>"; GetPlansDir answers the root.
	plansRoot := filepath.Dir(planDir)
	var owner *workspace.WorkspaceNode
	for _, node := range nodes {
		dir, err := locator.GetPlansDir(node)
		if err != nil || dir != plansRoot {
			continue
		}
		// Every worktree in a group shares its plans dir; ScanForAllPlans
		// attributes the group to its main project, so prefer that here too
		// rather than whichever worktree happens to be enumerated first.
		if owner == nil || (owner.IsWorktree() && !node.IsWorktree()) {
			owner = node
		}
	}
	if owner == nil {
		return "", "", ""
	}

	workDir, repo, branch, _ = jobattr.JobWorkspace(
		jobattr.NewIndex(nodes), owner, worktreeName, owner.Path, owner.Name)
	return workDir, repo, branch
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

	// Only set CompletedAt for terminal states. "orphaned" is not one: it
	// records that the daemon lost track of the process, and stamping a
	// completion time would present that uncertainty as a finished run.
	if status != "pending_user" && status != "running" && status != "orphaned" {
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
	case "orphaned":
		updateType = store.UpdateJobOrphaned
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
		// LoadPlanLenient, not LoadPlan: this path wants ONE job's identity out
		// of the plan, and everything downstream of it reads only plan.Name,
		// plan.Directory and plan.Config (AppendAgentTranscript and
		// ArchiveWorkflowRuns splice files by path; neither touches a job's
		// PromptBody). LoadPlan materializes every job's prompt body, and a job
		// file accumulates its agent transcript — the perf-audit plan alone is
		// hundreds of KB per job across a hundred jobs, so the whole-plan read
		// showed up as 41.5 MB of allocation churn per completion on the
		// 2026-08-13 heap profile, to answer a question about one job.
		//
		// Leniency is also the better failure mode here: LoadPlan fails the
		// whole plan when any ONE job file has broken frontmatter, which cost a
		// completed job its transcript for a reason unrelated to it.
		plan, problems := orchestration.LoadPlanLenient(info.PlanDir)
		if plan == nil {
			jr.ulog.Warn("Failed to load plan for auto-transcript").
				Field("plan_dir", info.PlanDir).
				Field("problems", len(problems)).
				Log(ctx)
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
