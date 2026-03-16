// Package hooks provides utilities for executing daemon hooks.
package hooks

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/sirupsen/logrus"
)

// Executor handles executing daemon hooks.
type Executor struct {
	cfg    *config.Config
	logger *logrus.Entry
}

// NewExecutor creates a new hook executor.
func NewExecutor(cfg *config.Config) *Executor {
	return &Executor{
		cfg:    cfg,
		logger: logging.NewLogger("daemon.hooks"),
	}
}

// UpdateConfig updates the executor's config reference.
func (e *Executor) UpdateConfig(cfg *config.Config) {
	e.cfg = cfg
}

// ExecuteOnSkillSync runs the on_skill_sync hooks.
// workspacePath is passed as an environment variable to the hooks.
// syncedSkills is a list of skill names that were synced.
func (e *Executor) ExecuteOnSkillSync(ctx context.Context, workspacePath string, syncedSkills []string, changed bool) {
	if e.cfg == nil || e.cfg.Daemon == nil || e.cfg.Daemon.Hooks == nil {
		return
	}

	hooks := e.cfg.Daemon.Hooks.OnSkillSync
	if len(hooks) == 0 {
		return
	}

	for _, hook := range hooks {
		// Check run_if condition
		if hook.RunIf == "changes" && !changed {
			continue
		}

		e.executeHook(ctx, hook, map[string]string{
			"GROVE_WORKSPACE_PATH": workspacePath,
			"GROVE_SYNCED_SKILLS":  strings.Join(syncedSkills, ","),
		})
	}
}

// executeHook runs a single hook command with the provided environment variables.
func (e *Executor) executeHook(ctx context.Context, hook config.HookCommand, env map[string]string) {
	if hook.Command == "" {
		return
	}

	// Create a timeout context for the hook execution
	hookCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, "sh", "-c", hook.Command)

	// Set environment variables
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		e.logger.WithFields(logrus.Fields{
			"hook":   hook.Name,
			"error":  err,
			"output": string(output),
		}).Warn("Hook execution failed")
		return
	}

	if len(output) > 0 {
		e.logger.WithFields(logrus.Fields{
			"hook":   hook.Name,
			"output": string(output),
		}).Debug("Hook completed")
	} else {
		e.logger.WithField("hook", hook.Name).Debug("Hook completed")
	}
}
