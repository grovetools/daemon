package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/grovetools/core/pkg/models"
)

func artifactFixture(t *testing.T, jobID string) (string, string) {
	t.Helper()
	plan := t.TempDir()
	root := filepath.Join(plan, ".artifacts", jobID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.md"), []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	return plan, root
}

func TestBuildArtifactBundleRegularFiles(t *testing.T) {
	plan, _ := artifactFixture(t, "job")
	bundle, err := buildArtifactBundle(plan, "job")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Manifest.JobID != "job" || bundle.Manifest.Origin != "" || len(bundle.Files) != 1 || string(bundle.Files[0].Data) != "done" {
		t.Fatalf("unexpected bundle: %#v", bundle)
	}
}

func TestValidateAgentArtifactContentsRequiresCompletedArchive(t *testing.T) {
	bundle := &models.ArtifactBundle{Manifest: models.ArtifactManifest{Files: []models.ArtifactManifestEntry{
		{Path: "metadata.json"}, {Path: "final-report.md"}, {Path: "sessions/pi.jsonl"},
	}}}
	if err := validateAgentArtifactContents(bundle); err != nil {
		t.Fatal(err)
	}
	bundle.Manifest.Files = bundle.Manifest.Files[:2]
	if err := validateAgentArtifactContents(bundle); err == nil {
		t.Fatal("accepted publication without transcript")
	}
}

func TestBuildArtifactBundleRejectsFileTypesAndBounds(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		plan, root := artifactFixture(t, "job")
		if err := os.Symlink(filepath.Join(filepath.Dir(root), "other"), filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		if _, err := buildArtifactBundle(plan, "job"); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		plan, root := artifactFixture(t, "job")
		if err := os.Link(filepath.Join(root, "report.md"), filepath.Join(root, "second.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := buildArtifactBundle(plan, "job"); err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("device-like-fifo", func(t *testing.T) {
		plan, root := artifactFixture(t, "job")
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}
		if _, err := buildArtifactBundle(plan, "job"); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		plan, root := artifactFixture(t, "job")
		huge := filepath.Join(root, "huge")
		f, err := os.Create(huge)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if err := os.Truncate(huge, models.ArtifactBundleMaxBytes+1); err != nil {
			t.Fatal(err)
		}
		if _, err := buildArtifactBundle(plan, "job"); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("count", func(t *testing.T) {
		plan, root := artifactFixture(t, "job")
		for i := 0; i < models.ArtifactBundleMaxFiles; i++ {
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%03d", i)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := buildArtifactBundle(plan, "job"); err == nil || !strings.Contains(err.Error(), "count") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestBuildArtifactBundleRejectsCrossJobIdentity(t *testing.T) {
	plan, _ := artifactFixture(t, "job-a")
	if _, err := buildArtifactBundle(plan, "../job-a"); err == nil {
		t.Fatal("accepted traversal job identity")
	}
	if _, err := buildArtifactBundle(plan, "job-b"); err == nil {
		t.Fatal("accepted another job's root")
	}
}
