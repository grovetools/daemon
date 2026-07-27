package jobrunner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/models"
)

// One job must own one daemon record. Submissions used to mint a second,
// filename-derived key for a job that already had a Flow ID: the duplicate
// carried no type, owned nothing on disk (artifacts are keyed by the Flow ID),
// and still won lookups — which is how `aglogs` answered with a job.log instead
// of a transcript, and how restart recovery evaluated one job twice.
func TestCollapseDuplicatesMergesFilenameKeyedRecord(t *testing.T) {
	dir := t.TempDir()
	p := NewPersistenceWithDir(dir)

	planDir := t.TempDir()
	p.Save(&models.JobInfo{
		ID:      "git-status-mitigations-4cf449ea",
		Type:    "interactive_agent",
		PlanDir: planDir,
		JobFile: "34-git-status-mitigations.md",
		Status:  "running",
	})
	p.Save(&models.JobInfo{
		ID:          "34-git-status-mitigations-f9edf8",
		PlanDir:     planDir,
		JobFile:     "34-git-status-mitigations.md",
		Status:      "failed",
		AgentTarget: "native",
		LogFilePath: filepath.Join(planDir, ".artifacts", "git-status-mitigations-4cf449ea", "job.log"),
	})

	merged, removed := p.CollapseDuplicates()
	if merged == 0 || removed != 1 {
		t.Fatalf("expected the filename-keyed record to be merged and removed, merged=%d removed=%d", merged, removed)
	}

	if _, err := os.Stat(filepath.Join(dir, "34-git-status-mitigations-f9edf8.json")); !os.IsNotExist(err) {
		t.Fatalf("duplicate record still on disk, stat error = %v", err)
	}

	jobs := p.Load()
	if len(jobs) != 1 {
		t.Fatalf("expected exactly one record for the job, got %d", len(jobs))
	}
	survivor := jobs[0]
	if survivor.ID != "git-status-mitigations-4cf449ea" {
		t.Fatalf("the Flow job ID is the identity, got %q", survivor.ID)
	}
	if survivor.Status != "running" {
		t.Fatalf("the duplicate must not overwrite the canonical record's lifecycle state, got %q", survivor.Status)
	}
	if survivor.AgentTarget != "native" || survivor.LogFilePath == "" {
		t.Fatalf("launch details recorded only on the duplicate were lost: %+v", survivor)
	}
}

// A duplicate with no Flow-ID partner still has an identity — it is written in
// the job file's frontmatter.
func TestCollapseDuplicatesRekeysLoneRecordFromFrontmatter(t *testing.T) {
	dir := t.TempDir()
	p := NewPersistenceWithDir(dir)

	planDir := t.TempDir()
	jobFile := "12-some-job.md"
	content := "---\nid: some-job-abc123\ntitle: some job\ntype: headless_agent\nstatus: running\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(planDir, jobFile), []byte(content), 0o600); err != nil {
		t.Fatalf("writing job file: %v", err)
	}

	p.Save(&models.JobInfo{
		ID:      "12-some-job-aabbcc",
		PlanDir: planDir,
		JobFile: jobFile,
		Status:  "running",
		PID:     4321,
	})

	if _, removed := p.CollapseDuplicates(); removed != 1 {
		t.Fatalf("expected the filename-keyed record to be replaced, removed=%d", removed)
	}

	jobs := p.Load()
	if len(jobs) != 1 {
		t.Fatalf("expected one record, got %d", len(jobs))
	}
	if jobs[0].ID != "some-job-abc123" {
		t.Fatalf("record should be rekeyed to the Flow job ID, got %q", jobs[0].ID)
	}
	if jobs[0].Type != models.JobType("headless_agent") {
		t.Fatalf("every record must carry a type, got %q", jobs[0].Type)
	}
	if jobs[0].PID != 4321 {
		t.Fatalf("rekeying must preserve the record's contents, got %+v", jobs[0])
	}
}

// A record whose job file is gone can't have its identity recovered; guessing
// one would be worse than leaving stale history alone.
func TestCollapseDuplicatesLeavesUnresolvableRecordAlone(t *testing.T) {
	dir := t.TempDir()
	p := NewPersistenceWithDir(dir)

	p.Save(&models.JobInfo{
		ID:      "9-vanished-plan-ffeedd",
		PlanDir: filepath.Join(t.TempDir(), "gone"),
		JobFile: "9-vanished-plan.md",
		Status:  "failed",
	})

	if merged, removed := p.CollapseDuplicates(); merged != 0 || removed != 0 {
		t.Fatalf("expected no change, merged=%d removed=%d", merged, removed)
	}
	if len(p.Load()) != 1 {
		t.Fatal("the record must survive untouched")
	}
}

func TestIsFilenameKeyed(t *testing.T) {
	cases := []struct {
		id, jobFile string
		want        bool
	}{
		{"34-git-status-mitigations-f9edf8", "34-git-status-mitigations.md", true},
		{"git-status-mitigations-4cf449ea", "34-git-status-mitigations.md", false},
		{"34-git-status-mitigations", "34-git-status-mitigations.md", false},
		{"", "34-git-status-mitigations.md", false},
		{"34-git-status-mitigations-f9edf8", "", false},
	}
	for _, c := range cases {
		if got := isFilenameKeyed(c.id, c.jobFile); got != c.want {
			t.Errorf("isFilenameKeyed(%q, %q) = %v, want %v", c.id, c.jobFile, got, c.want)
		}
	}
}
