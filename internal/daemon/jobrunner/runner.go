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
	"github.com/sirupsen/logrus"
)

// JobRunner manages the job queue, worker pool, and execution lifecycle.
type JobRunner struct {
	queue     chan *models.JobInfo
	workers   int
	running   map[string]context.CancelFunc
	runtime   orchestration.Runtime
	mu        sync.RWMutex
	store     *store.Store
	logger    *logrus.Entry
	persister *Persistence
}

// New creates a new JobRunner with the given store, runtime, and worker count.
func New(st *store.Store, runtime orchestration.Runtime, workers int, persister *Persistence) *JobRunner {
	if workers <= 0 {
		workers = 4
	}
	return &JobRunner{
		queue:     make(chan *models.JobInfo, 1000),
		workers:   workers,
		running:   make(map[string]context.CancelFunc),
		runtime:   runtime,
		store:     st,
		logger:    grovelogging.NewLogger("jobrunner"),
		persister: persister,
	}
}

// Start restores persisted queued jobs and launches the worker pool.
func (jr *JobRunner) Start(ctx context.Context) {
	// Restore queued jobs from persistence
	if jr.persister != nil {
		restored := jr.persister.Load()
		for _, job := range restored {
			if job.Status == "queued" {
				jr.logger.WithField("job_id", job.ID).Info("Restoring queued job")
				jr.store.ApplyUpdate(store.Update{
					Type:    store.UpdateJobSubmitted,
					Source:  "jobrunner",
					Payload: job,
				})
				jr.queue <- job
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
}

// Submit enqueues a new job for execution.
func (jr *JobRunner) Submit(ctx context.Context, req models.JobSubmitRequest) (*models.JobInfo, error) {
	timeout := 30 * time.Minute
	if req.Timeout != "" {
		if d, err := time.ParseDuration(req.Timeout); err == nil {
			timeout = d
		}
	}

	baseName := strings.TrimSuffix(req.JobFile, ".md")
	jobID := fmt.Sprintf("%s-%s", baseName, uuid.New().String()[:6])

	info := &models.JobInfo{
		ID:          jobID,
		PlanDir:     req.PlanDir,
		JobFile:     req.JobFile,
		Priority:    req.Priority,
		TimeoutStr:  timeout.String(),
		Env:         req.Env,
		Status:      "queued",
		SubmittedAt: time.Now(),
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
	jr.logger.WithFields(logrus.Fields{
		"job_id":   info.ID,
		"plan_dir": info.PlanDir,
		"job_file": info.JobFile,
	}).Info("Job submitted")

	return info, nil
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

	return fmt.Errorf("job %s is not running or queued", jobID)
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
			jr.logger.Errorf("Job %s panicked: %v", info.ID, r)
			jr.markDone(info, "failed", fmt.Sprintf("panic: %v", r))
			jr.cleanupRunning(info.ID)
		}
	}()

	timeout := 30 * time.Minute
	if info.TimeoutStr != "" {
		if d, err := time.ParseDuration(info.TimeoutStr); err == nil {
			timeout = d
		}
	}

	jobCtx, cancel := context.WithTimeout(ctx, timeout)
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

	jr.logger.WithFields(logrus.Fields{
		"job_id":   info.ID,
		"plan_dir": info.PlanDir,
		"job_file": info.JobFile,
	}).Info("Job started")

	// Load plan and execute
	plan, err := orchestration.LoadPlan(info.PlanDir)
	if err != nil {
		jr.markDone(info, "failed", fmt.Sprintf("load plan: %v", err))
		return
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
		jr.markDone(info, "completed", "")
	}
}

func (jr *JobRunner) markDone(info *models.JobInfo, status, errMsg string) {
	now := time.Now()
	info.Status = status
	info.Error = errMsg
	info.CompletedAt = &now

	if jr.persister != nil {
		jr.persister.Save(info)
	}

	updateType := store.UpdateJobCompleted
	switch status {
	case "failed":
		updateType = store.UpdateJobFailed
	case "cancelled":
		updateType = store.UpdateJobCancelled
	}
	jr.store.ApplyUpdate(store.Update{
		Type:    updateType,
		Source:  "jobrunner",
		Payload: info,
	})

	jr.logger.WithFields(logrus.Fields{
		"job_id": info.ID,
		"status": status,
		"error":  errMsg,
	}).Info("Job finished")
}

func (jr *JobRunner) cleanupRunning(jobID string) {
	jr.mu.Lock()
	if cancel, ok := jr.running[jobID]; ok {
		cancel()
		delete(jr.running, jobID)
	}
	jr.mu.Unlock()
}
