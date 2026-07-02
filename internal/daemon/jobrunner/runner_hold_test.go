package jobrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// writeHoldTestPlan creates a minimal plan directory with one runnable shell
// job and the given .grove-plan.yml contents, returning the plan dir.
func writeHoldTestPlan(t *testing.T, planConfig string) string {
	t.Helper()
	dir := t.TempDir()

	job := `---
id: job-1
title: First Job
status: pending
type: shell
---
echo hello`
	if err := os.WriteFile(filepath.Join(dir, "01-job.md"), []byte(job), 0o600); err != nil {
		t.Fatalf("writing job file: %v", err)
	}
	if planConfig != "" {
		if err := os.WriteFile(filepath.Join(dir, ".grove-plan.yml"), []byte(planConfig), 0o600); err != nil {
			t.Fatalf("writing plan config: %v", err)
		}
	}
	return dir
}

func TestAreDependenciesMet_HeldPlanBlocksJob(t *testing.T) {
	jr := newTestRunner(store.New())

	heldDir := writeHoldTestPlan(t, "status: hold\n")
	info := &models.JobInfo{ID: "j1", PlanDir: heldDir, JobFile: "01-job.md"}
	if jr.areDependenciesMet(info) {
		t.Error("areDependenciesMet() = true for a held plan, want false")
	}

	// Same plan without the hold: the pending job is runnable.
	openDir := writeHoldTestPlan(t, "")
	info = &models.JobInfo{ID: "j2", PlanDir: openDir, JobFile: "01-job.md"}
	if !jr.areDependenciesMet(info) {
		t.Error("areDependenciesMet() = false for a plan not on hold, want true")
	}
}

func TestSubmit_HeldPlanJobStaysBlocked(t *testing.T) {
	jr := newTestRunner(store.New())
	heldDir := writeHoldTestPlan(t, "status: hold\n")

	info, err := jr.Submit(context.Background(), models.JobSubmitRequest{
		PlanDir: heldDir,
		JobFile: "01-job.md",
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if info.Status != "blocked" {
		t.Errorf("submitted job status = %q, want %q", info.Status, "blocked")
	}

	jr.blockedMu.Lock()
	_, inBlocked := jr.blocked[info.ID]
	jr.blockedMu.Unlock()
	if !inBlocked {
		t.Error("held plan's job not in blocked queue")
	}
	if len(jr.queue) != 0 {
		t.Errorf("run queue length = %d, want 0", len(jr.queue))
	}

	// evaluateBlockedJobs must NOT promote the job while the hold persists.
	jr.evaluateBlockedJobs()
	if len(jr.queue) != 0 {
		t.Errorf("run queue length after evaluateBlockedJobs = %d, want 0 (plan still held)", len(jr.queue))
	}

	// Lifting the hold promotes the job on the next evaluation pass.
	if err := os.WriteFile(filepath.Join(heldDir, ".grove-plan.yml"), []byte("status: \"\"\n"), 0o600); err != nil {
		t.Fatalf("clearing hold: %v", err)
	}
	jr.evaluateBlockedJobs()
	if len(jr.queue) != 1 {
		t.Errorf("run queue length after unhold = %d, want 1", len(jr.queue))
	}
}

func TestExecuteJob_HeldPlanReblocksInsteadOfRunning(t *testing.T) {
	jr := newTestRunner(store.New())
	heldDir := writeHoldTestPlan(t, "status: hold\n")

	// Simulate a job that was already dequeued when the hold was set.
	info := &models.JobInfo{
		ID:      "queued-then-held",
		PlanDir: heldDir,
		JobFile: "01-job.md",
		Status:  "queued",
	}

	jr.executeJob(context.Background(), info)

	if info.Status != "blocked" {
		t.Errorf("job status after executeJob on held plan = %q, want %q", info.Status, "blocked")
	}
	if info.StartedAt != nil {
		t.Error("StartedAt not cleared for re-blocked job")
	}
	if info.CompletedAt != nil {
		t.Error("re-blocked job must not be marked completed/failed")
	}

	jr.blockedMu.Lock()
	_, inBlocked := jr.blocked[info.ID]
	jr.blockedMu.Unlock()
	if !inBlocked {
		t.Error("re-blocked job not in blocked queue")
	}
}
