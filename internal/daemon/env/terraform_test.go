package env

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
)

func TestMapTerraformOutputs_ExplicitMap(t *testing.T) {
	m := NewManager()

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

	// The http(s) value mapped via output_env_map should populate Endpoints.
	// The postgres URL should NOT.
	foundAPI := false
	for _, ep := range resp.Endpoints {
		if ep == "https://api.example.com" {
			foundAPI = true
		}
		if ep == "postgres://localhost:5432/mydb" {
			t.Error("non-http(s) value should not be treated as an endpoint")
		}
	}
	if !foundAPI {
		t.Errorf("expected https://api.example.com in endpoints (output_env_map path), got %v", resp.Endpoints)
	}
}

func TestMapTerraformOutputs_DisplayEndpointsFilter(t *testing.T) {
	m := NewManager()

	outputs := map[string]tfOutput{
		"api_url":     {Value: "https://api.example.com"},
		"admin_url":   {Value: "https://admin.example.com"},
		"console_url": {Value: "https://console.example.com"},
	}

	config := map[string]interface{}{
		"output_env_map": map[string]interface{}{
			"api_url":     "API_URL",
			"admin_url":   "ADMIN_URL",
			"console_url": "CONSOLE_URL",
		},
		// Only API_URL should surface as an endpoint — even though all three
		// are http(s) values.
		"display_endpoints": []interface{}{"API_URL"},
	}

	resp := &coreenv.EnvResponse{
		EnvVars: make(map[string]string),
	}

	m.mapTerraformOutputs(outputs, config, resp)

	// All three should be in EnvVars
	if resp.EnvVars["API_URL"] != "https://api.example.com" {
		t.Errorf("expected API_URL env var, got %v", resp.EnvVars)
	}
	if resp.EnvVars["ADMIN_URL"] != "https://admin.example.com" {
		t.Errorf("expected ADMIN_URL env var, got %v", resp.EnvVars)
	}

	// Only API_URL's value should be in Endpoints
	if len(resp.Endpoints) != 1 {
		t.Fatalf("expected exactly 1 endpoint (filtered via display_endpoints), got %d: %v", len(resp.Endpoints), resp.Endpoints)
	}
	if resp.Endpoints[0] != "https://api.example.com" {
		t.Errorf("expected filtered endpoint https://api.example.com, got %s", resp.Endpoints[0])
	}
}

func TestMapTerraformOutputs_DisplayEndpointsFilter_AutoExport(t *testing.T) {
	// Verify the filter also applies in the default/auto-export path.
	m := NewManager()

	outputs := map[string]tfOutput{
		"api_url":   {Value: "https://api.example.com"},
		"admin_url": {Value: "https://admin.example.com"},
	}

	config := map[string]interface{}{
		"display_endpoints": []interface{}{"API_URL"},
	}

	resp := &coreenv.EnvResponse{
		EnvVars: make(map[string]string),
	}

	m.mapTerraformOutputs(outputs, config, resp)

	if len(resp.Endpoints) != 1 || resp.Endpoints[0] != "https://api.example.com" {
		t.Errorf("expected exactly [https://api.example.com], got %v", resp.Endpoints)
	}
}

func TestMapTerraformOutputs_DefaultAutoExport(t *testing.T) {
	m := NewManager()

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

func TestGenerateEnvId(t *testing.T) {
	tests := []struct {
		name     string
		worktree string
	}{
		{"basic", "feature-123"},
		{"main", "main"},
		{"empty", ""},
		{"long", "this-is-a-very-long-worktree-name-for-testing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := generateEnvId(tt.worktree)
			if id < 1 || id > 255 {
				t.Errorf("expected env_id in [1,255], got %d", id)
			}
			// Verify determinism
			id2 := generateEnvId(tt.worktree)
			if id != id2 {
				t.Errorf("expected deterministic result, got %d and %d", id, id2)
			}
		})
	}

	// Verify different worktrees produce different IDs (high probability)
	id1 := generateEnvId("feature-a")
	id2 := generateEnvId("feature-b")
	if id1 == id2 {
		t.Logf("Warning: different worktrees produced same env_id (unlikely but possible): %d", id1)
	}
}

