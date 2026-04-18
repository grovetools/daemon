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
	}

	return resp, err
}

// Down stops an environment based on the provider specified in the request.
func (m *Manager) Down(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	m.ulog.Info("Stopping environment").Field("provider", req.Provider).Log(ctx)

	switch req.Provider {
	case "native":
		return m.nativeDown(ctx, req)
	case "docker":
		return m.dockerDown(ctx, req)
	case "terraform":
		return m.terraformDown(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", req.Provider)
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

		// Native processes died with the old daemon — clean up stale state
		if stateFile.Provider == "native" {
			if err := os.Remove(statePath); err != nil {
				m.ulog.Warn("Failed to remove stale native state").
					Err(err).
					Field("path", statePath).
					Log(ctx)
			}
			// Also remove .env.local files so grove env status shows "stopped"
			os.Remove(filepath.Join(stateDir, ".env.local"))
			os.Remove(filepath.Join(node.Path, ".env.local"))
			m.ulog.Info("Cleaned up stale native environment state (processes died with previous daemon)").
				Field("worktree", node.Name).
				Log(ctx)
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
