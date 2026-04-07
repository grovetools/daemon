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

	"github.com/sirupsen/logrus"
)

type activeTunnel struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	log    *os.File
}

// TunnelManager orchestrates background tunnel processes tied to worktree lifecycles.
type TunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]*activeTunnel // Keyed by "worktree/name"
	logger  *logrus.Entry
}

func NewTunnelManager(logger *logrus.Entry) *TunnelManager {
	return &TunnelManager{
		tunnels: make(map[string]*activeTunnel),
		logger:  logger.WithField("component", "tunnels"),
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
func (tm *TunnelManager) Start(parentCtx context.Context, worktree, name, cmdTemplate string, port int, workspaceDir string, envVars map[string]string, logDir string) error {
	tmpl, err := template.New("tunnel").Parse(cmdTemplate)
	if err != nil {
		return fmt.Errorf("invalid tunnel command template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, TunnelContext{AllocatedPort: port}); err != nil {
		return fmt.Errorf("failed to render tunnel command: %w", err)
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
			tm.logger.WithError(err).Warnf("Failed to create tunnel log directory %s", logDir)
		} else {
			logPath := filepath.Join(logDir, "tunnel-"+name+".log")
			lf, lerr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if lerr != nil {
				tm.logger.WithError(lerr).Warnf("Failed to create tunnel log file %s", logPath)
			} else {
				logFile = lf
				cmd.Stdout = lf
				cmd.Stderr = lf
			}
		}
	}

	if err := cmd.Start(); err != nil {
		cancel()
		if logFile != nil {
			logFile.Close()
		}
		return fmt.Errorf("failed to start tunnel %s: %w", name, err)
	}

	key := worktree + "/" + name
	tm.mu.Lock()
	tm.tunnels[key] = &activeTunnel{cmd: cmd, cancel: cancel, log: logFile}
	tm.mu.Unlock()

	tm.logger.WithFields(logrus.Fields{
		"worktree": worktree,
		"tunnel":   name,
		"port":     port,
	}).Debug("Tunnel started")
	return nil
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
