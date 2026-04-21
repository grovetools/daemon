package env

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	Ports   *PortAllocator
	Proxy   *ProxyManager
	Tunnels *TunnelManager

	mu   sync.Mutex
	envs map[string]*RunningEnv // Keyed by worktree name
	ulog *logging.UnifiedLogger
}

// NewManager creates a new environment manager.
func NewManager() *Manager {
	return &Manager{
		Ports:   NewPortAllocator(),
		Proxy:   NewProxyManager(),
		Tunnels: NewTunnelManager(),
		envs:    make(map[string]*RunningEnv),
		ulog:    logging.NewUnifiedLogger("groved.env.manager"),
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
func (m *Manager) Down(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	m.ulog.Info("Stopping environment").Field("provider", req.Provider).Log(ctx)

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
// It iterates all known workspaces and checks for .grove/env/state.json files.
// For docker/terraform providers, it re-registers allocated ports.
// For native providers, it cleans up stale state since processes died with the old daemon.
func (m *Manager) Restore(provider *workspace.Provider) {
	if provider == nil {
		return
	}

	ctx := context.Background()
	for _, node := range provider.All() {
		stateDir := filepath.Join(node.Path, ".grove", "env")
		statePath := filepath.Join(stateDir, "state.json")

		data, err := os.ReadFile(statePath)
		if err != nil {
			continue // No state file, skip
		}

		var stateFile coreenv.EnvStateFile
		if err := json.Unmarshal(data, &stateFile); err != nil {
			m.ulog.Warn("Failed to parse env state").
				Err(err).
				Field("path", statePath).
				Log(ctx)
			continue
		}

		// For native envs, clean up any docker-backed containers orphaned
		// by the previous daemon. Keep state.json + .env.local on disk so
		// `grove env down` remains the authoritative teardown path — native
		// processes on macOS are reparented to PID 1 when the daemon exits
		// and stay alive, so deleting their state file here would orphan
		// them from the TUI/CLI.
		if stateFile.Provider == "native" {
			for _, svc := range stateFile.Services {
				containerName := fmt.Sprintf("grove-%s-%s", node.Name, svc.Name)
				_ = exec.Command("docker", "rm", "-f", containerName).Run()
			}
			continue
		}

		m.mu.Lock()
		runningEnv := &RunningEnv{
			Provider:    stateFile.Provider,
			Worktree:    node.Name,
			Environment: stateFile.Environment,
			ManagedBy:   stateFile.ManagedBy,
			StateDir:    stateDir,
			Ports:       make(map[string]int),
		}

		// Re-register allocated ports to prevent collisions
		for svcName, port := range stateFile.Ports {
			label := fmt.Sprintf("%s/%s", node.Name, svcName)
			m.Ports.Reserve(label, port)
			runningEnv.Ports[svcName] = port
		}

		m.envs[node.Name] = runningEnv
		m.mu.Unlock()

		m.ulog.Info("Restored environment from state file").
			Field("worktree", node.Name).
			Field("provider", stateFile.Provider).
			Field("services", len(stateFile.Ports)).
			Log(ctx)
	}
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
