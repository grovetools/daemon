package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/flow/pkg/orchestration"
)

// writeTestJob writes a minimal flow job markdown with the given status and
// returns its path.
func writeTestJob(t *testing.T, dir, status string) string {
	t.Helper()
	path := filepath.Join(dir, "01-test-job.md")
	content := "---\n" +
		"id: test-job\n" +
		"title: test-job\n" +
		"status: " + status + "\n" +
		"type: interactive_agent\n" +
		"model: claude-opus-4-8\n" +
		"---\n\nbody\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write job: %v", err)
	}
	return path
}

func jobStatus(t *testing.T, path string) string {
	t.Helper()
	job, err := orchestration.LoadJob(path)
	if err != nil {
		t.Fatalf("LoadJob: %v", err)
	}
	return string(job.Status)
}

func registerSession(s *Store, jobID, jobFilePath string) {
	s.ApplyUpdate(Update{
		Type: UpdateSessionIntent,
		Payload: &SessionIntentPayload{
			JobID:       jobID,
			JobFilePath: jobFilePath,
			PlanName:    "test-plan",
			Title:       "test-job",
		},
	})
}

func setStatus(s *Store, jobID, status string) {
	s.ApplyUpdate(Update{
		Type:    UpdateSessionStatus,
		Payload: &SessionStatusPayload{JobID: jobID, Status: status},
	})
}

// TestSessionStatusSyncsToJobMarkdown is the Phase-3 keystone: a session status
// change to pending_user (and back to running) must be written into the job
// markdown front-matter for sessions that carry a JobFilePath.
func TestSessionStatusSyncsToJobMarkdown(t *testing.T) {
	dir := t.TempDir()
	jobPath := writeTestJob(t, dir, "running")

	s := New()
	registerSession(s, "test-job", jobPath)

	// running -> pending_user must be mirrored to the markdown.
	setStatus(s, "test-job", "pending_user")
	if got := jobStatus(t, jobPath); got != "pending_user" {
		t.Fatalf("after pending_user: job markdown status = %q, want pending_user", got)
	}

	// pending_user -> running must be mirrored back.
	setStatus(s, "test-job", "running")
	if got := jobStatus(t, jobPath); got != "running" {
		t.Fatalf("after running: job markdown status = %q, want running", got)
	}
}

// TestSessionStatusNeverDowngradesTerminal guards against clobbering a finished
// job: a late pending_user must NOT overwrite a completed/failed markdown.
func TestSessionStatusNeverDowngradesTerminal(t *testing.T) {
	for _, terminal := range []string{"completed", "failed", "abandoned"} {
		dir := t.TempDir()
		jobPath := writeTestJob(t, dir, terminal)

		s := New()
		registerSession(s, "test-job", jobPath)
		setStatus(s, "test-job", "pending_user")

		if got := jobStatus(t, jobPath); got != terminal {
			t.Fatalf("terminal %q was downgraded to %q", terminal, got)
		}
	}
}

// TestSessionStatusNoJobFilePathIsNoop ensures raw (non-flow) Claude sessions
// without a JobFilePath don't crash and have nothing to write.
func TestSessionStatusNoJobFilePathIsNoop(t *testing.T) {
	s := New()
	registerSession(s, "raw-session", "") // empty JobFilePath
	// Must not panic.
	setStatus(s, "raw-session", "pending_user")
}

// TestSessionStatusIgnoresUnrelatedTransitions ensures a non-pending/non-running
// status (e.g. idle) does not touch the markdown.
func TestSessionStatusIgnoresUnrelatedTransitions(t *testing.T) {
	dir := t.TempDir()
	jobPath := writeTestJob(t, dir, "running")

	s := New()
	registerSession(s, "test-job", jobPath)
	setStatus(s, "test-job", "idle")

	if got := jobStatus(t, jobPath); got != "running" {
		t.Fatalf("idle transition altered markdown to %q, want running", got)
	}
	_ = orchestration.JobStatusPendingUser
}
