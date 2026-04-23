package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/env/services"
)

// startLocalServices spawns the bare processes described by req.Config["services"]
// and tracks them on runningEnv. It is shared between the native provider and the
// terraform provider (which uses it for hybrid local-API + cloud-DB workflows).
//
// resp.EnvVars is read both as input (so service-level env can reference values
// already populated by terraform outputs or tunnels via $VAR substitution) and
// written to (each service's port_env / explicit env entries are added).
//
// On failure, all processes already started during this call are cancelled
// before returning the error.
func (m *Manager) startLocalServices(
	ctx context.Context,
	req coreenv.EnvRequest,
	runningEnv *RunningEnv,
	resp *coreenv.EnvResponse,
	baseEnv []string,
	logDir string,
) error {
	entries := services.ParseAndSort(req.Config)
	if len(entries) == 0 {
		return nil
	}

	worktree := req.Workspace.Name

	// Track which services this invocation started so partial-failure cleanup
	// only kills its own processes (not pre-existing ones).
	started := make([]string, 0)
	cleanupStarted := func() {
		for _, name := range started {
			if cancel, ok := runningEnv.Cancels[name]; ok {
				cancel()
			}
			if cmd, ok := runningEnv.Processes[name]; ok {
				_ = cmd.Wait()
			}
		}
	}

	for _, entry := range entries {
		svcName := entry.Name
		svcConfig := entry.Raw

		if entry.Type != "docker" && entry.Command == "" {
			continue
		}

		port, err := m.Ports.Allocate(fmt.Sprintf("%s/%s", worktree, svcName))
		if err != nil {
			cleanupStarted()
			return fmt.Errorf("failed to allocate port for %s: %w", svcName, err)
		}
		runningEnv.Ports[svcName] = port

		if entry.PortEnv != "" {
			resp.EnvVars[entry.PortEnv] = fmt.Sprintf("%d", port)
		}

		svcCtx, cancel := context.WithCancel(context.Background())

		var cmd *exec.Cmd
		var containerName string

		if entry.Type == "docker" {
			dockerArgs, cname, derr := buildDockerServiceArgs(worktree, svcName, svcConfig, port, req.Workspace.Path, resp.EnvVars)
			if derr != nil {
				cancel()
				cleanupStarted()
				return derr
			}
			containerName = cname
			// Eagerly remove any stale container with this name (e.g. from a
			// previous crashed daemon that didn't clean up).
			_ = exec.Command("docker", "rm", "-f", containerName).Run()

			// Surface env vars declared on the service to resp.EnvVars so
			// subsequent services can reference them (matches native behavior).
			for k, v := range entry.Env {
				val, _ := v.(string)
				resp.EnvVars[k] = services.ExpandEnvVars(val, resp.EnvVars)
			}

			cmd = exec.CommandContext(svcCtx, "docker", dockerArgs...)
			runningEnv.ServiceCommands[svcName] = "docker " + shellJoin(dockerArgs)
			runningEnv.ContainerNames[svcName] = containerName

			// Wrap cancel so teardown forcefully removes the container even
			// if the docker CLI was SIGKILLed before it could clean up.
			baseCancel := cancel
			cancel = func() {
				_ = exec.Command("docker", "rm", "-f", containerName).Run()
				baseCancel()
			}
			runningEnv.Cancels[svcName] = cancel
		} else {
			cmd = exec.CommandContext(svcCtx, "sh", "-c", entry.Command)
			runningEnv.Cancels[svcName] = cancel
			runningEnv.ServiceCommands[svcName] = entry.Command
		}

		// Working directory resolution (working_dir or first volume's host_path)
		if entry.WorkingDir != "" {
			wd := entry.WorkingDir
			if filepath.IsAbs(wd) {
				cmd.Dir = wd
			} else {
				cmd.Dir = filepath.Join(req.Workspace.Path, wd)
			}
			if err := os.MkdirAll(cmd.Dir, 0755); err != nil {
				cancel()
				cleanupStarted()
				return fmt.Errorf("failed to create working directory %s for service %s: %w", cmd.Dir, svcName, err)
			}
			if entry.Volumes == nil {
				resp.Volumes = append(resp.Volumes, coreenv.VolumeState{
					Path:    wd,
					Persist: false,
				})
			}
		} else if entry.Volumes != nil {
			for _, volCfgRaw := range entry.Volumes {
				if volCfg, ok := volCfgRaw.(map[string]interface{}); ok {
					if hp, ok := volCfg["host_path"].(string); ok && hp != "" {
						if filepath.IsAbs(hp) {
							cmd.Dir = hp
						} else {
							cmd.Dir = filepath.Join(req.Workspace.Path, hp)
						}
						break
					}
				}
			}
			if cmd.Dir == "" {
				cmd.Dir = req.Workspace.Path
			}
		} else {
			cmd.Dir = req.Workspace.Path
		}

		// Build process environment: baseEnv + every value already in resp.EnvVars
		// (tunnel-allocated ports, terraform outputs, prior services' port_envs).
		cmd.Env = append([]string{}, baseEnv...)
		for k, v := range resp.EnvVars {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
		if entry.PortEnv != "" {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", entry.PortEnv, port))
		}

		for k, v := range entry.Env {
			val, _ := v.(string)
			resolved := services.ExpandEnvVars(val, resp.EnvVars)
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, resolved))
			resp.EnvVars[k] = resolved
		}

		// Volumes: create directories, run pre-start restore commands.
		for volName, volCfgRaw := range entry.Volumes {
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

			if persist {
				for _, lockFile := range []string{"status", "lock"} {
					lockPath := filepath.Join(absPath, lockFile)
					if _, err := os.Stat(lockPath); err == nil {
						os.Remove(lockPath)
						m.ulog.Debug("Removed stale lock file").
							Field("service", svcName).
							Field("lock_file", lockFile).
							Log(ctx)
					}
				}
			}

			resp.Volumes = append(resp.Volumes, coreenv.VolumeState{
				Path:          hostPath,
				Persist:       persist,
				ContainerPath: containerPath,
			})

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

						if logDir != "" {
							restoreLogPath := filepath.Join(logDir, svcName+"-restore.log")
							rlf, err := os.OpenFile(restoreLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
							if err == nil {
								rc.Stdout = rlf
								rc.Stderr = rlf
								defer rlf.Close()
							}
						}

						if err := rc.Run(); err != nil {
							cancel()
							cleanupStarted()
							return fmt.Errorf("volume restore failed for service %s volume %s: %w", svcName, volName, err)
						}

						m.ulog.Info("Volume restore completed").
							Field("service", svcName).
							Field("volume", volName).
							Log(ctx)
					}
				}
			}
		}

		// Pre-start bootstrap hook: run svc-declared command if missing_path
		// is absent. Uses the outer request ctx as parent so shutdown/env-down
		// cancels the in-flight bootstrap process; restore.command above uses
		// a detached command and does NOT honor shutdown — we intentionally
		// diverge so long installs (npm install, cargo fetch) can't survive a
		// daemon restart.
		if err := m.runServiceBootstrap(ctx, svcName, svcConfig, cmd.Dir, cmd.Env, logDir); err != nil {
			cancel()
			cleanupStarted()
			return err
		}

		// Service stdout/stderr -> .grove/env/logs/<service>.log
		var logFile *os.File
		if logDir != "" {
			logPath := filepath.Join(logDir, svcName+".log")
			lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				m.ulog.Warn("Failed to create log file").
					Err(err).
					Field("service", svcName).
					Log(ctx)
			} else {
				logFile = lf
				cmd.Stdout = logFile
				cmd.Stderr = logFile
			}
		}

		m.ulog.Info("Starting local service").
			Field("service", svcName).
			Field("command", entry.Command).
			Field("port", port).
			Field("order", entry.Order).
			Log(ctx)

		pgid, err := m.Supervisor.Spawn(ctx, svcName, cmd)
		if err != nil {
			cancel()
			if logFile != nil {
				logFile.Close()
			}
			cleanupStarted()
			return fmt.Errorf("failed to start service %s: %w", svcName, err)
		}
		runningEnv.Processes[svcName] = cmd
		if pgid > 0 {
			if runningEnv.NativePGIDs == nil {
				runningEnv.NativePGIDs = make(map[string]int)
			}
			runningEnv.NativePGIDs[svcName] = pgid
		}
		started = append(started, svcName)

		go func(name string, c *exec.Cmd, lf *os.File) {
			bgCtx := context.Background()
			err := c.Wait()
			if lf != nil {
				lf.Close()
			}
			if err != nil {
				m.ulog.Warn("Service exited with error").
					Err(err).
					Field("service", name).
					Log(bgCtx)
			} else {
				m.ulog.Info("Service exited").Field("service", name).Log(bgCtx)
			}
		}(svcName, cmd, logFile)

		// TCP health check
		if entry.HealthCheck != nil && entry.HealthCheck.Type == "tcp" {
			timeoutSec := entry.HealthCheck.TimeoutSeconds
			target := fmt.Sprintf("127.0.0.1:%d", port)
			deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)

			m.ulog.Info("Waiting for TCP health check").
				Field("service", svcName).
				Field("target", target).
				Field("timeout", timeoutSec).
				Log(ctx)

			healthy := false
			for time.Now().Before(deadline) {
				if services.ProbeTCP(target, 500*time.Millisecond) {
					healthy = true
					break
				}
				time.Sleep(500 * time.Millisecond)
			}

			if !healthy {
				cancel()
				cleanupStarted()
				return fmt.Errorf("health check failed for service %s: port %d not ready after %ds", svcName, port, timeoutSec)
			}

			m.ulog.Info("Health check passed").Field("service", svcName).Log(ctx)
		}

		// Post-start lifecycle hook
		if lc := entry.Lifecycle; lc != nil {
			shouldRun := true
			var markerPath string

			if lc.PostStartMode == "once" {
				for _, volCfgRaw := range entry.Volumes {
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
					Field("mode", lc.PostStartMode).
					Log(ctx)

				lcCmd := exec.Command("sh", "-c", lc.PostStart)
				lcCmd.Dir = req.Workspace.Path
				lcCmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port))
				for k, v := range resp.EnvVars {
					lcCmd.Env = append(lcCmd.Env, fmt.Sprintf("%s=%s", k, v))
				}

				if logDir != "" {
					lcLogPath := filepath.Join(logDir, svcName+"-lifecycle.log")
					llf, err := os.OpenFile(lcLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
					if err == nil {
						lcCmd.Stdout = llf
						lcCmd.Stderr = llf
						defer llf.Close()
					}
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
					if lc.PostStartMode == "once" && markerPath != "" {
						os.WriteFile(markerPath, []byte("initialized\n"), 0644)
					}
				}
			}
		}

		if paths, ok := svcConfig["cleanup_paths"].([]interface{}); ok {
			for _, p := range paths {
				if s, ok := p.(string); ok {
					resp.CleanupPaths = append(resp.CleanupPaths, s)
				}
			}
		}

		if entry.Route != "" {
			m.registerProxyRoute(ctx, worktree, entry.Route, port)
			if resp.ProxyRoutes == nil {
				resp.ProxyRoutes = make(map[string]int)
			}
			resp.ProxyRoutes[entry.Route] = port
			resp.Endpoints = append(resp.Endpoints, fmt.Sprintf("http://%s.%s.grove.local", entry.Route, worktree))
		}
	}

	return nil
}
