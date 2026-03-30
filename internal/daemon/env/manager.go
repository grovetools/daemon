package env

import (
	"context"
	"fmt"
	"os/exec"
	"sync"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/sirupsen/logrus"
)

// RunningEnv tracks the state of an active environment.
type RunningEnv struct {
	Provider  string
	Worktree  string
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
	default:
		return nil, fmt.Errorf("unsupported provider: %s", req.Provider)
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

// Note: nativeUp, nativeDown, dockerUp, dockerDown are implemented in their respective files.
