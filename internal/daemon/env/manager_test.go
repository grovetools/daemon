package env

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
)

// TestRestore_NativeStateSurvives verifies that Restore does NOT delete
// state.json or .env.local for a native environment on daemon boot.
// On macOS, native processes are reparented to PID 1 when the daemon exits
// and stay alive; deleting their state file would orphan them from the
// TUI/CLI. grove env down is the authoritative teardown path.
func TestRestore_NativeStateSurvives(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "tier1-c")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	stateFile := coreenv.EnvStateFile{
		Provider: "native",
		Services: []coreenv.ServiceState{
			{Name: "clickhouse", Port: 49200, Status: "running"},
		},
		Ports: map[string]int{"CLICKHOUSE_PORT": 49200},
	}
	stateBytes, _ := json.MarshalIndent(stateFile, "", "  ")

	statePath := filepath.Join(stateDir, "state.json")
	if err := os.WriteFile(statePath, stateBytes, 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	envLocalStateDir := filepath.Join(stateDir, ".env.local")
	if err := os.WriteFile(envLocalStateDir, []byte("FOO=bar\n"), 0644); err != nil {
		t.Fatalf("write .env.local (stateDir): %v", err)
	}
	envLocalRoot := filepath.Join(wtPath, ".env.local")
	if err := os.WriteFile(envLocalRoot, []byte("FOO=bar\n"), 0644); err != nil {
		t.Fatalf("write .env.local (root): %v", err)
	}

	node := &workspace.WorkspaceNode{Name: "tier1-c", Path: wtPath}
	provider := workspace.NewProviderFromNodes([]*workspace.WorkspaceNode{node})

	m := NewManager()
	m.Restore(provider)

	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state.json was deleted by Restore: %v", err)
	}
	if _, err := os.Stat(envLocalStateDir); err != nil {
		t.Errorf(".env.local (stateDir) was deleted by Restore: %v", err)
	}
	if _, err := os.Stat(envLocalRoot); err != nil {
		t.Errorf(".env.local (root) was deleted by Restore: %v", err)
	}
}

// TestRestore_NativeSkipsEnvRegistration verifies that Restore does NOT
// add native envs to m.envs. Native processes can't be controlled across
// daemon restarts (we don't track PIDs), so re-registering would give the
// false impression that the daemon owns them. The state file stays on disk
// for informational purposes; grove env down is a no-op post-restart.
func TestRestore_NativeSkipsEnvRegistration(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "tier1-c")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	stateFile := coreenv.EnvStateFile{
		Provider: "native",
		Services: []coreenv.ServiceState{{Name: "api", Port: 3000, Status: "running"}},
	}
	stateBytes, _ := json.MarshalIndent(stateFile, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), stateBytes, 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	node := &workspace.WorkspaceNode{Name: "tier1-c", Path: wtPath}
	provider := workspace.NewProviderFromNodes([]*workspace.WorkspaceNode{node})

	m := NewManager()
	m.Restore(provider)

	if _, ok := m.envs["tier1-c"]; ok {
		t.Errorf("native env was wrongly registered in m.envs")
	}
}

// TestWriteStateFile_DaemonOwned verifies the daemon-side helper produces a
// state.json with workspace_name + workspace_path populated and the runtime
// view of ports/services merged in. This is the Phase 1 contract.
func TestWriteStateFile_DaemonOwned(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "tier1-c")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	m := NewManager()
	m.envs["tier1-c"] = &RunningEnv{
		Provider:        "native",
		Worktree:        "tier1-c",
		Environment:     "default",
		ManagedBy:       "user",
		StateDir:        stateDir,
		Ports:           map[string]int{"kitchen-api": 49301, "clickhouse": 49302},
		ServiceCommands: map[string]string{"kitchen-api": "cargo run"},
		ContainerNames:  map[string]string{"clickhouse": "grove-tier1-c-clickhouse"},
	}

	req := coreenv.EnvRequest{
		Provider:  "native",
		Profile:   "default",
		StateDir:  stateDir,
		ManagedBy: "user",
		Workspace: &workspace.WorkspaceNode{Name: "tier1-c", Path: wtPath},
	}
	resp := &coreenv.EnvResponse{
		Status:    "running",
		EnvVars:   map[string]string{"KITCHEN_API_PORT": "49301"},
		Endpoints: []string{"http://localhost:49301"},
	}

	if err := m.writeStateFile(t.Context(), req, resp); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}

	var got coreenv.EnvStateFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.WorkspaceName != "tier1-c" {
		t.Errorf("WorkspaceName = %q, want tier1-c", got.WorkspaceName)
	}
	if got.WorkspacePath != wtPath {
		t.Errorf("WorkspacePath = %q, want %q", got.WorkspacePath, wtPath)
	}
	if got.Provider != "native" {
		t.Errorf("Provider = %q, want native", got.Provider)
	}
	if got.Ports["kitchen-api"] != 49301 {
		t.Errorf("Ports[kitchen-api] = %d, want 49301", got.Ports["kitchen-api"])
	}
	if got.DockerContainers["clickhouse"] != "grove-tier1-c-clickhouse" {
		t.Errorf("DockerContainers[clickhouse] = %q, want grove-tier1-c-clickhouse", got.DockerContainers["clickhouse"])
	}
	if len(got.Services) != 2 {
		t.Errorf("Services count = %d, want 2", len(got.Services))
	}
}

// TestRemoveStateFile_NoOpWhenAbsent verifies that removeStateFile is
// idempotent — calling it twice (or against a path that never existed) is
// safe.
func TestRemoveStateFile_NoOpWhenAbsent(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	m := NewManager()
	req := coreenv.EnvRequest{
		StateDir:  stateDir,
		Workspace: &workspace.WorkspaceNode{Name: "t", Path: tmp},
	}
	// First call: no file exists. Should not error/log noisily.
	m.removeStateFile(t.Context(), req)
	// Second call after writing then removing.
	statePath := filepath.Join(stateDir, "state.json")
	if err := os.WriteFile(statePath, []byte("{}"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m.removeStateFile(t.Context(), req)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state.json still present after removeStateFile: %v", err)
	}
}

// TestRestore_DockerEnvRegistered verifies the non-native branch still
// re-hydrates m.envs from state.json so ports aren't re-allocated elsewhere.
func TestRestore_DockerEnvRegistered(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "tier1-a")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	stateFile := coreenv.EnvStateFile{
		Provider: "docker",
		Ports:    map[string]int{"API_PORT": 52000, "WEB_PORT": 52001},
	}
	stateBytes, _ := json.MarshalIndent(stateFile, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), stateBytes, 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	node := &workspace.WorkspaceNode{Name: "tier1-a", Path: wtPath}
	provider := workspace.NewProviderFromNodes([]*workspace.WorkspaceNode{node})

	m := NewManager()
	m.Restore(provider)

	re, ok := m.envs["tier1-a"]
	if !ok {
		t.Fatalf("docker env was not registered in m.envs")
	}
	if re.Provider != "docker" {
		t.Errorf("provider = %q, want docker", re.Provider)
	}
	if re.Ports["API_PORT"] != 52000 {
		t.Errorf("API_PORT port = %d, want 52000", re.Ports["API_PORT"])
	}
}
