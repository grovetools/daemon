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

// DaemonSkillWatcherScenario tests that the daemon's skill watcher properly
// syncs skills to workspaces when skill files change.
func DaemonSkillWatcherScenario() *harness.Scenario {
	return harness.NewScenario(
		"daemon-skill-watcher",
		"Verify daemon background skill watcher syncs skills on file changes",
		[]string{"daemon", "watcher", "skills"},
		[]harness.Step{
			harness.NewStep("setup workspace and skill directories", func(ctx *harness.Context) error {
				// Create a workspace directory with git repo (required for workspace detection)
				workspaceDir := ctx.NewDir("test-workspace")
				if err := os.MkdirAll(workspaceDir, 0755); err != nil {
					return err
				}

				// Initialize a real git repo (skill sync requires proper git root)
				gitInit := command.New("git", "init").Dir(workspaceDir)
				if result := gitInit.Run(); result.ExitCode != 0 {
					return fmt.Errorf("git init failed: %s", result.Stderr)
				}

				// Create grove.toml with skills configuration
				groveToml := filepath.Join(workspaceDir, "grove.toml")
				groveTomlContent := `name = "test-workspace"

[skills]
use = ["test-skill"]
providers = ["claude"]
`
				if err := os.WriteFile(groveToml, []byte(groveTomlContent), 0644); err != nil {
					return err
				}

				// Create notebook root for skills
				notebookRoot := filepath.Join(ctx.HomeDir(), ".grove", "notebooks", "main")
				if err := os.MkdirAll(notebookRoot, 0755); err != nil {
					return err
				}

				// Create global config that registers the test workspace as a grove
				// and configures notebook for skill discovery (following skills test pattern)
				globalConfigDir := filepath.Join(ctx.ConfigDir(), "grove")
				if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
					return err
				}
				globalConfig := filepath.Join(globalConfigDir, "grove.toml")
				globalConfigContent := `version = "1.0"

[groves.test]
path = "` + workspaceDir + `"

[notebooks.rules]
default = "main"

[notebooks.definitions.main]
root_dir = "` + notebookRoot + `"
`
				if err := os.WriteFile(globalConfig, []byte(globalConfigContent), 0644); err != nil {
					return err
				}

				// Create skills directory in notebook for the workspace
				notebookSkillsDir := filepath.Join(notebookRoot, "workspaces", "test-workspace", "skills")
				if err := os.MkdirAll(notebookSkillsDir, 0755); err != nil {
					return err
				}

				// Store paths for later steps
				ctx.Set("workspace_dir", workspaceDir)
				ctx.Set("notebook_skills_dir", notebookSkillsDir)
				ctx.Set("skill_dest", filepath.Join(workspaceDir, ".claude", "skills", "test-skill", "SKILL.md"))

				return nil
			}),
			harness.NewStep("start daemon with skill watching", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				// Set up state directory for PID
				groveStateDir := filepath.Join(ctx.StateDir(), "grove")
				if err := fs.EnsureDir(groveStateDir); err != nil {
					return err
				}

				groveRunDir := filepath.Join(ctx.RuntimeDir(), "grove")
				if err := fs.EnsureDir(groveRunDir); err != nil {
					return err
				}

				ctx.Set("pid_path", filepath.Join(groveStateDir, "groved.pid"))

				// Start daemon - it will auto-sync skills when they change
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
			harness.NewStep("create skill file and verify sync", func(ctx *harness.Context) error {
				notebookSkillsDir := ctx.GetString("notebook_skills_dir")
				skillDir := filepath.Join(notebookSkillsDir, "test-skill")

				// Create the skill directory and SKILL.md file in the notebook
				if err := os.MkdirAll(skillDir, 0755); err != nil {
					return err
				}

				skillContent := `---
name: test-skill
description: A test skill for daemon watcher E2E testing
---

# Test Skill

This is a test skill for daemon watcher E2E testing.

## Usage

Use this skill to verify the daemon's skill sync functionality.
`
				skillFile := filepath.Join(skillDir, "SKILL.md")
				if err := os.WriteFile(skillFile, []byte(skillContent), 0644); err != nil {
					return err
				}

				// Wait for the daemon's fsnotify watcher to detect the change
				// and sync the skill to the workspace
				skillDest := ctx.GetString("skill_dest")
				opts := wait.Options{
					Timeout:      15 * time.Second,
					PollInterval: 500 * time.Millisecond,
					Immediate:    false,
				}

				err := wait.For(func() (bool, error) {
					return fs.Exists(skillDest), nil
				}, opts)
				if err != nil {
					return err
				}

				// Verify the synced skill content
				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("skill synced to workspace", nil, fs.AssertExists(skillDest))
					v.Equal("skill contains expected content", nil, fs.AssertContains(skillDest, "Test Skill"))
				})
			}),
			harness.NewStep("modify config and verify re-sync", func(ctx *harness.Context) error {
				workspaceDir := ctx.GetString("workspace_dir")
				notebookSkillsDir := ctx.GetString("notebook_skills_dir")

				// Create a new skill in the notebook
				newSkillDir := filepath.Join(notebookSkillsDir, "new-skill")
				if err := os.MkdirAll(newSkillDir, 0755); err != nil {
					return err
				}

				newSkillContent := `---
name: new-skill
description: A new skill added after initial config
---

# New Skill

This skill was added after the initial config.
`
				if err := os.WriteFile(filepath.Join(newSkillDir, "SKILL.md"), []byte(newSkillContent), 0644); err != nil {
					return err
				}

				// Update grove.toml to include the new skill
				groveToml := filepath.Join(workspaceDir, "grove.toml")
				newConfig := `name = "test-workspace"

[skills]
use = ["test-skill", "new-skill"]
providers = ["claude"]
`
				if err := os.WriteFile(groveToml, []byte(newConfig), 0644); err != nil {
					return err
				}

				// Wait for config watcher to detect the change and sync
				newSkillDest := filepath.Join(workspaceDir, ".claude", "skills", "new-skill", "SKILL.md")
				opts := wait.Options{
					Timeout:      15 * time.Second,
					PollInterval: 500 * time.Millisecond,
					Immediate:    false,
				}

				err := wait.For(func() (bool, error) {
					return fs.Exists(newSkillDest), nil
				}, opts)
				if err != nil {
					return err
				}

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("new skill synced to workspace", nil, fs.AssertExists(newSkillDest))
					v.Equal("new skill contains expected content", nil, fs.AssertContains(newSkillDest, "New Skill"))
				})
			}),
		},
	).WithTeardown(
		harness.NewStep("cleanup daemon process", func(ctx *harness.Context) error {
			binary, _ := FindBinary()
			if binary != "" {
				// Try graceful stop first
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

			// Force kill if still running
			if proc, ok := ctx.Get("daemon_process").(*command.Process); ok && proc != nil {
				_ = proc.Kill()
			}
			return nil
		}),
	)
}

