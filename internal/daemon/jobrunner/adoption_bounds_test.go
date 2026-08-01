package jobrunner

import (
	"context"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

// TestJobRecentlyActive pins the window that keeps boot recovery off the
// machine's entire job history. The persisted store is a file per job ever
// submitted — thousands deep — and every boot used to re-finalize all of them.
func TestJobRecentlyActive(t *testing.T) {
	recent := time.Now().Add(-time.Hour)
	ancient := time.Now().Add(-30 * 24 * time.Hour)

	cases := []struct {
		name string
		job  *models.JobInfo
		want bool
	}{
		{"completed recently", &models.JobInfo{CompletedAt: &recent}, true},
		{"completed weeks ago", &models.JobInfo{CompletedAt: &ancient}, false},
		{
			"completion beats an old start",
			&models.JobInfo{StartedAt: &ancient, CompletedAt: &recent},
			true,
		},
		{
			"a stale record that never completed falls back to StartedAt",
			&models.JobInfo{StartedAt: &ancient},
			false,
		},
		{"never started, submitted recently", &models.JobInfo{SubmittedAt: recent}, true},
		{"never started, submitted long ago", &models.JobInfo{SubmittedAt: ancient}, false},
		{"no timestamps at all is old", &models.JobInfo{}, false},
		{"nil", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobRecentlyActive(tc.job); got != tc.want {
				t.Fatalf("jobRecentlyActive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAdoptRunningAgentsRunsOnce is the duplicate-sweep guard. Adoption is boot
// recovery: a second pass re-walks every persisted job and starts a second
// poller goroutine per live agent. The observable here is the reconcile — a
// running job with a dead PID and no .status becomes "orphaned" once, and a
// record put back to "running" afterwards is left alone.
func TestAdoptRunningAgentsRunsOnce(t *testing.T) {
	jr, persister := newRecoveryRunner(t)
	planDir, jobFile := planWithJob(t, "once-job")

	persister.Save(&models.JobInfo{
		ID:      "once-job",
		Type:    "interactive_agent",
		PlanDir: planDir,
		JobFile: jobFile,
		Status:  "running",
		PID:     deadPID,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jr.AdoptRunningAgents(ctx)
	if got := reload(t, persister, "once-job").Status; got != "orphaned" {
		t.Fatalf("first sweep left status %q, want orphaned", got)
	}

	// Put the record back the way the first sweep found it. A second sweep
	// would reconcile it again; the once-guard means nothing happens.
	persister.Save(&models.JobInfo{
		ID:      "once-job",
		Type:    "interactive_agent",
		PlanDir: planDir,
		JobFile: jobFile,
		Status:  "running",
		PID:     deadPID,
	})
	jr.AdoptRunningAgents(ctx)
	if got := reload(t, persister, "once-job").Status; got != "running" {
		t.Fatalf("adoption ran a second time (status %q); it must be a boot-time singleton", got)
	}
}
