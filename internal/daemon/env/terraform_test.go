package env

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/sirupsen/logrus"
)

func TestMapTerraformOutputs_ExplicitMap(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	m := NewManager(logger.WithField("test", true))

	outputs := map[string]tfOutput{
		"db_url":      {Value: "postgres://localhost:5432/mydb"},
		"api_url":     {Value: "https://api.example.com"},
		"secret_key":  {Value: "supersecret", Sensitive: true},
		"unused_port": {Value: 9999},
	}

	config := map[string]interface{}{
		"output_env_map": map[string]interface{}{
			"db_url":  "DATABASE_URL",
			"api_url": "API_ENDPOINT",
		},
	}

	resp := &coreenv.EnvResponse{
		EnvVars: make(map[string]string),
	}

	m.mapTerraformOutputs(outputs, config, resp)

	if resp.EnvVars["DATABASE_URL"] != "postgres://localhost:5432/mydb" {
		t.Errorf("expected DATABASE_URL=postgres://localhost:5432/mydb, got %s", resp.EnvVars["DATABASE_URL"])
	}
	if resp.EnvVars["API_ENDPOINT"] != "https://api.example.com" {
		t.Errorf("expected API_ENDPOINT=https://api.example.com, got %s", resp.EnvVars["API_ENDPOINT"])
	}
	// Unmapped outputs should not appear
	if _, ok := resp.EnvVars["UNUSED_PORT"]; ok {
		t.Error("unmapped output should not appear when explicit map is provided")
	}
}

func TestMapTerraformOutputs_DefaultAutoExport(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	m := NewManager(logger.WithField("test", true))

	outputs := map[string]tfOutput{
		"db_url":     {Value: "postgres://localhost:5432/mydb"},
		"api_url":    {Value: "https://api.example.com"},
		"secret_key": {Value: "supersecret", Sensitive: true},
		"port":       {Value: "8080"},
	}

	config := map[string]interface{}{} // No explicit map

	resp := &coreenv.EnvResponse{
		EnvVars: make(map[string]string),
	}

	m.mapTerraformOutputs(outputs, config, resp)

	if resp.EnvVars["DB_URL"] != "postgres://localhost:5432/mydb" {
		t.Errorf("expected DB_URL, got %v", resp.EnvVars)
	}
	if resp.EnvVars["PORT"] != "8080" {
		t.Errorf("expected PORT=8080, got %s", resp.EnvVars["PORT"])
	}
	// Sensitive outputs should be excluded
	if _, ok := resp.EnvVars["SECRET_KEY"]; ok {
		t.Error("sensitive output should not be auto-exported")
	}
	// URL outputs should appear as endpoints
	found := false
	for _, ep := range resp.Endpoints {
		if ep == "https://api.example.com" {
			found = true
		}
	}
	if !found {
		t.Error("expected https://api.example.com in endpoints")
	}
}

func TestGroveContext_Write(t *testing.T) {
	dir := t.TempDir()

	gctx := groveContext{
		GroveEcosystem: "my-ecosystem",
		GroveProject:   "my-project",
		GroveWorktree:  "feature-123",
		GroveBranch:    "feature/issue-123",
		GrovePlanDir:   dir,
	}

	varsPath := filepath.Join(dir, "grove_context.auto.tfvars.json")
	varsBytes, err := json.MarshalIndent(gctx, "", "  ")
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if err := os.WriteFile(varsPath, varsBytes, 0644); err != nil {
		t.Fatalf("write error: %v", err)
	}

	// Read back and verify
	data, err := os.ReadFile(varsPath)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}

	var result groveContext
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if result.GroveEcosystem != "my-ecosystem" {
		t.Errorf("expected ecosystem my-ecosystem, got %s", result.GroveEcosystem)
	}
	if result.GroveWorktree != "feature-123" {
		t.Errorf("expected worktree feature-123, got %s", result.GroveWorktree)
	}
}

func TestTerraformUp_RequiresWorkspace(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	m := NewManager(logger.WithField("test", true))

	req := coreenv.EnvRequest{
		Provider: "terraform",
		PlanDir:  t.TempDir(),
	}

	_, err := m.terraformUp(nil, req)
	if err == nil {
		t.Error("expected error for nil workspace")
	}
}

func TestTerraformDown_NoState(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	m := NewManager(logger.WithField("test", true))

	tmpDir := t.TempDir()
	req := coreenv.EnvRequest{
		Provider:  "terraform",
		PlanDir:   tmpDir,
		Config:    map[string]interface{}{"path": "./infra"},
		Workspace: &workspace.WorkspaceNode{Name: "tf-test", Path: tmpDir},
	}

	resp, err := m.terraformDown(nil, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "stopped" {
		t.Errorf("expected status stopped, got %s", resp.Status)
	}
}
