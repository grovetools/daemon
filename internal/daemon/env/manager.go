package env

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/grovetools/core/logging"
	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
)

// RunningEnv tracks the state of an active environment.
type RunningEnv struct {
	Provider        string
	Worktree        string
	Environment     string // Named environment profile (empty = default)
	ManagedBy       string
	StateDir        string                         // Path to .grove/env/ directory
	Ports           map[string]int
	Processes       map[string]*exec.Cmd          // Tracked natively spawned processes
	Cancels         map[string]context.CancelFunc // Used to terminate native processes
	ServiceCommands map[string]string             // Service name -> command string (for state persistence/restart)
	ContainerNames  map[string]string             // Service name -> docker container name (for docker-backed native services)
	NativePGIDs     map[string]int                // Service/tunnel name -> process group id (for cross-restart teardown; populated in Phase 2)
}

// Manager is the central coordinator for all active environments.
type Manager struct {
	Ports      *PortAllocator
	Proxy      *ProxyManager
	Tunnels    *TunnelManager
	Supervisor NativeSupervisor

	mu   sync.Mutex
	envs map[string]*RunningEnv // Keyed by worktree name
	ulog *logging.UnifiedLogger
}

// NewManager creates a new environment manager.
func NewManager() *Manager {
	supervisor := NewPGIDSupervisor()
	return &Manager{
		Ports:      NewPortAllocator(),
		Proxy:      NewProxyManager(),
		Tunnels:    NewTunnelManagerWithSupervisor(supervisor),
		Supervisor: supervisor,
		envs:       make(map[string]*RunningEnv),
		ulog:       logging.NewUnifiedLogger("groved.env.manager"),
	}
}

