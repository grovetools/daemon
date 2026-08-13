package cmd

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108: intentional debug endpoint for daemon operator use
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/grovetools/core/command"
	"github.com/grovetools/core/config"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/logging/logutil"
	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/pairwatch"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/pathutil"
	"github.com/grovetools/daemon/internal/daemon/autonomous"
	"github.com/grovetools/daemon/internal/daemon/buildqueue"
	daemonchannels "github.com/grovetools/daemon/internal/daemon/channels"
	"github.com/grovetools/daemon/internal/daemon/collector"
	"github.com/grovetools/daemon/internal/daemon/engine"
	daemonenv "github.com/grovetools/daemon/internal/daemon/env"
	daemonhooks "github.com/grovetools/daemon/internal/daemon/hooks"
	"github.com/grovetools/daemon/internal/daemon/jobrunner"
	"github.com/grovetools/daemon/internal/daemon/logstreamer"
	"github.com/grovetools/daemon/internal/daemon/pidfile"
	"github.com/grovetools/daemon/internal/daemon/satellite"
	"github.com/grovetools/daemon/internal/daemon/server"
	daemonssh "github.com/grovetools/daemon/internal/daemon/ssh"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
	"github.com/grovetools/daemon/internal/daemon/theming"
	"github.com/grovetools/daemon/internal/daemon/watcher"
	"github.com/grovetools/flow/pkg/orchestration"
	"github.com/grovetools/grove-gemini/pkg/gemini"
	"github.com/grovetools/memory/pkg/memory"
	notifyconfig "github.com/grovetools/notify/pkg/config"
	tuimux "github.com/grovetools/tuimux/api/client"
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

// envBasePathsFromConfig returns the absolute filesystem roots the env
// manager should walk when restoring state on boot. Pulled from the
// configured grove sources (cfg.Groves), with `~` / env-var expansion
// applied so the daemon's WalkDir doesn't try to descend a literal "~"
// directory, plus the XDG worktree base so Manager.Restore finds the
// state.json of XDG-located worktrees. Disabled groves are skipped;
// duplicates are de-duplicated.
func envBasePathsFromConfig(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]bool)
	roots := make([]string, 0, len(cfg.Groves)+1)
	for _, src := range cfg.Groves {
		if src.Enabled != nil && !*src.Enabled {
			continue
		}
		if src.Path == "" {
			continue
		}
		abs, err := pathutil.Expand(src.Path)
		if err != nil {
			abs = src.Path
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		roots = append(roots, abs)
	}
	if wtd := paths.WorktreesDir(); wtd != "" && !seen[wtd] {
		seen[wtd] = true
		roots = append(roots, wtd)
	}
	return roots
}

// resolveTuimuxPath finds the tuimux binary, checking BinDir first then
// falling back to PATH. groved spawns this real binary (which has a "daemon"
// subcommand) to stand up the out-of-process PTY backplane; it must not
// re-exec itself, as groved has no "daemon" subcommand.
func resolveTuimuxPath() (string, error) {
	binDirPath := filepath.Join(paths.BinDir(), "tuimux")
	if _, err := os.Stat(binDirPath); err == nil {
		return binDirPath, nil
	}
	p, err := exec.LookPath("tuimux")
	if err != nil {
		return "", fmt.Errorf("tuimux not found in %s or PATH", paths.BinDir())
	}
	return p, nil
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
	cmd.AddCommand(newGrovedUpgradeCmd())
	cmd.AddCommand(newGrovedStatusCmd())
	cmd.AddCommand(newGrovedConfigCmd())
	cmd.AddCommand(newGrovedMonitorCmd())
	cmd.AddCommand(newGrovedHealthCmd())
	cmd.AddCommand(newGrovedSyncDBCmd())

	return cmd
}

func newGrovedSyncDBCmd() *cobra.Command {
	var path string
	var yes bool
	cmd := &cobra.Command{
		Use:   "sync-db-archive-rebuild",
		Short: "Archive a legacy name-keyed sync.db and create a fresh id-keyed database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return fmt.Errorf("refusing sync.db transition without --yes")
			}
			running, pid, err := pidfile.IsRunning(paths.PidFilePath())
			if err != nil {
				return fmt.Errorf("check global daemon state: %w", err)
			}
			if running {
				return fmt.Errorf("global daemon is running with PID %d; stop it before rebuilding sync.db", pid)
			}
			if path == "" {
				path = syncdb.DefaultDBPath()
			}
			receipt, err := syncdb.ArchiveAndRebuild(path)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "sync.db path (defaults to the global daemon database)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the destructive archive-and-rebuild transition")
	return cmd
}

func newGrovedHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Single-pane-of-glass daemon health check",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := daemon.NewGlobalClient()
			defer func() { _ = client.Close() }()

			if !client.IsRunning() {
				fmt.Println("Daemon is not running")
				os.Exit(1)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			cfg, cfgErr := client.GetConfig(ctx)
			chStatus, chErr := client.GetChannelStatus(ctx)

			fmt.Println("=== groved health ===")
			fmt.Println()

			if cfgErr == nil {
				fmt.Printf("Daemon uptime:      %s\n", time.Since(cfg.StartedAt).Round(time.Second))
			} else {
				fmt.Printf("Daemon uptime:      (error: %v)\n", cfgErr)
			}

			sigPID, sigAge := signalCLIProcess()
			if sigPID > 0 {
				fmt.Printf("signal-cli PID:     %d (running %s)\n", sigPID, sigAge)
			} else {
				fmt.Println("signal-cli PID:     not running")
			}

			if chErr == nil {
				aliveStr := "dead"
				if !chStatus.SignalEnabled {
					aliveStr = "disabled (signal not configured)"
				} else if chStatus.SignalIsAlive {
					aliveStr = "alive"
				} else if chStatus.SignalStopped {
					aliveStr = "stopped"
				}
				fmt.Printf("Inbound reader:     %s\n", aliveStr)
				fmt.Printf("Restart count:      %d\n", chStatus.SignalRestartCount)
				if chStatus.SignalLastRestart != nil {
					fmt.Printf("Last restart:       %s\n", chStatus.SignalLastRestart.Format(time.RFC3339))
				}
				if chStatus.SignalEnabled && !chStatus.SignalIsAlive && chStatus.SignalLastError != "" {
					fmt.Printf("Last error:         %s\n", trimStatusError(chStatus.SignalLastError))
				}
				fmt.Printf("Route table size:   %d\n", chStatus.ActiveRoutes)
				fmt.Printf("Registered claws:   %d\n", chStatus.RefCount)
				if chStatus.LastInboundTimestamp != nil {
					fmt.Printf("Last inbound:       %s\n", chStatus.LastInboundTimestamp.Format(time.RFC3339))
				} else {
					fmt.Println("Last inbound:       (none)")
				}
			} else {
				fmt.Printf("Channel status:     (error: %v)\n", chErr)
			}

			return nil
		},
	}
}

const defaultDaemonMemoryLimit = int64(2 << 30) // 2 GiB allocation-spike backstop

func applyDefaultDaemonMemoryLimit(getenv func(string) string, setLimit func(int64) int64) bool {
	if strings.TrimSpace(getenv("GOMEMLIMIT")) != "" {
		return false
	}
	setLimit(defaultDaemonMemoryLimit)
	return true
}

func newGrovedStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon",
		Long:  "Start the grove daemon in foreground mode.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Bound heap high-water retention unless the operator supplied an
			// explicit GOMEMLIMIT (including "off"). This mitigates transcript
			// allocation spikes; it is not a substitute for collector throttling.
			memoryLimitDefaulted := applyDefaultDaemonMemoryLimit(os.Getenv, debug.SetMemoryLimit)

			// Route all daemon logs to central system log
			grovelogging.SetGlobalScope(grovelogging.ScopeSystem)

			ulog := grovelogging.NewUnifiedLogger("groved.main")
			if memoryLimitDefaulted {
				ulog.Info("Applied default daemon memory limit").Field("bytes", defaultDaemonMemoryLimit).Log(cmd.Context())
			}

			// Resolve scope (--scope flag > current working directory). An empty
			// scope preserves the legacy global socket/pidfile, so existing dev
			// and test workflows continue unchanged.
			scope, _ := cmd.Flags().GetString("scope")
			// `upgrade` passes the predecessor's already-resolved scope verbatim
			// via --scope-verbatim so GROVE_SCOPE (and thus the socket its child
			// clients reconnect to) is byte-identical across the swap. Re-resolving
			// it here would re-derive a different string (case-normalization, cwd
			// drift) and break that reconnection.
			verbatimScope, _ := cmd.Flags().GetBool("scope-verbatim")
			if scope != "" && !verbatimScope {
				var err error
				if scope, err = resolveExplicitScope(scope, workspace.ResolveScope(scope)); err != nil {
					return err
				}
			}
			// Export GROVE_SCOPE so jobrunner and any PTYs spawned by this
			// daemon inherit the scope naturally via os.Environ().
			_ = os.Setenv("GROVE_SCOPE", scope)

			pidPath, _ := cmd.Flags().GetString("pidfile")
			if pidPath == "" {
				pidPath = paths.PidFilePath(scope)
			}
			sockPath, _ := cmd.Flags().GetString("socket")
			if sockPath == "" {
				sockPath = paths.SocketPath(scope)
			}

			// Declare this process a daemon before ANY boot step can run
			// borrowed code. groved executes flow's orchestration package
			// in-process (the JobRunner runs jobs by calling into it) and that
			// code reaches for daemon.NewWithAutoStart, whose "start one if
			// needed" contract is wrong inside a daemon: a groved that forks a
			// sibling for another worktree creates a daemon with no clients and
			// no work, which idles until --auto-shutdown reaps it and reappears
			// on the next poll. On a fleet restart that turned one restart into
			// one surplus daemon per active worktree. From here on the factory
			// hands in-process callers this daemon's own client instead.
			daemon.MarkInProcessDaemon(sockPath)

			autoShutdown, _ := cmd.Flags().GetBool("auto-shutdown")
			pairPID, _ := cmd.Flags().GetInt("pair-with-pid")
			if pairPID <= 0 {
				// A sandbox that exports GROVE_DAEMON_PAIR_PID pairs every
				// daemon born inside it, including one launched by a bare
				// `groved start` that no factory rewrote the argv for.
				pairPID = daemon.PairPIDFromEnv()
			}
			readyFd, _ := cmd.Flags().GetInt("ready-fd")
			// The inherited readiness fd arrives without CLOEXEC (the parent
			// passed it deliberately). Mark it now, before any boot step can
			// spawn a child: a long-lived child (the scoped tuimux daemon)
			// otherwise inherits a duplicate of the pipe's write end, and the
			// parent's readiness Read never sees EOF even after our own
			// OnReady close — every auto-start client then burns its full
			// handshake timeout while the socket sits bound and idle.
			if readyFd > 0 {
				syscall.CloseOnExec(readyFd)
			}

			// Start pprof if requested
			if port, _ := cmd.Flags().GetInt("pprof-port"); port > 0 {
				go func() {
					bgCtx := context.Background()
					addr := fmt.Sprintf("localhost:%d", port)
					ulog.Info("Starting pprof server").Field("addr", addr).Log(bgCtx)
					if err := http.ListenAndServe(addr, nil); err != nil { //nolint:gosec // G114: pprof debug server, no timeout needed
						ulog.Error("Failed to start pprof server").Err(err).Log(bgCtx)
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
					ulog.Error("Failed to release pidfile").Err(err).Log(context.Background())
				}
			}()

			// Record the exact resolved scope in a sidecar next to the pidfile so
			// `groved upgrade` can hand the successor the identical GROVE_SCOPE.
			// Only scoped daemons need it (the unscoped scope is the empty string,
			// known without a sidecar). We deliberately do NOT remove the sidecar
			// on shutdown: during a graceful upgrade the successor writes it before
			// this process tears down, and removing it on the drain path would race
			// the successor's write. Stale sidecars are harmless and `status
			// --prune` clears them.
			if scope != "" {
				if err := os.WriteFile(scopeSidecarPath(pidPath), []byte(scope), 0o644); err != nil { //nolint:gosec // G306: sibling of the world-readable pidfile
					ulog.Warn("Failed to write scope sidecar").Err(err).Log(context.Background())
				}
			}

			// 2. Load config for daemon settings. A malformed canonical config is
			// not a defaults case: defaults would let topology-dependent work run
			// against an invented topology. Bind a status-only daemon instead and
			// require an explicit restart after repair.
			cfg, err := config.LoadDefault()
			if err != nil {
				ulog.Error("Failed to load config; serving status only until restart").Err(err).Log(context.Background())
				httpPort, _ := cmd.Flags().GetInt("http-port")
				return serveConfigDegraded(cmd.Context(), autoShutdown, scope, sockPath, httpPort, readyFd, pairPID, err, ulog)
			}

			// Parse intervals from config with defaults
			// Defaults: git=10s, session=2s, workspace=5m, plan=5m, note=5m
			// Long intervals are safe because event-driven watchers handle real-time updates.
			gitInterval := 10 * time.Second
			sessionInterval := 2 * time.Second
			workspaceInterval := 5 * time.Minute
			planInterval := 5 * time.Minute
			noteInterval := 5 * time.Minute
			// The rate floor under the flow watcher's aggregated plan-stats
			// recount. Unlike the intervals above this is not a poll period —
			// the recount is event-driven — it is the minimum spacing between
			// two of them, so a plans directory being written into
			// continuously cannot keep the portfolio-wide pass running.
			planStatsMinInterval := 30 * time.Second

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
			// The floor's override lives in the environment rather than in
			// core's DaemonConfig because a field there invalidates grove's
			// generated config schema (see core/CLAUDE.md), which is a third
			// repo. An explicit 0 removes the floor, restoring "every
			// lifecycle event recounts the portfolio".
			if raw := os.Getenv("GROVE_PLANSTATS_MIN_INTERVAL_MS"); raw != "" {
				if ms, err := strconv.Atoi(raw); err == nil && ms >= 0 {
					planStatsMinInterval = time.Duration(ms) * time.Millisecond
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
			eng := engine.New(st)

			// Register collectors with configured intervals based on flags.
			// WorkspaceCollector is intentionally unscoped — it populates the
			// global workspace list so nav can show everything even on a
			// worktree-scoped daemon.
			if isEnabled("workspace") {
				eng.Register(collector.NewWorkspaceCollector(workspaceInterval))
				// Tier-0 machine sync is a workspace enrichment. It is local and
				// bounded (registry files + git metadata only), and runs in scoped
				// daemons too so their workspace API does not silently lose it.
				eng.Register(collector.NewMachineSyncCollector(0))
			}
			if isEnabled("git") {
				// The global collector owns boot + hourly reconciliation. Scoped
				// collectors are passive RefreshPaths helpers and mirror all ambient
				// git state from the already-running global daemon.
				eng.Register(collector.NewGitStatusCollector(gitInterval, scope))
				if scope != "" {
					eng.Register(collector.NewGlobalGitMirrorCollector())
				}
			}
			var sessionColl *collector.SessionCollector
			if isEnabled("session") {
				sessionColl = collector.NewSessionCollector(sessionInterval, scope)
				eng.Register(sessionColl)
			}
			if isEnabled("plan") {
				eng.Register(collector.NewPlanCollector(planInterval))
				eng.Register(collector.NewJobCollector(planInterval, scope, cfg))
			}
			if isEnabled("note") {
				eng.Register(collector.NewNoteCollector(noteInterval))
			}
			if isEnabled("workflow") {
				eng.Register(collector.NewWorkflowCollector(0))
			}

			// 3.5 Setup context early (needed by JobRunner and Engine)
			ctx, cancel := context.WithCancel(context.Background())

			// Satellite federation collector (P8, C10). GLOBAL-ONLY (scope=="")
			// following the sync-handler precedent, NOT the isEnabled collector
			// path: scoped daemons gain satellite awareness only by talking to the
			// global daemon. Registered HERE — before eng.Start below — because
			// Engine.Start iterates the collector slice exactly once (F19), so a
			// Register after Start is silently dead. That F19 constraint is also
			// why ConnManager + collector are built even when the boot registry is
			// EMPTY: the registry hot-reloads via POST /api/satellites/reload
			// (`grove satellite up`/`down`), and a collector that only existed
			// once satellites did could never pick up the first `up` without a
			// daemon restart. Both are cheap and inert with zero entries — no
			// goroutines beyond the collector's reconcile tick, no sockets. The
			// ConnManager is built now (the Store already exists) so the
			// collector has its dialer; it is Started later in the watcher stage
			// where cm.Start's goroutines belong. satCM stays nil only on scoped
			// daemons and when the registry fails to LOAD (config error —
			// satellites disabled until fixed and the daemon restarted).
			// Machine identity: mint-or-read the durable, non-portable ULID in
			// $XDG_STATE_HOME/grove/machine.json. Whichever runs first — the
			// daemon or a `grove machine init` — mints; everyone after reads.
			// The global daemon does it unconditionally (identity is not a
			// sync feature) so the ID exists before any sync client, status
			// handler, or CLI asks. A failure here is non-fatal: sync clients
			// fall back to an empty DeviceID, exactly the pre-identity wire.
			if scope == "" {
				if id, ierr := machine.EnsureIdentity(); ierr != nil {
					ulog.Warn("Failed to resolve machine identity").Err(ierr).Log(ctx)
				} else {
					ulog.Info("Machine identity").
						Field("machine_id", id.ID).
						Field("machine_name", config.ResolveMachineName()).
						StructuredOnly().Log(ctx)
				}
			}

			var satCM *satellite.ConnManager
			if scope == "" {
				if reg, rerr := satellite.LoadRegistry(cfg); rerr != nil {
					ulog.Warn("Failed to load satellite registry, satellites disabled").Err(rerr).Log(ctx)
				} else {
					satCM = satellite.NewConnManager(reg, st)
					eng.Register(collector.NewSatelliteCollector(satCM, reg))
					ulog.Info("Satellite federation collector registered").
						Field("satellites", len(reg.Names())).Log(ctx)
				}
			}

			// 3.55 Boot ordering. --ready-at=bind (treemux's cold-start splash)
			// binds the socket first and runs the slow boot steps in a
			// background goroutine, streaming phase progress; the default
			// --ready-at=boot preserves the historical fully synchronous
			// bind-last ordering byte-for-byte (runBoot runs before serving,
			// and advanceBoot is a no-op because no client can connect yet).
			readyAt, _ := cmd.Flags().GetString("ready-at")
			earlyBind := readyAt == "bind"

			// Fast, side-effect-light setup wired BEFORE the socket can accept,
			// so these handlers never observe a nil dependency even under early
			// bind. The genuinely slow / late-wired dependencies (tuimux,
			// jobrunner, envmanager, channels) are set from runBoot instead.
			srv := server.New(autoShutdown)
			srv.SetScope(scope)
			srv.SetEngine(eng)
			srv.SetRunningConfig(&server.RunningConfig{
				GitInterval:       gitInterval,
				SessionInterval:   sessionInterval,
				WorkspaceInterval: workspaceInterval,
				PlanInterval:      planInterval,
				NoteInterval:      noteInterval,
				StartedAt:         time.Now(),
			})

			// Satellite dispatch (P9): give the server the SSH transport so a
			// `Satellite`-tagged /api/jobs submit forwards to that satellite
			// (M2 C1/C10). satCM is nil on scoped/satellite-less daemons, in
			// which case such submits get a 503. When present, also start the
			// advisory-lease releaser that clears .grove-lease.yml on a
			// forwarded job's terminal federated event (C14).
			srv.SetSatelliteConnManager(satCM)
			if satCM != nil {
				// Registry hot-reload behind POST /api/satellites/reload: re-read
				// config ∪ state from disk and diff-apply onto the ConnManager.
				// The closure owns the load so the server package stays ignorant
				// of the config loader. The config is re-loaded from disk (not
				// the captured boot cfg) so hand-edited [satellites.<name>]
				// tables hot-reload too, alongside the CLI-owned satellites.json
				// that `grove satellite up`/`down` actually write. A load error
				// leaves the live connections untouched (the handler 500s).
				srv.SetSatelliteReloader(func() (*satellite.ReloadSummary, error) {
					freshCfg, cerr := config.LoadDefault()
					if cerr != nil {
						return nil, cerr
					}
					reg, rerr := satellite.LoadRegistry(freshCfg)
					if rerr != nil {
						return nil, rerr
					}
					return satCM.Reload(reg), nil
				})
				srv.StartSatelliteLeaseReleaser(ctx)
				// ntfy-primary notification bridge (P10, M2 contract C18): fire
				// on remote-job terminal events. ntfy is the reliable
				// cross-machine transport; the notifier's SendSystem adjunct
				// no-ops on a headless host. URL/topic come from the existing
				// notify config loader ([notifications].ntfy); an empty topic (or
				// ntfy disabled) leaves the primary send off, system-notify only.
				ntfyURL, ntfyTopic := "", ""
				if ntfyCfg := notifyconfig.Load(); ntfyCfg != nil && ntfyCfg.Ntfy.Enabled {
					ntfyURL, ntfyTopic = ntfyCfg.Ntfy.URL, ntfyCfg.Ntfy.Topic
				}
				srv.StartSatelliteNotifier(ctx, ntfyURL, ntfyTopic)
			}

			// 3.65 Machine-wide build queue scheduler. Sized from [daemon.build]
			// max_parallel; defaults to max(2, NumCPU/2). Fast to construct, so
			// wired before bind.
			buildParallel := 0
			if cfg.Daemon != nil && cfg.Daemon.Build != nil {
				buildParallel = cfg.Daemon.Build.MaxParallel
			}
			buildScheduler := buildqueue.New(st, buildParallel)
			buildScheduler.Start(ctx)
			srv.SetBuildScheduler(buildScheduler)
			ulog.Info("Build queue started").
				Field("max_parallel", buildScheduler.MaxParallel()).
				Log(ctx)

			// 3.7 Log + workspace streamers (fast; the jr↔streamer link is wired
			// inside runBoot once the JobRunner exists).
			streamer := logstreamer.New(st, 1000, 10, 500*time.Millisecond)
			srv.SetLogStreamer(streamer)
			workspaceStreamer := logstreamer.NewWorkspaceStreamer(st, 10000)
			go workspaceStreamer.Start(ctx)
			srv.SetWorkspaceStreamer(workspaceStreamer)

			// Fires the instant the unix socket is bound — the earliest moment a
			// client can dial us. Two things hang off it:
			//
			//  1. If the parent passed --ready-fd, close that inherited fd. The
			//     parent reads EOF and stops polling — a deterministic
			//     handshake. No-op when readyFd is 0 (ad-hoc / manual startups).
			//  2. The GLOBAL daemon registers itself as the routing endpoint of
			//     last resort (daemon.RegisterDaemonHost). Without it, "no live
			//     registered UI host" and "no host at all" are the same answer,
			//     which is exactly wrong during a fleet restart: treemux is
			//     still coming back, the hosts registry is empty, and every
			//     scope-resolving client in a worktree concludes "no host" and
			//     spawns a scoped groved nothing will use. A live UI host always
			//     outranks this record, so treemux's routing is unchanged while
			//     it is up. Registered at BIND, not after boot, because the
			//     window this closes is measured in seconds. Scoped daemons
			//     never register: their socket would become the most specific
			//     host for their own subtree and could steal session traffic
			//     from a global treemux streaming a different daemon.
			var unregisterDaemonHost func()
			srv.OnReady = func() {
				if readyFd > 0 {
					if err := syscall.Close(readyFd); err != nil {
						ulog.Warn("failed to close ready-fd").
							Field("fd", readyFd).Err(err).Log(ctx)
					}
				}
				if scope != "" {
					return
				}
				unregister, herr := daemon.RegisterDaemonHost(scope, "groved")
				if herr != nil {
					ulog.Warn("Failed to register daemon host; scoped clients may spawn their own daemons").
						Err(herr).Log(ctx)
					return
				}
				unregisterDaemonHost = unregister
			}

			// Boot phases published under early bind. Each boundary updates the
			// BootStatus behind GET /api/system/boot and broadcasts it over the
			// store bus (the boot_phase SSE stream). No-op under the default
			// ordering, where runBoot finishes before the socket serves.
			bootPhases := []string{"tuimux", "jobrunner", "environment", "channels", "engine", "watchers", "ssh"}
			advanceBoot := func(idx int) {
				if !earlyBind {
					return
				}
				status := &daemon.BootStatus{
					Phase:      bootPhases[idx],
					PhaseIndex: idx + 1,
					PhaseTotal: len(bootPhases),
				}
				srv.SetBootStatus(status)
				st.BroadcastBootPhase(status)
			}

			// runBoot performs the remaining boot steps in today's exact order.
			// Under early bind it runs in a background goroutine AFTER the socket
			// is already accepting; otherwise it runs synchronously before
			// ListenAndServe, preserving the historical bind-last timing.
			runBoot := func() {
				// 3.6 Setup JobRunner
				jobsEnabled := true
				if cfg.Daemon != nil && cfg.Daemon.Jobs != nil && cfg.Daemon.Jobs.Enabled != nil {
					jobsEnabled = *cfg.Daemon.Jobs.Enabled
				}

				advanceBoot(0) // tuimux

				// Stand up the standalone tuimux daemon BEFORE the JobRunner so
				// adoption can query it. PTY ownership now lives out-of-process in
				// this daemon, so it survives a `groved upgrade` (the successor
				// re-discovers the same live socket). On failure we log and
				// continue with a nil client — adoption must fail-open, never block.
				var tuimuxClient *tuimux.ApiClient
				// Each daemon stands up / connects to a tuimux multiplexer keyed to
				// its own scope, so a scoped daemon's PTY inspector sees ONLY its own
				// PTYs. Empty scope resolves to the legacy machine-wide socket
				// (backward compat — currently-running shells aren't orphaned).
				// groved already exported GROVE_SCOPE above, so the spawned tuimux's
				// own DefaultSocketPath() default agrees; passing the resolved scope
				// explicitly is belt-and-suspenders.
				tuimuxSock := tuimux.ScopedSocketPath(scope)
				tuimuxBin, binErr := resolveTuimuxPath()
				if binErr != nil {
					ulog.Warn("tuimux binary not found; agent PTYs will be unavailable").
						Err(binErr).Log(ctx)
					tuimuxClient = nil
				} else {
					var tuimuxErr error
					tuimuxClient, tuimuxErr = tuimux.EnsureDaemonWithBinary(tuimuxSock, tuimuxBin)
					if tuimuxErr != nil {
						ulog.Warn("Failed to ensure tuimux daemon; agent PTYs will be unavailable").
							Err(tuimuxErr).Log(ctx)
						tuimuxClient = nil
					}
					// Give the server a hook to re-ensure this scoped tuimux if its
					// client goes stale (daemon died post-boot), so agent-pane spawns
					// self-heal instead of hard-failing for the life of this groved.
					// srv already exists (constructed above before this boot goroutine).
					srv.SetTuimuxReEnsure(func() (*tuimux.ApiClient, error) {
						return tuimux.EnsureDaemonWithBinary(tuimuxSock, tuimuxBin)
					})
				}
				// Wire tuimux into the session collector so it can kill out-of-process
				// PTYs when it detects a dead session PID (daemon-side reaper).
				if sessionColl != nil && tuimuxClient != nil {
					sessionColl.SetPtyKiller(tuimuxClient)
				}

				advanceBoot(1) // jobrunner

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

					jr = jobrunner.New(st, localRuntime, workers, persister, tuimuxClient)

					// Synchronously recover persisted sessions into the store BEFORE
					// adoption. The SessionCollector only populates the session map
					// once the engine starts (~200 lines below), so without this
					// adoption would see an empty map and could never read a job's
					// PtyID to verify its out-of-process PTY survived the upgrade.
					// Recovery failure must warn-and-continue, never block adoption.
					//
					// Scope-filter to this daemon's own scope (same as the collector
					// seed below): seeding the unfiltered global set here would let
					// adoption reap another scope's agents as orphans (their PtyIDs
					// live in a different scoped tuimux), reopening the cross-scope
					// leak this feature closes.
					if recovered, rerr := sessions.RecoverSessionsForScope(scope); rerr != nil {
						ulog.Warn("Synchronous session recovery failed; continuing").Err(rerr).Log(ctx)
					} else if len(recovered) > 0 {
						st.ApplyUpdate(store.Update{
							Type:    store.UpdateSessions,
							Source:  "boot_recovery",
							Payload: recovered,
						})
					}

					// Collapse duplicate job records before adoption reads them.
					// A job submitted through the daemon used to persist under a
					// filename-derived key alongside the Flow-ID-keyed record for
					// the same job, so adoption evaluated one job twice and the
					// typeless copy answered lookups that needed the real one.
					if collapsed, removed := persister.CollapseDuplicates(); collapsed > 0 || removed > 0 {
						ulog.Info("Collapsed duplicate daemon job records").
							Field("merged", collapsed).
							Field("removed", removed).
							Log(ctx)
					}

					// PHASE 2: Adopt running agents from previous daemon instance
					jr.AdoptRunningAgents(ctx)
					go jr.Start(ctx)
					ulog.Info("JobRunner started").Field("workers", workers).Log(ctx)

					// Deliberate, retention-bounded GC of the session registry.
					// Liveness sweeps only drop pid.lock now (metadata.json is the
					// job→transcript index), so this is what eventually reclaims
					// long-dead records. Boot-time and once a day thereafter.
					go func() {
						purge := func() {
							purged, perr := sessions.PurgeStaleSessions(sessions.DefaultSessionRetention)
							if perr != nil {
								ulog.Warn("Session registry GC failed").Err(perr).Log(ctx)
								return
							}
							if purged > 0 {
								ulog.Info("Session registry GC purged stale records").
									Field("purged", purged).
									Field("retention", sessions.DefaultSessionRetention.String()).
									Log(ctx)
							}
						}
						purge()
						ticker := time.NewTicker(24 * time.Hour)
						defer ticker.Stop()
						for {
							select {
							case <-ctx.Done():
								return
							case <-ticker.C:
								purge()
							}
						}
					}()
				}

				// Link the JobRunner to the (already-constructed) log streamer.
				if jr != nil {
					jr.SetOnJobDetached(streamer.NotifyJobDetached)
				}

				advanceBoot(2) // environment

				// 4. Setup env manager
				envManager := daemonenv.NewManager()

				// Restore environment state from disk to prevent port collisions.
				// Phase 4: walk configured ecosystem base paths directly with
				// filepath.WalkDir, which bypasses the racy workspace discovery
				// pass. Newly-created worktrees are guaranteed to be present in
				// the filesystem before Restore runs (the daemon was just
				// started after them) but are NOT guaranteed to be present in
				// workspace.Provider yet (fsnotify + git worktree creation
				// takes a beat to settle).
				basePaths := envBasePathsFromConfig(cfg)
				envManager.Restore(basePaths)

				// Only the global (unscoped) daemon binds :8443 — it owns the
				// shared *.grove.local proxy table. Scoped daemons inject a
				// client handle so they RPC their routes over to the global
				// daemon rather than maintaining their own (conflicting) bind.
				if scope == "" {
					go func() {
						if err := envManager.Proxy.ListenAndServe("127.0.0.1:8443"); err != nil {
							ulog.Warn("Proxy server stopped").Err(err).Log(context.Background())
						}
					}()
				} else {
					envManager.SetGlobalClient(daemon.NewGlobalClient())
				}

				srv.SetEnvManager(envManager)
				if jr != nil {
					srv.SetJobRunner(jr)
				}

				// Dashboard: ephemeral TCP listener, global daemon only. The
				// port is persisted so `grove env dashboard` can find us without
				// any discovery protocol. Scoped daemons never serve it.
				if scope == "" {
					if dashAddr, err := srv.StartDashboard(ctx); err != nil {
						ulog.Warn("dashboard server failed to start").Err(err).Log(ctx)
					} else {
						ulog.Info("Dashboard listening").
							Field("url", "http://"+dashAddr+"/dashboard").
							Log(ctx)
					}
				}

				// Wire the out-of-process tuimux client onto the server so PTY
				// create / input / interrupt route to the standalone tuimux daemon
				// instead of an embedded in-process manager. PTYs now survive a
				// `groved upgrade` because the tuimux daemon outlives groved.
				srv.SetTuimuxClient(tuimuxClient)

				// sendInputToSession delegates to Server.SendSessionInput so the
				// channels manager and autonomous pinger both benefit from the
				// mux-aware dispatch (direct treemux PTY write → SSE relay → tmux
				// fallback).
				sendInputToSession := func(ctx context.Context, jobID, message string) error {
					return srv.SendSessionInput(ctx, jobID, message)
				}

				advanceBoot(3) // channels

				// Initialize channel manager if signal is configured.
				// Declared at outer scope so the shutdown goroutine can call Stop().
				var chMgr *daemonchannels.Manager
				notifyCfg := notifyconfig.Load()
				if notifyCfg.Signal.Enabled || notifyCfg.HomeAssistant.Enabled {
					chMgr = daemonchannels.NewManager(st, daemonchannels.SignalConfig{
						Enabled:     notifyCfg.Signal.Enabled,
						CLIPath:     notifyCfg.Signal.CLIPath,
						Account:     notifyCfg.Signal.Account,
						Allowlist:   notifyCfg.Signal.Allowlist,
						Groups:      notifyCfg.Signal.Groups,
						Contacts:    notifyCfg.Signal.ContactsFlat(),
						NamedGroups: notifyCfg.Signal.NamedGroupsFlat(),
					}, daemonchannels.HAConfig{
						Enabled:          notifyCfg.HomeAssistant.Enabled,
						WebhookPort:      notifyCfg.HomeAssistant.WebhookPort,
						WebhookBind:      notifyCfg.HomeAssistant.WebhookBind,
						WebhookSecret:    notifyCfg.HomeAssistant.WebhookSecret,
						WebhookSecretErr: notifyCfg.HomeAssistant.WebhookSecretErr,
						URL:              notifyCfg.HomeAssistant.URL,
						Token:            notifyCfg.HomeAssistant.Token,
						DefaultSatellite: notifyCfg.HomeAssistant.DefaultSatellite,
					}, scope, sockPath)
					chMgr.SendInput = sendInputToSession
					// Scoped daemons proxy outbound sends to the global daemon
					// (which owns signal-cli) and register their sessions in
					// routing.json so global can forward inbound replies back.
					if scope != "" {
						chMgr.SetGlobalClient(daemon.NewGlobalClient())
					}
					chMgr.Start(ctx)
					srv.SetChannelManager(chMgr)
					ulog.Info("Channel manager initialized").
						Field("signal", notifyCfg.Signal.Enabled).
						Field("ha", notifyCfg.HomeAssistant.Enabled).
						Field("scope", scope).
						Field("proxy_mode", scope != "").
						Log(ctx)
				}

				// Register autonomous pinger as a collector
				pinger := autonomous.NewPinger(st, "")
				pinger.SendInput = sendInputToSession
				eng.Register(pinger)

				// 5. Handle Signals + auto-shutdown
				stop := make(chan os.Signal, 1)
				signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
				drain := make(chan os.Signal, 1)
				// PHASE 2: Listen for SIGUSR1 to trigger drain mode (zero-downtime upgrade)
				signal.Notify(drain, syscall.SIGUSR1)

				// 5.1 If paired to a parent PID, watch for its death and trigger
				// the same graceful shutdown pathway as a SIGTERM. This pipes
				// kernel-level parent-death events into pidfile cleanup, PTY
				// teardown, and server stop without bypassing any of them.
				if pairPID > 0 {
					pairwatch.Watch(cmd.Context(), pairPID, func() {
						stop <- syscall.SIGTERM
					})
				}

				// shutdownReq fires when the TerminalHub idle timer expires
				// (auto-shutdown mode). Nil if auto-shutdown is disabled.
				shutdownReq := srv.TerminalHubShutdownReq()

				// sshServer is set later (step 7.7) and closed by the signal handler.
				var sshServer *daemonssh.Server

				go func() {
					bgCtx := context.Background()
					select {
					case <-stop:
						ulog.Info("Received stop signal").Field("event", "daemon.stopped").Log(bgCtx)
					case <-shutdownReq:
						ulog.Info("Auto-shutdown fired (idle TerminalHub)").Field("event", "daemon.stopped").Log(bgCtx)
					}
					// Reap the agent PTYs this daemon owns. With PTYs out-of-process,
					// a plain stop (this SIGTERM/auto-shutdown path — NOT the SIGUSR1
					// drain path, which leaves PTYs untouched for upgrade survival)
					// must explicitly kill them via the tuimux daemon, leaving the
					// tuimux daemon process itself running (other tools may share it).
					//
					// Scope safety: st.GetSessions() only ever contains THIS scope's
					// sessions — the SessionCollector seeds the store via
					// RecoverSessionsForScope(scope) and never adopts foreign-scope
					// records — so this loop can only kill agents owned by this daemon's
					// scope. That is what closes the cross-scope reaping leak (Gap A).
					if tuimuxClient != nil {
						for _, sess := range st.GetSessions() {
							if sess.PtyID == "" {
								continue
							}
							if err := tuimuxClient.KillPty(sess.PtyID); err != nil {
								ulog.Warn("Failed to kill agent PTY on stop").
									Err(err).Field("pty_id", sess.PtyID).Log(bgCtx)
							}
						}
					}
					// A scoped tuimux is owned by this scoped daemon and has no other
					// owner once it exits — reap it so it doesn't orphan. The global
					// (scope == "") tuimux is shared and intended to persist, so we
					// NEVER stop it. This runs only on the plain-stop path (above);
					// the SIGUSR1 drain/upgrade path leaves the scoped tuimux running
					// so the successor re-binds the same socket and live PTYs survive.
					if scope != "" {
						if err := tuimux.StopDaemon(tuimuxSock); err != nil {
							ulog.Warn("Failed to stop scoped tuimux daemon on shutdown").
								Err(err).Field("socket", tuimuxSock).Log(bgCtx)
						}
					}
					envManager.Shutdown()    // Teardown all running environments and proxy routes
					streamer.Stop()          // Stop all job log tailing goroutines
					workspaceStreamer.Stop() // Stop workspace log aggregation
					if chMgr != nil {
						// Stop signal-cli daemon subprocess so it doesn't orphan.
						// A fresh signal-cli is spawned on the next groved boot,
						// restoring the stdout reader + cross-daemon inbound routing.
						chMgr.Stop(bgCtx)
					}
					if sshServer != nil {
						_ = sshServer.Stop()
					}
					cancel() // Stop the engine

					// Create shutdown context with timeout
					shutdownCtx, shutdownCancel := context.WithTimeout(bgCtx, 5*time.Second)
					defer shutdownCancel()

					if err := srv.Shutdown(shutdownCtx); err != nil {
						ulog.Error("Server shutdown error").Err(err).Log(bgCtx)
					}

					// Explicitly release pidfile and drop the daemon-host record
					// before exit in signal handler (os.Exit skips defers). A
					// leftover record is harmless — lookups skip dead pids and
					// the next registration prunes it — but leaving one behind
					// means every lookup pays a dial to a dead socket first.
					if unregisterDaemonHost != nil {
						unregisterDaemonHost()
					}
					_ = pidfile.Release(pidPath)
					os.Exit(0)
				}()

				// PHASE 2: Handle SIGUSR1 for graceful drain (zero-downtime upgrade)
				go func() {
					bgCtx := context.Background()
					<-drain
					ulog.Info("Received SIGUSR1 - entering drain mode").Log(bgCtx)
					srv.EnterDrainMode(bgCtx)
				}()

				// 5.5. Start inline monitor early so it captures all events from boot
				if monitor, _ := cmd.Flags().GetBool("monitor"); monitor {
					monitorFormat, _ := cmd.Flags().GetString("monitor-format")
					monitorCompact, _ := cmd.Flags().GetBool("monitor-compact")
					go runInlineMonitor(ctx, st, monitorFormat, monitorCompact)
				}

				advanceBoot(4) // engine

				// 6. Start Engine in background
				go eng.Start(ctx)

				// Rate ticker for the telemetry registry's *_per_min counters.
				// Deliberately NOT derived at request time: rates computed
				// against a caller's poll interval would differ between the
				// TUI (2s) and an agent curling once an hour, for the same
				// daemon, in the same second.
				go telemetry.Default().Run(ctx.Done())

				// 7. Start ConfigWatcher if enabled
				if configWatchEnabled(cfg) {
					debounceMs := configDebounceMs(cfg)
					// Track the resolved global tui.theme across reloads so a
					// dedicated theme_changed event (with the resolved palette)
					// fires only when the theme actually changes. The callback
					// runs on the watcher's single goroutine, so lastTheme
					// needs no locking.
					lastTheme := theming.CurrentThemeName()
					configWatcher, err := daemon.NewConfigWatcher(debounceMs, func(file string) {
						// Broadcast config reload event to all subscribers
						st.BroadcastConfigReload(file)

						// Diff the resolved theme and broadcast on change.
						themeName := theming.CurrentThemeName()
						if themeName == lastTheme {
							return
						}
						lastTheme = themeName
						if payload, ok := theming.BuildPayload(themeName); ok {
							st.BroadcastThemeChanged(themeName, payload)
							ulog.Info("Theme changed, broadcasting").
								Field("theme", themeName).Log(ctx)
						} else {
							ulog.Warn("Configured theme not found in registry, skipping theme broadcast").
								Field("theme", themeName).Log(ctx)
						}
					})
					if err != nil {
						ulog.Warn("Failed to start config watcher, continuing without it").Err(err).Log(ctx)
					} else {
						ulog.Info("Config watcher started").Log(ctx)
						go configWatcher.Start(ctx)
					}
				}

				// 7.2. The [[daemon.hooks.on_event]] dispatcher — the exec-side
				// subscription to the same bus /api/stream serves. Started
				// only when hooks are configured: an idle subscriber still
				// costs a channel and a non-blocking send per store update.
				// The reload closure re-reads config from disk rather than
				// closing over the boot cfg, so hand-edited hooks hot-reload
				// on config_reload (the same pattern SetSatelliteReloader
				// uses). Values from untrusted workspace layers were already
				// stripped by the exec-provenance gate inside LoadDefault.
				eventDispatcher := daemonhooks.NewDispatcher(st, daemonhooks.NewExecutor(cfg), cfg, config.LoadDefault)
				if eventDispatcher.HasHooks() {
					eventDispatcher.Start(ctx)
				} else {
					ulog.Debug("No [[daemon.hooks.on_event]] hooks configured; dispatcher idle").Log(ctx)
				}

				advanceBoot(5) // watchers

				// 7.5. Start UnifiedWatcher with registered domain handlers
				unifiedWatcher, err := watcher.NewUnifiedWatcher(st, 100*time.Millisecond)
				if err != nil {
					ulog.Warn("Failed to start unified watcher, continuing without it").Err(err).Log(ctx)
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
							ulog.Warn("Failed to initialize skill handler").Err(err).Log(ctx)
						} else {
							unifiedWatcher.Register(skillHandler)
							ulog.Info("Skill handler registered with unified watcher").Log(ctx)
						}
					}

					// Register SettingsHandler to reconcile .claude settings if enabled
					autoSyncClaudeSettings := true
					if cfg.Daemon != nil && cfg.Daemon.AutoSyncClaudeSettings != nil {
						autoSyncClaudeSettings = *cfg.Daemon.AutoSyncClaudeSettings
					}

					if autoSyncClaudeSettings {
						debounceMs := 1000
						if cfg.Daemon != nil && cfg.Daemon.SkillSyncDebounceMs > 0 {
							debounceMs = cfg.Daemon.SkillSyncDebounceMs
						}

						settingsHandler, err := watcher.NewSettingsHandler(st, cfg, debounceMs)
						if err != nil {
							ulog.Warn("Failed to initialize Claude settings handler").Err(err).Log(ctx)
						} else {
							unifiedWatcher.Register(settingsHandler)
							ulog.Info("Claude settings handler registered with unified watcher").Log(ctx)
						}
					}

					// Register WorkspaceHandler for instant discovery on fs changes
					if isEnabled("workspace") {
						workspaceHandler := watcher.NewWorkspaceHandler(st, cfg, 2000)
						unifiedWatcher.Register(workspaceHandler)
						ulog.Info("Workspace handler registered with unified watcher").Log(ctx)
					}

					// The global daemon is the single event-driven git owner. Its
					// recursive FSEvents stream covers every worktree and git dir;
					// UnifiedWatcher retains git-internal watches as a narrow fallback.
					// Scoped daemons mirror global deltas and register no git watcher.
					if isEnabled("git") && scope == "" {
						gitHandler := watcher.NewGitHandler(st, 150).SetBroadCoverage(true)
						unifiedWatcher.Register(gitHandler)
						go watcher.RunGlobalGitEvents(ctx, st, gitHandler)
						ulog.Info("Global event-driven git handler registered").Log(ctx)
					}

					// Register FlowHandler for plan directory watching
					if isEnabled("plan") {
						flowHandler := watcher.NewFlowHandler(st, cfg, 2000).
							SetPlanStatsMinInterval(planStatsMinInterval)
						unifiedWatcher.Register(flowHandler)
						ulog.Info("Flow handler registered with unified watcher").
							Field("plan_stats_min_interval_ms", planStatsMinInterval.Milliseconds()).
							Log(ctx)
					}

					// Register NoteHandler for note directory watching
					if isEnabled("note") {
						noteHandler := watcher.NewNoteHandler(st, cfg, 3000)
						unifiedWatcher.Register(noteHandler)
						ulog.Info("Note handler registered with unified watcher").Log(ctx)
					}

					// Register NavHandler for nav keymap state (sessions.yml) watching
					navHandler := watcher.NewNavHandler(st)
					unifiedWatcher.Register(navHandler)
					ulog.Info("Nav handler registered with unified watcher").Log(ctx)

					// Register MemoryHandler for auto-indexing content.
					// Only the global daemon owns the SQLite DB and embedder; scoped
					// daemons proxy /api/memory/* to global via server-side forwarding.
					if scope == "" {
						dbPath, err := pathutil.Expand("~/.local/share/grove/memory/memory.db")
						if err == nil {
							memStore, err := memory.Open(dbPath, 3072) // gemini-embedding-001 outputs 3072 dimensions
							if err != nil {
								ulog.Warn("Failed to initialize memory store, indexing disabled").Err(err).Log(ctx)
							} else {
								// The embedder is optional: without a Gemini client the
								// memory store still indexes and serves FTS (keyword)
								// search; only semantic (vector) search is unavailable.
								// Use grove-gemini's config resolver (secrets.toml, env var, api_key_command)
								var embedder memory.Embedder
								geminiClient, err := gemini.NewClient(ctx, "")
								if err != nil {
									ulog.Warn("Failed to initialize Gemini client, memory will run without semantic search").Err(err).Log(ctx)
								} else {
									embedder = memory.NewGeminiEmbedder(geminiClient, gemini.DefaultEmbeddingModel)
								}

								memoryHandler := watcher.NewMemoryHandler(st, cfg, memStore, embedder, 5000)
								unifiedWatcher.Register(memoryHandler)
								ulog.Info("Memory handler registered with unified watcher").
									Field("fts_enabled", true).
									Field("semantic_available", embedder != nil).
									Log(ctx)

								// Share the same store + embedder (possibly nil) with the
								// HTTP server so /api/memory/* handlers can serve TUI
								// clients without opening a second SQLite connection.
								srv.SetMemoryStore(memStore, embedder, dbPath)
							}
						}
					}

					// Register SyncHandler for notebook sync change capture.
					// DARK BY DEFAULT, but no longer BOOT-GATED: the handler is
					// constructed unconditionally on the global daemon and stays
					// dormant while ~/.config/grove/sync.toml carries no
					// workspace subscriptions — no watches, no sync.db, no
					// transport, zero behavior change. sync.db is opened lazily
					// by the handler (SetDeferredDB) the first time a
					// subscription exists.
					//
					// The old gate decided at boot, so a first-ever `grove join`
					// wrote a perfectly valid sync.toml that nothing picked up
					// until the daemon was restarted. The config reload the join
					// already triggers now wakes the handler in place. Like
					// memory.db, sync.db is owned by the global daemon only;
					// scoped daemons proxy /api/sync/* to global.
					if scope == "" {
						syncCfg, err := config.LoadSyncConfig()
						if err != nil {
							ulog.Warn("Failed to load sync config, sync starts dormant").Err(err).Log(ctx)
							syncCfg = nil
						}
						syncHandler := watcher.NewSyncHandler(st, cfg, syncCfg, nil, 0, 0)
						syncHandler.SetDeferredDB(
							func() (*syncdb.DB, error) { return syncdb.Open(syncdb.DefaultDBPath()) },
							srv.SetSyncDB,
						)
						unifiedWatcher.Register(syncHandler)
						srv.SetSyncKick(syncHandler.KickAntiEntropy)
						srv.SetSyncSubscriptions(syncHandler.SyncSubscriptions)
						srv.SetSyncAuthFailure(syncHandler.AuthFailure)
						srv.SetSyncDBError(syncHandler.SyncDBError)
						srv.SetNotespaceAdopted(syncHandler.AdoptedNotespace)
						srv.SetSyncNotespaceRoots(syncHandler.NotespaceRoots)
						srv.SetSyncMaintenance(syncHandler.BeginMaintenance, syncHandler.EndMaintenance)
						workspaces := 0
						if syncCfg != nil {
							workspaces = len(syncCfg.Workspaces)
						}
						ulog.Info("Sync handler registered with unified watcher").
							Field("workspaces", workspaces).
							Field("dormant", workspaces == 0).
							Log(ctx)
					}

					// Start the satellite ConnManager (P7 federation transport) that
					// the P8 SatelliteCollector — registered before eng.Start above
					// (F19 ordering) — dials through. Construction + collector
					// registration happened up top where the collector slice is
					// still mutable; only Start() belongs here, where ctx is live
					// and the ConnManager's per-satellite goroutines join the other
					// watcher-stage goroutines. satCM is nil on a satellite-less
					// machine (empty registry) → skip, exactly as before.
					if satCM != nil {
						satCM.Start(ctx)
						ulog.Info("Satellite ConnManager started").Log(ctx)
					}

					ulog.Info("Unified watcher started").Log(ctx)
					go unifiedWatcher.Start(ctx)
				}

				// 7.5b. Forge poller: the global daemon's read-only view of PR +
				// checks state for the ecosystem's repos. Gated twice over
				// (explicit config opt-in AND a present `gh`) — see
				// startForgePoller. Outside the unified-watcher block on
				// purpose: it is a polling goroutine, not a filesystem handler,
				// and a failed watcher must not silently take it with it.
				if scope == "" {
					// Wire the poller's read seam onto the HTTP server so
					// GET /api/forge/state can serve the cache. Registering it
					// ONLY when the poller actually started is what makes the
					// endpoint able to say "poller off" — a surface that gets an
					// empty repo list with no such signal renders it as "no pull
					// requests" (STATE.md D4).
					if poller := startForgePoller(ctx, st, cfg, ulog); poller != nil {
						srv.SetForgeSnapshotter(poller.ProviderName(), poller)
					}
				}

				// 7.6. Log retention janitor: sweep dated *.log files older than the
				// configured retention out of the state logs dir, on start and then
				// every 24h. Judged by filename date (fallback ModTime); today's
				// file is never touched.
				logCfg := grovelogging.GetDefaultLoggingConfig()
				if uerr := cfg.UnmarshalExtension("logging", &logCfg); uerr != nil {
					ulog.Debug("Failed to parse logging config for retention janitor, using defaults").Err(uerr).Log(ctx)
				}
				retentionDays := logCfg.File.RetentionDays
				if retentionDays <= 0 {
					retentionDays = 14
				}
				go runLogRetentionJanitor(ctx, ulog, retentionDays)

				advanceBoot(6) // ssh

				// 7.7. Start SSH server if enabled
				var sshCfg *config.DaemonSSHConfig
				if cfg.Daemon != nil {
					sshCfg = cfg.Daemon.SSH
				}
				if s, err := daemonssh.New(sshCfg); err != nil {
					ulog.Warn("Failed to start SSH server").Err(err).Log(ctx)
				} else if s != nil {
					s.SetStore(st)
					// PTYs are now owned out-of-process by the tuimux daemon; the
					// SSH server's in-process ptyManager listing is left nil (its
					// daemon-owned-PTY paths are nil-guarded and become no-ops).
					sshServer = s
					go func() {
						if err := sshServer.Start(); err != nil {
							ulog.Warn("SSH server stopped").Err(err).Log(context.Background())
						}
					}()
				}

				// Boot complete: report Done so GET /api/system/boot and the
				// boot_phase stream flip a waiting splash out of its loading
				// state.
				if earlyBind {
					done := &daemon.BootStatus{Done: true, PhaseIndex: len(bootPhases), PhaseTotal: len(bootPhases)}
					srv.SetBootStatus(done)
					st.BroadcastBootPhase(done)
				}
			} // end runBoot

			// 8. Start Server.
			httpPort, _ := cmd.Flags().GetInt("http-port")
			ulog.Info("Starting daemon").Field("event", "daemon.started").Field("pid", os.Getpid()).Log(ctx)

			if earlyBind {
				// Bind first so the spawning client unblocks in milliseconds,
				// seed an in-progress BootStatus, run the slow boot steps in the
				// background, then serve. The socket is dialable the instant
				// Listen returns; requests queue in the kernel accept backlog
				// until Serve begins accepting.
				srv.SetBootStatus(&daemon.BootStatus{PhaseTotal: len(bootPhases)})
				if err := srv.Listen(sockPath, httpPort); err != nil {
					return fmt.Errorf("server error: %w", err)
				}
				go runBoot()
				if err := srv.Serve(); err != nil {
					return fmt.Errorf("server error: %w", err)
				}
				return nil
			}

			// Default (--ready-at=boot): preserve the historical fully
			// synchronous bind-last ordering — every boot step finishes before
			// the socket binds, so no client ever observes a booting daemon.
			runBoot()
			if err := srv.ListenAndServe(sockPath, httpPort); err != nil {
				return fmt.Errorf("server error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringSlice("collectors", []string{"all"}, "Comma-separated list of collectors to enable (git, session, workspace, plan, note, workflow)")
	cmd.Flags().Int("pprof-port", 0, "Port to start pprof server on (0 to disable)")
	cmd.Flags().Int("http-port", 0, "Port to start HTTP server on for browser access (web terminal viewer, 0 to disable)")
	cmd.Flags().Bool("monitor", false, "Stream daemon activity to stdout")
	cmd.Flags().String("monitor-format", "full", "Output format for --monitor: text, json, full, rich, pretty")
	cmd.Flags().Bool("monitor-compact", true, "Disable spacing between monitor log entries")
	cmd.Flags().String("scope", "", "Ecosystem scope path for this daemon (empty = global/unscoped)")
	cmd.Flags().Bool("scope-verbatim", false, "Treat --scope as already resolved; skip ResolveScope (set by `groved upgrade` to preserve the predecessor's exact scope)")
	_ = cmd.Flags().MarkHidden("scope-verbatim")
	cmd.Flags().String("socket", "", "Override socket path (empty = derive from --scope)")
	cmd.Flags().String("pidfile", "", "Override pidfile path (empty = derive from --scope)")
	cmd.Flags().Bool("auto-shutdown", false, "Exit after 2m with no terminal WebSocket clients connected")
	cmd.Flags().Int("pair-with-pid", 0, "Shut down when this parent PID exits (0 disables pairing)")
	cmd.Flags().Int("ready-fd", 0, "Close this inherited file descriptor once the socket is bound (0 disables readiness signaling)")
	cmd.Flags().String("ready-at", "boot", "When to consider the daemon ready: 'boot' (default) binds the socket last, after every boot step (historical ordering); 'bind' binds early and streams boot progress while the slow steps run in the background")

	return cmd
}

// serveConfigDegraded binds the normal daemon socket but intentionally builds
// none of the topology-dependent graph (Store/Engine collectors, job/build
// queues, environment manager, watchers, sync/memory databases, or SSH).
// Recovery is restart-only so normal boot remains the single construction
// path for that graph.
func serveConfigDegraded(
	ctx context.Context,
	autoShutdown bool,
	scope, socketPath string,
	httpPort, readyFd, pairPID int,
	configErr error,
	ulog *grovelogging.UnifiedLogger,
) error {
	srv := server.New(autoShutdown)
	srv.SetScope(scope)
	srv.SetRunningConfig(&server.RunningConfig{StartedAt: time.Now()})
	srv.SetConfigDegradation(configErr.Error())
	srv.SetBootStatus(&daemon.BootStatus{Done: true, Err: configErr.Error()})

	var unregisterDaemonHost func()
	srv.OnReady = func() {
		if readyFd > 0 {
			if err := syscall.Close(readyFd); err != nil {
				ulog.Warn("failed to close ready-fd").Field("fd", readyFd).Err(err).Log(ctx)
			}
		}
		if scope == "" {
			unregister, err := daemon.RegisterDaemonHost(scope, "groved")
			if err != nil {
				ulog.Warn("Failed to register degraded daemon host").Err(err).Log(ctx)
			} else {
				unregisterDaemonHost = unregister
			}
		}
	}

	if err := srv.Listen(socketPath, httpPort); err != nil {
		return fmt.Errorf("degraded server error: %w", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	if pairPID > 0 {
		pairwatch.Watch(ctx, pairPID, func() {
			select {
			case stop <- syscall.SIGTERM:
			default:
			}
		})
	}

	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
		case <-srv.TerminalHubShutdownReq():
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if unregisterDaemonHost != nil {
			unregisterDaemonHost()
		}
		if err := srv.Shutdown(shutdownCtx); err != nil {
			ulog.Error("Degraded server shutdown error").Err(err).Log(shutdownCtx)
		}
	}()

	ulog.Warn("Daemon listening in config-degraded status-only mode").
		Field("socket", socketPath).
		Field("recovery", "restart-only").
		Err(configErr).
		Log(ctx)
	if err := srv.Serve(); err != nil {
		return fmt.Errorf("degraded server error: %w", err)
	}
	return nil
}

const (
	maxActiveLogBytes = int64(256 << 20) // 256 MiB per active dated log
	logSizeCheckEvery = 5 * time.Minute
)

// runLogRetentionJanitor owns daemon-side log hygiene: daily age retention plus
// a frequent copy-truncate size backstop. Copy-truncate preserves the active
// logger fd, unlike renaming the live file (which would strand tailers/writers).
func runLogRetentionJanitor(ctx context.Context, ulog *grovelogging.UnifiedLogger, retentionDays int) {
	logsDir := filepath.Join(paths.StateDir(), "logs")
	retentionSweep := func() {
		deleted, freed, err := sweepOldLogs(logsDir, retentionDays, time.Now())
		entry := ulog.Debug("Log retention sweep: nothing to delete")
		if deleted > 0 {
			entry = ulog.Info("Log retention sweep").Field("event", "log.retention_sweep")
		}
		entry = entry.Field("deleted", deleted).Field("freed_bytes", freed).
			Field("retention_days", retentionDays).Field("dir", logsDir)
		if err != nil {
			entry = entry.Err(err)
		}
		entry.Log(ctx)
	}
	sizeSweep := func() {
		rotated, err := rotateOversizedLogs(logsDir, maxActiveLogBytes, time.Now())
		if rotated > 0 || err != nil {
			entry := ulog.Info("Rotated oversized active logs").Field("event", "log.size_rotation").
				Field("rotated", rotated).Field("max_bytes", maxActiveLogBytes).Field("dir", logsDir)
			if err != nil {
				entry = entry.Err(err)
			}
			entry.Log(ctx)
		}
	}

	retentionSweep()
	sizeSweep()
	retentionTicker := time.NewTicker(24 * time.Hour)
	sizeTicker := time.NewTicker(logSizeCheckEvery)
	defer retentionTicker.Stop()
	defer sizeTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retentionTicker.C:
			retentionSweep()
		case <-sizeTicker.C:
			sizeSweep()
		}
	}
}

