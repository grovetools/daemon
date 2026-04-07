package env

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	coreenv "github.com/grovetools/core/pkg/env"
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
	services, ok := req.Config["services"].(map[string]interface{})
	if !ok {
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

	entries := parseServiceEntries(services)

	for _, entry := range entries {
		svcName := entry.Name
		svcConfig := entry.Config

		cmdStr, _ := svcConfig["command"].(string)
		portEnv, _ := svcConfig["port_env"].(string)
		route, _ := svcConfig["route"].(string)

		if cmdStr == "" {
			continue
		}

		port, err := m.Ports.Allocate(fmt.Sprintf("%s/%s", worktree, svcName))
		if err != nil {
			cleanupStarted()
			return fmt.Errorf("failed to allocate port for %s: %w", svcName, err)
		}
		runningEnv.Ports[svcName] = port
		runningEnv.ServiceCommands[svcName] = cmdStr

		if portEnv != "" {
			resp.EnvVars[portEnv] = fmt.Sprintf("%d", port)
		}

		svcCtx, cancel := context.WithCancel(context.Background())
		runningEnv.Cancels[svcName] = cancel

		cmd := exec.CommandContext(svcCtx, "sh", "-c", cmdStr)

		// Working directory resolution (working_dir or first volume's host_path)
		if wd, ok := svcConfig["working_dir"].(string); ok && wd != "" {
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
			if _, hasVolumes := svcConfig["volumes"]; !hasVolumes {
				resp.Volumes = append(resp.Volumes, coreenv.VolumeState{
					Path:    wd,
					Persist: false,
				})
			}
		} else if volumes, ok := svcConfig["volumes"].(map[string]interface{}); ok {
			for _, volCfgRaw := range volumes {
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
		if portEnv != "" {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", portEnv, port))
		}

		if envMap, ok := svcConfig["env"].(map[string]interface{}); ok {
			for k, v := range envMap {
				val, _ := v.(string)
				resolved := resolveEnvVars(val, resp.EnvVars)
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, resolved))
				resp.EnvVars[k] = resolved
			}
		}

		// Volumes: create directories, run pre-start restore commands.
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
					m.logger.WithError(err).Warnf("Failed to create volume directory %s for service %s", absPath, svcName)
					continue
				}

				persist, _ := volCfg["persist"].(bool)
				containerPath, _ := volCfg["container_path"].(string)

				if persist {
					for _, lockFile := range []string{"status", "lock"} {
						lockPath := filepath.Join(absPath, lockFile)
						if _, err := os.Stat(lockPath); err == nil {
							os.Remove(lockPath)
							m.logger.WithField("service", svcName).Debugf("Removed stale lock file: %s", lockFile)
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
							m.logger.WithField("service", svcName).
								WithField("volume", volName).
								Info("Running volume restore command")

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

							m.logger.WithField("service", svcName).
								WithField("volume", volName).
								Info("Volume restore completed")
						}
					}
				}
			}
		}

		// Service stdout/stderr -> .grove/env/logs/<service>.log
		var logFile *os.File
		if logDir != "" {
			logPath := filepath.Join(logDir, svcName+".log")
			lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				m.logger.WithError(err).Warnf("Failed to create log file for %s", svcName)
			} else {
				logFile = lf
				cmd.Stdout = logFile
				cmd.Stderr = logFile
			}
		}

		m.logger.WithField("service", svcName).
			WithField("command", cmdStr).
			WithField("port", port).
			WithField("order", entry.Order).
			Info("Starting local service")

		if err := cmd.Start(); err != nil {
			cancel()
			if logFile != nil {
				logFile.Close()
			}
			cleanupStarted()
			return fmt.Errorf("failed to start service %s: %w", svcName, err)
		}
		runningEnv.Processes[svcName] = cmd
		started = append(started, svcName)

		go func(name string, c *exec.Cmd, lf *os.File) {
			err := c.Wait()
			if lf != nil {
				lf.Close()
			}
			if err != nil {
				m.logger.WithError(err).Warnf("Service %s exited with error", name)
			} else {
				m.logger.WithField("service", name).Info("Service exited")
			}
		}(svcName, cmd, logFile)

		// TCP health check
		if hc, ok := svcConfig["health_check"].(map[string]interface{}); ok {
			if hcType, _ := hc["type"].(string); hcType == "tcp" {
				timeoutSec := 30
				if ts, ok := hc["timeout_seconds"].(int64); ok {
					timeoutSec = int(ts)
				} else if ts, ok := hc["timeout_seconds"].(float64); ok {
					timeoutSec = int(ts)
				}

				target := fmt.Sprintf("127.0.0.1:%d", port)
				deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)

				m.logger.WithField("service", svcName).
					WithField("target", target).
					WithField("timeout", timeoutSec).
					Info("Waiting for TCP health check")

				healthy := false
				for time.Now().Before(deadline) {
					conn, err := net.DialTimeout("tcp", target, 500*time.Millisecond)
					if err == nil {
						conn.Close()
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

				m.logger.WithField("service", svcName).Info("Health check passed")
			}
		}

		// Post-start lifecycle hook
		if lifecycle, ok := svcConfig["lifecycle"].(map[string]interface{}); ok {
			postStart, _ := lifecycle["post_start"].(string)
			if postStart != "" {
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
							m.logger.WithField("service", svcName).Info("Skipping post_start (once mode, already initialized)")
						}
					}
				}

				if shouldRun {
					m.logger.WithField("service", svcName).
						WithField("mode", mode).
						Info("Running post-start lifecycle hook")

					lcCmd := exec.Command("sh", "-c", postStart)
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
						m.logger.WithError(err).Warnf("Post-start lifecycle hook failed for service %s", svcName)
					} else {
						m.logger.WithField("service", svcName).Info("Post-start lifecycle hook completed")
						if mode == "once" && markerPath != "" {
							os.WriteFile(markerPath, []byte("initialized\n"), 0644)
						}
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

		if route != "" {
			m.Proxy.Register(worktree, route, port)
			resp.Endpoints = append(resp.Endpoints, fmt.Sprintf("http://%s.%s.grove.local:8443", route, worktree))
		}
	}

	return nil
}
