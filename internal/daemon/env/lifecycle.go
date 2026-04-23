package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	coreenv "github.com/grovetools/core/pkg/env"
)

// runPreStopHook executes req.Config["lifecycle"]["pre_stop"] (if set) before
// any provider-specific teardown. Failure is logged but never blocks teardown
// — callers expect the convention "pre_stop = '... || true'" in grove.toml.
//
// stateDir, when non-empty, is used to write the hook's stdout/stderr to
// <stateDir>/logs/env-prestop.log. workDir is the cwd for the spawned shell.
// extraEnv layers on top of os.Environ() (last writer wins per exec semantics).
func (m *Manager) runPreStopHook(ctx context.Context, req coreenv.EnvRequest, workDir, stateDir string, extraEnv map[string]string) {
	lc, ok := req.Config["lifecycle"].(map[string]interface{})
	if !ok {
		return
	}
	preStop, _ := lc["pre_stop"].(string)
	if preStop == "" {
		return
	}

	m.ulog.Info("Running pre_stop hook").
		Field("command", preStop).
		Log(ctx)

	cmd := exec.Command("sh", "-c", preStop)
	cmd.Dir = workDir
	cmd.Env = append([]string{}, os.Environ()...)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if stateDir != "" {
		logsDir := filepath.Join(stateDir, "logs")
		if err := os.MkdirAll(logsDir, 0755); err == nil {
			logPath := filepath.Join(logsDir, "env-prestop.log")
			if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
				cmd.Stdout = lf
				cmd.Stderr = lf
				defer lf.Close()
			}
		}
	}

	if err := cmd.Run(); err != nil {
		m.ulog.Warn("pre_stop hook exited non-zero (proceeding with teardown)").
			Err(err).
			Log(ctx)
	}
}

// runStartupCommands executes any entries in req.Config["commands"] flagged
// with `startup = true` after a successful Up. Each command runs in workDir
// with extraEnv layered onto os.Environ(). Commands run sequentially in
// alphabetical order for determinism (Go map iteration is randomized). A
// failing startup command logs a warning but does not fail the overall Up.
func (m *Manager) runStartupCommands(ctx context.Context, req coreenv.EnvRequest, workDir, stateDir string, extraEnv map[string]string) {
	cmds, ok := req.Config["commands"].(map[string]interface{})
	if !ok || len(cmds) == 0 {
		return
	}

	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	sort.Strings(names)

	var logsDir string
	if stateDir != "" {
		logsDir = filepath.Join(stateDir, "logs")
		_ = os.MkdirAll(logsDir, 0755)
	}

	for _, name := range names {
		cfg, ok := cmds[name].(map[string]interface{})
		if !ok {
			continue
		}
		startup, _ := cfg["startup"].(bool)
		if !startup {
			continue
		}
		cmdStr, _ := cfg["command"].(string)
		if cmdStr == "" {
			continue
		}

		m.ulog.Info("Running startup command").
			Field("name", name).
			Log(ctx)

		c := exec.Command("sh", "-c", cmdStr)
		c.Dir = workDir
		c.Env = append([]string{}, os.Environ()...)
		for k, v := range extraEnv {
			c.Env = append(c.Env, fmt.Sprintf("%s=%s", k, v))
		}

		if logsDir != "" {
			logPath := filepath.Join(logsDir, "cmd-"+name+"-startup.log")
			if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
				c.Stdout = lf
				c.Stderr = lf
				defer lf.Close()
			}
		}

		if err := c.Run(); err != nil {
			m.ulog.Warn("startup command failed (continuing)").
				Err(err).
				Field("name", name).
				Log(ctx)
		}
	}
}
