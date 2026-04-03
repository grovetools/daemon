package jobrunner

import (
	"context"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

func newTestRunner(st *store.Store) *JobRunner {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	return &JobRunner{
		store:   st,
		blocked: make(map[string]*models.JobInfo),
		queue:   make(chan *models.JobInfo, 100),
		running: make(map[string]context.CancelFunc),
		logger:  logger.WithField("test", true),
	}
}

func TestIsJobTerminal(t *testing.T) {
	tests := []struct {
		status   string
		expected bool
	}{
		{"completed", true},
		{"failed", true},
		{"cancelled", true},
		{"idle", true},
		{"interrupted", true},
		{"abandoned", true},
		{"running", false},
		{"queued", false},
		{"pending", false},
		{"pending_user", false},
		{"todo", false},
		{"blocked", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := isJobTerminal(tt.status)
			if got != tt.expected {
				t.Errorf("isJobTerminal(%q) = %v, want %v", tt.status, got, tt.expected)
			}
		})
	}
}

func TestWatchTransitions_TerminalAgentTriggersProcessing(t *testing.T) {
	st := store.New()

	// Pre-populate a terminal interactive_agent job
	agentJob := &models.JobInfo{
		ID:     "agent-job-1",
		Status: "completed",
		Type:   "interactive_agent",
	}

	jr := newTestRunner(st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the watcher
	go jr.watchTransitions(ctx)

	// Give the watcher time to subscribe
	time.Sleep(50 * time.Millisecond)

	// Publish a terminal agent job via UpdateJobsDiscovered
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobsDiscovered,
		Source:  "collector",
		Payload: []*models.JobInfo{agentJob},
	})

	// Give the watcher time to process (appendTranscriptAsync will fail gracefully
	// since there's no real plan, but it shouldn't panic)
	time.Sleep(100 * time.Millisecond)

	// Cancel to stop the watcher
	cancel()
}

func TestWatchTransitions_IgnoresNonTerminal(t *testing.T) {
	st := store.New()

	jr := newTestRunner(st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go jr.watchTransitions(ctx)
	time.Sleep(50 * time.Millisecond)

	// Publish a running (non-terminal) job
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateJobsDiscovered,
		Source: "collector",
		Payload: []*models.JobInfo{
			{ID: "running-job", Status: "running", Type: "interactive_agent"},
		},
	})

	// No panic or hang expected
	time.Sleep(100 * time.Millisecond)
	cancel()
}

func TestWatchTransitions_Deduplication(t *testing.T) {
	st := store.New()

	jr := newTestRunner(st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go jr.watchTransitions(ctx)
	time.Sleep(50 * time.Millisecond)

	job := &models.JobInfo{
		ID:     "dedup-job",
		Status: "completed",
		Type:   "shell", // non-agent, so evaluateBlockedJobs is called instead of appendTranscriptAsync
	}

	// Send the same terminal job twice
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobsDiscovered,
		Source:  "collector",
		Payload: []*models.JobInfo{job},
	})
	time.Sleep(50 * time.Millisecond)

	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobsDiscovered,
		Source:  "collector",
		Payload: []*models.JobInfo{job},
	})

	// Second update should be a no-op (deduplication via processed map)
	time.Sleep(100 * time.Millisecond)
	cancel()
}

func TestWatchTransitions_ClearsProcessedOnRevert(t *testing.T) {
	st := store.New()

	jr := newTestRunner(st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go jr.watchTransitions(ctx)
	time.Sleep(50 * time.Millisecond)

	jobID := "revert-job"

	// First: terminal state (processed)
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateJobsDiscovered,
		Source: "collector",
		Payload: []*models.JobInfo{
			{ID: jobID, Status: "completed", Type: "shell"},
		},
	})
	time.Sleep(50 * time.Millisecond)

	// Revert to non-terminal (should clear from processed map)
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateJobsDiscovered,
		Source: "collector",
		Payload: []*models.JobInfo{
			{ID: jobID, Status: "running", Type: "shell"},
		},
	})
	time.Sleep(50 * time.Millisecond)

	// Terminal again (should be processed anew, not deduplicated)
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateJobsDiscovered,
		Source: "collector",
		Payload: []*models.JobInfo{
			{ID: jobID, Status: "completed", Type: "shell"},
		},
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
}

func TestWatchTransitions_UpdateJobCompleted(t *testing.T) {
	st := store.New()

	jr := newTestRunner(st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go jr.watchTransitions(ctx)
	time.Sleep(50 * time.Millisecond)

	// Send UpdateJobCompleted (single job payload)
	job := &models.JobInfo{
		ID:     "completed-job",
		Status: "completed",
		Type:   "shell",
	}
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobCompleted,
		Source:  "jobrunner",
		Payload: job,
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
}

func TestWatchTransitions_SessionEnd(t *testing.T) {
	st := store.New()

	// Pre-populate a job in the store so GetJob can find it
	agentJob := &models.JobInfo{
		ID:     "session-end-job",
		Status: "completed",
		Type:   "interactive_agent",
	}
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobSubmitted,
		Source:  "test",
		Payload: agentJob,
	})

	jr := newTestRunner(st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go jr.watchTransitions(ctx)
	time.Sleep(50 * time.Millisecond)

	// Send UpdateSessionEnd
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessionEnd,
		Source: "session",
		Payload: &store.SessionEndPayload{
			JobID:   "session-end-job",
			Outcome: "completed",
		},
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
}
