// Package jobrunner provides adoption logic for recovering orphaned agent processes.
// PHASE 2: When the daemon restarts, it scans for agents that were running and adopts them.
package jobrunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
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

	for _, job := range jobs {
		if job.Status != "running" {
			continue
		}

		if job.PID <= 0 {
			// No PID recorded — mark as failed ungraceful
			job.Status = "failed"
			job.Error = "daemon restarted with no recorded PID"
			now := time.Now()
			job.CompletedAt = &now
			jr.persister.Save(job)
			jr.store.ApplyUpdate(store.Update{
				Type:    store.UpdateJobFailed,
				Source:  "adoption",
				Payload: job,
			})
			failed++
			jr.ulog.Warn("Marked orphaned job as failed (no PID)").
				Field("job_id", job.ID).
				Log(ctx)
			continue
		}

		// Check if PID is still alive via kill(pid, 0)
		alive := jr.isPIDAlive(job.PID)
		if !alive {
			// Process is dead — try to read .status file for exit code
			statusPath := jr.getStatusFilePath(job)
			if statusContent, err := jr.readStatusFile(statusPath); err == nil {
				// .status file exists — use its exit code
				if statusContent.ExitCode == 0 {
					job.Status = "completed"
				} else {
					job.Status = "failed"
					job.Error = "agent exited with code: " + string(rune(statusContent.ExitCode))
				}
			} else {
				// No .status file — assume ungraceful crash
				job.Status = "failed"
				job.Error = "daemon restarted; agent process exited without status file"
			}

			now := time.Now()
			job.CompletedAt = &now
			jr.persister.Save(job)
			jr.store.ApplyUpdate(store.Update{
				Type:    store.UpdateJobFailed,
				Source:  "adoption",
				Payload: job,
			})
			failed++
			jr.ulog.Info("Adoption: process dead, marked job complete").
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

// isPIDAlive checks if a process ID is still running via kill(pid, 0).
// Returns true if the process exists, false otherwise.
func (jr *JobRunner) isPIDAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
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
						jr.markDone(job, "failed", "agent exited with code: "+string(rune(statusContent.ExitCode)))
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
