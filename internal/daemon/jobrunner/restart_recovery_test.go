package jobrunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// deadPID is far above any live process id, so kill(pid, 0) fails without the
// test having to spawn and reap anything.
const deadPID = 4194303

// newRecoveryRunner builds a JobRunner wired to a temp persistence dir, with
// the session registry and daemon state redirected into the test's own tree so
// liveness resolution can't read the developer's real registry.
func newRecoveryRunner(t *testing.T) (*JobRunner, *Persistence) {
	t.Helper()
	t.Setenv("GROVE_HOME", t.TempDir())

	persister := NewPersistenceWithDir(t.TempDir())
	jr := newTestRunner(store.New())
	jr.persister = persister
	return jr, persister
}

func planWithJob(t *testing.T, id string) (planDir, jobFile string) {
	t.Helper()
	planDir = t.TempDir()
	jobFile = "1-" + id + ".md"
	content := "---\nid: " + id + "\ntitle: test job\ntype: interactive_agent\nstatus: running\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(planDir, jobFile), []byte(content), 0o600); err != nil {
		t.Fatalf("writing job file: %v", err)
	}
	return planDir, jobFile
}

func writeStatusFile(t *testing.T, planDir, jobID string, exitCode int) {
	t.Helper()
	dir := filepath.Join(planDir, ".artifacts", jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	data, err := json.Marshal(statusFileContent{ExitCode: exitCode, JobID: jobID, Timestamp: time.Now().Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".status"), data, 0o600); err != nil {
		t.Fatalf("write status: %v", err)
	}
}

func reload(t *testing.T, p *Persistence, jobID string) *models.JobInfo {
	t.Helper()
	for _, job := range p.Load() {
		if job.ID == jobID {
			return job
		}
	}
	t.Fatalf("job %s missing from persistence", jobID)
	return nil
}

// An agent process outlives the daemon by design — it runs under a PTY the mux
// owns, or as a detached process group. Restart recovery used to mark every
// job that was running as failed with no liveness check at all, contradicting
// an agent that then went on working (and writing its transcript) for another
// 53 minutes.
func TestStartLeavesRunningJobAloneWhenAgentIsAlive(t *testing.T) {
	jr, persister := newRecoveryRunner(t)
	planDir, jobFile := planWithJob(t, "live-job")

	persister.Save(&models.JobInfo{
		ID:      "live-job",
		Type:    "interactive_agent",
		PlanDir: planDir,
		JobFile: jobFile,
		Status:  "running",
		PID:     os.Getpid(), // this test process is unambiguously alive
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jr.Start(ctx)

	job := reload(t, persister, "live-job")
	if job.Status != "running" {
		t.Fatalf("a live agent's job must stay running, got %q (%s)", job.Status, job.Error)
	}
	if job.CompletedAt != nil {
		t.Fatalf("a live agent's job must not be stamped completed_at")
	}
}

// Losing track of a process is a claim about the daemon, not a verdict on the
// agent's work, so it gets a distinct non-terminal state instead of failed.
func TestStartMarksUnverifiableJobOrphanedNotFailed(t *testing.T) {
	jr, persister := newRecoveryRunner(t)
	planDir, jobFile := planWithJob(t, "lost-job")

	persister.Save(&models.JobInfo{
		ID:      "lost-job",
		Type:    "interactive_agent",
		PlanDir: planDir,
		JobFile: jobFile,
		Status:  "running",
		PID:     deadPID,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jr.Start(ctx)

	job := reload(t, persister, "lost-job")
	if job.Status != "orphaned" {
		t.Fatalf("expected orphaned for unknown liveness, got %q", job.Status)
	}
	if isJobTerminal(job.Status) {
		t.Fatal("orphaned must not be a terminal status")
	}
	if job.CompletedAt != nil {
		t.Fatal("orphaned must not stamp completed_at — the job did not complete")
	}
}

// A recorded exit is real evidence, and still reconciles to a terminal state.
func TestStartReconcilesFromStatusFile(t *testing.T) {
	jr, persister := newRecoveryRunner(t)
	planDir, jobFile := planWithJob(t, "exited-job")
	writeStatusFile(t, planDir, "exited-job", 0)

	persister.Save(&models.JobInfo{
		ID:      "exited-job",
		Type:    "headless_agent",
		PlanDir: planDir,
		JobFile: jobFile,
		Status:  "running",
		PID:     deadPID,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jr.Start(ctx)

	job := reload(t, persister, "exited-job")
	if job.Status != "completed" {
		t.Fatalf("expected completed from a zero-exit .status, got %q (%s)", job.Status, job.Error)
	}
	if job.CompletedAt == nil {
		t.Fatal("a terminal status must carry completed_at")
	}
}

func TestReconcileLostJobReportsNonZeroExitAsFailed(t *testing.T) {
	jr, _ := newRecoveryRunner(t)
	planDir, jobFile := planWithJob(t, "failed-job")
	writeStatusFile(t, planDir, "failed-job", 3)

	status, errMsg := jr.reconcileLostJob(&models.JobInfo{
		ID: "failed-job", PlanDir: planDir, JobFile: jobFile,
	})
	if status != "failed" {
		t.Fatalf("a recorded non-zero exit is evidence of failure, got %q", status)
	}
	if errMsg == "" {
		t.Fatal("failure must carry the exit code in its message")
	}
}

// The session registry knows the agent's own PID; job.PID may name a launcher
// that has long exited.
func TestJobAgentAliveUsesSessionRegistryPID(t *testing.T) {
	jr, _ := newRecoveryRunner(t)

	registryDir := filepath.Join(os.Getenv("GROVE_HOME"), "state", "grove", "hooks", "sessions", "native-1")
	if err := os.MkdirAll(registryDir, 0o755); err != nil {
		t.Fatalf("mkdir registry: %v", err)
	}
	metadata := map[string]any{
		"session_id":        "pty-job",
		"job_id":            "pty-job",
		"claude_session_id": "native-1",
		"pid":               os.Getpid(),
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(registryDir, "metadata.json"), data, 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	job := &models.JobInfo{ID: "pty-job", Type: "interactive_agent", PID: deadPID}
	if !jr.jobAgentAlive(job, map[string]bool{}) {
		t.Fatal("a live PID in the session registry means the agent is alive, whatever job.PID says")
	}
}

// A live PTY carrying the job's id is direct evidence the agent survived.
func TestJobAgentAliveUsesLivePTYTag(t *testing.T) {
	jr, _ := newRecoveryRunner(t)

	job := &models.JobInfo{ID: "pty-job", Type: "interactive_agent", PID: deadPID}
	if !jr.jobAgentAlive(job, map[string]bool{"pty-job": true}) {
		t.Fatal("a live PTY tagged with the job id means the agent is alive")
	}
	if jr.jobAgentAlive(job, map[string]bool{"other-job": true}) {
		t.Fatal("a PTY belonging to another job says nothing about this one")
	}
}
