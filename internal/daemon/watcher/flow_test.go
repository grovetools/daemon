package watcher

import (
	"os"
	"path/filepath"
	"testing"
)

func writeIndexedPlan(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".grove-plan.yml"), []byte("status: live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01-job.md"), []byte("---\nid: job\ntitle: job\ntype: oneshot\nstatus: pending\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIndexedPlansSeparatesArchiveContainerFromArchivedPlans(t *testing.T) {
	plansDir := t.TempDir()
	writeIndexedPlan(t, filepath.Join(plansDir, "live-plan"))
	writeIndexedPlan(t, filepath.Join(plansDir, ".archive", "old-plan"))
	writeIndexedPlan(t, filepath.Join(plansDir, ".artifacts", "not-a-plan"))

	got := loadIndexedPlans(plansDir)
	if len(got) != 2 {
		t.Fatalf("indexed %d entries, want live + archived: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, entry := range got {
		seen[entry.plan.Name] = entry.archived
		if entry.plan.Name == ".archive" {
			t.Fatal("archive container was indexed as a plan")
		}
	}
	if archived, ok := seen["live-plan"]; !ok || archived {
		t.Fatalf("live plan classification = %v, present=%v", archived, ok)
	}
	if archived, ok := seen["old-plan"]; !ok || !archived {
		t.Fatalf("archived plan classification = %v, present=%v", archived, ok)
	}
	if _, ok := seen["not-a-plan"]; ok {
		t.Fatal("hidden organizational directory was descended")
	}
}
