package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/grovetools/core/command"
	"github.com/grovetools/core/config"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/logging/logutil"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/util/pathutil"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/sirupsen/logrus"
	"github.com/grovetools/daemon/internal/daemon/collector"
	"github.com/grovetools/daemon/internal/daemon/engine"
	daemonenv "github.com/grovetools/daemon/internal/daemon/env"
	"github.com/grovetools/daemon/internal/daemon/jobrunner"
	"github.com/grovetools/daemon/internal/daemon/logstreamer"
	"github.com/grovetools/daemon/internal/daemon/autonomous"
	daemonchannels "github.com/grovetools/daemon/internal/daemon/channels"
	"github.com/grovetools/daemon/internal/daemon/pidfile"
	"github.com/grovetools/daemon/internal/daemon/server"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/agentlogs/pkg/agentstream"
	notifyconfig "github.com/grovetools/notify/pkg/config"
	"github.com/grovetools/daemon/internal/daemon/watcher"
	"github.com/grovetools/flow/pkg/orchestration"
	"github.com/grovetools/grove-gemini/pkg/gemini"
	"github.com/grovetools/memory/pkg/memory"
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
			// Route all daemon logs to central system log
			grovelogging.SetGlobalScope(grovelogging.ScopeSystem)

			logger := grovelogging.NewLogger("groved")
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
			// Defaults: git=10s, session=2s, workspace=5m, plan=5m, note=5m
			// Long intervals are safe because event-driven watchers handle real-time updates.
			gitInterval := 10 * time.Second
			sessionInterval := 2 * time.Second
			workspaceInterval := 5 * time.Minute
			planInterval := 5 * time.Minute
			noteInterval := 5 * time.Minute

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

			// Suppress pretty CLI output from in-process job executors.
			// The daemon's monitor uses fmt.Print directly — it does not go through
			// the global writer. But executor code (cx context generation, ulog.Emit()
			// without ctx) falls through to GetGlobalOutput() which defaults to os.Stdout.
			// Redirecting it to io.Discard prevents that output from leaking to the terminal.
			grovelogging.SetGlobalOutput(io.Discard)

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
				eng.Register(collector.NewJobCollector(planInterval))
			}
			if isEnabled("note") {
				eng.Register(collector.NewNoteCollector(noteInterval))
			}

			// 3.5 Setup context early (needed by JobRunner and Engine)
			ctx, cancel := context.WithCancel(context.Background())

			// 3.6 Setup JobRunner
			jobsEnabled := true
			if cfg.Daemon != nil && cfg.Daemon.Jobs != nil && cfg.Daemon.Jobs.Enabled != nil {
				jobsEnabled = *cfg.Daemon.Jobs.Enabled
			}

			var jr *jobrunner.JobRunner
			if jobsEnabled {
				workers := 4
				if cfg.Daemon != nil && cfg.Daemon.Jobs != nil && cfg.Daemon.Jobs.MaxConcurrent > 0 {
					workers = cfg.Daemon.Jobs.MaxConcurrent
				}

				execTimeout := 30 * time.Minute
				if cfg.Daemon != nil && cfg.Daemon.Jobs != nil && cfg.Daemon.Jobs.DefaultTimeout != "" {
					if d, err := time.ParseDuration(cfg.Daemon.Jobs.DefaultTimeout); err == nil {
						execTimeout = d
					}
				}

				execConfig := &orchestration.ExecutorConfig{
					MaxPromptLength: 1000000,
					Timeout:         execTimeout,
					RetryCount:      2,
					Model:           "default",
				}
				localRuntime := orchestration.NewLocalRuntime(
					execConfig,
					&command.RealExecutor{},
					&noopStatusUpdater{},
					orchestration.NewDefaultLogger(),
				)

				var persistDir string
				if cfg.Daemon != nil && cfg.Daemon.Jobs != nil && cfg.Daemon.Jobs.PersistDir != "" {
					persistDir = cfg.Daemon.Jobs.PersistDir
				}
				persister := jobrunner.NewPersistenceWithDir(persistDir)

				jr = jobrunner.New(st, localRuntime, workers, persister)
				go jr.Start(ctx)
				logger.WithField("workers", workers).Info("JobRunner started")
			}

			// 3.7 Setup LogStreamer
			logBufSize := 1000
			logMaxSubs := 10
			logPollInterval := 500 * time.Millisecond
			streamer := logstreamer.New(st, logBufSize, logMaxSubs, logPollInterval)

			// 4. Setup Server with engine and env manager
			envManager := daemonenv.NewManager(logger)

			// Restore environment state from disk to prevent port collisions
			restoreLogger := logrus.New()
			restoreLogger.SetLevel(logrus.WarnLevel)
			discoveryService := workspace.NewDiscoveryService(restoreLogger)
			if discoveryResult, err := discoveryService.DiscoverAll(); err == nil {
				wsProvider := workspace.NewProvider(discoveryResult)
				envManager.Restore(wsProvider)
			} else {
				logger.WithError(err).Warn("Failed to discover workspaces for env restore")
			}

			// Start proxy server in background on standard grove proxy port
			go func() {
				if err := envManager.Proxy.ListenAndServe(":8443"); err != nil {
					logger.WithError(err).Warn("Proxy server stopped")
				}
			}()

			srv := server.New(logger)
			srv.SetEngine(eng)
			srv.SetEnvManager(envManager)
			if jr != nil {
				srv.SetJobRunner(jr)
			}
			srv.SetLogStreamer(streamer)

			// sendInputToTmux sends a message to an agent running in a tmux pane.
			// Uses agentstream.SendInput which handles Escape+i for vim-style agents.
			sendInputToTmux := func(ctx context.Context, tmuxTarget, message string) error {
				return agentstream.SendInput(ctx, tmuxTarget, message)
			}

			// Initialize channel manager if signal is configured
			notifyCfg := notifyconfig.Load()
			if notifyCfg.Signal.Enabled {
				chMgr := daemonchannels.NewManager(st, daemonchannels.SignalConfig{
					Enabled:   notifyCfg.Signal.Enabled,
					CLIPath:   notifyCfg.Signal.CLIPath,
					Account:   notifyCfg.Signal.Account,
					Allowlist: notifyCfg.Signal.Allowlist,
				})
				chMgr.SendInput = sendInputToTmux
				chMgr.Start(ctx)
				srv.SetChannelManager(chMgr)
				logger.Info("Channel manager initialized (signal enabled)")
			}

			// Register autonomous pinger as a collector
			pinger := autonomous.NewPinger(st, "")
			pinger.SendInput = sendInputToTmux
			eng.Register(pinger)

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
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

			go func() {
				<-stop
				logger.Info("Received stop signal")
				envManager.Shutdown() // Teardown all running environments and proxy routes
				streamer.Stop()       // Stop all log tailing goroutines
				cancel()              // Stop the engine

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

			// 5.5. Start inline monitor early so it captures all events from boot
			if monitor, _ := cmd.Flags().GetBool("monitor"); monitor {
				monitorFormat, _ := cmd.Flags().GetString("monitor-format")
				monitorCompact, _ := cmd.Flags().GetBool("monitor-compact")
				go runInlineMonitor(ctx, st, monitorFormat, monitorCompact)
			}

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

			// 7.5. Start UnifiedWatcher with registered domain handlers
			unifiedWatcher, err := watcher.NewUnifiedWatcher(st, 100*time.Millisecond, logger)
			if err != nil {
				logger.WithError(err).Warn("Failed to start unified watcher, continuing without it")
			} else {
				// Register SkillHandler if auto-sync is enabled
				autoSync := true
				if cfg.Daemon != nil && cfg.Daemon.AutoSyncSkills != nil {
					autoSync = *cfg.Daemon.AutoSyncSkills
				}

				if autoSync {
					debounceMs := 1000
					if cfg.Daemon != nil && cfg.Daemon.SkillSyncDebounceMs > 0 {
						debounceMs = cfg.Daemon.SkillSyncDebounceMs
					}

					skillHandler, err := watcher.NewSkillHandler(st, cfg, debounceMs)
					if err != nil {
						logger.WithError(err).Warn("Failed to initialize skill handler")
					} else {
						unifiedWatcher.Register(skillHandler)
						logger.Info("Skill handler registered with unified watcher")
					}
				}

				// Register WorkspaceHandler for instant discovery on fs changes
				if isEnabled("workspace") {
					workspaceHandler := watcher.NewWorkspaceHandler(st, cfg, 2000)
					unifiedWatcher.Register(workspaceHandler)
					logger.Info("Workspace handler registered with unified watcher")
				}

				// Register FlowHandler for plan directory watching
				if isEnabled("plan") {
					flowHandler := watcher.NewFlowHandler(st, cfg, 2000)
					unifiedWatcher.Register(flowHandler)
					logger.Info("Flow handler registered with unified watcher")
				}

				// Register NoteHandler for note directory watching
				if isEnabled("note") {
					noteHandler := watcher.NewNoteHandler(st, cfg, 3000)
					unifiedWatcher.Register(noteHandler)
					logger.Info("Note handler registered with unified watcher")
				}

				// Register MemoryHandler for auto-indexing content
				dbPath, err := pathutil.Expand("~/.local/share/grove/memory/memory.db")
				if err == nil {
					memStore, err := memory.Open(dbPath, 3072) // gemini-embedding-001 outputs 3072 dimensions
					if err != nil {
						logger.WithError(err).Warn("Failed to initialize memory store, indexing disabled")
					} else {
						// Use grove-gemini's config resolver (secrets.toml, env var, api_key_command)
						geminiClient, err := gemini.NewClient(ctx, "")
						if err != nil {
							logger.WithError(err).Warn("Failed to initialize Gemini client, memory indexing disabled")
						} else {
							embedder := memory.NewEmbedder(geminiClient, gemini.DefaultEmbeddingModel)

							memoryHandler := watcher.NewMemoryHandler(st, cfg, memStore, embedder, 5000)
							unifiedWatcher.Register(memoryHandler)
							logger.Info("Memory handler registered with unified watcher")

							// Share the same store + embedder with the HTTP server so
							// /api/memory/* handlers can serve TUI clients without
							// opening a second SQLite connection.
							srv.SetMemoryStore(memStore, embedder, dbPath)
						}
					}
				}

				logger.Info("Unified watcher started")
				go unifiedWatcher.Start(ctx)
			}

			// 8. Start Server (Blocking)
			httpPort, _ := cmd.Flags().GetInt("http-port")
			logger.WithField("pid", os.Getpid()).Info("Starting daemon")
			if err := srv.ListenAndServe(sockPath, httpPort); err != nil {
				return fmt.Errorf("server error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringSlice("collectors", []string{"all"}, "Comma-separated list of collectors to enable (git, session, workspace, plan, note)")
	cmd.Flags().Int("pprof-port", 0, "Port to start pprof server on (0 to disable)")
	cmd.Flags().Int("http-port", 0, "Port to start HTTP server on for browser access (web terminal viewer, 0 to disable)")
	cmd.Flags().Bool("monitor", false, "Stream daemon activity to stdout")
	cmd.Flags().String("monitor-format", "full", "Output format for --monitor: text, json, full, rich, pretty")
	cmd.Flags().Bool("monitor-compact", true, "Disable spacing between monitor log entries")

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
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Monitor daemon activity in real-time",
		Long:  "Subscribe to the daemon event stream and print activity logs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			grovelogging.SetGlobalScope(grovelogging.ScopeSystem)

			format, _ := cmd.Flags().GetString("format")
			compact, _ := cmd.Flags().GetBool("compact")

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

			emit, ms := monitorEmitter("groved.monitor", format, compact)
			emit("info", "Monitoring daemon activity", nil)

			for update := range stream {
				switch update.UpdateType {
				case "initial":
					ms.lastWorkspaces = len(update.Workspaces)
					emit("info", "Connected", map[string]interface{}{
						"workspaces": len(update.Workspaces),
					})
				case "workspaces":
					source := update.Source
					if source == "" {
						source = "unknown"
					}
					fields := map[string]interface{}{
						"source":     source,
						"workspaces": len(update.Workspaces),
					}
					if update.Scanned > 0 && update.Scanned != len(update.Workspaces) {
						fields["scanned"] = update.Scanned
					}
					level := "debug"
					if len(update.Workspaces) != ms.lastWorkspaces {
						level = "info"
						ms.lastWorkspaces = len(update.Workspaces)
					}
					emit(level, formatSource(source), fields)
				case "sessions":
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
					summary := fmt.Sprintf("%d/%d/%d/%d/%d/%d", len(update.Sessions), running, pending, interactive, flowJobs, openCode)
					level := "debug"
					if summary != ms.lastSessions {
						level = "info"
						ms.lastSessions = summary
					}
					emit(level, "Session", map[string]interface{}{
						"total":       len(update.Sessions),
						"running":     running,
						"pending":     pending,
						"interactive": interactive,
						"flow":        flowJobs,
						"opencode":    openCode,
					})
				case "focus":
					level := "debug"
					if update.Scanned != ms.lastFocus {
						level = "info"
						ms.lastFocus = update.Scanned
					}
					emit(level, "Focus", map[string]interface{}{
						"workspaces": update.Scanned,
					})
				case "config_reload":
					configFile := update.ConfigFile
					if configFile == "" {
						configFile = "unknown"
					}
					emit("info", "Config Reload", map[string]interface{}{
						"file": configFile,
					})
				case "watcher_status":
					if p, ok := update.Payload.(map[string]interface{}); ok {
						emit("info", "Watcher", p)
					}
				case "skill_sync":
					if p, ok := update.Payload.(map[string]interface{}); ok {
						if errStr, _ := p["error"].(string); errStr != "" {
							emit("error", "Skill Sync", p)
						} else if skillsList, ok := p["synced_skills"].([]interface{}); ok && len(skillsList) > 0 {
							emit("info", "Skill Sync", p)
						}
					}
				case "session":
					if p, ok := update.Payload.(map[string]interface{}); ok {
						jobID, _ := p["job_id"].(string)
						if jobID == "" {
							jobID, _ = p["session_id"].(string)
						}
						if _, hasNativeID := p["native_id"]; hasNativeID {
							emit("info", "Session Confirmed", p)
						} else if _, hasStatus := p["status"].(string); hasStatus {
							emit("info", "Session Status", p)
						} else if _, hasOutcome := p["outcome"].(string); hasOutcome {
							emit("warning", "Session Ended", p)
						} else if _, hasTitle := p["title"].(string); hasTitle {
							emit("info", "Session Intent", p)
						}
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().String("format", "full", "Output format: text, json, full, rich, pretty")
	cmd.Flags().Bool("compact", true, "Disable spacing between log entries")

	return cmd
}

// monitorState tracks previous values for change detection.
type monitorState struct {
	lastWorkspaces int
	lastSessions   string // serialized summary for comparison
	lastFocus      int
}

// monitorEmitter returns a function that writes structured JSON to the system log
// and prints formatted output to stdout in the requested format.
// Routine polling events are emitted at DEBUG unless values changed.
func monitorEmitter(component, format string, compact bool) (func(level, msg string, fields map[string]interface{}), *monitorState) {
	ulog := grovelogging.NewUnifiedLogger(component)
	state := &monitorState{}

	emit := func(level, msg string, fields map[string]interface{}) {
		// Build the structured entry and write to log file only
		entry := ulog.Info(msg).StructuredOnly()
		switch level {
		case "warning":
			entry = ulog.Warn(msg).StructuredOnly()
		case "error":
			entry = ulog.Error(msg).StructuredOnly()
		case "debug":
			entry = ulog.Debug(msg).StructuredOnly()
		}
		if fields != nil {
			entry.Fields(fields)
		}
		entry.Emit()

		// Don't print debug events to terminal (they still go to log file)
		if level == "debug" {
			return
		}

		// Build a log map for the format function
		logMap := map[string]interface{}{
			"time":      time.Now().Format(time.RFC3339),
			"level":     level,
			"msg":       msg,
			"component": component,
		}
		for k, v := range fields {
			logMap[k] = v
		}

		fmt.Print(logutil.FormatLogLine(logMap, "system", format, compact))
	}

	return emit, state
}

// runInlineMonitor subscribes to the store directly and prints updates to stdout.
// This avoids the need to connect via the HTTP client and captures events from startup.
func runInlineMonitor(ctx context.Context, st *store.Store, format string, compact bool) {
	emit, ms := monitorEmitter("groved.monitor", format, compact)

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-sub:
			if !ok {
				return
			}
			switch update.Type {
			case store.UpdateWorkspaces:
				source := update.Source
				if source == "" {
					source = "unknown"
				}
				wsCount := 0
				fields := map[string]interface{}{"source": source}
				if wsMap, ok := update.Payload.(map[string]*models.EnrichedWorkspace); ok {
					wsCount = len(wsMap)
					fields["workspaces"] = wsCount
					if update.Scanned > 0 && update.Scanned != wsCount {
						fields["scanned"] = update.Scanned
					}
				}
				level := "debug"
				if wsCount != ms.lastWorkspaces {
					level = "info"
					ms.lastWorkspaces = wsCount
				}
				emit(level, formatSource(source), fields)
			case store.UpdateSessions:
				if sessions, ok := update.Payload.([]*models.Session); ok {
					var interactive, flowJobs, openCode, running, pending int
					for _, s := range sessions {
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
					summary := fmt.Sprintf("%d/%d/%d/%d/%d/%d", len(sessions), running, pending, interactive, flowJobs, openCode)
					level := "debug"
					if summary != ms.lastSessions {
						level = "info"
						ms.lastSessions = summary
					}
					emit(level, "Session", map[string]interface{}{
						"total":       len(sessions),
						"running":     running,
						"pending":     pending,
						"interactive": interactive,
						"flow":        flowJobs,
						"opencode":    openCode,
					})
				}
			case store.UpdateFocus:
				level := "debug"
				if update.Scanned != ms.lastFocus {
					level = "info"
					ms.lastFocus = update.Scanned
				}
				emit(level, "Focus", map[string]interface{}{
					"workspaces": update.Scanned,
				})
			case store.UpdateConfigReload:
				file, _ := update.Payload.(string)
				emit("info", "Config Reload", map[string]interface{}{
					"file": file,
				})
			case store.UpdateWatcherStatus:
				if p, ok := update.Payload.(map[string]string); ok {
					fields := map[string]interface{}{}
					for k, v := range p {
						fields[k] = v
					}
					emit("info", "Watcher", fields)
				} else if p, ok := update.Payload.(map[string]interface{}); ok {
					emit("info", "Watcher", p)
				}
			case store.UpdateSkillSync:
				if p, ok := update.Payload.(store.SkillSyncPayload); ok {
					fields := map[string]interface{}{
						"workspace": p.Workspace,
					}
					if p.Error != "" {
						fields["error"] = p.Error
						emit("error", "Skill Sync", fields)
					} else if len(p.SyncedSkills) > 0 {
						fields["synced"] = len(p.SyncedSkills)
						fields["dest_paths"] = p.DestPaths
						emit("info", "Skill Sync", fields)
					}
				}
			case store.UpdateSessionIntent:
				if p, ok := update.Payload.(*store.SessionIntentPayload); ok {
					emit("info", "Session Intent", map[string]interface{}{
						"job_id": p.JobID,
						"plan":   p.PlanName,
						"title":  p.Title,
					})
				}
			case store.UpdateSessionConfirmation:
				if p, ok := update.Payload.(*store.SessionConfirmationPayload); ok {
					emit("info", "Session Confirmed", map[string]interface{}{
						"job_id":    p.JobID,
						"pid":       p.PID,
						"native_id": truncateID(p.NativeID),
					})
				}
			case store.UpdateSessionStatus:
				if p, ok := update.Payload.(*store.SessionStatusPayload); ok {
					emit("info", "Session Status", map[string]interface{}{
						"job_id": p.JobID,
						"status": p.Status,
					})
				}
			case store.UpdateSessionEnd:
				if p, ok := update.Payload.(*store.SessionEndPayload); ok {
					emit("warning", "Session Ended", map[string]interface{}{
						"job_id":  p.JobID,
						"outcome": p.Outcome,
					})
				}
			}
		}
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

// noopStatusUpdater satisfies orchestration.StatusUpdater without doing anything.
// The daemon's JobRunner manages status updates via the store, not via this callback.
type noopStatusUpdater struct{}

func (n *noopStatusUpdater) UpdateJobStatus(job *orchestration.Job, status orchestration.JobStatus) error {
	return nil
}

func (n *noopStatusUpdater) UpdateJobMetadata(job *orchestration.Job, meta orchestration.JobMetadata) error {
	return nil
}