// DaemonSkillWatcherPruneScenario tests that the daemon prunes skills
// when they are removed from configuration.
func DaemonSkillWatcherPruneScenario() *harness.Scenario {
	return harness.NewScenario(
		"daemon-skill-watcher-prune",
		"Verify daemon prunes skills when removed from config",
		[]string{"daemon", "watcher", "skills", "prune"},
		[]harness.Step{
			harness.NewStep("setup workspace with skills", func(ctx *harness.Context) error {
				// Create workspace with git repo
				workspaceDir := ctx.NewDir("prune-workspace")
				if err := os.MkdirAll(workspaceDir, 0755); err != nil {
					return err
				}

				// Initialize a real git repo
				gitInit := command.New("git", "init").Dir(workspaceDir)
				if result := gitInit.Run(); result.ExitCode != 0 {
					return fmt.Errorf("git init failed: %s", result.Stderr)
				}

				// Create grove.toml with two skills
				groveToml := filepath.Join(workspaceDir, "grove.toml")
				groveTomlContent := `name = "prune-workspace"

[skills]
use = ["keep-skill", "remove-skill"]
providers = ["claude"]
`
				if err := os.WriteFile(groveToml, []byte(groveTomlContent), 0644); err != nil {
					return err
				}

				// Create notebook root for skills
				notebookRoot := filepath.Join(ctx.HomeDir(), ".grove", "notebooks", "main")
				notebookSkillsDir := filepath.Join(notebookRoot, "workspaces", "prune-workspace", "skills")
				if err := os.MkdirAll(notebookSkillsDir, 0755); err != nil {
					return err
				}

				// Create global config
				globalConfigDir := filepath.Join(ctx.ConfigDir(), "grove")
				if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
					return err
				}
				globalConfig := filepath.Join(globalConfigDir, "grove.toml")
				globalConfigContent := `version = "1.0"

[groves.test]
path = "` + workspaceDir + `"

[notebooks.rules]
default = "main"

[notebooks.definitions.main]
root_dir = "` + notebookRoot + `"
`
				if err := os.WriteFile(globalConfig, []byte(globalConfigContent), 0644); err != nil {
					return err
				}

				// Create skills in notebook
				for _, skillName := range []string{"keep-skill", "remove-skill"} {
					skillDir := filepath.Join(notebookSkillsDir, skillName)
					if err := os.MkdirAll(skillDir, 0755); err != nil {
						return err
					}
					content := `---
name: ` + skillName + `
description: Test skill for pruning
---

# ` + strings.Title(strings.ReplaceAll(skillName, "-", " ")) + `

Skill content.
`
					if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
						return err
					}
				}

				ctx.Set("workspace_dir", workspaceDir)
				ctx.Set("remove_skill_dest", filepath.Join(workspaceDir, ".claude", "skills", "remove-skill", "SKILL.md"))
				ctx.Set("keep_skill_dest", filepath.Join(workspaceDir, ".claude", "skills", "keep-skill", "SKILL.md"))

				return nil
			}),
			harness.NewStep("start daemon and wait for initial sync", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				groveStateDir := filepath.Join(ctx.StateDir(), "grove")
				if err := fs.EnsureDir(groveStateDir); err != nil {
					return err
				}

				groveRunDir := filepath.Join(ctx.RuntimeDir(), "grove")
				if err := fs.EnsureDir(groveRunDir); err != nil {
					return err
				}

				ctx.Set("pid_path", filepath.Join(groveStateDir, "groved.pid"))

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

				// Wait for daemon to start and initial sync
				pidPath := ctx.GetString("pid_path")
				removeSkillDest := ctx.GetString("remove_skill_dest")

				opts := wait.Options{
					Timeout:      15 * time.Second,
					PollInterval: 500 * time.Millisecond,
					Immediate:    true,
				}

				// Wait for daemon PID file
				if err := wait.For(func() (bool, error) {
					return fs.Exists(pidPath), nil
				}, opts); err != nil {
					return err
				}

				// Wait for initial skill sync
				return wait.For(func() (bool, error) {
					return fs.Exists(removeSkillDest), nil
				}, opts)
			}),
			harness.NewStep("remove skill from config and verify prune", func(ctx *harness.Context) error {
				workspaceDir := ctx.GetString("workspace_dir")
				removeSkillDest := ctx.GetString("remove_skill_dest")
				keepSkillDest := ctx.GetString("keep_skill_dest")

				// Verify both skills exist before removal
				if err := ctx.Check("remove-skill exists before update", fs.AssertExists(removeSkillDest)); err != nil {
					return err
				}
				if err := ctx.Check("keep-skill exists before update", fs.AssertExists(keepSkillDest)); err != nil {
					return err
				}

				// Update config to remove one skill
				groveToml := filepath.Join(workspaceDir, "grove.toml")
				newConfig := `name = "prune-workspace"

[skills]
use = ["keep-skill"]
providers = ["claude"]
`
				if err := os.WriteFile(groveToml, []byte(newConfig), 0644); err != nil {
					return err
				}

				// Wait for the skill to be pruned
				opts := wait.Options{
					Timeout:      15 * time.Second,
					PollInterval: 500 * time.Millisecond,
					Immediate:    false,
				}

				err := wait.For(func() (bool, error) {
					return !fs.Exists(removeSkillDest), nil
				}, opts)
				if err != nil {
					return err
				}

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("removed skill was pruned", nil, fs.AssertNotExists(removeSkillDest))
					v.Equal("kept skill still exists", nil, fs.AssertExists(keepSkillDest))
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
