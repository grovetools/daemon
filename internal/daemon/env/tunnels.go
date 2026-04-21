package env

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"github.com/grovetools/core/logging"
)

type activeTunnel struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	log    *os.File
	pgid   int
}

// TunnelManager orchestrates background tunnel processes tied to worktree lifecycles.
type TunnelManager struct {
	mu         sync.Mutex
	tunnels    map[string]*activeTunnel // Keyed by "worktree/name"
	supervisor NativeSupervisor
	ulog       *logging.UnifiedLogger
}

// NewTunnelManager creates a new tunnel manager backed by the default
// PGID supervisor. Kept for backward compatibility with callers/tests that
// don't want to wire a manager-owned supervisor.
func NewTunnelManager() *TunnelManager {
	return NewTunnelManagerWithSupervisor(NewPGIDSupervisor())
}

// NewTunnelManagerWithSupervisor creates a tunnel manager that delegates
// process spawning + stopping to the supplied NativeSupervisor.
func NewTunnelManagerWithSupervisor(supervisor NativeSupervisor) *TunnelManager {
	if supervisor == nil {
		supervisor = NewPGIDSupervisor()
	}
	return &TunnelManager{
		tunnels:    make(map[string]*activeTunnel),
		supervisor: supervisor,
		ulog:       logging.NewUnifiedLogger("groved.env.tunnels"),
	}
}

// TunnelContext holds template variables for tunnel command rendering.
type TunnelContext struct {
	AllocatedPort int
}

// Start templates the command with the allocated port and executes it as a background process.
// workspaceDir sets the cmd's working directory so commands like
// `cd kitchen-app/infra && terraform output ...` resolve relative paths
// against the workspace, not the daemon's cwd.
// envVars is appended to the subprocess environment so the tunnel command can
// reference values populated by the provider (e.g. $DB_INSTANCE_NAME exposed
// via output_env_map). Pass nil if no extra env is needed.
// If logDir is non-empty, stdout+stderr are captured to <logDir>/tunnel-<name>.log so that
// errors from the underlying command (e.g. gcloud auth failures) are visible to the user.
//
// The returned int is the supervisor-assigned PGID for the spawned tunnel
// process. Callers persist it into EnvStateFile.NativePGIDs so the daemon
// can reap the tunnel tree after a restart.
func (tm *TunnelManager) Start(parentCtx context.Context, worktree, name, cmdTemplate string, port int, workspaceDir string, envVars map[string]string, logDir string) (int, error) {
	tmpl, err := template.New("tunnel").Parse(cmdTemplate)
	if err != nil {
		return 0, fmt.Errorf("invalid tunnel command template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, TunnelContext{AllocatedPort: port}); err != nil {
		return 0, fmt.Errorf("failed to render tunnel command: %w", err)
	}
	renderedCmd := buf.String()

	ctx, cancel := context.WithCancel(parentCtx)
	cmd := exec.CommandContext(ctx, "sh", "-c", renderedCmd)
	if workspaceDir != "" {
		cmd.Dir = workspaceDir
	}
	if len(envVars) > 0 {
		cmd.Env = os.Environ()
		for k, v := range envVars {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	var logFile *os.File
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			tm.ulog.Warn("Failed to create tunnel log directory").
				Err(err).
				Field("log_dir", logDir).
				Log(parentCtx)
		} else {
			logPath := filepath.Join(logDir, "tunnel-"+name+".log")
			lf, lerr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if lerr != nil {
				tm.ulog.Warn("Failed to create tunnel log file").
					Err(lerr).
					Field("log_path", logPath).
					Log(parentCtx)
			} else {
				logFile = lf
				cmd.Stdout = lf
				cmd.Stderr = lf
			}
		}
	}

	pgid, err := tm.supervisor.Spawn(parentCtx, "tunnel-"+name, cmd)
	if err != nil {
		cancel()
		if logFile != nil {
			logFile.Close()
		}
		return 0, fmt.Errorf("failed to start tunnel %s: %w", name, err)
	}

	key := worktree + "/" + name
	tm.mu.Lock()
	tm.tunnels[key] = &activeTunnel{cmd: cmd, cancel: cancel, log: logFile, pgid: pgid}
	tm.mu.Unlock()

	tm.ulog.Debug("Tunnel started").
		Field("worktree", worktree).
		Field("tunnel", name).
		Field("port", port).
		Field("pgid", pgid).
		Log(parentCtx)
	return pgid, nil
}

// StopAll kills all running tunnel processes for the given worktree.
func (tm *TunnelManager) StopAll(worktree string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	prefix := worktree + "/"
	for key, tunnel := range tm.tunnels {
		if strings.HasPrefix(key, prefix) {
			tunnel.cancel()
			lf := tunnel.log
			c := tunnel.cmd
			// Wait for process cleanup in the background to avoid blocking
			go func() {
				_ = c.Wait()
				if lf != nil {
					lf.Close()
				}
			}()
			delete(tm.tunnels, key)
		}
	}
}