// Up starts an environment based on the provider specified in the request.
// On failure, it rolls back the registered environment entry and releases any allocated ports.
// On success, it writes .grove/env/state.json from the resulting RunningEnv + EnvResponse
// so the daemon is the single authoritative writer of that file.
func (m *Manager) Up(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	m.ulog.Info("Starting environment").Field("provider", req.Provider).Log(ctx)

	var resp *coreenv.EnvResponse
	var err error

	switch req.Provider {
	case "native":
		resp, err = m.nativeUp(ctx, req)
	case "docker":
		resp, err = m.dockerUp(ctx, req)
	case "terraform":
		resp, err = m.terraformUp(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", req.Provider)
	}

	if err != nil && req.Workspace != nil {
		worktree := req.Workspace.Name
		m.mu.Lock()
		runningEnv, exists := m.envs[worktree]
		if exists {
			// Kill any native processes that were started before the failure
			for name, cancel := range runningEnv.Cancels {
				cancel()
				if cmd, ok := runningEnv.Processes[name]; ok {
					_ = cmd.Wait()
				}
			}
			delete(m.envs, worktree)
		}
		m.mu.Unlock()
		m.Tunnels.StopAll(worktree)
		m.Proxy.Unregister(worktree)
		m.Ports.ReleaseAll(worktree)
		m.ulog.Info("Rolled back failed environment registration").Field("worktree", worktree).Log(ctx)
		return resp, err
	}

	if err == nil && resp != nil && req.Workspace != nil {
		if writeErr := m.writeStateFile(ctx, req, resp); writeErr != nil {
			m.ulog.Warn("Failed to write env state file").
				Err(writeErr).
				Field("worktree", req.Workspace.Name).
				Log(ctx)
		}
	}

	return resp, err
}

// Down stops an environment based on the provider specified in the request.
// On successful teardown the daemon also deletes .grove/env/state.json so it
// remains the single authoritative manager of that file.
//
// If the daemon has no in-memory record of the env (e.g. the daemon was
// restarted between Up and Down), Down falls back to reading state.json
// from req.StateDir and uses the persisted WorkspaceName/Path to ensure
// the providers' down paths target the correct worktree. The provider-
// specific down handlers then reap NativePGIDs / DockerContainers from
// the same state file — this is the disk-lazy teardown path that makes
// "daemon died, user ran grove env down, env is gone" always true.
func (m *Manager) Down(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	m.ulog.Info("Stopping environment").Field("provider", req.Provider).Log(ctx)

	// Disk-lazy fallback: when the in-memory map is empty (post-restart) but
	// state.json exists, prefer the persisted Workspace identity over whatever
	// the client computed. This keeps the down handler from operating on the
	// wrong map key (e.g. parent-ecosystem vs. concrete worktree) and avoids
	// drifting away from the on-disk truth.
	if stateFile, err := m.readStateFile(req); err == nil && stateFile != nil {
		if stateFile.WorkspaceName != "" && stateFile.WorkspacePath != "" {
			if req.Workspace == nil || req.Workspace.Name != stateFile.WorkspaceName || req.Workspace.Path != stateFile.WorkspacePath {
				patched := workspace.WorkspaceNode{
					Name: stateFile.WorkspaceName,
					Path: stateFile.WorkspacePath,
				}
				req.Workspace = &patched
			}
		}
		if req.Provider == "" {
			req.Provider = stateFile.Provider
		}
	}

	var resp *coreenv.EnvResponse
	var err error
	switch req.Provider {
	case "native":
		resp, err = m.nativeDown(ctx, req)
	case "docker":
		resp, err = m.dockerDown(ctx, req)
	case "terraform":
		resp, err = m.terraformDown(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", req.Provider)
	}

	if err == nil {
		m.removeStateFile(ctx, req)
	}
	return resp, err
}

// readStateFile loads .grove/env/state.json from req's effective state dir.
// Returns (nil, nil) when the file doesn't exist; an error only on read or
// JSON-parse failure.
func (m *Manager) readStateFile(req coreenv.EnvRequest) (*coreenv.EnvStateFile, error) {
	stateDir := req.EffectiveStateDir()
	if stateDir == "" {
		return nil, nil
	}
	statePath := filepath.Join(stateDir, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sf coreenv.EnvStateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", statePath, err)
	}
	return &sf, nil
}

// reapPersistedNatives stops every entry in stateFile.NativePGIDs via the
// supervisor and force-removes every container in stateFile.DockerContainers.
// Both maps may be nil/empty (silent no-op). Errors are logged but not
// returned — teardown always proceeds best-effort.
func (m *Manager) reapPersistedNatives(ctx context.Context, stateFile *coreenv.EnvStateFile) {
	if stateFile == nil {
		return
	}
	for name, pgid := range stateFile.NativePGIDs {
		if err := m.Supervisor.Stop(pgid); err != nil {
			m.ulog.Warn("Failed to stop native process group from state file").
				Err(err).
				Field("name", name).
				Field("pgid", pgid).
				Log(ctx)
		}
	}
	for name, container := range stateFile.DockerContainers {
		if container == "" {
			continue
		}
		if err := exec.Command("docker", "rm", "-f", container).Run(); err != nil {
			m.ulog.Debug("docker rm -f returned non-zero (likely already gone)").
				Err(err).
				Field("service", name).
				Field("container", container).
				Log(ctx)
		}
	}
}

// writeStateFile renders the current RunningEnv + EnvResponse + EnvRequest into
// a coreenv.EnvStateFile and persists it to <stateDir>/state.json. Callers must
// pass a request with a non-nil Workspace.
func (m *Manager) writeStateFile(ctx context.Context, req coreenv.EnvRequest, resp *coreenv.EnvResponse) error {
	stateDir := req.EffectiveStateDir()
	if stateDir == "" {
		return fmt.Errorf("write state file: empty state dir")
	}

	worktree := req.Workspace.Name
	m.mu.Lock()
	runningEnv := m.envs[worktree]
	m.mu.Unlock()

	stateFile := coreenv.EnvStateFile{
		Provider:      req.Provider,
		Environment:   req.Profile,
		ManagedBy:     req.ManagedBy,
		WorkspaceName: req.Workspace.Name,
		WorkspacePath: req.Workspace.Path,
		EnvVars:       resp.EnvVars,
		Endpoints:     resp.Endpoints,
		CleanupPaths:  resp.CleanupPaths,
		Volumes:       resp.Volumes,
		State:         resp.State,
	}

	if runningEnv != nil {
		if len(runningEnv.Ports) > 0 {
			stateFile.Ports = make(map[string]int, len(runningEnv.Ports))
			stateFile.Services = make([]coreenv.ServiceState, 0, len(runningEnv.Ports))
			for name, port := range runningEnv.Ports {
				stateFile.Ports[name] = port
				stateFile.Services = append(stateFile.Services, coreenv.ServiceState{
					Name:   name,
					Port:   port,
					Status: "running",
				})
			}
		}
		if len(runningEnv.ServiceCommands) > 0 {
			stateFile.ServiceCommands = make(map[string]string, len(runningEnv.ServiceCommands))
			for k, v := range runningEnv.ServiceCommands {
				stateFile.ServiceCommands[k] = v
			}
		}
		if len(runningEnv.ContainerNames) > 0 {
			stateFile.DockerContainers = make(map[string]string, len(runningEnv.ContainerNames))
			for k, v := range runningEnv.ContainerNames {
				stateFile.DockerContainers[k] = v
			}
		}
		if len(runningEnv.NativePGIDs) > 0 {
			stateFile.NativePGIDs = make(map[string]int, len(runningEnv.NativePGIDs))
			for k, v := range runningEnv.NativePGIDs {
				stateFile.NativePGIDs[k] = v
			}
		}
	}

	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(&stateFile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state file: %w", err)
	}
	statePath := filepath.Join(stateDir, "state.json")
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	m.ulog.Debug("Wrote env state file").
		Field("worktree", worktree).
		Field("path", statePath).
		Log(ctx)
	return nil
}

// removeStateFile deletes <stateDir>/state.json on a successful Down. Errors
// other than "file already gone" are logged but not returned — the env is down
// either way, and a stale state.json is the worst outcome.
func (m *Manager) removeStateFile(ctx context.Context, req coreenv.EnvRequest) {
	stateDir := req.EffectiveStateDir()
	if stateDir == "" {
		return
	}
	statePath := filepath.Join(stateDir, "state.json")
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		m.ulog.Warn("Failed to remove env state file").
			Err(err).
			Field("path", statePath).
			Log(ctx)
	}
}

// Status returns the current status of an environment for a given worktree.
func (m *Manager) Status(worktree string) *coreenv.EnvResponse {
	m.mu.Lock()
	defer m.mu.Unlock()

	env, exists := m.envs[worktree]
	if !exists {
		return &coreenv.EnvResponse{Status: "stopped"}
	}

	services := make([]coreenv.ServiceState, 0, len(env.Ports))
	for name, port := range env.Ports {
		services = append(services, coreenv.ServiceState{
			Name:   name,
			Port:   port,
			Status: "running",
		})
	}

	state := map[string]string{
		"provider":   env.Provider,
		"managed_by": env.ManagedBy,
	}
	if env.Environment != "" {
		state["environment"] = env.Environment
	}
	return &coreenv.EnvResponse{
		Status: "running",
		State:  state,
	}
}

// Restore reloads environment state from disk on daemon boot.
//
// Phase 4 rewrite: the previous implementation walked workspace.Provider's
// in-memory project list, which depends on git/fsnotify discovery completing
// before boot — racy for newly-created worktrees. The new implementation
// walks the configured ecosystem base paths directly with filepath.WalkDir
// (Go's stdlib filepath.Glob does not support recursive `**`) and looks for
// any `.grove/env/state.json`. Worktree identity comes from the file's
// persisted WorkspaceName, so derivation never has to round-trip through
// the workspace discovery layer.
//
// For docker/terraform providers, the re-discovered state files seed
// m.envs (so port collisions are prevented). For native providers, any
// docker-backed services from the previous run are force-removed but
// state.json + .env.local stay on disk — native processes are reparented
// to PID 1 on macOS and remain reachable by the disk-lazy nativeDown path
// added in Phase 3.
func (m *Manager) Restore(basePaths []string) {
	ctx := context.Background()

	statePaths := m.findStateFiles(ctx, basePaths)
	for _, statePath := range statePaths {
		stateDir := filepath.Dir(statePath)

		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}

		var stateFile coreenv.EnvStateFile
		if err := json.Unmarshal(data, &stateFile); err != nil {
			m.ulog.Warn("Failed to parse env state").
				Err(err).
				Field("path", statePath).
				Log(ctx)
			continue
		}

		// Worktree name: prefer the persisted value (WorkspaceName), fall
		// back to filepath inference for legacy state files written by an
		// older daemon. Inference: <worktree>/.grove/env/state.json — the
		// worktree dir is two parents up from stateDir.
		worktree := stateFile.WorkspaceName
		if worktree == "" {
			worktree = filepath.Base(filepath.Dir(filepath.Dir(stateDir)))
		}

		if stateFile.Provider == "native" {
			for _, svc := range stateFile.Services {
				containerName := fmt.Sprintf("grove-%s-%s", worktree, svc.Name)
				_ = exec.Command("docker", "rm", "-f", containerName).Run()
			}
			for _, container := range stateFile.DockerContainers {
				if container == "" {
					continue
				}
				_ = exec.Command("docker", "rm", "-f", container).Run()
			}
			continue
		}

		m.mu.Lock()
		runningEnv := &RunningEnv{
			Provider:    stateFile.Provider,
			Worktree:    worktree,
			Environment: stateFile.Environment,
			ManagedBy:   stateFile.ManagedBy,
			StateDir:    stateDir,
			Ports:       make(map[string]int),
		}

		for svcName, port := range stateFile.Ports {
			label := fmt.Sprintf("%s/%s", worktree, svcName)
			m.Ports.Reserve(label, port)
			runningEnv.Ports[svcName] = port
		}

		m.envs[worktree] = runningEnv
		m.mu.Unlock()

		m.ulog.Info("Restored environment from state file").
			Field("worktree", worktree).
			Field("provider", stateFile.Provider).
			Field("services", len(stateFile.Ports)).
			Field("path", statePath).
			Log(ctx)
	}
}