// rotateOversizedLogs archives and truncates active *.log files over maxBytes.
// Archives are gzip-compressed (JSON logs shrink ~25x, and a part file only
// exists for postmortem reading). Existing part files are ignored.
// Best-effort walking mirrors sweepOldLogs.
func rotateOversizedLogs(logsDir string, maxBytes int64, now time.Time) (rotated int, firstErr error) {
	if maxBytes <= 0 {
		return 0, nil
	}
	if _, err := os.Stat(logsDir); err != nil {
		return 0, nil
	}
	_ = filepath.WalkDir(logsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".log") || strings.Contains(d.Name(), "-part-") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() <= maxBytes {
			return nil
		}
		base := strings.TrimSuffix(path, ".log")
		archive := fmt.Sprintf("%s-part-%s.log.gz", base, now.Format("20060102T150405.000000000"))
		src, err := os.Open(path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		dst, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			gz := gzip.NewWriter(dst)
			_, err = io.CopyN(gz, src, info.Size())
			if closeErr := gz.Close(); err == nil {
				err = closeErr
			}
		}
		_ = src.Close()
		if dst != nil {
			if syncErr := dst.Sync(); err == nil {
				err = syncErr
			}
			if closeErr := dst.Close(); err == nil {
				err = closeErr
			}
		}
		if err == nil {
			err = os.Truncate(path, 0)
		}
		if err != nil {
			_ = os.Remove(archive)
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		rotated++
		return nil
	})
	return rotated, firstErr
}

