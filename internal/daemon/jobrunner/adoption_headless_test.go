package jobrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// writeHeadlessJobOnDisk writes a minimal plan dir with one headless job .md at
// the given frontmatter status and returns the JobInfo adoption would carry.
func writeHeadlessJobOnDisk(t *testing.T, status string) (planDir string, info *models.JobInfo) {
	t.Helper()
	planDir = t.TempDir()
	// Isolate grove state so any daemon.New()/EndSession is a no-op that never
	// touches real state.
	t.Setenv("GROVE_HOME", filepath.Join(planDir, "grovehome"))

	content := "---\nid: hjob\ntitle: headless job\nstatus: " + status +
		"\ntype: headless_agent\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(planDir, "hjob.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	info = &models.JobInfo{
		ID:      "hjob",
		PlanDir: planDir,
		JobFile: "hjob.md",
		Type:    models.JobType("headless_agent"),
	}
	return planDir, info
}

func writeStatusOnDisk(t *testing.T, planDir, jobID string, exitCode int) {
	t.Helper()
	dir := filepath.Join(planDir, ".artifacts", jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(fmt.Sprintf(`{"exit_code":%d,"timestamp":%q,"job_id":%q}`,
		exitCode, time.Now().Format(time.RFC3339), jobID))
	if err := os.WriteFile(filepath.Join(dir, ".status"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFinalizeHeadlessFrontmatter_DeadPIDFailed asserts adoption's headless
// finalize hook drives the job-file frontmatter to a terminal state from the
// .status file (nonzero exit → failed), the reconciliation the strand fix adds
// on top of markDone's JobInfo update.
func TestFinalizeHeadlessFrontmatter_DeadPIDFailed(t *testing.T) {
	planDir, info := writeHeadlessJobOnDisk(t, "idle")
	writeStatusOnDisk(t, planDir, info.ID, 3)

	jr := newTestRunner(store.New())
	jr.finalizeHeadlessFrontmatter(context.Background(), info)

	content, err := os.ReadFile(filepath.Join(planDir, "hjob.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "status: failed") {
		t.Errorf("expected frontmatter reconciled to failed, got:\n%s", content)
	}
	if !strings.Contains(string(content), "code: 3") {
		t.Errorf("expected last_error to carry exit code, got:\n%s", content)
	}
}

// TestFinalizeHeadlessFrontmatter_NonHeadlessNoop asserts the hook is a no-op
// for non-headless jobs (it must never touch interactive/isolated frontmatter).
func TestFinalizeHeadlessFrontmatter_NonHeadlessNoop(t *testing.T) {
	planDir, info := writeHeadlessJobOnDisk(t, "idle")
	info.Type = models.JobType("interactive_agent")
	writeStatusOnDisk(t, planDir, info.ID, 3)

	before, _ := os.ReadFile(filepath.Join(planDir, "hjob.md"))
	jr := newTestRunner(store.New())
	jr.finalizeHeadlessFrontmatter(context.Background(), info)
	after, _ := os.ReadFile(filepath.Join(planDir, "hjob.md"))

	if string(before) != string(after) {
		t.Errorf("expected no change for non-headless job\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