func TestBuildTfVarsPayload_BasicContext(t *testing.T) {
	dir := t.TempDir()
	req := coreenv.EnvRequest{
		Provider: "terraform",
		StateDir: dir,
		PlanDir:  dir,
		Config:   map[string]interface{}{},
		Workspace: &workspace.WorkspaceNode{
			Name:                "feature-123",
			Path:                dir,
			ParentEcosystemPath: "/tmp/my-ecosystem",
		},
	}

	payload, err := buildTfVarsPayload(req, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if payload["grove_worktree"] != "feature-123" {
		t.Errorf("expected grove_worktree=feature-123, got %v", payload["grove_worktree"])
	}
	if payload["grove_ecosystem"] != "my-ecosystem" {
		t.Errorf("expected grove_ecosystem=my-ecosystem, got %v", payload["grove_ecosystem"])
	}
	if payload["env_name"] != "feature-123" {
		t.Errorf("expected env_name=feature-123, got %v", payload["env_name"])
	}

	envId, ok := payload["grove_env_id"].(float64)
	if !ok {
		t.Fatalf("expected grove_env_id to be a number, got %T", payload["grove_env_id"])
	}
	if envId < 1 || envId > 255 {
		t.Errorf("expected grove_env_id in [1,255], got %v", envId)
	}
}

func TestBuildTfVarsPayload_UserVarsOverride(t *testing.T) {
	dir := t.TempDir()
	req := coreenv.EnvRequest{
		Provider: "terraform",
		StateDir: dir,
		PlanDir:  dir,
		Config: map[string]interface{}{
			"vars": map[string]interface{}{
				"env_name":   "custom-name",
				"project_id": "my-gcp-project",
				"region":     "us-central1",
			},
		},
		Workspace: &workspace.WorkspaceNode{
			Name: "feature-123",
			Path: dir,
		},
	}

	payload, err := buildTfVarsPayload(req, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// User override should win
	if payload["env_name"] != "custom-name" {
		t.Errorf("expected env_name=custom-name (user override), got %v", payload["env_name"])
	}
	// User vars should be present
	if payload["project_id"] != "my-gcp-project" {
		t.Errorf("expected project_id=my-gcp-project, got %v", payload["project_id"])
	}
	if payload["region"] != "us-central1" {
		t.Errorf("expected region=us-central1, got %v", payload["region"])
	}
	// Grove context should still be present
	if payload["grove_worktree"] != "feature-123" {
		t.Errorf("expected grove_worktree=feature-123, got %v", payload["grove_worktree"])
	}
}

func TestBuildTfVarsPayload_WithImageVars(t *testing.T) {
	dir := t.TempDir()
	req := coreenv.EnvRequest{
		Provider:  "terraform",
		StateDir:  dir,
		PlanDir:   dir,
		Config:    map[string]interface{}{},
		Workspace: &workspace.WorkspaceNode{Name: "main", Path: dir},
	}

	imageVars := map[string]string{
		"image_api": "gcr.io/proj/api:grove-main-12345",
		"image_web": "gcr.io/proj/web:grove-main-12345",
	}

	payload, err := buildTfVarsPayload(req, imageVars, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if payload["image_api"] != "gcr.io/proj/api:grove-main-12345" {
		t.Errorf("expected image_api, got %v", payload["image_api"])
	}
	if payload["image_web"] != "gcr.io/proj/web:grove-main-12345" {
		t.Errorf("expected image_web, got %v", payload["image_web"])
	}
}

func TestBuildTfVarsPayload_WithSharedOutputs(t *testing.T) {
	dir := t.TempDir()
	req := coreenv.EnvRequest{
		Provider:  "terraform",
		StateDir:  dir,
		PlanDir:   dir,
		Config:    map[string]interface{}{},
		Workspace: &workspace.WorkspaceNode{Name: "feature-x", Path: dir},
	}

	sharedOutputs := map[string]interface{}{
		"vpc_id":       "vpc-abc123",
		"subnet_id":    "subnet-def456",
		"registry_url": "us-docker.pkg.dev/proj/repo",
	}

	payload, err := buildTfVarsPayload(req, nil, sharedOutputs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	shared, ok := payload["shared"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected shared to be a map, got %T", payload["shared"])
	}
	if shared["vpc_id"] != "vpc-abc123" {
		t.Errorf("expected shared.vpc_id=vpc-abc123, got %v", shared["vpc_id"])
	}
	if shared["registry_url"] != "us-docker.pkg.dev/proj/repo" {
		t.Errorf("expected shared.registry_url, got %v", shared["registry_url"])
	}
}

func TestWriteTfVars(t *testing.T) {
	dir := t.TempDir()
	payload := map[string]interface{}{
		"grove_worktree": "feature-123",
		"env_name":       "feature-123",
		"grove_env_id":   42,
		"project_id":     "my-project",
	}

	varsPath, err := writeTfVars(dir, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(varsPath)
	if err != nil {
		t.Fatalf("failed to read tfvars: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal tfvars: %v", err)
	}

	if result["grove_worktree"] != "feature-123" {
		t.Errorf("expected grove_worktree=feature-123, got %v", result["grove_worktree"])
	}
	if result["project_id"] != "my-project" {
		t.Errorf("expected project_id=my-project, got %v", result["project_id"])
	}
}

func TestResolveBackend_Local(t *testing.T) {
	req := coreenv.EnvRequest{
		Config:    map[string]interface{}{},
		Workspace: &workspace.WorkspaceNode{Name: "test"},
	}

	bc := resolveBackend(req)
	if bc.Type != "local" {
		t.Errorf("expected local backend, got %s", bc.Type)
	}
}

func TestResolveBackend_GCS(t *testing.T) {
	req := coreenv.EnvRequest{
		Config: map[string]interface{}{
			"state_backend": "gcs",
			"state_bucket":  "my-bucket",
			"path":          "kitchen-app/infra",
			"branch":        "feature-x",
		},
		Workspace: &workspace.WorkspaceNode{
			Name:                "feature-x",
			Path:                t.TempDir(),
			ParentEcosystemPath: "/tmp/kitchen-env",
		},
	}

	bc := resolveBackend(req)
	if bc.Type != "gcs" {
		t.Errorf("expected gcs backend, got %s", bc.Type)
	}
	if bc.Bucket != "my-bucket" {
		t.Errorf("expected bucket=my-bucket, got %s", bc.Bucket)
	}
	// Prefix: <ecosystem>/<path_first_component>/<branch>
	if bc.Prefix != "kitchen-env/kitchen-app/feature-x" {
		t.Errorf("expected prefix=kitchen-env/kitchen-app/feature-x, got %s", bc.Prefix)
	}
}

func TestResolveBackend_GCS_NoBucket(t *testing.T) {
	req := coreenv.EnvRequest{
		Config: map[string]interface{}{
			"state_backend": "gcs",
			// No state_bucket — should fallback to local
		},
		Workspace: &workspace.WorkspaceNode{Name: "test"},
	}

	bc := resolveBackend(req)
	if bc.Type != "local" {
		t.Errorf("expected local fallback when bucket is missing, got %s", bc.Type)
	}
}

func TestWriteBackendOverride_GCS(t *testing.T) {
	dir := t.TempDir()
	bc := backendConfig{Type: "gcs", Bucket: "my-bucket", Prefix: "eco/wt"}

	overridePath, err := writeBackendOverride(dir, bc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overridePath == "" {
		t.Fatal("expected override path for GCS backend")
	}

	data, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatalf("failed to read override: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal override: %v", err)
	}

	tf, ok := result["terraform"].(map[string]interface{})
	if !ok {
		t.Fatal("expected terraform key in override")
	}
	backend, ok := tf["backend"].(map[string]interface{})
	if !ok {
		t.Fatal("expected backend key")
	}
	if _, ok := backend["gcs"]; !ok {
		t.Fatal("expected gcs backend block")
	}

	// Verify file name
	if filepath.Base(overridePath) != "_grove_backend_override.tf.json" {
		t.Errorf("unexpected file name: %s", filepath.Base(overridePath))
	}
}

func TestWriteBackendOverride_Local(t *testing.T) {
	dir := t.TempDir()
	bc := backendConfig{Type: "local"}

	overridePath, err := writeBackendOverride(dir, bc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overridePath != "" {
		t.Errorf("expected empty path for local backend, got %s", overridePath)
	}
}

func TestBuildInitArgs_Local(t *testing.T) {
	bc := backendConfig{Type: "local"}
	args := buildInitArgs(bc)

	if len(args) != 3 {
		t.Fatalf("expected 3 args for local, got %d: %v", len(args), args)
	}
	if args[0] != "init" || args[1] != "-input=false" || args[2] != "-reconfigure" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestBuildInitArgs_GCS(t *testing.T) {
	bc := backendConfig{Type: "gcs", Bucket: "my-bucket", Prefix: "eco/wt"}
	args := buildInitArgs(bc)

	if len(args) != 5 {
		t.Fatalf("expected 5 args for GCS, got %d: %v", len(args), args)
	}
	if args[3] != "-backend-config=bucket=my-bucket" {
		t.Errorf("expected bucket config, got %s", args[3])
	}
	if args[4] != "-backend-config=prefix=eco/wt" {
		t.Errorf("expected prefix config, got %s", args[4])
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
		EnvName:        "feature-123",
		GroveEnvId:     42,
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
	if result.EnvName != "feature-123" {
		t.Errorf("expected env_name feature-123, got %s", result.EnvName)
	}
	if result.GroveEnvId != 42 {
		t.Errorf("expected grove_env_id 42, got %d", result.GroveEnvId)
	}
}

func TestTerraformUp_RequiresWorkspace(t *testing.T) {
	m := NewManager()

	req := coreenv.EnvRequest{
		Provider: "terraform",
		PlanDir:  t.TempDir(),
	}

	_, err := m.terraformUp(context.TODO(), req)
	if err == nil {
		t.Error("expected error for nil workspace")
	}
}

func TestTerraformDown_NoState(t *testing.T) {
	m := NewManager()

	tmpDir := t.TempDir()
	req := coreenv.EnvRequest{
		Provider:  "terraform",
		PlanDir:   tmpDir,
		StateDir:  tmpDir,
		Config:    map[string]interface{}{"path": "./infra"},
		Workspace: &workspace.WorkspaceNode{Name: "tf-test", Path: tmpDir},
	}

	resp, err := m.terraformDown(context.TODO(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "stopped" {
		t.Errorf("expected status stopped, got %s", resp.Status)
	}
}

func TestTerraformDown_CleanFlag(t *testing.T) {
	m := NewManager()

	tmpDir := t.TempDir()

	// Create fake state artifacts (no terraform.tfstate so destroy is skipped,
	// but create .terraform dir and backup to test --clean removes them)
	os.MkdirAll(filepath.Join(tmpDir, ".terraform"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "terraform.tfstate.backup"), []byte("{}"), 0644)

	req := coreenv.EnvRequest{
		Provider:  "terraform",
		PlanDir:   tmpDir,
		StateDir:  tmpDir,
		Config:    map[string]interface{}{"path": "./infra"},
		Workspace: &workspace.WorkspaceNode{Name: "tf-clean-test", Path: tmpDir},
		Clean:     true,
	}

	resp, err := m.terraformDown(context.TODO(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "stopped" {
		t.Errorf("expected status stopped, got %s", resp.Status)
	}

	// Verify cleanup happened
	if _, err := os.Stat(filepath.Join(tmpDir, ".terraform")); !os.IsNotExist(err) {
		t.Error("expected .terraform directory to be removed with --clean")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "terraform.tfstate.backup")); !os.IsNotExist(err) {
		t.Error("expected terraform.tfstate.backup to be removed with --clean")
	}
}

// TestTerraformDown_SkipDestroy_HonoredByDefault asserts that with
// skip_destroy=true and no ForceDestroy flag, terraformDown returns
// early without proceeding to the terraform destroy + tfvars cleanup
// phase. Observable marker: a pre-existing grove_context.auto.tfvars.json
// should NOT be removed (the unconditional cleanup at the end of the
// non-skip path is never reached).
func TestTerraformDown_SkipDestroy_HonoredByDefault(t *testing.T) {
	m := NewManager()
	tmpDir := t.TempDir()
	tfvarsPath := filepath.Join(tmpDir, "grove_context.auto.tfvars.json")
	if err := os.WriteFile(tfvarsPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	req := coreenv.EnvRequest{
		Provider: "terraform",
		PlanDir:  tmpDir,
		StateDir: tmpDir,
		Config: map[string]interface{}{
			"path":         "./infra",
			"skip_destroy": true,
		},
		Workspace: &workspace.WorkspaceNode{Name: "tf-skip-test", Path: tmpDir},
	}

	resp, err := m.terraformDown(context.TODO(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "stopped" {
		t.Errorf("expected status stopped, got %s", resp.Status)
	}
	// skip_destroy should have forced early return BEFORE the
	// tfvars cleanup block — the file should still exist.
	if _, err := os.Stat(tfvarsPath); os.IsNotExist(err) {
		t.Error("expected tfvars preserved by skip_destroy early return; got removed")
	}
}

// TestTerraformDown_ForceDestroy_OverridesSkipDestroy asserts that
// with skip_destroy=true AND ForceDestroy=true, the early return is
// bypassed and the function proceeds to the destroy + cleanup phase.
// Observable marker: the pre-existing grove_context.auto.tfvars.json
// is removed by the unconditional cleanup at the end.
func TestTerraformDown_ForceDestroy_OverridesSkipDestroy(t *testing.T) {
	m := NewManager()
	tmpDir := t.TempDir()
	tfvarsPath := filepath.Join(tmpDir, "grove_context.auto.tfvars.json")
	if err := os.WriteFile(tfvarsPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	req := coreenv.EnvRequest{
		Provider: "terraform",
		PlanDir:  tmpDir,
		StateDir: tmpDir,
		Config: map[string]interface{}{
			"path":         "./infra",
			"skip_destroy": true,
		},
		Workspace:    &workspace.WorkspaceNode{Name: "tf-force-test", Path: tmpDir},
		ForceDestroy: true,
	}

	resp, err := m.terraformDown(context.TODO(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "stopped" {
		t.Errorf("expected status stopped, got %s", resp.Status)
	}
	// ForceDestroy bypasses skip_destroy early return, so the final
	// tfvars cleanup runs and removes the file.
	if _, err := os.Stat(tfvarsPath); !os.IsNotExist(err) {
		t.Error("expected tfvars removed (ForceDestroy bypassed skip_destroy); still present")
	}
}

func TestTerraformDown_NoCleanPreservesState(t *testing.T) {
	m := NewManager()

	tmpDir := t.TempDir()

	// Create fake state artifacts (no actual TF state, so destroy won't run)
	os.MkdirAll(filepath.Join(tmpDir, ".terraform"), 0755)

	req := coreenv.EnvRequest{
		Provider:  "terraform",
		PlanDir:   tmpDir,
		StateDir:  tmpDir,
		Config:    map[string]interface{}{"path": "./infra"},
		Workspace: &workspace.WorkspaceNode{Name: "tf-preserve-test", Path: tmpDir},
		Clean:     false,
	}

	_, err := m.terraformDown(context.TODO(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// .terraform should be preserved without --clean
	if _, err := os.Stat(filepath.Join(tmpDir, ".terraform")); os.IsNotExist(err) {
		t.Error("expected .terraform directory to be preserved without --clean")
	}
}
