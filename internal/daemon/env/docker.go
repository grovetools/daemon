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
	Ports   []string `yaml:"ports"`
	Volumes []string `yaml:"volumes,omitempty"`
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
		Provider:    "docker",
		Worktree:    worktree,
		Environment: req.Profile,
		Ports:       make(map[string]int),
	}
	m.envs[worktree] = runningEnv
	m.mu.Unlock()

	// Resolve config.env (static values + cmd-based secrets)
	baseEnv, err := ResolveConfigEnv(ctx, req.Config, req.Workspace.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve environment variables: %w", err)
	}

	resp := &coreenv.EnvResponse{
		Status:  "running",
		EnvVars: make(map[string]string),
	}

	override := ComposeOverride{Services: make(map[string]ComposeService)}
	baseComposeFile := "docker-compose.yml"
	if customFile, ok := req.Config["compose_file"].(string); ok && customFile != "" {
		baseComposeFile = customFile
	}

	// Set up log directory for restore/lifecycle logs
	logDir := filepath.Join(req.Workspace.Path, ".grove", "env", "logs")
	_ = os.MkdirAll(logDir, 0755)

	// Parse services to map ports, proxy routes, and volumes
	services, _ := req.Config["services"].(map[string]interface{})
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

		svc := ComposeService{}
		if portEnv != "" {
			// Port flows through compose env substitution on the base file
			// (e.g. "${CLICKHOUSE_PORT:-8123}:8123"). Emitting a second
			// 127.0.0.1:<port>:<container_port> in the override would make
			// compose append both to the service's ports list and attempt
			// two binds on the same host port, which conflicts.
			resp.EnvVars[portEnv] = fmt.Sprintf("%d", hostPort)
		} else {
			// No port_env declared — keep authoritative override binding.
			svc.Ports = []string{fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort)}
		}

		if route != "" {
			m.Proxy.Register(worktree, route, hostPort)
			resp.Endpoints = append(resp.Endpoints, fmt.Sprintf("http://%s.%s.grove.local:8443", route, worktree))
		}

		// Parse volumes: create host dirs, add bindings, run pre-start restore
		if volumes, ok := svcConfig["volumes"].(map[string]interface{}); ok {
			for volName, volCfgRaw := range volumes {
				volCfg, ok := volCfgRaw.(map[string]interface{})
				if !ok {
					continue
				}
				hostPath, _ := volCfg["host_path"].(string)
				if hostPath == "" {
					continue
				}

				absPath := hostPath
				if !filepath.IsAbs(hostPath) {
					absPath = filepath.Join(req.Workspace.Path, hostPath)
				}
				if err := os.MkdirAll(absPath, 0755); err != nil {
					m.ulog.Warn("Failed to create volume directory").
						Err(err).
						Field("path", absPath).
						Field("service", svcName).
						Log(ctx)
					continue
				}

				persist, _ := volCfg["persist"].(bool)
				containerPath, _ := volCfg["container_path"].(string)

				resp.Volumes = append(resp.Volumes, coreenv.VolumeState{
					Path:          hostPath,
					Persist:       persist,
					ContainerPath: containerPath,
				})

				// Add volume binding to compose override if container_path is set
				if containerPath != "" {
					svc.Volumes = append(svc.Volumes, fmt.Sprintf("%s:%s", absPath, containerPath))
				}

				// Pre-start restore: run restore.command on host if volume dir is empty
				if restoreCfg, ok := volCfg["restore"].(map[string]interface{}); ok {
					restoreCmd, _ := restoreCfg["command"].(string)
					if restoreCmd != "" {
						empty, _ := isDirEmpty(absPath)
						if empty {
							m.ulog.Info("Running volume restore command").
								Field("service", svcName).
								Field("volume", volName).
								Log(ctx)

							restoreEnv := append(os.Environ(), fmt.Sprintf("GROVE_VOLUME_HOST_PATH=%s", absPath))
							for k, v := range resp.EnvVars {
								restoreEnv = append(restoreEnv, fmt.Sprintf("%s=%s", k, v))
							}

							rc := exec.Command("sh", "-c", restoreCmd)
							rc.Dir = req.Workspace.Path
							rc.Env = restoreEnv

							restoreLogPath := filepath.Join(logDir, svcName+"-restore.log")
							if rlf, err := os.OpenFile(restoreLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
								rc.Stdout = rlf
								rc.Stderr = rlf
								defer rlf.Close()
							}

							if err := rc.Run(); err != nil {
								return nil, fmt.Errorf("volume restore failed for service %s volume %s: %w", svcName, volName, err)
							}

							m.ulog.Info("Volume restore completed").
								Field("service", svcName).
								Field("volume", volName).
								Log(ctx)
						}
					}
				}
			}
		}

		override.Services[svcName] = svc
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
	cmd.Env = append([]string{}, baseEnv...)
	for k, v := range resp.EnvVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker compose up failed: %w\nOutput: %s", err, string(output))
	}

	// Post-start lifecycle hooks: run on host after compose up
	for svcName, svcConfigRaw := range services {
		svcConfig, ok := svcConfigRaw.(map[string]interface{})
		if !ok {
			continue
		}

		lifecycle, ok := svcConfig["lifecycle"].(map[string]interface{})
		if !ok {
			continue
		}
		postStart, _ := lifecycle["post_start"].(string)
		if postStart == "" {
			continue
		}

		mode, _ := lifecycle["post_start_mode"].(string)
		if mode == "" {
			mode = "always"
		}

		shouldRun := true
		var markerPath string

		if mode == "once" {
			if volumes, ok := svcConfig["volumes"].(map[string]interface{}); ok {
				for _, volCfgRaw := range volumes {
					if volCfg, ok := volCfgRaw.(map[string]interface{}); ok {
						if hp, _ := volCfg["host_path"].(string); hp != "" {
							if filepath.IsAbs(hp) {
								markerPath = filepath.Join(hp, ".grove_init")
							} else {
								markerPath = filepath.Join(req.Workspace.Path, hp, ".grove_init")
							}
							break
						}
					}
				}
			}
			if markerPath != "" {
				if _, err := os.Stat(markerPath); err == nil {
					shouldRun = false
					m.ulog.Info("Skipping post_start (once mode, already initialized)").
						Field("service", svcName).
						Log(ctx)
				}
			}
		}

		if shouldRun {
			m.ulog.Info("Running post-start lifecycle hook").
				Field("service", svcName).
				Field("mode", mode).
				Log(ctx)

			lcCmd := exec.Command("sh", "-c", postStart)
			lcCmd.Dir = req.Workspace.Path
			lcCmd.Env = os.Environ()
			for k, v := range resp.EnvVars {
				lcCmd.Env = append(lcCmd.Env, fmt.Sprintf("%s=%s", k, v))
			}

			lcLogPath := filepath.Join(logDir, svcName+"-lifecycle.log")
			if llf, err := os.OpenFile(lcLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
				lcCmd.Stdout = llf
				lcCmd.Stderr = llf
				defer llf.Close()
			}

			if err := lcCmd.Run(); err != nil {
				m.ulog.Warn("Post-start lifecycle hook failed").
					Err(err).
					Field("service", svcName).
					Log(ctx)
			} else {
				m.ulog.Info("Post-start lifecycle hook completed").
					Field("service", svcName).
					Log(ctx)
				if mode == "once" && markerPath != "" {
					os.WriteFile(markerPath, []byte("initialized\n"), 0644)
				}
			}
		}
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
		m.ulog.Warn("docker compose down failed").
			Err(err).
			Field("output", string(output)).
			Log(ctx)
	}

	// Cleanup override file
	overridePath := filepath.Join(req.PlanDir, "docker-compose.override.yml")
	_ = os.Remove(overridePath)

	m.Proxy.Unregister(worktree)
	m.Ports.ReleaseAll(worktree)

	return &coreenv.EnvResponse{Status: "stopped"}, nil
}