// sweepOldLogs walks logsDir recursively and deletes every *.log (and
// *.log.gz part archive) file that logFileExpired judges older than
// retentionDays. Best-effort: unreadable entries are skipped; the first
// removal error is reported alongside whatever was deleted. A missing logsDir
// is a no-op.
func sweepOldLogs(logsDir string, retentionDays int, now time.Time) (deleted int, freed int64, firstErr error) {
	if _, err := os.Stat(logsDir); err != nil {
		return 0, 0, nil
	}
	_ = filepath.WalkDir(logsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || (!strings.HasSuffix(d.Name(), ".log") && !strings.HasSuffix(d.Name(), ".log.gz")) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if !logFileExpired(d.Name(), info.ModTime(), now, retentionDays) {
			return nil
		}
		if rerr := os.Remove(path); rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			return nil
		}
		deleted++
		freed += info.Size()
		return nil
	})
	return deleted, freed, firstErr
}

// logFileExpired reports whether a dated grove log file is older than
// retentionDays, judged by the YYYY-MM-DD date embedded in the filename
// (e.g. system-2026-07-01.log), falling back to modTime when the name carries
// no parseable date. Today's (or a future-dated) file is never expired.
// Comparison is at day granularity: a file is deleted only when its day is
// strictly before now minus retentionDays.
func logFileExpired(name string, modTime, now time.Time, retentionDays int) bool {
	fileDate, ok := logFileDate(name)
	if !ok {
		fileDate = modTime
	}
	day := fileDate.Format("2006-01-02")
	// ISO dates compare lexicographically.
	if day >= now.Format("2006-01-02") {
		return false
	}
	return day < now.AddDate(0, 0, -retentionDays).Format("2006-01-02")
}

