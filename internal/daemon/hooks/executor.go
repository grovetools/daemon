// Package hooks provides utilities for executing daemon hooks.
package hooks

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
)

// DefaultHookTimeout bounds a daemon hook that does not set `timeout`.
//
// Thirty seconds is what the skill-sync executor has always enforced (as a
// hard-coded constant that ignored the configured value); keeping it as the
// DEFAULT rather than raising it to core's documented 600 preserves that
// behavior for every existing config while making the knob actually work.
// A hook that legitimately runs longer now says so.
const DefaultHookTimeout = 30 * time.Second

// Executor handles executing daemon hooks.
type Executor struct {
	cfg  *config.Config
	ulog *logging.UnifiedLogger

	// running tracks in-flight hooks by identity so cancel_previous can kill
	// the previous run. Only hooks that ask for it are tracked. The value is a
	// pointer so a finishing run can tell "my slot" from "a newer run's slot"
	// by identity — func values are not comparable in Go.
	mu      sync.Mutex
	running map[string]*inflightHook
}

// inflightHook is one tracked cancel_previous run.
type inflightHook struct {
	cancel context.CancelFunc
}

// NewExecutor creates a new hook executor.
func NewExecutor(cfg *config.Config) *Executor {
	return &Executor{
		cfg:     cfg,
		ulog:    logging.NewUnifiedLogger("groved.hooks"),
		running: make(map[string]*inflightHook),
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

		e.ExecuteHook(ctx, hook, HookRun{
			Env: map[string]string{
				"GROVE_WORKSPACE_PATH": workspacePath,
				"GROVE_SYNCED_SKILLS":  strings.Join(syncedSkills, ","),
			},
		})
	}
}

// HookRun carries the per-invocation inputs for a hook.
type HookRun struct {
	// Env is layered on top of the daemon's own environment.
	Env map[string]string
	// Stdin is written to the hook's standard input and then closed. Empty
	// means the hook reads from /dev/null.
	Stdin []byte
	// Key overrides the identity used for cancel_previous. Defaults to the
	// hook's name (or its command).
	Key string
}

// ExecuteHook runs a single hook command, honoring the full HookCommand
// lifecycle: `timeout`, `cancel_previous`, and the `disable_env`/`enable_env`
// gates. Those fields were declared in core/config from the start but the
// daemon's executor ignored every one of them — a hook that set timeout = 300
// was still killed at 30 seconds, and a hook gated behind enable_env ran
// unconditionally.
//
// It blocks until the hook exits or its timeout fires. Callers that must not
// block (the event dispatcher) run it on their own goroutine.
func (e *Executor) ExecuteHook(ctx context.Context, hook config.HookCommand, run HookRun) {
	if hook.Command == "" {
		return
	}
	if skip, reason := envGateSkip(hook); skip {
		e.ulog.Debug("Hook skipped by environment gate").
			Field("hook", hook.Name).Field("reason", reason).Log(ctx)
		return
	}

	key := run.Key
	if key == "" {
		key = firstNonEmpty(hook.Name, hook.Command)
	}

	timeout := DefaultHookTimeout
	if hook.Timeout > 0 {
		timeout = time.Duration(hook.Timeout) * time.Second
	}
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if hook.CancelPrevious {
		mine := &inflightHook{cancel: cancel}
		e.mu.Lock()
		if previous := e.running[key]; previous != nil {
			previous.cancel()
			e.ulog.Debug("Cancelled the previous run of a hook").Field("hook", key).Log(ctx)
		}
		e.running[key] = mine
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			// Only clear the slot if it is still ours: a newer run may have
			// replaced it, and clearing that would leak the newer process past
			// the next cancel_previous.
			if e.running[key] == mine {
				delete(e.running, key)
			}
			e.mu.Unlock()
		}()
	}

	cmd := exec.CommandContext(hookCtx, "sh", "-c", hook.Command) //nolint:gosec // G204: hook command from grove config

	// A hook is `sh -c "..."`, and sh forks for anything it does not exec
	// directly. Killing sh alone leaves those grandchildren running AND
	// holding the output pipe open, so CombinedOutput blocks until they
	// finish — which means `timeout` would appear not to work at all for the
	// commands most likely to need it. Run the hook in its own process group
	// and signal the group, the same shape env's PGIDSupervisor uses. WaitDelay
	// bounds the wait even if something escapes the group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	// Set environment variables
	cmd.Env = os.Environ()
	for k, v := range run.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(run.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(run.Stdin)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		// A cancel_previous kill is the configured behavior, not a failure.
		if hookCtx.Err() == context.Canceled {
			e.ulog.Debug("Hook superseded by a newer event").
				Field("hook", hook.Name).Log(ctx)
			return
		}
		level := e.ulog.Warn("Hook execution failed")
		if hookCtx.Err() == context.DeadlineExceeded {
			level = e.ulog.Warn("Hook timed out")
		}
		level.Err(err).
			Field("hook", hook.Name).
			Field("timeout", timeout.String()).
			Field("output", string(output)).
			Log(ctx)
		return
	}

	if len(output) > 0 {
		e.ulog.Debug("Hook completed").
			Field("hook", hook.Name).
			Field("output", string(output)).
			Log(ctx)
	} else {
		e.ulog.Debug("Hook completed").Field("hook", hook.Name).Log(ctx)
	}
}

// envGateSkip applies the disable_env / enable_env gates. Both read the
// DAEMON's environment: these exist so an operator can mute or arm hooks for a
// whole daemon (a headless satellite muting desktop notifications, a laptop
// arming an experimental indexer) without editing config.
func envGateSkip(hook config.HookCommand) (bool, string) {
	if name := strings.TrimSpace(hook.DisableEnv); name != "" && os.Getenv(name) != "" {
		return true, name + " is set"
	}
	if name := strings.TrimSpace(hook.EnableEnv); name != "" && os.Getenv(name) == "" {
		return true, name + " is not set"
	}
	return false, ""
}
