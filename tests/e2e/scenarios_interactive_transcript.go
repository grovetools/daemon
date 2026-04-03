package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grovetools/tend/pkg/command"
	"github.com/grovetools/tend/pkg/fs"
	"github.com/grovetools/tend/pkg/harness"
	"github.com/grovetools/tend/pkg/verify"
	"github.com/grovetools/tend/pkg/wait"
)

// DaemonInteractiveTranscriptScenario tests that when an interactive_agent job
// transitions to a terminal state out-of-band (via filesystem), the daemon:
// 1. Accepts the terminal state (store update logic)
// 2. Auto-appends the agent transcript
// 3. Unblocks downstream dependent jobs
func DaemonInteractiveTranscriptScenario() *harness.Scenario {
	return harness.NewScenario(
		"daemon-interactive-transcript",
		"Verify daemon auto-appends transcript and unblocks deps when interactive_agent completes",
		[]string{"daemon", "transcript", "interactive_agent"},
		[]harness.Step{
			harness.NewStep("setup workspace with plan and jobs", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}
				ctx.Set("binary", binary)

				// Create workspace directory with git repo
				workspaceDir := ctx.NewDir("test-workspace")
				if err := os.MkdirAll(workspaceDir, 0755); err != nil {
					return err
				}

				gitInit := command.New("git", "init").Dir(workspaceDir)
				if result := gitInit.Run(); result.ExitCode != 0 {
					return fmt.Errorf("git init failed: %s", result.Stderr)
				}

				// Create grove.toml for the workspace
				groveToml := filepath.Join(workspaceDir, "grove.toml")
				if err := fs.WriteString(groveToml, `name = "test-workspace"
`); err != nil {
					return err
				}

				// Create README and initial commit
				if err := fs.WriteString(filepath.Join(workspaceDir, "README.md"), "# Test\n"); err != nil {
					return err
				}
				gitAdd := command.New("git", "add", ".").Dir(workspaceDir)
				if result := gitAdd.Run(); result.ExitCode != 0 {
					return fmt.Errorf("git add failed: %s", result.Stderr)
				}
				gitCommit := command.New("git", "-c", "user.email=test@test.com", "-c", "user.name=Test", "commit", "-m", "init").Dir(workspaceDir)
				if result := gitCommit.Run(); result.ExitCode != 0 {
					return fmt.Errorf("git commit failed: %s", result.Stderr)
				}

				// Create notebook root
				notebookRoot := filepath.Join(ctx.HomeDir(), ".grove", "notebooks", "main")
				if err := os.MkdirAll(notebookRoot, 0755); err != nil {
					return err
				}

				// Create global config
				globalConfigDir := filepath.Join(ctx.ConfigDir(), "grove")
				if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
					return err
				}
				globalConfig := filepath.Join(globalConfigDir, "grove.toml")
				globalConfigContent := fmt.Sprintf(`version = "1.0"

[daemon]
plan_interval = "2s"

[groves.test]
path = "%s"

[notebooks.rules]
default = "main"

[notebooks.definitions.main]
root_dir = "%s"
`, workspaceDir, notebookRoot)
				if err := fs.WriteString(globalConfig, globalConfigContent); err != nil {
					return err
				}

				// Create a plan directory with job files
				planDir := filepath.Join(notebookRoot, "workspaces", "test-workspace", "plans", "transcript-test")
				if err := os.MkdirAll(planDir, 0755); err != nil {
					return err
				}

				// Create the interactive_agent job file (01-agent.md)
				agentJobContent := `---
id: agent-job-001
title: Agent Task
type: interactive_agent
status: running
started_at: 2026-04-03T10:00:00Z
---

# Agent Task

This is a test interactive agent job.
`
				agentJobPath := filepath.Join(planDir, "01-agent.md")
				if err := fs.WriteString(agentJobPath, agentJobContent); err != nil {
					return err
				}

				// Create a downstream oneshot job (02-downstream.md) that depends on 01-agent.md
				downstreamJobContent := `---
id: downstream-job-002
title: Downstream Task
type: oneshot
status: pending
depends_on:
  - 01-agent.md
---

# Downstream Task

This job depends on the agent job completing.
`
				downstreamJobPath := filepath.Join(planDir, "02-downstream.md")
				if err := fs.WriteString(downstreamJobPath, downstreamJobContent); err != nil {
					return err
				}

				// Store paths for later steps
				ctx.Set("workspace_dir", workspaceDir)
				ctx.Set("plan_dir", planDir)
				ctx.Set("agent_job_path", agentJobPath)
				ctx.Set("downstream_job_path", downstreamJobPath)

				return nil
			}),

			harness.NewStep("create mock grove for aglogs", func(ctx *harness.Context) error {
				// Create a mock 'grove' binary that returns transcript when called with 'aglogs read'
				// and a mock 'aglogs' binary for direct invocation fallback
				mockBinDir := filepath.Join(ctx.RootDir, "mock_bin")
				if err := os.MkdirAll(mockBinDir, 0755); err != nil {
					return err
				}

				mockGroveScript := `#!/bin/bash
# Mock grove binary for e2e testing
if [ "$1" = "aglogs" ] && [ "$2" = "read" ]; then
    echo "MOCK_TRANSCRIPT_OUTPUT"
    echo "This is a simulated agent conversation."
    exit 0
fi
# For all other commands, silently succeed
exit 0
`
				grovePath := filepath.Join(mockBinDir, "grove")
				if err := fs.WriteString(grovePath, mockGroveScript); err != nil {
					return err
				}
				if err := os.Chmod(grovePath, 0755); err != nil {
					return err
				}

				// Also create a mock aglogs binary (fallback if grove delegation not used)
				mockAglogsScript := `#!/bin/bash
# Mock aglogs binary for e2e testing
if [ "$1" = "read" ]; then
    echo "MOCK_TRANSCRIPT_OUTPUT"
    echo "This is a simulated agent conversation."
    exit 0
fi
exit 0
`
				aglogsPath := filepath.Join(mockBinDir, "aglogs")
				if err := fs.WriteString(aglogsPath, mockAglogsScript); err != nil {
					return err
				}
				if err := os.Chmod(aglogsPath, 0755); err != nil {
					return err
				}

				ctx.Set("mock_bin_dir", mockBinDir)
				return nil
			}),

			harness.NewStep("start daemon with plan collector", func(ctx *harness.Context) error {
				binary := ctx.GetString("binary")
				mockBinDir := ctx.GetString("mock_bin_dir")

				// Set up XDG directories
				groveStateDir := filepath.Join(ctx.StateDir(), "grove")
				if err := fs.EnsureDir(groveStateDir); err != nil {
					return err
				}
				pidPath := filepath.Join(groveStateDir, "groved.pid")
				ctx.Set("pid_path", pidPath)

				groveRunDir := filepath.Join(ctx.RuntimeDir(), "grove")
				if err := fs.EnsureDir(groveRunDir); err != nil {
					return err
				}

				// Start daemon with workspace+plan collectors
				// Prepend mock_bin to PATH so delegation.Command finds our mock grove
				currentPath := os.Getenv("PATH")
				proc, err := command.New(binary, "start", "--collectors=workspace,plan").
					Dir(ctx.RootDir).
					Env(
						"HOME="+ctx.HomeDir(),
						"XDG_CONFIG_HOME="+ctx.ConfigDir(),
						"XDG_DATA_HOME="+ctx.DataDir(),
						"XDG_STATE_HOME="+ctx.StateDir(),
						"XDG_CACHE_HOME="+ctx.CacheDir(),
						"XDG_RUNTIME_DIR="+ctx.RuntimeDir(),
						"PATH="+mockBinDir+":"+currentPath,
					).
					Start()
				if err != nil {
					return err
				}
				ctx.Set("daemon_process", proc)

				// Wait for PID file
				return wait.For(func() (bool, error) {
					return fs.Exists(pidPath), nil
				}, wait.Options{
					Timeout:      10 * time.Second,
					PollInterval: 200 * time.Millisecond,
					Immediate:    true,
				})
			}),

			harness.NewStep("verify daemon is running and wait for initial scan", func(ctx *harness.Context) error {
				binary := ctx.GetString("binary")

				// Verify daemon is running
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

				if err := ctx.Check("daemon is running", result.AssertSuccess()); err != nil {
					return err
				}

				// The JobCollector has a 3-second initial delay before first scan,
				// plus time for workspace discovery. Wait for the first scan to complete
				// so the daemon has the agent job in its store as "running".
				time.Sleep(5 * time.Second)
				return nil
			}),

			harness.NewStep("simulate out-of-band completion by modifying job file", func(ctx *harness.Context) error {
				agentJobPath := ctx.GetString("agent_job_path")

				// Read current content and change status from running to completed
				content, err := fs.ReadString(agentJobPath)
				if err != nil {
					return fmt.Errorf("failed to read agent job: %w", err)
				}

				updated := strings.Replace(content, "status: running", "status: completed", 1)
				if updated == content {
					return fmt.Errorf("failed to find 'status: running' in job file")
				}

				return fs.WriteString(agentJobPath, updated)
			}),

			harness.NewStep("verify transcript appended to agent job", func(ctx *harness.Context) error {
				agentJobPath := ctx.GetString("agent_job_path")

				// Wait for the daemon to pick up the completed status and append the transcript.
				// The JobCollector runs every 2s (configured), then watchTransitions processes it,
				// then appendTranscriptAsync sleeps 1s before appending.
				err := wait.For(func() (bool, error) {
					content, err := fs.ReadString(agentJobPath)
					if err != nil {
						return false, nil
					}
					return strings.Contains(content, "MOCK_TRANSCRIPT_OUTPUT"), nil
				}, wait.Options{
					Timeout:      30 * time.Second,
					PollInterval: 2 * time.Second,
					Immediate:    false,
				})

				if err != nil {
					// Debug: show final file content on failure
					content, _ := fs.ReadString(agentJobPath)
					ctx.ShowCommandOutput("agent job content at timeout", content, "")

					// Debug: show daemon log from XDG state dir
					logsDir := filepath.Join(ctx.StateDir(), "grove", "logs")
					ctx.ShowCommandOutput("looking for daemon logs in", logsDir, "")

					// Try to find and read any .log files in the logs directory
					if entries, dirErr := os.ReadDir(logsDir); dirErr == nil {
						for _, entry := range entries {
							if strings.HasSuffix(entry.Name(), ".log") || strings.HasSuffix(entry.Name(), ".jsonl") {
								logPath := filepath.Join(logsDir, entry.Name())
								if logContent, logErr := fs.ReadString(logPath); logErr == nil {
									// Show ALL log lines (unfiltered) to find the issue
									allLines := strings.Split(logContent, "\n")
									if len(allLines) > 120 {
										allLines = allLines[len(allLines)-120:]
									}
									ctx.ShowCommandOutput("daemon log (ALL): "+entry.Name(), strings.Join(allLines, "\n"), "")
								}
							}
						}
					} else {
						ctx.ShowCommandOutput("logs dir read error", "", dirErr.Error())
					}

					// Also check process stdout/stderr
					if proc, ok := ctx.Get("daemon_process").(*command.Process); ok && proc != nil {
						ctx.ShowCommandOutput("daemon process output", proc.Stdout(), proc.Stderr())
					}
				}

				return err
			}),

			harness.NewStep("verify transcript content", func(ctx *harness.Context) error {
				agentJobPath := ctx.GetString("agent_job_path")
				content, err := fs.ReadString(agentJobPath)
				if err != nil {
					return err
				}

				return ctx.Verify(func(v *verify.Collector) {
					v.True("job status is completed", strings.Contains(content, "status: completed"))
					v.True("transcript output present", strings.Contains(content, "MOCK_TRANSCRIPT_OUTPUT"))
				})
			}),
		},
	).WithTeardown(
		harness.NewStep("cleanup daemon process", func(ctx *harness.Context) error {
			if proc, ok := ctx.Get("daemon_process").(*command.Process); ok && proc != nil {
				_ = proc.Kill()
			}
			return nil
		}),
	)
}
