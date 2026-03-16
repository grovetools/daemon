package cmd

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/daemon/internal/daemon/collector"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/pidfile"
	"github.com/grovetools/daemon/internal/daemon/server"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/watcher"
	"github.com/spf13/cobra"
)

// configWatchEnabled returns true if config watching is enabled in config.
// Defaults to true if not explicitly set to false.
func configWatchEnabled(cfg *config.Config) bool {
	if cfg.Daemon == nil || cfg.Daemon.ConfigWatch == nil {
		return true // Default enabled
	}
	return *cfg.Daemon.ConfigWatch
}

// configDebounceMs returns the config debounce setting or default (100ms).
func configDebounceMs(cfg *config.Config) int {
	if cfg.Daemon == nil || cfg.Daemon.ConfigDebounceMs <= 0 {
		return 100
	}
	return cfg.Daemon.ConfigDebounceMs
}

// NewGrovedCmd returns the groved daemon command with subcommands.
func NewGrovedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "groved",
		Short: "Grove ecosystem daemon",
		Long:  "Centralized state management daemon for the grove ecosystem.",
	}

	cmd.AddCommand(newGrovedStartCmd())
	cmd.AddCommand(newGrovedStopCmd())
	cmd.AddCommand(newGrovedStatusCmd())
	cmd.AddCommand(newGrovedConfigCmd())
	cmd.AddCommand(newGrovedMonitorCmd())

	return cmd
}

func newGrovedStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon",
		Long:  "Start the grove daemon in foreground mode.",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := logging.NewLogger("groved")
			pidPath := paths.PidFilePath()
			sockPath := paths.SocketPath()

			// Start pprof if requested
			if port, _ := cmd.Flags().GetInt("pprof-port"); port > 0 {
				go func() {
					addr := fmt.Sprintf("localhost:%d", port)
					logger.Infof("Starting pprof server on %s", addr)
					if err := http.ListenAndServe(addr, nil); err != nil {
						logger.WithError(err).Error("Failed to start pprof server")
					}
				}()
			}

			// Helper to check enabled collectors
			enabledCollectors, _ := cmd.Flags().GetStringSlice("collectors")
			isEnabled := func(name string) bool {
				for _, c := range enabledCollectors {
					if c == "all" || strings.TrimSpace(c) == name {
						return true
					}
				}
				return false
			}

			// 1. Acquire Lock
			if err := pidfile.Acquire(pidPath); err != nil {
				return fmt.Errorf("failed to start: %w", err)
			}
			defer func() {
				if err := pidfile.Release(pidPath); err != nil {
					logger.Errorf("Failed to release pidfile: %v", err)
				}
			}()

			// 2. Load config for daemon settings
			cfg, err := config.LoadDefault()
			if err != nil {
				logger.WithError(err).Warn("Failed to load config, using defaults")
				cfg = &config.Config{}
			}

			// Parse intervals from config with defaults
			// Defaults match the collector defaults: git=10s, session=2s, workspace=30s, plan=30s, note=60s
			gitInterval := 10 * time.Second
			sessionInterval := 2 * time.Second
			workspaceInterval := 30 * time.Second
			planInterval := 30 * time.Second
			noteInterval := 60 * time.Second

			if cfg.Daemon != nil {
				if cfg.Daemon.GitInterval != "" {
					if d, err := time.ParseDuration(cfg.Daemon.GitInterval); err == nil {
						gitInterval = d
					}
				}
				if cfg.Daemon.SessionInterval != "" {
					if d, err := time.ParseDuration(cfg.Daemon.SessionInterval); err == nil {
						sessionInterval = d
					}
				}
				if cfg.Daemon.WorkspaceInterval != "" {
					if d, err := time.ParseDuration(cfg.Daemon.WorkspaceInterval); err == nil {
						workspaceInterval = d
					}
				}
				if cfg.Daemon.PlanInterval != "" {
					if d, err := time.ParseDuration(cfg.Daemon.PlanInterval); err == nil {
						planInterval = d
					}
				}
				if cfg.Daemon.NoteInterval != "" {
					if d, err := time.ParseDuration(cfg.Daemon.NoteInterval); err == nil {
						noteInterval = d
					}
				}
			}

			// 3. Setup Store and Engine
			st := store.New()
			eng := engine.New(st, logger)

			// Register collectors with configured intervals based on flags
			if isEnabled("workspace") {
				eng.Register(collector.NewWorkspaceCollector(workspaceInterval))
			}
			if isEnabled("git") {
				eng.Register(collector.NewGitStatusCollector(gitInterval))
			}
			if isEnabled("session") {
				eng.Register(collector.NewSessionCollector(sessionInterval))
			}
			if isEnabled("plan") {
				eng.Register(collector.NewPlanCollector(planInterval))
			}
			if isEnabled("note") {
				eng.Register(collector.NewNoteCollector(noteInterval))
			}

			// 4. Setup Server with engine
			srv := server.New(logger)
			srv.SetEngine(eng)

			// Set running config for introspection
			srv.SetRunningConfig(&server.RunningConfig{
				GitInterval:       gitInterval,
				SessionInterval:   sessionInterval,
				WorkspaceInterval: workspaceInterval,
				PlanInterval:      planInterval,
				NoteInterval:      noteInterval,
				StartedAt:         time.Now(),
			})

			// 5. Handle Signals
			ctx, cancel := context.WithCancel(context.Background())
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

			go func() {
				<-stop
				logger.Info("Received stop signal")
				cancel() // Stop the engine

				// Create shutdown context with timeout
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shutdownCancel()

				if err := srv.Shutdown(shutdownCtx); err != nil {
					logger.Errorf("Server shutdown error: %v", err)
				}

				// Explicitly release pidfile before exit in signal handler
				_ = pidfile.Release(pidPath)
				os.Exit(0)
			}()

			// 6. Start Engine in background
			go eng.Start(ctx)

			// 7. Start ConfigWatcher if enabled
			if configWatchEnabled(cfg) {
				debounceMs := configDebounceMs(cfg)
				configWatcher, err := daemon.NewConfigWatcher(debounceMs, func(file string) {
					// Broadcast config reload event to all subscribers
					st.BroadcastConfigReload(file)
				})
				if err != nil {
					logger.WithError(err).Warn("Failed to start config watcher, continuing without it")
				} else {
					logger.Info("Config watcher started")
					go configWatcher.Start(ctx)
				}
			}

			// 7.5. Start SkillWatcher if enabled
			autoSync := true
			if cfg.Daemon != nil && cfg.Daemon.AutoSyncSkills != nil {
				autoSync = *cfg.Daemon.AutoSyncSkills
			}

			if autoSync {
				debounceMs := 1000
				if cfg.Daemon != nil && cfg.Daemon.SkillSyncDebounceMs > 0 {
					debounceMs = cfg.Daemon.SkillSyncDebounceMs
				}

				skillWatcher, err := watcher.NewSkillWatcher(st, cfg, debounceMs)
				if err != nil {
					logger.WithError(err).Warn("Failed to start skill watcher, continuing without it")
				} else {
					logger.Info("Skill watcher started")
					go skillWatcher.Start(ctx)
				}
			}

			// 8. Start Server (Blocking)
			logger.WithField("pid", os.Getpid()).Info("Starting daemon")
			if err := srv.ListenAndServe(sockPath); err != nil {
				return fmt.Errorf("server error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringSlice("collectors", []string{"all"}, "Comma-separated list of collectors to enable (git, session, workspace, plan, note)")
	cmd.Flags().Int("pprof-port", 0, "Port to start pprof server on (0 to disable)")

	return cmd
}

func newGrovedStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := paths.PidFilePath()

			running, pid, err := pidfile.IsRunning(pidPath)
			if err != nil {
				return fmt.Errorf("error checking status: %w", err)
			}

			if !running {
				fmt.Println("Daemon is not running")
				return nil
			}

			// Send SIGTERM
			process, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("failed to find process %d: %w", pid, err)
			}

			if err := process.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("failed to send stop signal: %w", err)
			}

			fmt.Printf("Sent SIGTERM to process %d\n", pid)
			return nil
		},
	}
}

func newGrovedStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := paths.PidFilePath()
			running, pid, err := pidfile.IsRunning(pidPath)

			if err != nil {
				return fmt.Errorf("error: %w", err)
			}

			if running {
				fmt.Printf("Running (PID: %d)\nSocket: %s\n", pid, paths.SocketPath())
			} else {
				fmt.Println("Stopped")
				os.Exit(1) // Return non-zero for stopped state (useful for scripts)
			}
			return nil
		},
	}
}

func newGrovedConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show running daemon configuration",
		Long:  "Query the running daemon to show its active configuration intervals.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := daemon.New()
			defer client.Close()

			if !client.IsRunning() {
				fmt.Println("Daemon is not running")
				os.Exit(1)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			cfg, err := client.GetConfig(ctx)
			if err != nil {
				return fmt.Errorf("failed to get config: %w", err)
			}

			fmt.Println("Running Daemon Configuration")
			fmt.Println("============================")
			fmt.Printf("Started At:         %s\n", cfg.StartedAt.Format(time.RFC3339))
			fmt.Printf("Uptime:             %s\n", time.Since(cfg.StartedAt).Round(time.Second))
			fmt.Println()
			fmt.Println("Collector Intervals:")
			fmt.Printf("  Git Status:       %s\n", cfg.GitInterval)
			fmt.Printf("  Session:          %s\n", cfg.SessionInterval)
			fmt.Printf("  Workspace:        %s\n", cfg.WorkspaceInterval)
			fmt.Printf("  Plan Stats:       %s\n", cfg.PlanInterval)
			fmt.Printf("  Note Counts:      %s\n", cfg.NoteInterval)

			return nil
		},
	}
}

func newGrovedMonitorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "monitor",
		Short: "Monitor daemon activity in real-time",
		Long:  "Subscribe to the daemon event stream and print activity logs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := daemon.New()
			defer client.Close()

			if !client.IsRunning() {
				fmt.Println("Daemon is not running")
				os.Exit(1)
			}

			ctx, cancel := context.WithCancel(context.Background())

			// Handle Ctrl+C gracefully
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-stop
				fmt.Println("\nDisconnecting...")
				cancel()
			}()

			stream, err := client.StreamState(ctx)
			if err != nil {
				return fmt.Errorf("failed to connect to stream: %w", err)
			}

			fmt.Println("Monitoring daemon activity (Ctrl+C to stop)...")
			fmt.Println("==============================================")

			for update := range stream {
				timestamp := time.Now().Format("15:04:05")
				switch update.UpdateType {
				case "initial":
					fmt.Printf("[%s] Connected: %d workspaces loaded\n", timestamp, len(update.Workspaces))
				case "workspaces":
					source := update.Source
					if source == "" {
						source = "unknown"
					}
					if update.Scanned > 0 && update.Scanned != len(update.Workspaces) {
						fmt.Printf("[%s] %s: scanned %d/%d\n", timestamp, formatSource(source), update.Scanned, len(update.Workspaces))
					} else {
						fmt.Printf("[%s] %s: %d workspaces\n", timestamp, formatSource(source), len(update.Workspaces))
					}
				case "sessions":
					// Count sessions by type/status
					var interactive, flowJobs, openCode, running, pending int
					for _, s := range update.Sessions {
						switch s.Type {
						case "opencode_session":
							openCode++
						case "interactive_agent", "agent", "oneshot", "chat", "headless_agent", "shell":
							flowJobs++
						default:
							interactive++
						}
						if s.Status == "running" {
							running++
						} else if s.Status == "pending_user" || s.Status == "idle" {
							pending++
						}
					}
					fmt.Printf("[%s] Session: %d total (%d running, %d pending) [interactive:%d flow:%d opencode:%d]\n",
						timestamp, len(update.Sessions), running, pending, interactive, flowJobs, openCode)
				case "focus":
					fmt.Printf("[%s] Focus: %d workspaces\n", timestamp, update.Scanned)
				case "config_reload":
					configFile := update.ConfigFile
					if configFile == "" {
						configFile = "unknown"
					}
					fmt.Printf("[%s] Config Reload: %s\n", timestamp, configFile)
				case "skill_sync":
					// Payload is a generic map after JSON unmarshaling over the stream
					if p, ok := update.Payload.(map[string]interface{}); ok {
						workspace, _ := p["workspace"].(string)
						errStr, _ := p["error"].(string)

						if errStr != "" {
							fmt.Printf("[%s] Skill Sync Error: %s - %s\n", timestamp, workspace, errStr)
						} else if skillsList, ok := p["synced_skills"].([]interface{}); ok && len(skillsList) > 0 {
							fmt.Printf("[%s] Skill Sync: %s synced %d skills to %v\n",
								timestamp, workspace, len(skillsList), p["dest_paths"])
						}
					}
				case "session":
					// Session lifecycle event - display details from payload
					if p, ok := update.Payload.(map[string]interface{}); ok {
						jobID, _ := p["job_id"].(string)
						if jobID == "" {
							// Try session_id for backwards compat
							jobID, _ = p["session_id"].(string)
						}

						// Detect event type from payload fields
						if _, hasNativeID := p["native_id"]; hasNativeID {
							// Confirmation event
							pid, _ := p["pid"].(float64)
							nativeID, _ := p["native_id"].(string)
							fmt.Printf("[%s] \033[32mSession Confirmed\033[0m: %s (PID: %.0f, native: %s)\n",
								timestamp, jobID, pid, truncateID(nativeID))
						} else if status, hasStatus := p["status"].(string); hasStatus {
							// Status update event
							fmt.Printf("[%s] \033[33mSession Status\033[0m: %s → %s\n",
								timestamp, jobID, status)
						} else if outcome, hasOutcome := p["outcome"].(string); hasOutcome {
							// End event
							fmt.Printf("[%s] \033[31mSession Ended\033[0m: %s (%s)\n",
								timestamp, jobID, outcome)
						} else if title, hasTitle := p["title"].(string); hasTitle {
							// Intent event
							planName, _ := p["plan_name"].(string)
							fmt.Printf("[%s] \033[36mSession Intent\033[0m: %s [%s] %s\n",
								timestamp, jobID, planName, title)
						} else {
							// Generic session event
							fmt.Printf("[%s] Session Event: %v\n", timestamp, p)
						}
					} else {
						fmt.Printf("[%s] Session: %s\n", timestamp, update.Source)
					}
				default:
					fmt.Printf("[%s] Update: %s\n", timestamp, update.UpdateType)
				}
			}

			return nil
		},
	}
}

// formatSource returns a human-readable label for the collector source.
func formatSource(source string) string {
	switch source {
	case "git":
		return "Git Status"
	case "workspace":
		return "Workspace Discovery"
	case "session":
		return "Session"
	case "plan":
		return "Plan Stats"
	case "note":
		return "Note Counts"
	case "config":
		return "Config Watcher"
	default:
		return source
	}
}

// truncateID truncates a UUID or long ID for display (first 8 chars).
func truncateID(id string) string {
	if len(id) > 8 {
		return id[:8] + "..."
	}
	return id
}
