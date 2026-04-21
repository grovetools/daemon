package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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
	if _, err := m.reconcileExistingEnv(ctx, worktree); err != nil {
		return nil, err
	}

	runningEnv := &RunningEnv{
		Provider:        "native",
		Worktree:        worktree,
		Environment:     req.Profile,
		StateDir:        req.StateDir,
		Ports:           make(map[string]int),
		Processes:       make(map[string]*exec.Cmd),
		Cancels:         make(map[string]context.CancelFunc),
		ServiceCommands: make(map[string]string),
		ContainerNames:  make(map[string]string),
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

	// Create log directory for service output
	logDir := filepath.Join(req.Workspace.Path, ".grove", "env", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		m.ulog.Warn("Failed to create log directory, process output will be discarded").
			Err(err).
			Log(ctx)
		logDir = ""
	}

	// 1. Process Services via the shared helper
	if err := m.startLocalServices(ctx, req, runningEnv, resp, baseEnv, logDir); err != nil {
		return nil, err
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

			pgid, err := m.Tunnels.Start(context.Background(), worktree, tunnelName, cmdStr, port, req.Workspace.Path, resp.EnvVars, logDir)
			if err != nil {
				m.ulog.Warn("Failed to start tunnel").
					Err(err).
					Field("tunnel", tunnelName).
					Log(ctx)
				continue
			}
			if pgid > 0 {
				if runningEnv.NativePGIDs == nil {
					runningEnv.NativePGIDs = make(map[string]int)
				}
				runningEnv.NativePGIDs["tunnel-"+tunnelName] = pgid
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
//
// Two paths are intentionally unified here:
//
//   - Hot path (m.envs has an entry): cancel each tracked Cmd via its
//     CancelFunc. The CancelFunc closures wired up in startLocalServices
//     also force-remove docker-backed service containers, so this branch
//     handles both bare processes and `type: docker` services.
//   - Disk-lazy path (m.envs is empty, e.g. after a daemon restart):
//     read state.json from disk and reap NativePGIDs via the supervisor
//     plus DockerContainers via `docker rm -f`. This is the path that
//     fails today — a missing m.envs entry used to silently no-op,
//     orphaning the reparented native processes that survived the daemon
//     restart.
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

	if exists && runningEnv != nil {
		for name, cancel := range runningEnv.Cancels {
			m.ulog.Info("Stopping native service").Field("service", name).Log(ctx)
			cancel()
			// The background goroutine handles Wait() and log file cleanup.
		}
	} else {
		// Disk-lazy: in-memory record is gone (typical post-restart).
		// Reap whatever the previous daemon recorded into state.json.
		stateFile, err := m.readStateFile(req)
		if err != nil {
			m.ulog.Warn("Failed to read env state for disk-lazy nativeDown").
				Err(err).
				Field("worktree", worktree).
				Log(ctx)
		}
		if stateFile != nil {
			m.ulog.Info("Disk-lazy native teardown").
				Field("worktree", worktree).
				Field("native_pgids", len(stateFile.NativePGIDs)).
				Field("docker_containers", len(stateFile.DockerContainers)).
				Log(ctx)
			m.reapPersistedNatives(ctx, stateFile)
		}
	}

	m.Tunnels.StopAll(worktree)
	m.unregisterProxyRoutes(ctx, worktree)
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
