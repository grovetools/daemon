package main

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/grovetools/tend/pkg/command"
	"github.com/grovetools/tend/pkg/fs"
	"github.com/grovetools/tend/pkg/harness"
	"github.com/grovetools/tend/pkg/verify"
	"github.com/grovetools/tend/pkg/wait"
)

// DaemonLifecycleScenario tests the daemon start, status, config, and stop commands.
func DaemonLifecycleScenario() *harness.Scenario {
	return harness.NewScenario(
		"daemon-lifecycle",
		"Verify daemon lifecycle commands (start, status, config, stop)",
		[]string{"daemon", "lifecycle"},
		[]harness.Step{
			harness.NewStep("start daemon", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				// Set up paths for PID and socket files using sandboxed XDG directories
				// The paths module uses: StateDir() = $XDG_STATE_HOME/grove
				groveStateDir := filepath.Join(ctx.StateDir(), "grove")
				if err := fs.EnsureDir(groveStateDir); err != nil {
					return err
				}

				// Store paths for later verification
				// PidFilePath() = StateDir()/groved.pid
				// SocketPath() = RuntimeDir()/grove/groved.sock
				ctx.Set("pid_path", filepath.Join(groveStateDir, "groved.pid"))
				groveRunDir := filepath.Join(ctx.RuntimeDir(), "grove")
				if err := fs.EnsureDir(groveRunDir); err != nil {
					return err
				}
				ctx.Set("socket_path", filepath.Join(groveRunDir, "groved.sock"))

				// Start the daemon as a background process
				// The harness provides RuntimeDir() with a short path for Unix sockets
				proc, err := command.New(binary, "start", "--collectors=workspace").
					Dir(ctx.RootDir).
					Env(
						"HOME="+ctx.HomeDir(),
						"XDG_CONFIG_HOME="+ctx.ConfigDir(),
						"XDG_DATA_HOME="+ctx.DataDir(),
						"XDG_STATE_HOME="+ctx.StateDir(),
						"XDG_CACHE_HOME="+ctx.CacheDir(),
						"XDG_RUNTIME_DIR="+ctx.RuntimeDir(),
					).
					Start()
				if err != nil {
					return err
				}

				// Store process for cleanup
				ctx.Set("daemon_process", proc)

				// Wait for the PID file to be created
				pidPath := ctx.GetString("pid_path")
				opts := wait.Options{
					Timeout:      10 * time.Second,
					PollInterval: 200 * time.Millisecond,
					Immediate:    true,
				}

				return wait.For(func() (bool, error) {
					return fs.Exists(pidPath), nil
				}, opts)
			}),
			harness.NewStep("verify pid and socket files exist", func(ctx *harness.Context) error {
				pidPath := ctx.GetString("pid_path")
				socketPath := ctx.GetString("socket_path")

				// Wait for socket file to be created (may take a moment after PID file)
				opts := wait.Options{
					Timeout:      5 * time.Second,
					PollInterval: 200 * time.Millisecond,
					Immediate:    true,
				}

				if err := wait.For(func() (bool, error) {
					return fs.Exists(socketPath), nil
				}, opts); err != nil {
					return err
				}

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("pid file exists", nil, fs.AssertExists(pidPath))
					v.Equal("socket file exists", nil, fs.AssertExists(socketPath))
				})
			}),
			harness.NewStep("check status shows running", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				result := command.New(binary, "status").
					Dir(ctx.RootDir).
					Env(
						"HOME="+ctx.HomeDir(),
						"XDG_CONFIG_HOME="+ctx.ConfigDir(),
						"XDG_DATA_HOME="+ctx.DataDir(),
						"XDG_STATE_HOME="+ctx.StateDir(),
						"XDG_CACHE_HOME="+ctx.CacheDir(),
						"XDG_RUNTIME_DIR="+ctx.RuntimeDir(),
					).
					Run()

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("exit code is 0", 0, result.ExitCode)
					v.True("stdout contains Running", strings.Contains(result.Stdout, "Running"))
					v.True("stdout contains PID", strings.Contains(result.Stdout, "PID:"))
					v.True("stdout contains socket path", strings.Contains(result.Stdout, "Socket:"))
				})
			}),
			harness.NewStep("check config shows intervals", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				result := command.New(binary, "config").
					Dir(ctx.RootDir).
					Env(
						"HOME="+ctx.HomeDir(),
						"XDG_CONFIG_HOME="+ctx.ConfigDir(),
						"XDG_DATA_HOME="+ctx.DataDir(),
						"XDG_STATE_HOME="+ctx.StateDir(),
						"XDG_CACHE_HOME="+ctx.CacheDir(),
						"XDG_RUNTIME_DIR="+ctx.RuntimeDir(),
					).
					Run()

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("exit code is 0", 0, result.ExitCode)
					v.True("stdout contains Running Daemon Configuration", strings.Contains(result.Stdout, "Running Daemon Configuration"))
					v.True("stdout contains Workspace interval", strings.Contains(result.Stdout, "Workspace:"))
				})
			}),
			harness.NewStep("stop daemon", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				result := command.New(binary, "stop").
					Dir(ctx.RootDir).
					Env(
						"HOME="+ctx.HomeDir(),
						"XDG_CONFIG_HOME="+ctx.ConfigDir(),
						"XDG_DATA_HOME="+ctx.DataDir(),
						"XDG_STATE_HOME="+ctx.StateDir(),
						"XDG_CACHE_HOME="+ctx.CacheDir(),
						"XDG_RUNTIME_DIR="+ctx.RuntimeDir(),
					).
					Run()

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("exit code is 0", 0, result.ExitCode)
					v.True("stdout contains SIGTERM", strings.Contains(result.Stdout, "SIGTERM"))
				})
			}),
			harness.NewStep("verify status shows stopped", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				// Wait for daemon to actually stop
				opts := wait.Options{
					Timeout:      5 * time.Second,
					PollInterval: 200 * time.Millisecond,
					Immediate:    false,
				}

				var statusResult *command.Result
				err = wait.For(func() (bool, error) {
					statusResult = command.New(binary, "status").
						Dir(ctx.RootDir).
						Env(
							"HOME="+ctx.HomeDir(),
							"XDG_CONFIG_HOME="+ctx.ConfigDir(),
							"XDG_DATA_HOME="+ctx.DataDir(),
							"XDG_STATE_HOME="+ctx.StateDir(),
							"XDG_CACHE_HOME="+ctx.CacheDir(),
							"XDG_RUNTIME_DIR="+ctx.RuntimeDir(),
						).
						Run()
					// Daemon reports exit code 1 when stopped
					return statusResult.ExitCode == 1, nil
				}, opts)
				if err != nil {
					return err
				}

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("exit code is 1 (stopped)", 1, statusResult.ExitCode)
					v.True("stdout contains Stopped", strings.Contains(statusResult.Stdout, "Stopped"))
				})
			}),
		},
	).WithTeardown(
		harness.NewStep("cleanup daemon process", func(ctx *harness.Context) error {
			// If daemon is still running, kill it
			if proc, ok := ctx.Get("daemon_process").(*command.Process); ok && proc != nil {
				_ = proc.Kill()
			}
			return nil
		}),
	)
}
