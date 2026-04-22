package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/grovetools/core/pkg/build"
	coreenv "github.com/grovetools/core/pkg/env"
	"gopkg.in/yaml.v3"
)

// ComposeOverride represents the structure of docker-compose.override.yml
type ComposeOverride struct {
	Services map[string]ComposeService `yaml:"services"`
}

// ComposeService represents a single service in the compose override.
type ComposeService struct {
	Image   string   `yaml:"image,omitempty"`
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
	if _, err := m.reconcileExistingEnv(ctx, worktree); err != nil {
		return nil, err
	}

	runningEnv := &RunningEnv{
		Provider:    "docker",
		Worktree:    worktree,
		Environment: req.Profile,
		StateDir:    req.StateDir,
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
		Status:      "running",
		EnvVars:     make(map[string]string),
		ProxyRoutes: make(map[string]int),
		State:       make(map[string]string),
	}

	// Read prior state (pre-up) so buildImagesIfStale can compare fingerprints.
	priorState, _ := m.readStateFile(req)

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

		// Coerce container_port defensively — json decodes numbers to float64,
		// but non-JSON transports (toml, msgpack, UseNumber) can surface int/int64.
		containerPort := toInt(svcConfig["container_port"])
		if containerPort == 0 {
			continue
		}

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
			m.registerProxyRoute(ctx, worktree, route, hostPort)
			resp.ProxyRoutes[route] = hostPort
			resp.Endpoints = append(resp.Endpoints, fmt.Sprintf("http://%s.%s.grove.local", route, worktree))
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

	// Gate image builds on a content fingerprint of each context. New or
	// changed contexts (or a --rebuild flag) trigger `docker build`;
	// unchanged contexts reuse the previously-tagged image.
	imageTags, err := m.buildImagesIfStale(ctx, req, resp, priorState)
	if err != nil {
		return nil, err
	}
	// Patch the compose override so compose consumes the pre-built image
	// instead of re-running its own build: block. Create entries for image
	// services that aren't already in the override (e.g. no container_port).
	for svc, tag := range imageTags {
		cs := override.Services[svc]
		cs.Image = tag
		override.Services[svc] = cs
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

	// Pre-start bootstrap hooks run on the host before `docker compose up`
	// so bind-mounted working dirs (e.g. node_modules) are populated before
	// containers start. Same schema as the native path; runs with baseEnv +
	// resp.EnvVars so host-side setup sees allocated ports.
	for svcName, svcConfigRaw := range services {
		svcConfig, ok := svcConfigRaw.(map[string]interface{})
		if !ok {
			continue
		}
		wd := req.Workspace.Path
		if v, ok := svcConfig["working_dir"].(string); ok && v != "" {
			if filepath.IsAbs(v) {
				wd = v
			} else {
				wd = filepath.Join(req.Workspace.Path, v)
			}
		}
		bootEnv := append([]string{}, baseEnv...)
		for k, v := range resp.EnvVars {
			bootEnv = append(bootEnv, fmt.Sprintf("%s=%s", k, v))
		}
		if err := m.runServiceBootstrap(ctx, svcName, svcConfig, wd, bootEnv, logDir); err != nil {
			return nil, err
		}
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

// buildImagesIfStale inspects req.Config["images"], computes a deterministic
// fingerprint of each build context, and runs `docker build` when the
// fingerprint has changed or a matching --rebuild entry is set. Successful
// builds write the new fingerprint into resp.State under "fingerprint_<svc>".
// Skipped builds carry the prior fingerprint forward so subsequent runs
// don't regress to "no prior fingerprint".
//
// Failure behavior:
//   - Fingerprint compute error → return error (fail-closed).
//   - Docker build error → return error, fingerprint NOT stored (next run retries).
//   - Missing prior fingerprint → treated as a mismatch → rebuild (fail-open).
func (m *Manager) buildImagesIfStale(ctx context.Context, req coreenv.EnvRequest, resp *coreenv.EnvResponse, prior *coreenv.EnvStateFile) (map[string]string, error) {
	images, ok := req.Config["images"].(map[string]interface{})
	if !ok || len(images) == 0 {
		return nil, nil
	}
	if resp.State == nil {
		resp.State = make(map[string]string)
	}

	worktree := req.Workspace.Name
	workspaceRoot := req.Workspace.Path
	logsDir := filepath.Join(req.EffectiveStateDir(), "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return nil, fmt.Errorf("create logs dir: %w", err)
	}

	result := make(map[string]string, len(images))

	for svc, cfgRaw := range images {
		cfg, ok := cfgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		// `cmd`-based images are terraform-profile territory (Phase B).
		// Skip them here so docker-local doesn't try to docker-build a
		// service whose config only defines a gcloud submit command.
		if c, _ := cfg["cmd"].(string); c != "" {
			continue
		}

		ctxPath, _ := cfg["context"].(string)
		if ctxPath == "" {
			ctxPath = "."
		}
		dockerfile, _ := cfg["dockerfile"].(string)

		absCtx := ctxPath
		if !filepath.IsAbs(absCtx) {
			absCtx = filepath.Join(workspaceRoot, absCtx)
		}

		absDockerfile := dockerfile
		if absDockerfile == "" {
			absDockerfile = filepath.Join(absCtx, "Dockerfile")
		} else if !filepath.IsAbs(absDockerfile) {
			absDockerfile = filepath.Join(workspaceRoot, absDockerfile)
		}

		imageTag := fmt.Sprintf("grove-%s-%s:latest", worktree, svc)
		result[svc] = imageTag

		hash, herr := build.HashContext(absCtx, absDockerfile)
		if herr != nil {
			return nil, fmt.Errorf("fingerprint %s: %w", svc, herr)
		}

		priorHash := ""
		if prior != nil && prior.State != nil {
			priorHash = prior.State["fingerprint_"+svc]
		}

		reason := ""
		switch {
		case req.ForceRebuild(svc):
			reason = "force"
		case priorHash == "":
			reason = "no prior fingerprint"
		case priorHash != hash:
			reason = "fingerprint mismatch"
		}

		if reason == "" {
			m.ulog.Info("Image up-to-date").
				Field("service", svc).
				Field("fingerprint", hash).
				Log(ctx)
			resp.State["fingerprint_"+svc] = priorHash
			continue
		}

		m.ulog.Info("Image build").
			Field("service", svc).
			Field("reason", reason).
			Field("tag", imageTag).
			Log(ctx)

		args := []string{"build", "-t", imageTag}
		if absDockerfile != "" {
			args = append(args, "-f", absDockerfile)
		}
		args = append(args, absCtx)

		buildCmd := exec.CommandContext(ctx, "docker", args...)
		buildCmd.Dir = workspaceRoot
		logFile := filepath.Join(logsDir, "build-"+svc+".log")
		out, berr := buildCmd.CombinedOutput()
		_ = os.WriteFile(logFile, out, 0644)
		if berr != nil {
			return nil, fmt.Errorf("docker build failed for %s: %w\nSee log: %s", svc, berr, logFile)
		}
		resp.State["fingerprint_"+svc] = hash
	}

	return result, nil
}

// dockerDown tears down the docker compose stack and cleans up routes/ports.
//
// Disk-lazy behavior: when m.envs[worktree] is empty (post-restart), the
// `docker compose -p grove-<worktree> down` shell-out is the authoritative
// teardown — it talks to dockerd, not to in-memory state. We just need an
// accurate cmd.Dir (the workspace path) so compose finds the base
// docker-compose.yml. That comes from req.Workspace.Path, which is patched
// from state.json by Manager.Down before we run.
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

	// Cleanup override file. Try the request-supplied PlanDir first (the
	// hot path), then fall back to the StateDir (where compose-managed
	// envs are written when grove env up runs from inside a worktree —
	// PlanDir == StateDir in that flow). The remove is best-effort either
	// way: a stale override doesn't keep dockerd busy.
	for _, dir := range []string{req.PlanDir, req.EffectiveStateDir()} {
		if dir == "" {
			continue
		}
		_ = os.Remove(filepath.Join(dir, "docker-compose.override.yml"))
	}

	m.unregisterProxyRoutes(ctx, worktree)
	m.Ports.ReleaseAll(worktree)

	return &coreenv.EnvResponse{Status: "stopped"}, nil
}
