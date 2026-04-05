package env

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	coreenv "github.com/grovetools/core/pkg/env"
)

// serviceEntry holds parsed service config for ordered startup.
type serviceEntry struct {
	Name   string
	Config map[string]interface{}
	Order  int
}

// nativeUp starts a native environment by spawning bare processes.
// Services are started in order (lowest first), with stdout/stderr logged to
// .grove/env/logs/<service>.log. On partial failure, all already-started
// processes are killed before returning.
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

	// Create log directory for service output
	logDir := filepath.Join(req.Workspace.Path, ".grove", "env", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		m.logger.WithError(err).Warn("Failed to create log directory, process output will be discarded")
		logDir = ""
	}

	// 1. Process Services (ordered by "order" field, then alphabetically)
	if services, ok := req.Config["services"].(map[string]interface{}); ok {
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

			// Allocate ephemeral port
			port, err := m.Ports.Allocate(fmt.Sprintf("%s/%s", worktree, svcName))
			if err != nil {
				cleanupStarted()
				return nil, fmt.Errorf("failed to allocate port for %s: %w", svcName, err)
			}
			runningEnv.Ports[svcName] = port
			runningEnv.ServiceCommands[svcName] = cmdStr

			// Record port env var so later services can reference it
			if portEnv != "" {
				resp.EnvVars[portEnv] = fmt.Sprintf("%d", port)
			}

			// Build process environment
			svcCtx, cancel := context.WithCancel(context.Background())
			runningEnv.Cancels[svcName] = cancel

			cmd := exec.CommandContext(svcCtx, "sh", "-c", cmdStr)

			// Resolve and create custom working directory if specified
			if wd, ok := svcConfig["working_dir"].(string); ok && wd != "" {
				if filepath.IsAbs(wd) {
					cmd.Dir = wd
				} else {
					cmd.Dir = filepath.Join(req.Workspace.Path, wd)
				}
				if err := os.MkdirAll(cmd.Dir, 0755); err != nil {
					cancel()
					cleanupStarted()
					return nil, fmt.Errorf("failed to create working directory %s for service %s: %w", cmd.Dir, svcName, err)
				}
				// If working_dir is set but no volumes block, create an implicit non-persistent volume
				if _, hasVolumes := svcConfig["volumes"]; !hasVolumes {
					resp.Volumes = append(resp.Volumes, coreenv.VolumeState{
						Path:    wd,
						Persist: false,
					})
				}
			} else if volumes, ok := svcConfig["volumes"].(map[string]interface{}); ok {
				// Use first volume's host_path as working directory if no explicit working_dir
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

			cmd.Env = os.Environ()

			// Inject port env var
			if portEnv != "" {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", portEnv, port))
			}

			// Inject service-level env vars, resolving $VAR references
			// against already-allocated port env vars
			if envMap, ok := svcConfig["env"].(map[string]interface{}); ok {
				for k, v := range envMap {
					val, _ := v.(string)
					resolved := resolveEnvVars(val, resp.EnvVars)
					cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, resolved))
					resp.EnvVars[k] = resolved
				}
			}

			// Parse volumes config, create directories, and run pre-start restore
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

					// Resolve relative paths against workspace
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

					resp.Volumes = append(resp.Volumes, coreenv.VolumeState{
						Path:          hostPath,
						Persist:       persist,
						ContainerPath: containerPath,
					})

					// Pre-start restore: run restore.command if volume dir is empty
					if restoreCfg, ok := volCfg["restore"].(map[string]interface{}); ok {
						restoreCmd, _ := restoreCfg["command"].(string)
						if restoreCmd != "" {
							empty, _ := isDirEmpty(absPath)
							if empty {
								m.logger.WithField("service", svcName).
									WithField("volume", volName).
									Info("Running volume restore command")

								restoreEnv := append(os.Environ(), fmt.Sprintf("GROVE_VOLUME_HOST_PATH=%s", absPath))
								// Inject all accumulated env vars
								for k, v := range resp.EnvVars {
									restoreEnv = append(restoreEnv, fmt.Sprintf("%s=%s", k, v))
								}

								rc := exec.Command("sh", "-c", restoreCmd)
								rc.Dir = req.Workspace.Path
								rc.Env = restoreEnv

								// Log restore output
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
									return nil, fmt.Errorf("volume restore failed for service %s volume %s: %w", svcName, volName, err)
								}

								m.logger.WithField("service", svcName).
									WithField("volume", volName).
									Info("Volume restore completed")
							}
						}
					}
				}
			}

			// Set up logging to file
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
				Info("Starting native service")

			if err := cmd.Start(); err != nil {
				cancel()
				if logFile != nil {
					logFile.Close()
				}
				cleanupStarted()
				return nil, fmt.Errorf("failed to start service %s: %w", svcName, err)
			}
			runningEnv.Processes[svcName] = cmd

			// Reap zombie and close log file when process exits
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

			// TCP health check: wait for service port to accept connections
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
						return nil, fmt.Errorf("health check failed for service %s: port %d not ready after %ds", svcName, port, timeoutSec)
					}

					m.logger.WithField("service", svcName).Info("Health check passed")
				}
			}

			// Post-start lifecycle hook: run after service is healthy
			if lifecycle, ok := svcConfig["lifecycle"].(map[string]interface{}); ok {
				postStart, _ := lifecycle["post_start"].(string)
				if postStart != "" {
					mode, _ := lifecycle["post_start_mode"].(string)
					if mode == "" {
						mode = "always"
					}

					shouldRun := true
					var markerPath string

					// For "once" mode, check for .grove_init marker in first volume's host_path
					if mode == "once" {
						// Find the first volume's absolute path for this service
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

						// Log lifecycle output
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
							// Log warning but don't kill the service — migrations may be partial
							m.logger.WithError(err).Warnf("Post-start lifecycle hook failed for service %s", svcName)
						} else {
							m.logger.WithField("service", svcName).Info("Post-start lifecycle hook completed")
							// Create marker file for "once" mode
							if mode == "once" && markerPath != "" {
								os.WriteFile(markerPath, []byte("initialized\n"), 0644)
							}
						}
					}
				}
			}

			// Legacy: collect cleanup paths from service config
			if paths, ok := svcConfig["cleanup_paths"].([]interface{}); ok {
				for _, p := range paths {
					if s, ok := p.(string); ok {
						resp.CleanupPaths = append(resp.CleanupPaths, s)
					}
				}
			}

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
		m.logger.WithField("service", name).Info("Stopping native service")
		cancel()
		// The background goroutine handles Wait() and log file cleanup
	}

	m.Tunnels.StopAll(worktree)
	m.Proxy.Unregister(worktree)
	m.Ports.ReleaseAll(worktree)

	return &coreenv.EnvResponse{Status: "stopped"}, nil
}

// parseServiceEntries extracts services from the config map and returns them
// sorted by the "order" field (ascending), then alphabetically by name.
func parseServiceEntries(services map[string]interface{}) []serviceEntry {
	entries := make([]serviceEntry, 0, len(services))
	for svcName, svcConfigRaw := range services {
		svcConfig, ok := svcConfigRaw.(map[string]interface{})
		if !ok {
			continue
		}
		order := 100 // default: high so explicitly-ordered services go first
		if o, ok := svcConfig["order"].(int64); ok {
			order = int(o)
		} else if o, ok := svcConfig["order"].(float64); ok {
			order = int(o)
		}
		entries = append(entries, serviceEntry{Name: svcName, Config: svcConfig, Order: order})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Order != entries[j].Order {
			return entries[i].Order < entries[j].Order
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

// resolveEnvVars expands $VAR and ${VAR} references in val using the provided
// env vars map, falling back to the process environment for unresolved vars.
func resolveEnvVars(val string, envVars map[string]string) string {
	return os.Expand(val, func(key string) string {
		if v, ok := envVars[key]; ok {
			return v
		}
		return os.Getenv(key)
	})
}

// isDirEmpty returns true if the directory exists but contains no entries.
func isDirEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}
