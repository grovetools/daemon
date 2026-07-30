package jobrunner

import (
	"context"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// submitForAgentTarget submits the shared hold-test plan's job with the given
// requested target and returns the JobInfo the runner built for it.
func submitForAgentTarget(t *testing.T, jr *JobRunner, planDir, requested string) *models.JobInfo {
	t.Helper()
	info, err := jr.Submit(context.Background(), models.JobSubmitRequest{
		PlanDir:     planDir,
		JobFile:     "01-job.md",
		AgentTarget: requested,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	return info
}

// A resubmission (`flow plan retry --run`, a re-run after an interrupted agent)
// must not lose the routing the first submission established. The store row is
// the fresher of the two sources, but it only survives until the job collector's
// next filesystem scan republishes the job without an agent_target.
func TestSubmit_RecoversAgentTargetFromStoreRecord(t *testing.T) {
	st := store.New()
	jr := newTestRunner(st)
	planDir := writeHoldTestPlan(t, "")

	first := submitForAgentTarget(t, jr, planDir, "tuimux")
	if first.AgentTarget != "tuimux" {
		t.Fatalf("first submission AgentTarget = %q, want %q", first.AgentTarget, "tuimux")
	}

	second := submitForAgentTarget(t, jr, planDir, "")
	if second.AgentTarget != "tuimux" {
		t.Errorf("resubmission AgentTarget = %q, want %q recovered from the store record", second.AgentTarget, "tuimux")
	}
}

// The persisted record is the source that survives a collector scan: it is
// written only by this runner, so it still carries the target after the store
// row has been replaced by a frontmatter-derived one.
func TestSubmit_RecoversAgentTargetFromPersistedRecordAfterScan(t *testing.T) {
	st := store.New()
	jr := newTestRunner(st)
	jr.persister = NewPersistenceWithDir(t.TempDir())
	planDir := writeHoldTestPlan(t, "")

	submitForAgentTarget(t, jr, planDir, "tuimux")

	// Exactly what collector.discoverJobsFromFilesystem publishes: a JobInfo
	// rebuilt from the job file's frontmatter, which knows no agent_target.
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateJobsDiscovered,
		Source: "job_collector",
		Payload: []*models.JobInfo{{
			ID:      "job-1",
			PlanDir: planDir,
			JobFile: "01-job.md",
			Type:    "shell",
			Status:  "running",
		}},
	})
	if scanned := st.GetJob("job-1"); scanned == nil || scanned.AgentTarget != "" {
		t.Fatalf("precondition: expected the scan to replace the row with an untargeted one, got %+v", scanned)
	}

	second := submitForAgentTarget(t, jr, planDir, "")
	if second.AgentTarget != "tuimux" {
		t.Errorf("resubmission AgentTarget = %q, want %q recovered from the persisted record", second.AgentTarget, "tuimux")
	}
}

// Recovery never overrides what the submitter asked for: a user who retries from
// a different terminal gets that terminal's routing, not the stale one.
func TestSubmit_ExplicitAgentTargetBeatsRecovery(t *testing.T) {
	st := store.New()
	jr := newTestRunner(st)
	jr.persister = NewPersistenceWithDir(t.TempDir())
	planDir := writeHoldTestPlan(t, "")

	submitForAgentTarget(t, jr, planDir, "tuimux")

	second := submitForAgentTarget(t, jr, planDir, "tmux")
	if second.AgentTarget != "tmux" {
		t.Errorf("resubmission AgentTarget = %q, want the explicitly requested %q", second.AgentTarget, "tmux")
	}
}

// A genuinely untagged FIRST submission keeps its empty target so the executor
// still fails hard on it — the daemon has no basis to invent routing, and that
// error is how a broken submission path gets noticed.
func TestSubmit_UntaggedFirstSubmissionStaysUntagged(t *testing.T) {
	jr := newTestRunner(store.New())
	jr.persister = NewPersistenceWithDir(t.TempDir())
	planDir := writeHoldTestPlan(t, "")

	info := submitForAgentTarget(t, jr, planDir, "")
	if info.AgentTarget != "" {
		t.Errorf("AgentTarget = %q, want empty for a first submission with nothing to recover", info.AgentTarget)
	}
}
