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

// DaemonHooksScenario tests that the daemon correctly executes configured
// on_skill_sync hooks when skills are synced.
func DaemonHooksScenario() *harness.Scenario {
	return harness.NewScenario(
		"daemon-hooks",
		"Verify daemon executes on_skill_sync hooks when skills change",
		[]string{"daemon", "hooks", "skills"},
		[]harness.Step{
			harness.NewStep("setup hooks config", func(ctx *harness.Context) error {
				// Create workspace directory with git repo
				workspaceDir := ctx.NewDir("hooks-workspace")
				if err := os.MkdirAll(workspaceDir, 0755); err != nil {
					return err
				}

				// Initialize a real git repo
				gitInit := command.New("git", "init").Dir(workspaceDir)
				if result := gitInit.Run(); result.ExitCode != 0 {
					return fmt.Errorf("git init failed: %s", result.Stderr)
				}

				// Create log file for hooks to write to
				logFile := filepath.Join(ctx.RootDir, "hooks.log")
				ctx.Set("log_file", logFile)
				ctx.Set("workspace_dir", workspaceDir)

				// Create notebook root for skills
				notebookRoot := filepath.Join(ctx.HomeDir(), ".grove", "notebooks", "main")
				notebookSkillsDir := filepath.Join(notebookRoot, "workspaces", "hooks-workspace", "skills")
				if err := os.MkdirAll(notebookSkillsDir, 0755); err != nil {
					return err
				}

				// Create global config directory
				configGroveDir := filepath.Join(ctx.ConfigDir(), "grove")
				if err := os.MkdirAll(configGroveDir, 0755); err != nil {
					return err
				}

				// Create grove.toml with groves, notebooks, and hooks configuration
				groveToml := filepath.Join(configGroveDir, "grove.toml")
				groveTomlContent := `version = "1.0"

[groves.test]
path = "` + workspaceDir + `"

[notebooks.rules]
default = "main"

[notebooks.definitions.main]
root_dir = "` + notebookRoot + `"

[daemon]

[[daemon.hooks.on_skill_sync]]
name = "test-skill-hook"
command = "echo \"synced: $GROVE_SYNCED_SKILLS\" >> '` + logFile + `'"
run_if = "changes"

[[daemon.hooks.on_skill_sync]]
name = "always-hook"
command = "echo 'skill_sync_triggered' >> '` + logFile + `'"
`
				if err := os.WriteFile(groveToml, []byte(groveTomlContent), 0644); err != nil {
					return err
				}

				// Create workspace grove.toml with skills config
				wsGroveToml := filepath.Join(workspaceDir, "grove.toml")
				wsConfig := `name = "hooks-workspace"

[skills]
use = ["hook-test-skill"]
providers = ["claude"]
`
				if err := os.WriteFile(wsGroveToml, []byte(wsConfig), 0644); err != nil {
					return err
				}

				ctx.Set("notebook_skills_dir", notebookSkillsDir)

				return nil
			}),
			harness.NewStep("start daemon", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				// Set up state directory
				groveStateDir := filepath.Join(ctx.StateDir(), "grove")
				if err := fs.EnsureDir(groveStateDir); err != nil {
					return err
				}

				groveRunDir := filepath.Join(ctx.RuntimeDir(), "grove")
				if err := fs.EnsureDir(groveRunDir); err != nil {
					return err
				}

				ctx.Set("pid_path", filepath.Join(groveStateDir, "groved.pid"))

				// Start daemon with workspace collector enabled
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

				ctx.Set("daemon_process", proc)

				// Wait for daemon to start
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
			harness.NewStep("trigger on_skill_sync hook", func(ctx *harness.Context) error {
				notebookSkillsDir := ctx.GetString("notebook_skills_dir")
				logFile := ctx.GetString("log_file")

				// Create the skill that the workspace is configured to use
				skillDir := filepath.Join(notebookSkillsDir, "hook-test-skill")
				if err := os.MkdirAll(skillDir, 0755); err != nil {
					return err
				}

				skillContent := `---
name: hook-test-skill
description: A skill to test on_skill_sync hooks
---

# Hook Test Skill

This skill tests the on_skill_sync hook.
`
				if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
					return err
				}

				// Wait for the skill sync hook to fire
				opts := wait.Options{
					Timeout:      15 * time.Second,
					PollInterval: 500 * time.Millisecond,
					Immediate:    false,
				}

				err := wait.For(func() (bool, error) {
					if !fs.Exists(logFile) {
						return false, nil
					}
					content, err := os.ReadFile(logFile)
					if err != nil {
						return false, nil
					}
					// Check for both hooks firing
					return strings.Contains(string(content), "skill_sync_triggered") &&
						strings.Contains(string(content), "hook-test-skill"), nil
				}, opts)
				if err != nil {
					return err
				}

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("hook log exists", nil, fs.AssertExists(logFile))
					v.Equal("always hook fired", nil, fs.AssertContains(logFile, "skill_sync_triggered"))
					v.Equal("changes hook fired with skill name", nil, fs.AssertContains(logFile, "hook-test-skill"))
				})
			}),
			harness.NewStep("verify run_if=changes respects condition", func(ctx *harness.Context) error {
				logFile := ctx.GetString("log_file")

				// Get the current log content to count occurrences
				content, err := os.ReadFile(logFile)
				if err != nil {
					return err
				}

				initialChangesCount := strings.Count(string(content), "synced:")
				initialAlwaysCount := strings.Count(string(content), "skill_sync_triggered")

				// Touch the skill file without making changes to content
				// This should NOT trigger the run_if=changes hook but MAY trigger the always hook
				notebookSkillsDir := ctx.GetString("notebook_skills_dir")
				skillFile := filepath.Join(notebookSkillsDir, "hook-test-skill", "SKILL.md")

				// Read and rewrite with same content
				skillContent, err := os.ReadFile(skillFile)
				if err != nil {
					return err
				}
				if err := os.WriteFile(skillFile, skillContent, 0644); err != nil {
					return err
				}

				// Wait a bit to ensure any potential sync would have happened
				time.Sleep(3 * time.Second)

				// Read log again
				newContent, err := os.ReadFile(logFile)
				if err != nil {
					return err
				}

				newChangesCount := strings.Count(string(newContent), "synced:")

				return ctx.Verify(func(v *verify.Collector) {
					// The "changes" hook should NOT fire again since content didn't change
					v.Equal("run_if=changes respected (no additional hook)", initialChangesCount, newChangesCount)
					// The always hook may or may not fire (depends on whether sync was triggered)
					// We just verify the changes hook didn't fire
					_ = initialAlwaysCount // Unused but we logged it for debugging
				})
			}),
		},
	).WithTeardown(
		harness.NewStep("cleanup daemon process", func(ctx *harness.Context) error {
			binary, _ := FindBinary()
			if binary != "" {
				_ = command.New(binary, "stop").
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
			}

			if proc, ok := ctx.Get("daemon_process").(*command.Process); ok && proc != nil {
				_ = proc.Kill()
			}
			return nil
		}),
	)
}
