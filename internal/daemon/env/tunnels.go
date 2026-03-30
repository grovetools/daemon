package env

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"text/template"

	"github.com/sirupsen/logrus"
)

type activeTunnel struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
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
func (tm *TunnelManager) Start(parentCtx context.Context, worktree, name, cmdTemplate string, port int) error {
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

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start tunnel %s: %w", name, err)
	}

	key := worktree + "/" + name
	tm.mu.Lock()
	tm.tunnels[key] = &activeTunnel{cmd: cmd, cancel: cancel}
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
			// Wait for process cleanup in the background to avoid blocking
			go tunnel.cmd.Wait()
			delete(tm.tunnels, key)
		}
	}
}
