package env

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/build"
	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
)

// TestBuildImagesIfStale_SkipOnFingerprintMatch verifies that when the
// prior state's fingerprint_<svc> matches the recomputed context hash,
// buildImagesIfStale returns the image tag but does NOT invoke docker
// (we confirm by never providing a docker binary — the code must not
// reach the exec step). It also confirms resp.State forwards the prior
// fingerprint so the next run still has a cache entry.
func TestBuildImagesIfStale_SkipOnFingerprintMatch(t *testing.T) {
	ws := t.TempDir()
	ctxDir := filepath.Join(ws, "app")
	if err := os.MkdirAll(ctxDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	df := filepath.Join(ctxDir, "Dockerfile")
	if err := os.WriteFile(df, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}

	hash, err := build.HashContext(ctxDir, df)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	m := NewManager()
	req := coreenv.EnvRequest{
		Provider: "docker",
		StateDir: filepath.Join(ws, ".grove", "env"),
		Workspace: &workspace.WorkspaceNode{
			Name: "wt-test",
			Path: ws,
		},
		Config: map[string]interface{}{
			"images": map[string]interface{}{
				"api": map[string]interface{}{
					"context":    "app",
					"dockerfile": "app/Dockerfile",
				},
			},
		},
	}
	resp := &coreenv.EnvResponse{State: map[string]string{}}
	prior := map[string]string{"fingerprint_api": hash}

	tags, err := m.buildImagesIfStale(context.Background(), req, resp, prior)
	if err != nil {
		t.Fatalf("buildImagesIfStale: %v", err)
	}
	if got := tags["api"]; got != "grove-wt-test-api:latest" {
		t.Errorf("want image tag grove-wt-test-api:latest, got %q", got)
	}
	if got := resp.State["fingerprint_api"]; got != hash {
		t.Errorf("want fingerprint forwarded %q, got %q", hash, got)
	}
}

// TestBuildImagesIfStale_NoImagesConfig verifies the helper is a no-op
// when req.Config has no "images" key — the docker provider must still
// work for compose-only setups without any grove-managed image builds.
func TestBuildImagesIfStale_NoImagesConfig(t *testing.T) {
	m := NewManager()
	resp := &coreenv.EnvResponse{State: map[string]string{}}
	tags, err := m.buildImagesIfStale(context.Background(), coreenv.EnvRequest{
		Workspace: &workspace.WorkspaceNode{Name: "x", Path: t.TempDir()},
	}, resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tags != nil {
		t.Errorf("expected nil map, got %v", tags)
	}
}

// TestForceRebuild exercises the request-level helper used by
// buildImagesIfStale to decide the rebuild reason.
func TestForceRebuild(t *testing.T) {
	cases := []struct {
		rebuild []string
		svc     string
		want    bool
	}{
		{nil, "api", false},
		{[]string{}, "api", false},
		{[]string{"all"}, "api", true},
		{[]string{"api"}, "api", true},
		{[]string{"web"}, "api", false},
		{[]string{"api", "web"}, "web", true},
	}
	for _, c := range cases {
		r := coreenv.EnvRequest{Rebuild: c.rebuild}
		if got := r.ForceRebuild(c.svc); got != c.want {
			t.Errorf("ForceRebuild(%v, %q) = %v, want %v", c.rebuild, c.svc, got, c.want)
		}
	}
}
