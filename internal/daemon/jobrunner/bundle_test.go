package jobrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

const minimalJob = `---
id: test-job-abc123
title: Test Job
status: pending
type: oneshot
---
body
`

// TestWriteBundleFilesRoundTripAndIdempotence covers materialize (C12): first
// write lands the files; a second identical write is a no-op skip.
func TestWriteBundleFilesRoundTripAndIdempotence(t *testing.T) {
	jr := newTestRunner(store.New())
	planDir := filepath.Join(t.TempDir(), "myplan")

	bundle := &models.PlanBundle{
		Workspace: "ws",
		PlanName:  "myplan",
		Files: map[string][]byte{
			"01-job.md":       []byte(minimalJob),
			".grove-plan.yml": []byte("status: active\n"),
			"rules/a.rules":   []byte("include: **/*.go\n"),
		},
	}

	if err := jr.writeBundleFiles(planDir, bundle); err != nil {
		t.Fatalf("first writeBundleFiles: %v", err)
	}
	for key, want := range bundle.Files {
		got, err := os.ReadFile(filepath.Join(planDir, filepath.FromSlash(key)))
		if err != nil {
			t.Fatalf("reading materialized %q: %v", key, err)
		}
		if string(got) != string(want) {
			t.Errorf("materialized %q = %q, want %q", key, got, want)
		}
	}

	// Second identical write must not error and must leave content intact.
	info, _ := os.Stat(filepath.Join(planDir, "01-job.md"))
	if err := jr.writeBundleFiles(planDir, bundle); err != nil {
		t.Fatalf("idempotent re-write: %v", err)
	}
	info2, _ := os.Stat(filepath.Join(planDir, "01-job.md"))
	if !info.ModTime().Equal(info2.ModTime()) {
		t.Errorf("byte-identical re-write rewrote the file (mtime changed)")
	}
}

// TestWriteBundleFilesRejectsTraversal covers the filepath.IsLocal guard (D2).
func TestWriteBundleFilesRejectsTraversal(t *testing.T) {
	jr := newTestRunner(store.New())
	planDir := filepath.Join(t.TempDir(), "myplan")
	bundle := &models.PlanBundle{
		Workspace: "ws", PlanName: "myplan",
		Files: map[string][]byte{"../escape.md": []byte("nope")},
	}
	if err := jr.writeBundleFiles(planDir, bundle); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(planDir), "escape.md")); err == nil {
		t.Fatal("traversal file was written outside the plan dir")
	}
}

// TestWriteBundleFilesRefusesRunningJobDiff covers D3: a differing file that a
// running job is using refuses the whole submit.
func TestWriteBundleFilesRefusesRunningJobDiff(t *testing.T) {
	st := store.New()
	jr := newTestRunner(st)
	planDir := filepath.Join(t.TempDir(), "myplan")

	v1 := &models.PlanBundle{Workspace: "ws", PlanName: "myplan", Files: map[string][]byte{"01-job.md": []byte("v1")}}
	if err := jr.writeBundleFiles(planDir, v1); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Register a running job on this plan dir + job file.
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateJobSubmitted,
		Source: "test",
		Payload: &models.JobInfo{
			ID: "r1", Status: "running", PlanDir: planDir, JobFile: "01-job.md",
		},
	})

	v2 := &models.PlanBundle{Workspace: "ws", PlanName: "myplan", Files: map[string][]byte{"01-job.md": []byte("v2-different")}}
	if err := jr.writeBundleFiles(planDir, v2); err == nil {
		t.Fatal("expected refusal when a running job's file differs")
	}
	// The running job's file must be untouched.
	got, _ := os.ReadFile(filepath.Join(planDir, "01-job.md"))
	if string(got) != "v1" {
		t.Errorf("running job's file was overwritten: %q", got)
	}
}

// TestSubmitNormalizesRelativePlanDir is the F1 regression (C15): Submit must
// store an ABSOLUTE plan dir so LoadPlan re-stats the right directory instead of
// resolving a relative path against the daemon cwd ("job not found").
func TestSubmitNormalizesRelativePlanDir(t *testing.T) {
	tmp := t.TempDir()
	planDir := filepath.Join(tmp, "myplan")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "01-job.md"), []byte(minimalJob), 0o600); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	jr := newTestRunner(store.New())
	info, err := jr.Submit(context.Background(), models.JobSubmitRequest{
		PlanDir: "myplan", // relative — the bug's trigger
		JobFile: "01-job.md",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !filepath.IsAbs(info.PlanDir) {
		t.Fatalf("F1 not fixed: PlanDir left relative: %q", info.PlanDir)
	}
	gotR, _ := filepath.EvalSymlinks(info.PlanDir)
	wantR, _ := filepath.EvalSymlinks(planDir)
	if gotR != wantR {
		t.Fatalf("PlanDir = %q, want %q", gotR, wantR)
	}
}