// findStateFiles walks each base path looking for `.grove/env/state.json`.
// Hidden directories that aren't `.grove` itself, `node_modules`, and
// `vendor` are skipped to keep the walk cheap on big trees.
func (m *Manager) findStateFiles(ctx context.Context, basePaths []string) []string {
	var found []string
	for _, base := range basePaths {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				// Don't abort the whole walk on a single broken symlink or
				// permission denial; just skip the offending entry.
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == "node_modules" || name == "vendor" {
					return fs.SkipDir
				}
				if strings.HasPrefix(name, ".") && name != "." && name != ".grove" && name != ".grove-worktrees" {
					return fs.SkipDir
				}
				return nil
			}
			if d.Name() != "state.json" {
				return nil
			}
			// Only accept files under `.grove/env/state.json`.
			parent := filepath.Base(filepath.Dir(p))
			grandparent := filepath.Base(filepath.Dir(filepath.Dir(p)))
			if parent == "env" && grandparent == ".grove" {
				found = append(found, p)
			}
			return nil
		})
		if err != nil {
			m.ulog.Warn("Env state walk encountered error").
				Err(err).
				Field("base", base).
				Log(ctx)
		}
	}
	return found
}

// cleanupStaleEnv tears down the side effects of a stale RunningEnv — one whose
// worktree directory has been deleted without a clean `grove env down`. It is
// safe to call with any provider kind; unused fields on the RunningEnv (e.g.
// Cancels for docker/terraform) are simply no-ops. Callers MUST NOT hold
// Manager.mu when calling this — Tunnels/Proxy/Ports take their own locks.
func (m *Manager) cleanupStaleEnv(ctx context.Context, worktree string, runningEnv *RunningEnv) {
	if runningEnv == nil {
		return
	}
	// Kill any native processes that were started before the worktree disappeared.
	for name, cancel := range runningEnv.Cancels {
		if cancel != nil {
			cancel()
		}
		if cmd, ok := runningEnv.Processes[name]; ok && cmd != nil {
			_ = cmd.Wait()
		}
	}
	if m.Tunnels != nil {
		m.Tunnels.StopAll(worktree)
	}
	if m.Proxy != nil {
		m.Proxy.Unregister(worktree)
	}
	if m.Ports != nil {
		m.Ports.ReleaseAll(worktree)
	}
	m.ulog.Info("Reconciled stale environment (worktree directory gone)").
		Field("worktree", worktree).
		Field("provider", runningEnv.Provider).
		Field("state_dir", runningEnv.StateDir).
		Log(ctx)
}

