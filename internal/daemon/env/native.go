package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	coreenv "github.com/grovetools/core/pkg/env"
)

// nativeUp starts a native environment by spawning bare processes.
// On partial failure, all already-started processes are killed before returning.
func (m *Manager) nativeUp(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	if req.Workspace == nil {
		return nil, fmt.Errorf("native provider requires a workspace")
	}
	worktree := req.Workspace.Name
	m.mu.Lock()
	if _, exists := m.envs[worktree]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("environment already running for worktree: %s", worktree)
	}

	runningEnv := &RunningEnv{
		Provider:        "native",
		Worktree:        worktree,
		Ports:           make(map[string]int),
		Processes:       make(map[string]*exec.Cmd),
		Cancels:         make(map[string]context.CancelFunc),
		ServiceCommands: make(map[string]string),
	}
	m.envs[worktree] = runningEnv
	m.mu.Unlock()

	// cleanupStarted kills all already-started native processes.
	// Called on partial failure before returning an error.
	cleanupStarted := func() {
		for name, cancel := range runningEnv.Cancels {
			cancel()
			if cmd, ok := runningEnv.Processes[name]; ok {
				_ = cmd.Wait()
			}
		}
	}

	resp := &coreenv.EnvResponse{
		Status:  "running",
		EnvVars: make(map[string]string),
	}

	// 1. Process Services
	if services, ok := req.Config["services"].(map[string]interface{}); ok {
		for svcName, svcConfigRaw := range services {
			svcConfig, ok := svcConfigRaw.(map[string]interface{})
			if !ok {
				continue
			}

			cmdStr, _ := svcConfig["command"].(string)
			portEnv, _ := svcConfig["port_env"].(string)
			route, _ := svcConfig["route"].(string)

			if cmdStr == "" {
				continue
			}

			// Allocate ephemeral port
			port, err := m.Ports.Allocate(fmt.Sprintf("%s/%s", worktree, svcName))
			if err != nil {
				cleanupStarted()
				return nil, fmt.Errorf("failed to allocate port for %s: %w", svcName, err)
			}
			runningEnv.Ports[svcName] = port
			runningEnv.ServiceCommands[svcName] = cmdStr

			// Spawn process
			svcCtx, cancel := context.WithCancel(context.Background())
			runningEnv.Cancels[svcName] = cancel

			cmd := exec.CommandContext(svcCtx, "sh", "-c", cmdStr)
			cmd.Dir = req.Workspace.Path
			cmd.Env = os.Environ()
			if portEnv != "" {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", portEnv, port))
				resp.EnvVars[portEnv] = fmt.Sprintf("%d", port)
			}

			if err := cmd.Start(); err != nil {
				cancel()
				cleanupStarted()
				return nil, fmt.Errorf("failed to start service %s: %w", svcName, err)
			}
			runningEnv.Processes[svcName] = cmd

			// Register Proxy
			if route != "" {
				m.Proxy.Register(worktree, route, port)
				resp.Endpoints = append(resp.Endpoints, fmt.Sprintf("http://%s.%s.grove.local:8443", route, worktree))
			}
		}
	}

	// 2. Process Tunnels
	if tunnels, ok := req.Config["tunnels"].(map[string]interface{}); ok {
		for tunnelName, tunnelCfgRaw := range tunnels {
			tunnelCfg, ok := tunnelCfgRaw.(map[string]interface{})
			if !ok {
				continue
			}

			cmdStr, _ := tunnelCfg["command"].(string)
			localPortEnv, _ := tunnelCfg["local_port_env"].(string)
			urlTemplate, _ := tunnelCfg["url_template"].(string)

			port, err := m.Ports.Allocate(fmt.Sprintf("%s/tunnel-%s", worktree, tunnelName))
			if err != nil {
				continue
			}
			runningEnv.Ports["tunnel-"+tunnelName] = port

			if err := m.Tunnels.Start(context.Background(), worktree, tunnelName, cmdStr, port); err != nil {
				m.logger.WithError(err).Warnf("Failed to start tunnel %s", tunnelName)
				continue
			}

			if localPortEnv != "" {
				resp.EnvVars[localPortEnv] = fmt.Sprintf("%d", port)
			}
			if urlTemplate != "" {
				// Simple string replacement for URL template
				finalURL := strings.ReplaceAll(urlTemplate, "{{.AllocatedPort}}", fmt.Sprintf("%d", port))
				resp.EnvVars[localPortEnv] = finalURL
			}
		}
	}

	return resp, nil
}

// nativeDown stops a native environment and cleans up.
func (m *Manager) nativeDown(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	if req.Workspace == nil {
		return nil, fmt.Errorf("native provider requires a workspace")
	}
	worktree := req.Workspace.Name

	m.mu.Lock()
	runningEnv, exists := m.envs[worktree]
	if exists {
		delete(m.envs, worktree)
	}
	m.mu.Unlock()

	if !exists {
		return &coreenv.EnvResponse{Status: "stopped"}, nil
	}

	// Kill all native processes
	for name, cancel := range runningEnv.Cancels {
		cancel()
		if cmd, ok := runningEnv.Processes[name]; ok {
			_ = cmd.Wait() // Reclaim zombie
		}
	}

	m.Tunnels.StopAll(worktree)
	m.Proxy.Unregister(worktree)
	m.Ports.ReleaseAll(worktree)

	return &coreenv.EnvResponse{Status: "stopped"}, nil
}
