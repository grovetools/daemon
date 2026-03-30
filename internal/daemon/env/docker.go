package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	coreenv "github.com/grovetools/core/pkg/env"
	"gopkg.in/yaml.v3"
)

// ComposeOverride represents the structure of docker-compose.override.yml
type ComposeOverride struct {
	Services map[string]ComposeService `yaml:"services"`
}

// ComposeService represents a single service in the compose override.
type ComposeService struct {
	Ports []string `yaml:"ports"`
}

// dockerUp orchestrates docker-compose with a generated override file.
func (m *Manager) dockerUp(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	if req.Workspace == nil {
		return nil, fmt.Errorf("docker provider requires a workspace")
	}
	worktree := req.Workspace.Name
	m.mu.Lock()
	if _, exists := m.envs[worktree]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("environment already running for worktree: %s", worktree)
	}

	runningEnv := &RunningEnv{
		Provider: "docker",
		Worktree: worktree,
		Ports:    make(map[string]int),
	}
	m.envs[worktree] = runningEnv
	m.mu.Unlock()

	resp := &coreenv.EnvResponse{
		Status:  "running",
		EnvVars: make(map[string]string),
	}

	override := ComposeOverride{Services: make(map[string]ComposeService)}
	baseComposeFile := "docker-compose.yml"
	if customFile, ok := req.Config["compose_file"].(string); ok && customFile != "" {
		baseComposeFile = customFile
	}

	// Parse services to map ports and proxy routes
	if services, ok := req.Config["services"].(map[string]interface{}); ok {
		for svcName, svcConfigRaw := range services {
			svcConfig, ok := svcConfigRaw.(map[string]interface{})
			if !ok {
				continue
			}

			// Read container_port as float64 (JSON/YAML unmarshaling default for numbers)
			containerPortRaw, ok := svcConfig["container_port"].(float64)
			if !ok {
				continue
			}
			containerPort := int(containerPortRaw)

			portEnv, _ := svcConfig["port_env"].(string)
			route, _ := svcConfig["route"].(string)

			// Allocate ephemeral host port
			hostPort, err := m.Ports.Allocate(fmt.Sprintf("%s/docker-%s", worktree, svcName))
			if err != nil {
				return nil, fmt.Errorf("failed to allocate port for %s: %w", svcName, err)
			}
			runningEnv.Ports[svcName] = hostPort

			// Add to override struct
			override.Services[svcName] = ComposeService{
				Ports: []string{fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort)},
			}

			if portEnv != "" {
				resp.EnvVars[portEnv] = fmt.Sprintf("%d", hostPort)
			}

			if route != "" {
				m.Proxy.Register(worktree, route, hostPort)
				resp.Endpoints = append(resp.Endpoints, fmt.Sprintf("http://%s.%s.grove.local:8443", route, worktree))
			}
		}
	}

	// Write override YAML to plan directory to avoid dirtying the git working tree
	overrideBytes, err := yaml.Marshal(&override)
	if err != nil {
		return nil, fmt.Errorf("failed to generate compose override: %w", err)
	}
	overridePath := filepath.Join(req.PlanDir, "docker-compose.override.yml")
	if err := os.WriteFile(overridePath, overrideBytes, 0644); err != nil {
		return nil, fmt.Errorf("failed to write compose override: %w", err)
	}

	// Execute Docker Compose
	projectName := fmt.Sprintf("grove-%s", worktree)
	baseComposeAbs := filepath.Join(req.Workspace.Path, baseComposeFile)

	cmd := exec.CommandContext(ctx, "docker", "compose", "-p", projectName, "-f", baseComposeAbs, "-f", overridePath, "up", "-d")
	cmd.Dir = req.Workspace.Path
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker compose up failed: %w\nOutput: %s", err, string(output))
	}

	return resp, nil
}

// dockerDown tears down the docker compose stack and cleans up routes/ports.
func (m *Manager) dockerDown(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	if req.Workspace == nil {
		return nil, fmt.Errorf("docker provider requires a workspace")
	}
	worktree := req.Workspace.Name

	m.mu.Lock()
	_, exists := m.envs[worktree]
	if exists {
		delete(m.envs, worktree)
	}
	m.mu.Unlock()

	projectName := fmt.Sprintf("grove-%s", worktree)
	cmd := exec.CommandContext(ctx, "docker", "compose", "-p", projectName, "down", "--volumes", "--remove-orphans")
	cmd.Dir = req.Workspace.Path
	if output, err := cmd.CombinedOutput(); err != nil {
		m.logger.WithError(err).Warnf("docker compose down failed: %s", string(output))
	}

	// Cleanup override file
	overridePath := filepath.Join(req.PlanDir, "docker-compose.override.yml")
	_ = os.Remove(overridePath)

	m.Proxy.Unregister(worktree)
	m.Ports.ReleaseAll(worktree)

	return &coreenv.EnvResponse{Status: "stopped"}, nil
}