// reconcileExistingEnv inspects m.envs[worktree] under m.mu. If the entry is
// stale (StateDir missing on disk), it releases the lock, cleans up the stale
// env, re-acquires the lock, deletes the map entry, and returns (true, nil).
// Callers resume provisioning with m.mu held.
//
// If the entry represents a live environment, returns (false, err) with an
// "environment already running" error and m.mu released.
//
// If there is no existing entry, returns (false, nil) with m.mu held.
//
// Preconditions:
//   - m.mu is held on entry.
//
// Postconditions (success / new-provision cases):
//   - m.mu is held on return.
func (m *Manager) reconcileExistingEnv(ctx context.Context, worktree string) (bool, error) {
	existing, exists := m.envs[worktree]
	if !exists {
		return false, nil
	}
	// A stale entry is one whose StateDir no longer exists on disk (e.g. user
	// `rm -rf .grove-worktrees/<name>` without running `grove env down`).
	if existing.StateDir != "" {
		if _, err := os.Stat(existing.StateDir); os.IsNotExist(err) {
			m.mu.Unlock()
			m.cleanupStaleEnv(ctx, worktree, existing)
			m.mu.Lock()
			delete(m.envs, worktree)
			return true, nil
		}
	}
	m.mu.Unlock()
	return false, fmt.Errorf("environment already running for worktree: %s", worktree)
}

// Shutdown tears down all running environments for graceful daemon shutdown.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for worktree := range m.envs {
		m.Tunnels.StopAll(worktree)
		m.Proxy.Unregister(worktree)
		m.Ports.ReleaseAll(worktree)
	}
	m.envs = make(map[string]*RunningEnv)
	m.ulog.Info("All environments shut down").Log(context.Background())
}

// Note: nativeUp, nativeDown, dockerUp, dockerDown, terraformUp, terraformDown
// are implemented in their respective files.