// logFileDate extracts the trailing YYYY-MM-DD date from a dated log filename
// like "system-2026-07-01.log" or "workspace-2026-07-01.log". ok=false when
// the name has no parseable date suffix.
func logFileDate(name string) (time.Time, bool) {
	const layout = "2006-01-02"
	base := strings.TrimSuffix(name, ".log")
	if base == name || len(base) < len(layout) {
		return time.Time{}, false
	}
	t, err := time.Parse(layout, base[len(base)-len(layout):])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func newGrovedUpgradeCmd() *cobra.Command {
	var scope string
	var global bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Gracefully replace the running daemon with the current binary",
		Long: `Zero-downtime upgrade: signals the running daemon with SIGUSR1 to enter
drain mode (unlink the socket, finish in-flight requests), then starts this
groved binary on the freed socket. The new daemon adopts running detached
agents by PID, so live agent panes and headless jobs survive the swap.

By default the target daemon is inferred from the current working directory
(workspace.ResolveScope) — the same scope treemux pins — so running this inside
a worktree upgrades that worktree's scoped daemon, not the global one. Use
--global to force the unscoped daemon, or --scope <label> to target by label.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Select the running daemon to upgrade from the live enumeration —
			// the same source of truth `status` and `kill` use.
			//
			// Target precedence: --global (unscoped) > --scope <label> (legacy
			// label match) > CWD-inferred scope (workspace.ResolveScope(cwd)).
			//
			// We never silently fall back to the unscoped daemon when a specific
			// scope was requested/inferred but not found: an earlier version did,
			// and `upgrade` then drained and replaced the *unscoped* daemon by
			// mistake. No match is always an error.
			if global && scope != "" {
				return fmt.Errorf("--global and --scope are mutually exclusive")
			}

			entries, err := enumerateDaemons()
			if err != nil {
				return fmt.Errorf("enumerate daemons: %w", err)
			}

			// Infer the CWD scope (the same resolution treemux uses) for the
			// default target; --global / --scope override it inside the resolver.
			var cwdScope string
			if !global && scope == "" {
				cwd, werr := os.Getwd()
				if werr != nil {
					return fmt.Errorf("resolve cwd: %w", werr)
				}
				cwdScope = workspace.ResolveScope(cwd)
			}
			matches, targetDesc := resolveUpgradeTarget(global, scope, cwdScope)

			var match *daemonEntry
			var runningLabels []string
			for i := range entries {
				e := entries[i]
				if !e.Running {
					continue
				}
				runningLabels = append(runningLabels, displayScope(e.Scope))
				if !matches(e) {
					continue
				}
				if match != nil {
					return fmt.Errorf("%s matches multiple running daemons; `groved kill` the extras first", targetDesc)
				}
				m := e
				match = &m
			}

			if match == nil {
				return fmt.Errorf("no running daemon for %s (running: %s)", targetDesc, strings.Join(runningLabels, ", "))
			}

			// A scoped successor must inherit the predecessor's exact scope string
			// (its GROVE_SCOPE), recorded in the .scope sidecar at start. Without it
			// we cannot guarantee the successor's child clients reconnect to the same
			// socket, so refuse rather than guess.
			if match.Scope != "" && match.ExactScope == "" {
				return fmt.Errorf("daemon for %s has no .scope sidecar (started by an older binary); restart it under the current binary, then upgrade", targetDesc)
			}

			return daemon.UpgradeRunning(cmd.Context(), match.PidPath, match.SockPath, match.ExactScope)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "label of the scoped daemon to upgrade (overrides the default CWD-inferred target)")
	cmd.Flags().BoolVar(&global, "global", false, "upgrade the global/unscoped daemon (overrides the default CWD-inferred target)")
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

// probeTuimux reports the liveness of a running daemon's paired tuimux daemon:
// "ok" (socket answers), "dead" (socket present but not answering within the
// probe timeout), "missing" (no socket), or "-" (scope can't be mapped to a
// socket path). A scoped daemon without a .scope sidecar (ExactScope == "" but
// Scope != "") can't be mapped — ScopedSocketPath("") would wrongly resolve to
// the shared global socket — so it reports "-".
func probeTuimux(e daemonEntry) string {
	if e.Scope != "" && e.ExactScope == "" {
		return "-"
	}
	sock := tuimux.ScopedSocketPath(e.ExactScope)
	if _, err := os.Stat(sock); err != nil {
		return "missing"
	}
	// Reuse the client's lightest health call (GET /api/ping) but bound it to a
	// short timeout: a hung daemon must not stall `status`. The client's own
	// 5s timeout backstops the leaked goroutine after we've already returned.
	client := tuimux.NewApiClient(sock)
	done := make(chan error, 1)
	go func() { done <- client.Ping() }()
	select {
	case err := <-done:
		if err != nil {
			return "dead"
		}
		return "ok"
	case <-time.After(1 * time.Second):
		return "dead"
	}
}

func newGrovedStatusCmd() *cobra.Command {
	var prune bool
	var fixtures bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List all groved daemons (running and stale)",
		Long: `Enumerate every groved pidfile under the state dir and report whether
each daemon is running. Running daemons show PID, scope, age, and socket
path. Stale pidfiles (PID gone, file left behind) are listed separately.

Pass --prune to remove stale pidfiles. Stale sockets are also unlinked.

Pass --fixtures to additionally census daemons that this enumeration cannot
see at all: those a test harness spawned inside a sandboxed HOME, whose
pidfiles and sockets live under /tmp rather than the real state dir.

Exits 0 if at least one running daemon is found; exits 1 if none.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := enumerateDaemons()
			if err != nil {
				return fmt.Errorf("enumerate: %w", err)
			}

			var running, stale []daemonEntry
			for _, e := range entries {
				if e.Running {
					running = append(running, e)
				} else {
					stale = append(stale, e)
				}
			}

			if len(running) > 0 {
				fmt.Printf("%-8s  %-32s  %-10s  %-8s  %s\n", "PID", "SCOPE", "AGE", "TUIMUX", "SOCKET")
				for _, e := range running {
					fmt.Printf("%-8d  %-32s  %-10s  %-8s  %s\n",
						e.PID, displayScope(e.Scope), e.Age, probeTuimux(e), filepath.Base(e.SockPath))
				}

				// Connect-only: don't auto-start the global daemon just to
				// display status. NewGlobalClient() would spawn one if absent.
				client := daemon.New()
				defer func() { _ = client.Close() }()
				if client.IsRunning() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if chStatus, err := client.GetChannelStatus(ctx); err == nil {
						fmt.Println()
						fmt.Println("Signal pipeline:")
						if !chStatus.SignalEnabled {
							fmt.Println("  inbound reader: disabled (signal not configured)")
						} else if chStatus.SignalIsAlive {
							fmt.Printf("  inbound reader: alive  restarts: %d  claws: %d\n",
								chStatus.SignalRestartCount, chStatus.RefCount)
						} else {
							state := "dead"
							if chStatus.SignalStopped {
								state = "stopped"
							}
							fmt.Printf("  inbound reader: %s  restarts: %d  claws: %d\n",
								state, chStatus.SignalRestartCount, chStatus.RefCount)
							if chStatus.SignalLastError != "" {
								fmt.Printf("    last error: %s\n", trimStatusError(chStatus.SignalLastError))
							}
						}
					}
				}
			} else {
				fmt.Println("No daemons running")
			}

			if len(stale) > 0 {
				if prune {
					fmt.Printf("\nPruning %d stale pidfile(s):\n", len(stale))
					for _, e := range stale {
						if err := os.Remove(e.PidPath); err == nil {
							fmt.Printf("  removed %s\n", filepath.Base(e.PidPath))
						} else {
							fmt.Printf("  failed %s: %v\n", filepath.Base(e.PidPath), err)
						}
						// Also unlink the orphaned socket and .scope sidecar if present.
						if _, err := os.Stat(e.SockPath); err == nil {
							_ = os.Remove(e.SockPath)
						}
						_ = os.Remove(scopeSidecarPath(e.PidPath))
					}
				} else {
					fmt.Printf("\nStale pidfiles (%d) — pass --prune to remove:\n", len(stale))
					for _, e := range stale {
						fmt.Printf("  %s (last PID %d — not running)\n", filepath.Base(e.PidPath), e.PID)
					}
				}
			}

			// Sweep orphaned .scope sidecars whose pidfile is gone (the daemon
			// exited cleanly and released the pidfile, but a sidecar can linger if
			// it was left by an upgrade or an unclean stop). These have no entry in
			// the pidfile-keyed enumeration above, so handle them separately.
			if prune {
				sidecars, _ := filepath.Glob(filepath.Join(paths.StateDir(), "groved*.scope"))
				for _, sc := range sidecars {
					pidSibling := strings.TrimSuffix(sc, ".scope") + ".pid"
					if _, err := os.Stat(pidSibling); os.IsNotExist(err) {
						if os.Remove(sc) == nil {
							fmt.Printf("  removed orphan sidecar %s\n", filepath.Base(sc))
						}
					}
				}
			}

			// Fixture daemons are structurally invisible to everything above:
			// the enumeration walks pidfiles under the REAL state dir, while a
			// fixture's pidfile and socket live inside a sandboxed HOME under
			// /tmp. That is why seven of them accumulated unnoticed until
			// somebody read `ps` during a performance investigation. This is a
			// census only — collecting them is `tend clean`'s job, because the
			// harnesses that own them are tend's.
			if fixtures {
				found, ferr := daemon.FindFixtureDaemons()
				if ferr != nil {
					return fmt.Errorf("scanning fixture namespaces: %w", ferr)
				}
				fmt.Println()
				fmt.Println("Fixture daemons (sandboxed test namespaces under /tmp):")
				fmt.Print(daemon.FormatFixtureDaemons(found))
				if len(found) > 0 {
					fmt.Println("\nCollect abandoned ones with: tend clean --dry-run")
				}
			}

			if len(running) == 0 {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false, "Remove stale pidfiles (and their orphaned sockets)")
	cmd.Flags().BoolVar(&fixtures, "fixtures", false,
		"Also census daemons serving sockets inside test-fixture namespaces (/tmp/tend-*, /tmp/tendlab-*, /tmp/tuipilot-*)")
	return cmd
}

func newGrovedConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show running daemon configuration",
		Long:  "Query the running daemon to show its active configuration intervals.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := daemon.New()
			defer func() { _ = client.Close() }()

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
			defer func() { _ = client.Close() }()

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

// monitorEmitter returns a function that prints formatted output to stdout in
// the requested format. The authoritative audit trail lives on the domain
// components (watchers, server, collectors); this emitter is a stdout-only
// presentation layer and must not write to the structured log file.
func monitorEmitter(component, format string, compact bool) (func(level, msg string, fields map[string]interface{}), *monitorState) {
	state := &monitorState{}

	emit := func(level, msg string, fields map[string]interface{}) {
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
