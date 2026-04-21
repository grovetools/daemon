package env

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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

	m := NewManager()
	m.Restore([]string{tmp})

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
		Provider:      "native",
		WorkspaceName: "tier1-c",
		WorkspacePath: wtPath,
		Services:      []coreenv.ServiceState{{Name: "api", Port: 3000, Status: "running"}},
	}
	stateBytes, _ := json.MarshalIndent(stateFile, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), stateBytes, 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	m := NewManager()
	m.Restore([]string{tmp})

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

// fakeSupervisor records every Stop call for assertion in disk-lazy tests.
type fakeSupervisor struct {
	stops []int
}

func (f *fakeSupervisor) Spawn(_ context.Context, _ string, cmd *exec.Cmd) (int, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func (f *fakeSupervisor) Stop(pgid int) error {
	f.stops = append(f.stops, pgid)
	return nil
}

// TestNativeDown_DiskLazyAfterRestart simulates the post-restart scenario:
// state.json on disk has NativePGIDs but m.envs is empty. nativeDown must
// signal each persisted PGID via the Supervisor (not silently no-op).
func TestNativeDown_DiskLazyAfterRestart(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "tier1-c")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	stateFile := coreenv.EnvStateFile{
		Provider:      "native",
		WorkspaceName: "tier1-c",
		WorkspacePath: wtPath,
		NativePGIDs:   map[string]int{"kitchen-api": 99001, "tunnel-clickhouse": 99002},
	}
	data, _ := json.MarshalIndent(&stateFile, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), data, 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	m := NewManager()
	fake := &fakeSupervisor{}
	m.Supervisor = fake

	req := coreenv.EnvRequest{
		Provider:  "native",
		StateDir:  stateDir,
		Workspace: &workspace.WorkspaceNode{Name: "tier1-c", Path: wtPath},
	}
	resp, err := m.Down(t.Context(), req)
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if resp == nil || resp.Status != "stopped" {
		t.Errorf("Down resp = %+v, want status stopped", resp)
	}

	if len(fake.stops) != 2 {
		t.Errorf("Supervisor.Stop calls = %d, want 2 (got %v)", len(fake.stops), fake.stops)
	}
	gotPgids := map[int]bool{}
	for _, p := range fake.stops {
		gotPgids[p] = true
	}
	if !gotPgids[99001] || !gotPgids[99002] {
		t.Errorf("Supervisor.Stop pgids = %v, want both 99001 and 99002", fake.stops)
	}

	// state.json should be removed after a successful Down.
	if _, err := os.Stat(filepath.Join(stateDir, "state.json")); !os.IsNotExist(err) {
		t.Errorf("state.json still present after disk-lazy Down: %v", err)
	}
}

// TestDown_PatchesWorkspaceFromStateFile verifies that when state.json has
// authoritative WorkspaceName/Path that differs from req.Workspace, the
// daemon trusts the on-disk identity. This protects against the parent-
// ecosystem-vs-worktree node mix-up flow/cmd/plan_init.go used to hit.
func TestDown_PatchesWorkspaceFromStateFile(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "tier1-c")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	stateFile := coreenv.EnvStateFile{
		Provider:      "native",
		WorkspaceName: "tier1-c",
		WorkspacePath: wtPath,
		NativePGIDs:   map[string]int{"kitchen-api": 99003},
	}
	data, _ := json.MarshalIndent(&stateFile, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), data, 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	m := NewManager()
	fake := &fakeSupervisor{}
	m.Supervisor = fake

	// Client passes the parent ecosystem node by mistake — the daemon must
	// reroute to the worktree from state.json.
	req := coreenv.EnvRequest{
		Provider:  "native",
		StateDir:  stateDir,
		Workspace: &workspace.WorkspaceNode{Name: "kitchen-env", Path: filepath.Dir(wtPath)},
	}
	if _, err := m.Down(t.Context(), req); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if len(fake.stops) != 1 || fake.stops[0] != 99003 {
		t.Errorf("Supervisor.Stop calls = %v, want [99003]", fake.stops)
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
		Provider:      "docker",
		WorkspaceName: "tier1-a",
		WorkspacePath: wtPath,
		Ports:         map[string]int{"API_PORT": 52000, "WEB_PORT": 52001},
	}
	stateBytes, _ := json.MarshalIndent(stateFile, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), stateBytes, 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	m := NewManager()
	m.Restore([]string{tmp})

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

// TestRestore_FindsDeeplyNestedStateFiles verifies the WalkDir-based Restore
// finds .grove/env/state.json files at arbitrary depths under the supplied
// ecosystem base paths. This is the workspace-discovery-race fix from Phase
// 4 — no longer dependent on workspace.Provider's pre-populated map.
func TestRestore_FindsDeeplyNestedStateFiles(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "kitchen-env", ".grove-worktrees", "tier1-d")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	stateFile := coreenv.EnvStateFile{
		Provider:      "docker",
		WorkspaceName: "tier1-d",
		WorkspacePath: wtPath,
		Ports:         map[string]int{"api": 51234},
	}
	data, _ := json.MarshalIndent(&stateFile, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := NewManager()
	m.Restore([]string{tmp})

	re, ok := m.envs["tier1-d"]
	if !ok {
		t.Fatalf("expected tier1-d to be restored from a nested .grove/env/state.json under %s", tmp)
	}
	if re.Ports["api"] != 51234 {
		t.Errorf("Ports[api] = %d, want 51234", re.Ports["api"])
	}
	if re.StateDir != stateDir {
		t.Errorf("StateDir = %q, want %q", re.StateDir, stateDir)
	}
}
