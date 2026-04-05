package env

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/sirupsen/logrus"
)

// RunningEnv tracks the state of an active environment.
type RunningEnv struct {
	Provider  string
	Worktree  string
	ManagedBy string
	StateDir  string                         // Path to .grove/env/ directory
	Ports     map[string]int
	Processes map[string]*exec.Cmd          // Tracked natively spawned processes
	Cancels   map[string]context.CancelFunc // Used to terminate native processes
}

// Manager is the central coordinator for all active environments.
type Manager struct {
	Ports   *PortAllocator
	Proxy   *ProxyManager
	Tunnels *TunnelManager

	mu     sync.Mutex
	envs   map[string]*RunningEnv // Keyed by worktree name
	logger *logrus.Entry
}

func NewManager(logger *logrus.Entry) *Manager {
	return &Manager{
		Ports:   NewPortAllocator(),
		Proxy:   NewProxyManager(logger),
		Tunnels: NewTunnelManager(logger),
		envs:    make(map[string]*RunningEnv),
		logger:  logger.WithField("component", "env_manager"),
	}
}

// Up starts an environment based on the provider specified in the request.
func (m *Manager) Up(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	m.logger.WithField("provider", req.Provider).Info("Starting environment")

	switch req.Provider {
	case "native":
		return m.nativeUp(ctx, req)
	case "docker":
		return m.dockerUp(ctx, req)
	case "terraform":
		return m.terraformUp(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", req.Provider)
	}
}

// Down stops an environment based on the provider specified in the request.
func (m *Manager) Down(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	m.logger.WithField("provider", req.Provider).Info("Stopping environment")

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

	return &coreenv.EnvResponse{
		Status: "running",
		State: map[string]string{
			"provider":   env.Provider,
			"managed_by": env.ManagedBy,
		},
	}
}

// Restore reloads environment state from disk on daemon boot.
// It iterates all known workspaces and checks for .grove/env/state.json files,
// re-registering allocated ports to prevent collisions.
func (m *Manager) Restore(provider *workspace.Provider) {
	if provider == nil {
		return
	}

	for _, node := range provider.All() {
		stateDir := filepath.Join(node.Path, ".grove", "env")
		statePath := filepath.Join(stateDir, "state.json")

		data, err := os.ReadFile(statePath)
		if err != nil {
			continue // No state file, skip
		}

		var stateFile coreenv.EnvStateFile
		if err := json.Unmarshal(data, &stateFile); err != nil {
			m.logger.WithError(err).Warnf("Failed to parse env state at %s", statePath)
			continue
		}

		m.mu.Lock()
		runningEnv := &RunningEnv{
			Provider:  stateFile.Provider,
			Worktree:  node.Name,
			ManagedBy: stateFile.ManagedBy,
			StateDir:  stateDir,
			Ports:     make(map[string]int),
		}

		// Re-register allocated ports to prevent collisions
		for svcName, port := range stateFile.Ports {
			label := fmt.Sprintf("%s/%s", node.Name, svcName)
			m.Ports.Reserve(label, port)
			runningEnv.Ports[svcName] = port
		}

		m.envs[node.Name] = runningEnv
		m.mu.Unlock()

		m.logger.WithFields(logrus.Fields{
			"worktree": node.Name,
			"provider": stateFile.Provider,
			"services": len(stateFile.Ports),
		}).Info("Restored environment from state file")
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
	m.logger.Info("All environments shut down")
}

// Note: nativeUp, nativeDown, dockerUp, dockerDown, terraformUp, terraformDown
// are implemented in their respective files.
