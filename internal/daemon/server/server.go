// Package server provides the HTTP server for the grove daemon.
package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	netpprof "net/http/pprof"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/grovetools/agentlogs/pkg/agentstream"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	coredaemon "github.com/grovetools/core/pkg/daemon"
	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/repo"
	"github.com/grovetools/core/pkg/sessions"
	coretmux "github.com/grovetools/core/pkg/tmux"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/version"
	"github.com/grovetools/daemon/internal/daemon/assistant"
	"github.com/grovetools/daemon/internal/daemon/buildqueue"
	"github.com/grovetools/daemon/internal/daemon/channels"
	"github.com/grovetools/daemon/internal/daemon/engine"
	daemonenv "github.com/grovetools/daemon/internal/daemon/env"
	"github.com/grovetools/daemon/internal/daemon/jobrunner"
	"github.com/grovetools/daemon/internal/daemon/logstreamer"
	"github.com/grovetools/daemon/internal/daemon/satellite"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
	"github.com/grovetools/daemon/internal/daemon/theming"
	"github.com/grovetools/daemon/internal/daemon/watcher"
	"github.com/grovetools/daemon/internal/enrichment"
	daemonweb "github.com/grovetools/daemon/web"
	"github.com/grovetools/flow/pkg/orchestration"
	"github.com/grovetools/memory/pkg/memory"
	navbindings "github.com/grovetools/nav/pkg/bindings"
	tuimux "github.com/grovetools/tuimux/api/client"
	"github.com/grovetools/tuimux/hub"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/sync/singleflight"
)

// RunningConfig holds the active configuration intervals being used by the daemon.
// This is exposed via the /api/config endpoint so clients can verify what config is active.
type RunningConfig struct {
	GitInterval       time.Duration `json:"git_interval"`
	SessionInterval   time.Duration `json:"session_interval"`
	WorkspaceInterval time.Duration `json:"workspace_interval"`
	PlanInterval      time.Duration `json:"plan_interval"`
	NoteInterval      time.Duration `json:"note_interval"`
	StartedAt         time.Time     `json:"started_at"`
}

// ConfigDegradation is the stable machine-readable reason a daemon is serving
// status only. Recovery is deliberately restart-only: a process that skipped
// constructing collectors, queues, watchers, and databases cannot safely grow
// those subsystems in place after an arbitrary config edit.
type ConfigDegradation struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Recovery string `json:"recovery"`
}

// Server manages the daemon's HTTP server over a Unix socket.
type Server struct {
	ulog              *logging.UnifiedLogger
	server            *http.Server
	engine            *engine.Engine
	runningConfig     *RunningConfig
	buildScheduler    *buildqueue.Scheduler
	logStreamer       *logstreamer.LogStreamer
	workspaceStreamer *logstreamer.WorkspaceStreamer

	// Nav mutations are read-modify-write operations on one sessions.yml.
	// Serialize all HTTP mutation endpoints so concurrent treemux/nav clients
	// cannot both load the same old snapshot and have the later rename erase
	// the earlier edit.
	navBindingsMu sync.Mutex

	// Late-wired dependencies. Under --ready-at=bind the socket binds and the
	// server starts accepting BEFORE these are set (they're wired from the
	// background boot goroutine), so a handler can race their assignment.
	// atomic.Pointer makes each read/write safe; every read site loads once
	// and nil-checks the local. Under the default bind-last ordering they are
	// all set before Serve, so the atomics are simply free.
	jobRunner      atomic.Pointer[jobrunner.JobRunner]
	envManager     atomic.Pointer[daemonenv.Manager]
	channelManager atomic.Pointer[channels.Manager]

	// assistantSupervisor is the ensure-running loop behind the rail's
	// assistant pane (spec §3.3). Nil on every daemon whose ecosystem has no
	// enabled [assistant] block, which is the default.
	assistantSupervisor atomic.Pointer[assistant.Supervisor]

	// satelliteCM is the SSH transport to registered satellites (M2 C1/C10).
	// It is wired ONLY on the global daemon (scope==""); scoped daemons leave
	// it nil, so a `Satellite`-tagged submit there returns 503. handleJobs uses
	// it to forward a satellite-routed job to that satellite's existing
	// POST /api/jobs (C3 — the satellite gains no new verb).
	satelliteCM atomic.Pointer[satellite.ConnManager]

	// satelliteReloadFn re-reads the satellite registry from disk and applies
	// it to the ConnManager (POST /api/satellites/reload). Wired by groved.go
	// alongside SetSatelliteConnManager on the global daemon only; nil means
	// satellites are unavailable here (scoped daemon, or the boot registry
	// load errored) and the handler 4xxes. atomic for the same early-bind
	// reason as satelliteCM.
	satelliteReloadFn atomic.Pointer[func() (*satellite.ReloadSummary, error)]

	// satelliteLeases maps a forwarded job's ID to the LOCAL plan dir the
	// laptop wrote a .grove-lease.yml into at dispatch (M2 C14). The lease is
	// removed when the job's federated terminal event arrives (see
	// StartSatelliteLeaseReleaser); a laptop restart loses this map and the
	// lease then releases by TTL expiry. Guarded by satelliteLeasesMu.
	satelliteLeasesMu sync.Mutex
	satelliteLeases   map[string]string

	// satelliteNotifyFn is the terminal-notification sink StartSatelliteNotifier
	// dispatches to. Nil (production) means notifySatelliteTerminal; tests set
	// it to observe the bridge without real osascript/ntfy I/O.
	satelliteNotifyFn func(ctx context.Context, job *models.JobInfo, updType store.UpdateType, ntfyURL, ntfyTopic string)

	// statsCache caches the /api/system/stats process-table sample so
	// polling clients cost at most one `ps` exec per statsSampleMaxAge
	// (see stats_handler.go). Seeded in the background from Listen so the
	// first request reports interval-true CPU%.
	statsCache procStatsCache

	// bootStatus is the source of truth for GET /api/system/boot. Nil until
	// the early-bind boot goroutine sets it; handleSystemBoot then reports
	// Done=true (the daemon only reaches that handler once serving, which
	// under bind-last means boot already finished).
	bootStatus atomic.Pointer[coredaemon.BootStatus]

	// configDegradation is set once at boot when the canonical config cannot
	// load. While present, status remains available but every new work
	// submission is rejected before it can reach a queue or runner.
	configDegradation atomic.Pointer[ConfigDegradation]

	// sseSubscribers / sseSubscribersFiltered count live /api/stream
	// subscriptions and how many of those declared a subscribe-time filter.
	// They back the sse.subscribers* counters on /api/system/stats — the
	// denominator for sse.events.*, which are meaningless without knowing how
	// many streams were open.
	sseSubscribers         atomic.Int64
	sseSubscribersFiltered atomic.Int64

	// frames is the marshal-once cache shared by every /api/stream subscriber:
	// one conversion and one json.Marshal per store sequence instead of one per
	// subscriber. See stream_frames.go.
	frames *frameCache

	// scope is the daemon's configured ecosystem scope — empty for the
	// global/unscoped daemon, non-empty for scoped daemons. Proxy
	// registration handlers gate on this: only the global daemon accepts
	// RegisterProxyRoute / UnregisterProxyRoutes, so scoped daemons can't
	// accidentally grow a competing route table.
	scope string

	// tuimuxClient talks to the standalone tuimux daemon that owns agent
	// PTYs out-of-process. Agent spawn, input, and interrupt route through
	// it so PTY panes survive a `groved upgrade`. May be nil if the tuimux
	// daemon could not be started. Late-wired (its EnsureDaemonWithBinary
	// spawn is deferred past early bind), so it's atomic like the other
	// boot-goroutine deps above.
	tuimuxClient atomic.Pointer[tuimux.ApiClient]

	// tuimuxReEnsure re-spawns/reconnects the paired tuimux daemon when the
	// current client goes stale (its daemon died out from under us). Wired once
	// at boot from groved when the tuimux binary resolved; handleAgentSpawn
	// calls it to self-heal a dead client instead of hard-failing. nil when
	// tuimux is unavailable. Atomic to match the late-wired deps above.
	tuimuxReEnsure atomic.Pointer[tuimuxReEnsureFn]

	// forgeSource is the running forge poller's read seam, wired via
	// SetForgeSnapshotter on the global daemon when the poller actually
	// started. Nil is meaningful: GET /api/forge/state answers enabled=false
	// rather than an empty list (see forge.go). Atomic like the other
	// late-wired boot deps above.
	forgeSource atomic.Pointer[forgeReadSeam]

	// Memory store + embedder are wired via SetMemoryStore so /api/memory/*
	// handlers can serve the same instance the MemoryHandler watcher uses.
	memStore    memory.DocumentStore
	memEmbedder memory.Embedder
	memDBPath   string

	// syncDB is wired via SetSyncDB on the global daemon when sync becomes
	// configured (dark by default — no sync config, no DB). Scoped daemons
	// leave it nil and proxy /api/sync/* to the global daemon. Atomic like the
	// other late-wired deps above: the watcher opens sync.db lazily — possibly
	// long after boot, when a first-ever `grove join` lands — and stores it
	// here while HTTP handlers are already serving.
	syncDB atomic.Pointer[syncdb.DB]

	// syncKick (SetSyncKick) triggers an immediate anti-entropy pass for a
	// workspace ("" = all) after /api/sync/repush voids synced state. Nil
	// when sync is not configured; then the hourly tick picks the reset up.
	syncKick func(workspace string)
	// syncSubscriptions (SetSyncSubscriptions) reads the watcher's live sync
	// config — server URL plus per-workspace pull/mode — so /api/sync/status
	// can say where each workspace syncs. Nil when sync is not configured;
	// then the status payload omits those fields.
	syncSubscriptions func() (string, []config.SyncWorkspace)
	// syncAuthFailure (SetSyncAuthFailure) reads the watcher's token-rejection
	// state so /api/sync/status can say "the server is rejecting this
	// machine's token" instead of reporting a plausible-looking idle sync.
	// Nil when sync is not configured; then the status payload omits it.
	syncAuthFailure func() (string, time.Time, bool)
	// syncContested / syncAdoptContested (SetSyncContested) are the W3.5
	// adoption surface: the watcher's contested verdicts, and the operator's
	// adoption of one. Nil when sync is not configured; then both endpoints
	// answer that rather than an empty list, which would read as "nothing is
	// withheld" on a daemon that is not watching anything.
	syncContested        func() []syncdb.ContestedNotespace
	syncAdoptContested   func(string) (syncdb.ContestedNotespace, string, error)
	syncDBError          func() string
	notespaceAdopted     func(root, displayName string)
	syncNotespaceRoots   func([]string) (map[string]string, error)
	syncBeginMaintenance func(context.Context) error
	syncEndMaintenance   func()
	maintenanceMu        sync.RWMutex
	maintenanceTargets   map[string]bool

	// captureWaiters holds pending GET /api/agents/{id}/capture requests.
	// The HTTP handler blocks on the channel until groveterm sends the
	// capture response via POST /api/agents/{id}/capture_response.
	captureWaitersMu sync.Mutex
	captureWaiters   map[string]chan string

	// finalOutputs retains the last screen text of agent panes whose PTY has
	// already exited, so capture can still answer once both live tiers are
	// dead. See agent_final_output.go for the retention bounds.
	finalOutputs *finalOutputStore

	// terminalHub routes WebSocket messages for multi-attach
	// (Primary/Follower groveterm instances).
	terminalHub *hub.Hub

	// repoGroup deduplicates concurrent /api/repos/ensure requests for the same URL+version.
	repoGroup singleflight.Group

	// OnReady, if non-nil, is invoked exactly once from inside ListenAndServe
	// after the unix socket has been bound and chmod'd — i.e. the earliest
	// moment a client can net.Dial the socket and have the kernel complete
	// the handshake. Wired up by groved.go to signal a ready-fd pipe back to
	// the factory, so daemon.NewWithAutoStart can block on pipe EOF instead
	// of polling with a guessed retry window.
	OnReady func()

	// PHASE 2: Drain mode fields for zero-downtime upgrade
	socketPath string
	listener   net.Listener
	drainMu    sync.Mutex
	isDraining bool
	requestsWg sync.WaitGroup

	// boundSocket is the pathname identity captured while the listener still
	// had an unguessable private name. Listen atomically publishes that socket
	// at socketPath, so this identity cannot accidentally capture a concurrent
	// replacement at the public path. Keeping it lets a daemon notice that it
	// has been unbound from the world instead of running on as an invisible
	// duplicate. Guarded by boundSocketMu; written once in Listen, read during
	// identity checks and cleanup.
	boundSocketMu sync.Mutex
	boundSocket   os.FileInfo
}

// New creates a new Server instance.
//
// autoShutdown enables the TerminalHub idle timer: when the last terminal
// WebSocket client disconnects, a 2-minute timer arms; if no client
// reconnects, the hub closes its ShutdownReq channel so groved's signal
// handler can initiate graceful shutdown through the same cleanup path
// used for SIGTERM.
func New(autoShutdown bool) *Server {
	hubCfg := hub.Config{
		AutoShutdown:   autoShutdown,
		InitialTimeout: 5 * time.Minute,
		IdleTimeout:    2 * time.Minute,
	}
	return &Server{
		ulog:               logging.NewUnifiedLogger("groved.server"),
		captureWaiters:     make(map[string]chan string),
		finalOutputs:       newFinalOutputStore(),
		terminalHub:        hub.NewHub(hubCfg),
		satelliteLeases:    make(map[string]string),
		maintenanceTargets: make(map[string]bool),
		frames:             newFrameCache(),
	}
}

// HasClients reports whether anything is currently attached to this daemon:
// a terminal WebSocket (the signal auto-shutdown's idle timer watches) or a
// live /api/stream subscriber (which holds a hub client lease for its
// lifetime, and is how a headless CLI or TUI keeps a daemon in use).
//
// It is the "is anyone home?" question the scoped self-yield asks before
// standing down in favor of a host daemon. It deliberately answers about
// ATTACHMENT, not about work: sessions and jobs are checked separately by the
// caller, because a daemon can own live agents with no client watching them.
func (s *Server) HasClients() bool {
	if s.terminalHub != nil && s.terminalHub.HasConnections() {
		return true
	}
	return s.sseSubscribers.Load() > 0
}

func (s *Server) TerminalHubShutdownReq() <-chan struct{} {
	if s.terminalHub == nil {
		return nil
	}
	return s.terminalHub.ShutdownReq()
}

// SetEngine sets the collector engine for the server.
func (s *Server) SetEngine(eng *engine.Engine) {
	s.engine = eng
}

// SetRunningConfig sets the running configuration for the server.
func (s *Server) SetRunningConfig(cfg *RunningConfig) {
	s.runningConfig = cfg
}

// SetJobRunner sets the job runner for the server.
func (s *Server) SetJobRunner(jr *jobrunner.JobRunner) {
	s.jobRunner.Store(jr)
}

// SetSatelliteConnManager wires the satellite SSH transport used to forward
// satellite-routed job submits (M2 C1/C10). Only the global daemon calls this
// with a non-nil manager; a nil cm (scoped daemon or empty registry) leaves
// satellite dispatch unavailable and handleJobs returns 503 for such submits.
func (s *Server) SetSatelliteConnManager(cm *satellite.ConnManager) {
	s.satelliteCM.Store(cm)
}

// SetSatelliteReloader wires the registry hot-reload closure behind
// POST /api/satellites/reload. groved.go passes a func that re-runs
// LoadRegistry from disk and hands the result to ConnManager.Reload — the
// server never touches the config loader itself. Global daemon only.
func (s *Server) SetSatelliteReloader(fn func() (*satellite.ReloadSummary, error)) {
	if fn != nil {
		s.satelliteReloadFn.Store(&fn)
	}
}

// SetLogStreamer sets the log streamer for the server.
func (s *Server) SetLogStreamer(ls *logstreamer.LogStreamer) {
	s.logStreamer = ls
}

// SetWorkspaceStreamer sets the workspace log streamer for the server.
func (s *Server) SetWorkspaceStreamer(ws *logstreamer.WorkspaceStreamer) {
	s.workspaceStreamer = ws
}

// SetEnvManager sets the environment manager for the server.
func (s *Server) SetEnvManager(m *daemonenv.Manager) {
	s.envManager.Store(m)
}

// SetChannelManager sets the channel manager for the server.
func (s *Server) SetChannelManager(m *channels.Manager) {
	s.channelManager.Store(m)
}

// SetScope records whether this daemon is global ("") or scoped (non-empty).
// Proxy handlers consult this to 400 scoped-daemon requests, matching the
// "only the global daemon owns the route table" architectural invariant.
func (s *Server) SetScope(scope string) {
	s.scope = scope
}

// SetTuimuxClient wires the standalone tuimux daemon client used to create
// and drive agent PTYs out-of-process. /api/pty/ and /api/hub/ are reverse
// proxied to the same tuimux daemon socket (see ListenAndServe).
func (s *Server) SetTuimuxClient(c *tuimux.ApiClient) {
	s.tuimuxClient.Store(c)
}

// tuimuxReEnsureFn re-ensures the paired tuimux daemon and returns a fresh
// client bound to its socket. Named so it can live in an atomic.Pointer.
type tuimuxReEnsureFn func() (*tuimux.ApiClient, error)

// SetTuimuxReEnsure wires the hook handleAgentSpawn uses to re-ensure the
// paired tuimux daemon after its client goes stale. Passing nil clears it.
func (s *Server) SetTuimuxReEnsure(fn tuimuxReEnsureFn) {
	if fn == nil {
		s.tuimuxReEnsure.Store(nil)
		return
	}
	s.tuimuxReEnsure.Store(&fn)
}

// SetBootStatus publishes the daemon's current boot progress for
// GET /api/system/boot. Called from the early-bind boot goroutine at each
// phase boundary; a nil status (never set) makes the endpoint report Done.
func (s *Server) SetBootStatus(status *coredaemon.BootStatus) {
	s.bootStatus.Store(status)
}

// SetConfigDegradation puts the server in restart-only status mode.
func (s *Server) SetConfigDegradation(message string) {
	s.configDegradation.Store(&ConfigDegradation{
		Code:     "config_load_failed",
		Message:  message,
		Recovery: "fix the configuration and restart groved",
	})
}

func (s *Server) configError() *ConfigDegradation {
	return s.configDegradation.Load()
}

func (s *Server) rejectSubmitWhenConfigDegraded(w http.ResponseWriter) bool {
	degraded := s.configError()
	if degraded == nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(struct {
		Error *ConfigDegradation `json:"error"`
	}{Error: degraded})
	return true
}

type degradedRouteMode uint8

const (
	degradedRouteRejected degradedRouteMode = iota
	degradedRouteHealth
	degradedRouteConfig
	degradedRouteBoot
	degradedRouteSyncStatus
)

type daemonRoute struct {
	pattern      string
	degradedMode degradedRouteMode
}

// daemonRoutes is the exhaustive normal HTTP route inventory. Config-degraded
// boot uses this same inventory to build a separate mux: four explicitly
// status-only handlers remain live and every other known route is replaced by
// the common structured 503 handler. New normal routes must be classified here
// or normal mux construction panics before serving.
var daemonRoutes = []daemonRoute{
	{pattern: "/health", degradedMode: degradedRouteHealth},
	{pattern: "/api/state"},
	{pattern: "/api/tasks"},
	{pattern: "/api/build/submit"},
	{pattern: "/api/build/cancel"},
	{pattern: "/api/build/jobs/"},
	{pattern: "/api/workspaces/"},
	{pattern: "/api/workspaces"},
	{pattern: "/api/plans"},
	{pattern: "/api/plan-index"},
	{pattern: "/api/sessions/intent"},
	{pattern: "/api/sessions/confirm"},
	{pattern: "/api/sessions/"},
	{pattern: "/api/sessions"},
	{pattern: "/api/stream"},
	{pattern: "/api/workspace/hud/stream"},
	{pattern: "/api/config", degradedMode: degradedRouteConfig},
	{pattern: "/api/focus"},
	{pattern: "/api/refresh"},
	{pattern: "/api/trust/seed"},
	{pattern: "/api/forge/state"},
	{pattern: "/api/notes/index"},
	{pattern: "/api/notes/event"},
	{pattern: "/api/workflows/event"},
	{pattern: "/api/workflows"},
	{pattern: "/api/subjobs/event"},
	{pattern: "/api/subjobs"},
	{pattern: "/api/logs/stream"},
	{pattern: "/api/env/up"},
	{pattern: "/api/env/down"},
	{pattern: "/api/env/status"},
	{pattern: "/api/proxy/register"},
	{pattern: "/api/proxy/unregister"},
	{pattern: "/api/repos/ensure"},
	{pattern: "/api/jobs/"},
	{pattern: "/api/jobs"},
	{pattern: "/api/satellite-artifacts/fetch"},
	{pattern: "/api/channels/send"},
	{pattern: "/api/channels/status"},
	{pattern: "/api/channels/cleanup"},
	{pattern: "/api/assistant/ensure"},
	{pattern: "/api/assistant/status"},
	{pattern: "/api/memory/search"},
	{pattern: "/api/memory/coverage"},
	{pattern: "/api/memory/status"},
	{pattern: "/api/memory/reindex"},
	{pattern: "/api/memory/analysis/gc"},
	{pattern: "/api/memory/analysis/workspaces"},
	{pattern: "/api/memory/analysis/ecosystems"},
	{pattern: "/api/memory/analysis/code"},
	{pattern: "/api/memory/analysis/concepts"},
	{pattern: "/api/memory/analysis/embeddings"},
	{pattern: "/api/memory/analysis/freshness"},
	{pattern: "/api/memory/analysis/duplicates"},
	{pattern: "/api/memory/analysis/notebooks"},
	{pattern: "/api/memory/analysis/context"},
	{pattern: "/api/machine"},
	{pattern: "/api/notespaces/adopt"},
	{pattern: "/api/sync/status", degradedMode: degradedRouteSyncStatus},
	{pattern: "/api/sync/allow"},
	{pattern: "/api/sync/history"},
	{pattern: "/api/sync/restore"},
	{pattern: "/api/sync/adopt"},
	{pattern: "/api/sync/incoming"},
	{pattern: "/api/sync/escrow"},
	{pattern: "/api/sync/apply"},
	{pattern: "/api/sync/maintenance"},
	{pattern: "/api/sync/repush"},
	{pattern: "/api/sync/documents"},
	{pattern: "/api/sync/outbox"},
	{pattern: "/api/sync/conflicts"},
	{pattern: "/api/sync/activity"},
	{pattern: "/api/sync/contested"},
	{pattern: "/api/sync/contested/adopt"},
	{pattern: "/web/treemux/"},
	{pattern: "/api/pty/"},
	{pattern: "/api/hub/"},
	{pattern: "/api/nav/bindings"},
	{pattern: "/api/nav/config"},
	{pattern: "/api/nav/groups/"},
	{pattern: "/api/nav/locked-keys"},
	{pattern: "/api/nav/last-accessed"},
	{pattern: "/api/satellites"},
	{pattern: "/api/satellites/reload"},
	{pattern: "/api/system/info"},
	{pattern: "/api/system/stats"},
	{pattern: "/api/system/boot", degradedMode: degradedRouteBoot},
	{pattern: "/api/system/treemux-status"},
	{pattern: "/api/agents/spawn"},
	{pattern: "/api/agents/"},
	{pattern: "/debug/pprof/"},
	{pattern: "/debug/pprof/cmdline"},
	{pattern: "/debug/pprof/profile"},
	{pattern: "/debug/pprof/symbol"},
	{pattern: "/debug/pprof/trace"},
}

type classifiedRouteRegistrar interface {
	HandleFunc(pattern string, handler http.HandlerFunc)
	Handle(pattern string, handler http.Handler)
}

type classifiedMux struct {
	mux       *http.ServeMux
	remaining map[string]struct{}
}

func newClassifiedMux() *classifiedMux {
	mux := http.NewServeMux()
	remaining := make(map[string]struct{}, len(daemonRoutes))
	for _, route := range daemonRoutes {
		if _, duplicate := remaining[route.pattern]; duplicate {
			panic("duplicate daemon route classification: " + route.pattern)
		}
		remaining[route.pattern] = struct{}{}
	}
	return &classifiedMux{mux: mux, remaining: remaining}
}

func (m *classifiedMux) consume(pattern string) {
	if _, ok := m.remaining[pattern]; !ok {
		panic("unclassified or duplicate normal daemon route: " + pattern)
	}
	delete(m.remaining, pattern)
}

func (m *classifiedMux) HandleFunc(pattern string, handler http.HandlerFunc) {
	m.consume(pattern)
	m.mux.HandleFunc(pattern, handler)
}

func (m *classifiedMux) Handle(pattern string, handler http.Handler) {
	m.consume(pattern)
	m.mux.Handle(pattern, handler)
}

func (m *classifiedMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

func (m *classifiedMux) assertComplete() {
	if len(m.remaining) == 0 {
		return
	}
	missing := make([]string, 0, len(m.remaining))
	for pattern := range m.remaining {
		missing = append(missing, pattern)
	}
	slices.Sort(missing)
	panic("classified daemon routes not mounted by normal mux: " + strings.Join(missing, ", "))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if degraded := s.configError(); degraded != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(struct {
			Status      string             `json:"status"`
			Degraded    bool               `json:"degraded"`
			ConfigError *ConfigDegradation `json:"config_error"`
		}{Status: "degraded", Degraded: true, ConfigError: degraded})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) registerConfigDegradedRoutes(mux *http.ServeMux) {
	for _, route := range daemonRoutes {
		var handler http.HandlerFunc
		switch route.degradedMode {
		case degradedRouteHealth:
			handler = s.handleHealth
		case degradedRouteConfig:
			handler = s.handleGetConfig
		case degradedRouteBoot:
			handler = s.handleSystemBoot
		case degradedRouteSyncStatus:
			handler = unixOnly(s.handleSyncStatus)
		case degradedRouteRejected:
			handler = func(w http.ResponseWriter, _ *http.Request) {
				_ = s.rejectSubmitWhenConfigDegraded(w)
			}
		default:
			panic("unknown degraded route mode")
		}
		mux.HandleFunc(route.pattern, handler)
	}
}

// normalRouteHandler is the only constructor for the daemon's normal API mux.
// The raw ServeMux is encapsulated by classifiedMux, so registration code can
// only add a route by consuming its daemonRoutes classification.
func (s *Server) normalRouteHandler() http.Handler {
	routes := newClassifiedMux()
	s.registerNormalRoutes(routes)
	routes.assertComplete()
	return routes
}

// registerNormalRoutes deliberately receives only the classified registrar,
// never a raw *http.ServeMux. Keep every normal daemon API registration here.
func (s *Server) registerNormalRoutes(routes classifiedRouteRegistrar) {
	// Health check endpoint.
	routes.HandleFunc("/health", s.handleHealth)

	// State API endpoints
	routes.HandleFunc("/api/state", s.handleGetState)
	routes.HandleFunc("/api/tasks", s.handlePostTaskReport)
	// Machine-wide build queue endpoints
	routes.HandleFunc("/api/build/submit", s.handleBuildSubmit)
	routes.HandleFunc("/api/build/cancel", s.handleBuildCancel)
	routes.HandleFunc("/api/build/jobs/", s.handleBuildJobSubpath)
	routes.HandleFunc("/api/workspaces/", s.handleWorkspaceSubpath)
	routes.HandleFunc("/api/workspaces", s.handleGetWorkspaces)
	routes.HandleFunc("/api/plans", s.handleGetPlans)
	routes.HandleFunc("/api/plan-index", s.handleGetPlanIndex)
	// Session endpoints - order matters! Most specific routes first.
	routes.HandleFunc("/api/sessions/intent", s.handleSessionIntent)
	routes.HandleFunc("/api/sessions/confirm", s.handleSessionConfirm)
	routes.HandleFunc("/api/sessions/", s.handleSessionByID)
	routes.HandleFunc("/api/sessions", s.handleSessions)
	routes.HandleFunc("/api/stream", s.handleStreamState)
	routes.HandleFunc("/api/workspace/hud/stream", s.handleStreamWorkspaceHUD)
	routes.HandleFunc("/api/config", s.handleGetConfig)
	routes.HandleFunc("/api/focus", s.handleFocus)
	routes.HandleFunc("/api/refresh", s.handleRefresh)
	// Privileged Claude folder-trust seeding — unix socket only. The daemon
	// derives the trusted paths from the worktree registry (never the caller),
	// so a sandboxed provisioner can delegate the ~/.claude.json write it cannot
	// perform itself. See handleSeedTrust for the security rationale.
	routes.HandleFunc("/api/trust/seed", unixOnly(s.handleSeedTrust))
	routes.HandleFunc("/api/forge/state", s.handleGetForgeState)
	routes.HandleFunc("/api/notes/index", s.handleNoteIndex)
	routes.HandleFunc("/api/notes/event", s.handleNoteEvent)
	// Workflow/subagent aggregation endpoints. Served on the 0600 unix
	// socket only (defensive: workflow payloads carry transcript-derived
	// content — prompts, last assistant messages — that must not be
	// reachable via the unauthenticated localhost TCP listener).
	routes.HandleFunc("/api/workflows/event", unixOnly(s.handleWorkflowEvent))
	routes.HandleFunc("/api/workflows", unixOnly(s.handleGetWorkflows))
	routes.HandleFunc("/api/subjobs/event", unixOnly(s.handleSubjobEvent))
	routes.HandleFunc("/api/subjobs", unixOnly(s.handleGetSubjobs))
	// Aggregated workspace log streaming
	routes.HandleFunc("/api/logs/stream", s.handleStreamWorkspaceLogs)
	// Environment management endpoints
	routes.HandleFunc("/api/env/up", s.handleEnvUp)
	routes.HandleFunc("/api/env/down", s.handleEnvDown)
	routes.HandleFunc("/api/env/status", s.handleEnvStatus)
	// Global-proxy route registration (global daemon only)
	routes.HandleFunc("/api/proxy/register", s.handleProxyRegister)
	routes.HandleFunc("/api/proxy/unregister", s.handleProxyUnregister)
	// Repo ensure endpoint (single-flighted clones).
	routes.HandleFunc("/api/repos/ensure", s.handleRepoEnsure)
	// Job management endpoints
	routes.HandleFunc("/api/jobs/", s.handleJobByID)
	routes.HandleFunc("/api/jobs", s.handleJobs)
	// Bounded artifact return. The guest-local job route publishes files;
	// this laptop-only route forwards over the pinned satellite transport.
	routes.HandleFunc("/api/satellite-artifacts/fetch", s.handleSatelliteArtifactFetch)
	// Channel management endpoints
	routes.HandleFunc("/api/channels/send", s.handleChannelSend)
	routes.HandleFunc("/api/channels/status", s.handleChannelStatus)
	routes.HandleFunc("/api/channels/cleanup", s.handleChannelCleanup)
	// Assistant supervisor endpoints (spec §3.3). The ensure endpoint is what
	// the rail's assistant pane calls when it finds no live head.
	routes.HandleFunc("/api/assistant/ensure", s.handleAssistantEnsure)
	routes.HandleFunc("/api/assistant/status", s.handleAssistantStatus)
	// Memory search endpoints
	routes.HandleFunc("/api/memory/search", s.handleMemorySearch)
	routes.HandleFunc("/api/memory/coverage", s.handleMemoryCoverage)
	routes.HandleFunc("/api/memory/status", s.handleMemoryStatus)
	routes.HandleFunc("/api/memory/reindex", s.handleMemoryReindex)
	// Memory analysis endpoints
	routes.HandleFunc("/api/memory/analysis/gc", s.handleMemoryAnalysisGC)
	routes.HandleFunc("/api/memory/analysis/workspaces", s.handleMemoryAnalysisWorkspaces)
	routes.HandleFunc("/api/memory/analysis/ecosystems", s.handleMemoryAnalysisEcosystems)
	routes.HandleFunc("/api/memory/analysis/code", s.handleMemoryAnalysisCode)
	routes.HandleFunc("/api/memory/analysis/concepts", s.handleMemoryAnalysisConcepts)
	routes.HandleFunc("/api/memory/analysis/embeddings", s.handleMemoryAnalysisEmbeddings)
	routes.HandleFunc("/api/memory/analysis/freshness", s.handleMemoryAnalysisFreshness)
	routes.HandleFunc("/api/memory/analysis/duplicates", s.handleMemoryAnalysisDuplicates)
	routes.HandleFunc("/api/memory/analysis/notebooks", s.handleMemoryAnalysisNotebooks)
	routes.HandleFunc("/api/memory/analysis/context", s.handleMemoryAnalysisContext)
	// Machine identity + declared intent (unix socket only: it is this host's
	// inventory). Every daemon can answer it — the reconciliation reads
	// config and the filesystem, not sync.db — so it is NOT scope-proxied.
	routes.HandleFunc("/api/machine", unixOnly(s.handleMachineStatus))
	// Explicit post-adopt authority. Missing stamps encountered by discovery
	// have no route here and remain non-mutating.
	routes.HandleFunc("/api/notespaces/adopt", unixOnly(s.handleNotespaceAdopt))
	// Sync endpoints — unix socket only (sync state is content-adjacent
	// metadata; never expose it on the unauthenticated TCP listener).
	// Scoped daemons proxy these to the global daemon, which owns sync.db.
	routes.HandleFunc("/api/sync/status", unixOnly(s.handleSyncStatus))
	routes.HandleFunc("/api/sync/allow", unixOnly(func(w http.ResponseWriter, r *http.Request) {
		// POST adds a quarantine override, DELETE removes it. ServeMux
		// panics on duplicate patterns, so dispatch by method here.
		if r.Method == http.MethodDelete {
			s.handleSyncDisallowQuarantine(w, r)
			return
		}
		s.handleSyncAllow(w, r)
	}))
	routes.HandleFunc("/api/sync/history", unixOnly(s.handleSyncHistory))
	routes.HandleFunc("/api/sync/restore", unixOnly(s.handleSyncRestore))
	// Adopt (P5, S5): user-initiated resolution of a diverged document. The
	// daemon fetches the server head + rolls the merge base; the CLI writes it.
	routes.HandleFunc("/api/sync/adopt", unixOnly(s.handleSyncAdopt))
	routes.HandleFunc("/api/sync/incoming", unixOnly(s.handleSyncIncoming))
	routes.HandleFunc("/api/sync/escrow", unixOnly(s.handleSyncEscrow))
	routes.HandleFunc("/api/sync/apply", unixOnly(s.handleSyncApply))
	routes.HandleFunc("/api/sync/maintenance", unixOnly(s.handleSyncMaintenance))
	// Repush: manual full re-push after a server recreate — voids synced
	// state (non-diverged docs) and kicks an immediate anti-entropy pass.
	routes.HandleFunc("/api/sync/repush", unixOnly(s.handleSyncRepush))
	// Read-only introspection for the dev UI / playground god-view.
	routes.HandleFunc("/api/sync/documents", unixOnly(s.handleSyncDocuments))
	routes.HandleFunc("/api/sync/outbox", unixOnly(s.handleSyncOutbox))
	routes.HandleFunc("/api/sync/conflicts", unixOnly(s.handleSyncConflicts))
	routes.HandleFunc("/api/sync/activity", unixOnly(s.handleSyncActivity))
	routes.HandleFunc("/api/sync/contested", unixOnly(s.handleSyncContested))
	routes.HandleFunc("/api/sync/contested/adopt", unixOnly(s.handleSyncAdoptContested))
	// Static web viewer files
	routes.Handle("/web/treemux/", http.StripPrefix("/web/treemux/", daemonweb.TreemuxFileServer()))

	// PTY + hub endpoints — reverse proxied to the standalone tuimux daemon
	// which owns the PTY master FDs out-of-process. Proxying (rather than
	// embedding a manager) is what lets agent panes survive a `groved
	// upgrade`: the tuimux daemon outlives groved, so the successor simply
	// re-proxies to the same live socket and clients auto-reconnect. The
	// proxy transparently handles WebSocket upgrades (/api/pty/attach,
	// /api/pty/subscribe) and SSE (/api/pty/events).
	// Proxy this daemon's /api/pty/* to ITS OWN scope-keyed tuimux socket so a
	// scoped daemon's inspector reads only its own PTY map. Empty scope resolves
	// to the legacy machine-wide socket (backward compat).
	tuimuxSock := tuimux.ScopedSocketPath(s.scope)
	ptyProxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "unix"
		},
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", tuimuxSock)
			},
		},
	}
	routes.Handle("/api/pty/", ptyProxy)
	routes.Handle("/api/hub/", ptyProxy)

	// Nav bindings endpoints
	routes.HandleFunc("/api/nav/bindings", s.handleNavBindings)
	routes.HandleFunc("/api/nav/config", s.handleNavConfig)
	routes.HandleFunc("/api/nav/groups/", s.handleNavGroup)
	routes.HandleFunc("/api/nav/locked-keys", s.handleNavLockedKeys)
	routes.HandleFunc("/api/nav/last-accessed", s.handleNavLastAccessedGroup)
	// Satellite federation read surface (P10, M2 contract C17). This is a
	// laptop-side READ endpoint returning the global daemon's ConnManager
	// health map — NOT an inbound verb the laptop invokes on a satellite, so
	// C3's direction invariant holds. The mux.HandleFunc count increases here
	// deliberately for this local read surface.
	routes.HandleFunc("/api/satellites", s.handleSatellites)
	// Satellite registry hot-reload (write surface, still laptop-side only —
	// C3's direction invariant holds; nothing here dials INTO a satellite
	// beyond what the ConnManager already owns). `grove satellite up`/`down`
	// POST here as their final step so registry changes apply without an
	// agent-killing daemon restart.
	routes.HandleFunc("/api/satellites/reload", s.handleSatellitesReload)
	// System endpoints
	routes.HandleFunc("/api/system/info", s.handleSystemInfo)
	routes.HandleFunc("/api/system/stats", s.handleSystemStats)
	// Seed the stats sampler in the background so the first
	// /api/system/stats request already has a previous snapshot and
	// reports interval-true CPU% instead of ps's decaying average. Chosen
	// over paying the warm-up in the first request: the handler never
	// blocks on a sampling interval.
	go func() { _, _ = s.statsCache.get(0) }()
	routes.HandleFunc("/api/system/boot", s.handleSystemBoot)
	routes.HandleFunc("/api/system/treemux-status", s.handleTerminalStatus)
	// Native agent pane relay endpoints. Spawn shells a caller-supplied
	// command, so it is pinned to the 0600 unix socket via unixOnly (defense in
	// depth behind the TCP listener's browserCrossSiteGuard): the only shipped
	// spawn client, core's RemoteClient, always dials the unix socket, and the
	// browser web viewer needs read-only SSE only — nothing legitimately spawns
	// over the localhost TCP listener.
	routes.HandleFunc("/api/agents/spawn", unixOnly(s.handleAgentSpawn))
	routes.HandleFunc("/api/agents/", s.handleAgentByID)

	// Runtime profiling, always mounted, unix socket only. Attributing groved's
	// live heap used to require restarting it with --pprof-port, which kills
	// every running agent pane — so in practice no audit ever got a heap
	// profile of the daemon that was actually misbehaving, and retention was
	// guessed at from goroutine counts. The socket is 0600 and already carries
	// strictly more sensitive payloads (transcripts, sync state), so exposing
	// profiles there widens nothing; unixOnly keeps them off the
	// unauthenticated localhost TCP listener the web viewer uses.
	//
	//	curl --unix-socket <sock> http://local/debug/pprof/heap > heap.pb.gz
	//	go tool pprof -top heap.pb.gz
	routes.HandleFunc("/debug/pprof/", unixOnly(netpprof.Index))
	routes.HandleFunc("/debug/pprof/cmdline", unixOnly(netpprof.Cmdline))
	routes.HandleFunc("/debug/pprof/profile", unixOnly(netpprof.Profile))
	routes.HandleFunc("/debug/pprof/symbol", unixOnly(netpprof.Symbol))
	routes.HandleFunc("/debug/pprof/trace", unixOnly(netpprof.Trace))
}

func (s *Server) configDegradedRouteHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerConfigDegradedRoutes(mux)
	return mux
}

// ListenAndServe binds the socket and then blocks serving on it — the
// original one-shot entrypoint, preserved for the default bind-last boot and
// for every existing caller/test. It is Listen followed by Serve.
func (s *Server) ListenAndServe(socketPath string, httpPort ...int) error {
	if err := s.Listen(socketPath, httpPort...); err != nil {
		return err
	}
	return s.Serve()
}

// Listen binds the daemon's unix socket (and optional localhost:httpPort TCP
// listener), builds the request mux, and fires OnReady — but does NOT start
// accepting yet. Split out from Serve so the early-bind boot path
// (--ready-at=bind) can bind the socket first (unblocking the client) and run
// the slow boot steps in the background before Serve begins accepting.
// If httpPort > 0, also listens on localhost:httpPort for browser access
// (web terminal viewer, API debugging).
func (s *Server) Listen(socketPath string, httpPort ...int) error {
	// PHASE 2: Store socket path for drain mode
	s.socketPath = socketPath

	// Ensure directory exists before creating the private bind name.
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil { //nolint:gosec // G301: daemon/test dir
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	// Bind and identify the socket under an unguessable private pathname, then
	// atomically publish it over the well-known path. Looking up the public path
	// after a direct bind is racy: another process can replace it between bind
	// and stat, causing us to remember the replacement as our own. The listener
	// has auto-unlink disabled because its remembered name is the private one;
	// all public-path cleanup below is explicit and identity checked.
	listener, info, err := bindPublishedUnixSocket(socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}
	s.boundSocketMu.Lock()
	s.boundSocket = info
	s.boundSocketMu.Unlock()

	// PHASE 2: Store listener for drain mode socket unlink
	s.listener = listener

	// The socket is bound and chmod'd — clients can connect now even though
	// Serve hasn't been called yet (kernel holds accept-queue entries until
	// we start accepting). Signal readiness before the mux/server setup so
	// the parent stops waiting at the earliest defensible point.
	if s.OnReady != nil {
		s.OnReady()
	}

	var mux http.Handler
	if s.configError() != nil {
		// This is intentionally not the normal mux wrapped in middleware. It is
		// a separately constructed status-only surface whose non-status entries
		// are inert structured-503 placeholders.
		mux = s.configDegradedRouteHandler()
	} else {
		mux = s.normalRouteHandler()
	}

	handler := h2c.NewHandler(mux, &http2.Server{})

	s.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Optionally start a TCP listener for browser access (web terminal viewer).
	if len(httpPort) > 0 && httpPort[0] > 0 {
		port := httpPort[0]
		go func() {
			bgCtx := context.Background()
			addr := fmt.Sprintf("localhost:%d", port)
			s.ulog.Info("HTTP server listening (web terminal viewer)").
				Field("addr", addr).
				Log(bgCtx)
			// The TCP listener is unauthenticated and shares the same
			// process-spawning mux as the 0600 unix socket, so its handler
			// (and ONLY its handler — the unix path above is left bare) is
			// wrapped to reject browser cross-site requests. This closes the
			// drive-by-web-page → RCE vector without affecting native clients.
			tcpServer := &http.Server{
				Addr:              addr,
				Handler:           browserCrossSiteGuard(handler),
				ReadHeaderTimeout: 10 * time.Second,
			}
			if err := tcpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.ulog.Error("HTTP server failed").Err(err).Log(bgCtx)
			}
		}()
	}

	s.ulog.Info("Daemon listening").Field("socket", socketPath).Log(context.Background())
	return nil
}

// Serve blocks accepting and handling requests on the socket bound by Listen.
// It must be called after Listen; calling it without a prior successful Listen
// panics on the nil server (a programmer error, not a runtime condition).
func (s *Server) Serve() error {
	defer func() { _, _ = s.removeBoundSocket() }()
	err := s.server.Serve(s.listener)

	// A graceful drain (SIGUSR1 → EnterDrainMode) closes the listener out from
	// under Serve, which then returns a "use of closed network connection"
	// accept error. During an upgrade that is the expected, successful end of
	// this daemon's life — the successor has taken the socket — not a failure.
	// Report it as a clean shutdown so `groved start` exits 0 instead of
	// printing "Error: server error: …" on a working upgrade. ErrServerClosed
	// (the SIGTERM/Shutdown path) is likewise not an error.
	s.drainMu.Lock()
	draining := s.isDraining
	s.drainMu.Unlock()
	if draining || errors.Is(err, http.ErrServerClosed) {
		s.ulog.Info("Listener closed for shutdown; exiting cleanly").Log(context.Background())
		return nil
	}
	return err
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.ulog.Info("Shutting down server...").Log(ctx)
	var shutdownErr error
	if s.server != nil {
		shutdownErr = s.server.Shutdown(ctx)
	}
	// Shutdown only knows about listeners after Serve has registered them. Close
	// explicitly as well so a Listen-without-Serve failure cannot leak a socket.
	if s.listener != nil {
		if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	if _, err := s.removeBoundSocket(); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	return shutdownErr
}

// IsDraining reports whether a graceful drain (SIGUSR1 → EnterDrainMode) is in
// progress. During a drain this daemon has deliberately given its socket up, so
// callers that police socket ownership must stand down.
func (s *Server) IsDraining() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.isDraining
}

// privateSocketPath returns a randomized sibling path whose basename is never
// longer than the public basename. Keeping it no longer is load-bearing on
// Darwin, where sockaddr_un paths are short: a valid public path near the limit
// must not become invalid merely because publication uses a private name.
func privateSocketPath(publicPath string) (string, error) {
	nameLen := len(filepath.Base(publicPath))
	if nameLen == 0 {
		return "", fmt.Errorf("socket path %q has no basename", publicPath)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate private socket name: %w", err)
	}
	encoded := hex.EncodeToString(random[:])
	if nameLen > len(encoded) {
		nameLen = len(encoded)
	}
	privateName := encoded[:nameLen]
	if privateName == filepath.Base(publicPath) {
		// Even a one-byte public basename must never be selected: bind cleanup
		// may unlink its candidate, and quarantine rename(path, path) is a no-op.
		alternate := byte('0')
		if privateName[0] == alternate {
			alternate = '1'
		}
		privateName = string(alternate) + privateName[1:]
	}
	return filepath.Join(filepath.Dir(publicPath), privateName), nil
}

// bindPublishedUnixSocket binds a short unguessable name beside publicPath,
// captures that private pathname's filesystem identity, and atomically renames
// it into place. The socket remains reachable after rename on supported Unix
// systems. The caller owns both the listener and identity-checked cleanup.
func bindPublishedUnixSocket(publicPath string) (net.Listener, os.FileInfo, error) {
	var lastErr error
	for range 32 {
		privatePath, err := privateSocketPath(publicPath)
		if err != nil {
			return nil, nil, err
		}
		listener, err := net.Listen("unix", privatePath)
		if err != nil {
			lastErr = err
			if errors.Is(err, syscall.EADDRINUSE) {
				continue
			}
			return nil, nil, err
		}
		ul, ok := listener.(*net.UnixListener)
		if !ok {
			_ = listener.Close()
			return nil, nil, fmt.Errorf("listener is %T, want *net.UnixListener", listener)
		}
		ul.SetUnlinkOnClose(false)

		info, err := os.Stat(privatePath)
		if err == nil {
			err = os.Chmod(privatePath, 0o600)
		}
		if err == nil {
			err = os.Rename(privatePath, publicPath)
		}
		if err != nil {
			_ = listener.Close()
			if info != nil {
				_ = os.Remove(privatePath)
			}
			return nil, nil, err
		}
		return listener, info, nil
	}
	return nil, nil, fmt.Errorf("could not allocate private socket name: %w", lastErr)
}

// removeSocketIfOwned atomically detaches the public direntry before deciding
// whether to delete it. A stat-then-remove sequence is unsafe: a successor can
// publish between those calls and be unlinked by its predecessor. If the
// detached entry belongs to somebody else, restore it with a no-overwrite hard
// link before removing the private quarantine link.
func removeSocketIfOwned(path string, identity os.FileInfo) (bool, error) {
	return removeSocketIfOwnedInterleaved(path, identity, nil, nil)
}

func removeSocketIfOwnedBeforeDetach(path string, identity os.FileInfo, beforeDetach func()) (bool, error) {
	return removeSocketIfOwnedInterleaved(path, identity, beforeDetach, nil)
}

// removeSocketIfOwnedInterleaved exposes the two publication boundaries only
// to deterministic package tests. Production cleanup passes nil hooks.
func removeSocketIfOwnedInterleaved(path string, identity os.FileInfo, beforeDetach func(), afterDetach func(string)) (bool, error) {
	if path == "" || identity == nil {
		return false, nil
	}
	if beforeDetach != nil {
		beforeDetach()
	}

	var quarantine string
	for range 32 {
		candidate, err := privateSocketPath(path)
		if err != nil {
			return false, err
		}
		// Reserve the destination with O_EXCL. Rename atomically replaces our
		// reservation, so another cleanup cannot choose the same quarantine and
		// we never overwrite an unrelated socket after a racy existence check.
		reservation, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		_ = reservation.Close()
		quarantine = candidate
		break
	}
	if quarantine == "" {
		return false, fmt.Errorf("could not allocate socket quarantine path")
	}
	if err := os.Rename(path, quarantine); err != nil {
		_ = os.Remove(quarantine)
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if afterDetach != nil {
		afterDetach(quarantine)
	}

	current, err := os.Stat(quarantine)
	if err != nil {
		return false, err
	}
	if os.SameFile(identity, current) {
		return true, os.Remove(quarantine)
	}

	// We detached a successor. Link it back only if no still-newer publisher has
	// already occupied the public path. The quarantine link is removed only
	// after another link preserves the successor inode.
	if err := os.Link(quarantine, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, fmt.Errorf("socket changed during cleanup; preserved displaced entry at %s", quarantine)
		}
		return false, fmt.Errorf("restore socket changed during cleanup: %w", err)
	}
	if err := os.Remove(quarantine); err != nil {
		return false, fmt.Errorf("remove restored socket quarantine: %w", err)
	}
	return false, nil
}

func (s *Server) removeBoundSocket() (bool, error) {
	s.boundSocketMu.Lock()
	bound := s.boundSocket
	s.boundSocketMu.Unlock()
	return removeSocketIfOwned(s.socketPath, bound)
}

// SocketIdentityLost reports whether the socket path this server bound is now
// occupied by a DIFFERENT file than the one it bound, plus a detail line for
// the log.
//
// This is the belt-and-suspenders half of single-daemon enforcement. The
// pidfile lock is what stops two cold starts, but Listen's unlink-then-bind
// means any process that reaches a bind on our path — an older binary that
// never took the lock, an operator running `groved start --socket` by hand —
// silently steals every future client while our listener goes on accepting on
// an inode with no name. A daemon in that state serves nobody, yet still runs
// collectors, watchers and its own signal-cli.
//
// Two states deliberately report "not lost":
//
//   - draining: we unlinked the socket ourselves and the successor binding it
//     is the whole point of `groved upgrade`.
//   - the path is missing entirely: an unlink is not a steal, and `groved
//     status --prune` (or a human) removing a socket file must not take the
//     live daemon behind it down with it.
func (s *Server) SocketIdentityLost() (bool, string) {
	if s.IsDraining() {
		return false, ""
	}

	s.boundSocketMu.Lock()
	bound := s.boundSocket
	s.boundSocketMu.Unlock()
	if bound == nil || s.socketPath == "" {
		return false, ""
	}

	current, err := os.Stat(s.socketPath)
	if err != nil {
		return false, ""
	}
	if os.SameFile(bound, current) {
		return false, ""
	}
	return true, fmt.Sprintf("socket %s is no longer the file this daemon bound", s.socketPath)
}

// EnterDrainMode implements zero-downtime upgrade: unlink the socket, refuse new requests,
// and exit once all in-flight requests complete.
// PHASE 2: Called on SIGUSR1 to allow a new daemon to bind the socket while this one
// finishes existing API calls and SSE streams.
func (s *Server) EnterDrainMode(ctx context.Context) {
	s.drainMu.Lock()
	if s.isDraining {
		s.drainMu.Unlock()
		return
	}
	s.isDraining = true
	s.drainMu.Unlock()

	s.ulog.Info("Entering drain mode").Log(ctx)

	// Unlink our socket immediately so the new daemon can bind. If the public
	// path is already a successor's socket, leave it untouched.
	if removed, err := s.removeBoundSocket(); err != nil {
		s.ulog.Warn("Failed to unlink socket").Field("path", s.socketPath).Err(err).Log(ctx)
	} else if removed {
		s.ulog.Info("Socket unlinked").Field("path", s.socketPath).Log(ctx)
	}

	// Close the listener to refuse new connections
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			s.ulog.Warn("Failed to close listener").Err(err).Log(ctx)
		} else {
			s.ulog.Info("Listener closed").Log(ctx)
		}
	}

	// Broadcast draining event to SSE subscribers so they reconnect to the new daemon
	// This is done via the log streamer's draining event
	if s.logStreamer != nil {
		s.logStreamer.NotifyDraining()
	}

	// Wait for in-flight requests to complete (bounded by HTTP read/write timeouts)
	drainTimeout := 30 * time.Second
	drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	// Poll until all requests are done or timeout
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-drainCtx.Done():
			s.ulog.Info("Drain timeout reached, exiting").Log(ctx)
			return
		case <-ticker.C:
			// Check if any requests are still in flight
			// For now, we just wait for the timeout since tracking individual
			// requests requires middleware. In practice, the HTTP timeouts
			// (ReadHeaderTimeout: 10s) bound the wait.
			s.ulog.Debug("Draining...").Log(ctx)
		}
	}
}

// handleGetState returns the complete daemon state as JSON.
func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	state := s.engine.Store().Get()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// handleGetWorkspaces returns all enriched workspaces as JSON.
func (s *Server) handleGetWorkspaces(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	workspaces := s.engine.Store().GetWorkspaces()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(workspaces)
}

// handleWorkspaceSubpath routes /api/workspaces/{workspace}/tasks to the task report handler.
func (s *Server) handleWorkspaceSubpath(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[len("/api/workspaces/"):]
	parts := splitPath(path)
	if len(parts) < 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	workspace := parts[0]
	switch parts[1] {
	case "tasks":
		s.handlePostTaskResult(w, r, workspace)
	case "test-results":
		s.handlePostTestReport(w, r, workspace)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handlePostTaskReport(w http.ResponseWriter, r *http.Request) {
	s.handlePostTaskResult(w, r, "")
}

func (s *Server) handlePostTaskResult(w http.ResponseWriter, r *http.Request, workspace string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var payload struct {
		Workspace    string `json:"workspace"`
		Verb         string `json:"verb"`
		ExitCode     int    `json:"exit_code"`
		CommitHash   string `json:"commit_hash"`
		DurationMs   int64  `json:"duration_ms"`
		ErrorSummary string `json:"error_summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if payload.Verb == "" {
		http.Error(w, "verb is required", http.StatusBadRequest)
		return
	}
	// Prefer workspace from body; fall back to URL-based workspace for old clients.
	if payload.Workspace != "" {
		workspace = payload.Workspace
	}
	if workspace == "" {
		http.Error(w, "workspace is required", http.StatusBadRequest)
		return
	}

	result := &models.TaskResult{
		ExitCode:     payload.ExitCode,
		CommitHash:   payload.CommitHash,
		DurationMs:   payload.DurationMs,
		Timestamp:    time.Now(),
		ErrorSummary: payload.ErrorSummary,
	}

	s.engine.Store().ApplyUpdate(store.Update{
		Type:   store.UpdateTaskResult,
		Source: "cli",
		Payload: &store.TaskResultPayload{
			Workspace: workspace,
			Verb:      payload.Verb,
			Result:    result,
		},
	})

	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

func (s *Server) handlePostTestReport(w http.ResponseWriter, r *http.Request, workspace string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var payload struct {
		Workspace string `json:"workspace"`
		models.TestReport
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	report := payload.TestReport
	if payload.Workspace != "" {
		workspace = payload.Workspace
	}
	if report.Verb == "" {
		http.Error(w, "verb is required", http.StatusBadRequest)
		return
	}
	if workspace == "" {
		http.Error(w, "workspace is required", http.StatusBadRequest)
		return
	}
	report.Timestamp = time.Now()

	s.engine.Store().ApplyUpdate(store.Update{
		Type:   store.UpdateTestReport,
		Source: "cli",
		Payload: &store.TestReportPayload{
			Workspace: workspace,
			Report:    &report,
		},
	})

	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// handleGetPlans returns the cached list of fully-parsed plans for a
// given plansDir as JSON. The browser TUI uses this to avoid scanning
// every plan's yaml frontmatter on its own refresh tick.
func (s *Server) handleGetPlanIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.engine.Store().GetPlanIndexSnapshot())
}

func (s *Server) handleGetPlans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		http.Error(w, "dir parameter required", http.StatusBadRequest)
		return
	}
	plans := s.engine.Store().GetPlans(dir)
	if plans == nil {
		plans = []*orchestration.Plan{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(plans)
}

// handleSessions handles GET for all sessions (path: /api/sessions).
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessions := s.engine.Store().GetSessions()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessions)
}

// handleSatellites handles GET for the satellite connection-health map
// (path: /api/satellites, P10 / M2 contract C17). The global daemon's
// ConnManager populates the Store; scoped/satellite-less daemons return an
// empty object. The store's SatelliteStatusPayload shares JSON tags with
// models.SatelliteStatus, so it serializes straight through. This is a
// laptop-side read surface only — C3's direction invariant is unaffected.
func (s *Server) handleSatellites(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	statuses := s.engine.Store().GetSatelliteStatuses()
	if statuses == nil {
		statuses = map[string]*store.SatelliteStatusPayload{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(statuses)
}

// handleSatellitesReload handles POST /api/satellites/reload: re-run the
// registry load from disk (config ∪ state file) and diff-apply it to the
// ConnManager, so `grove satellite up`/`down` take effect without a daemon
// restart. The response is the ReloadSummary (added/removed/changed/unchanged
// by name). 4xx on scoped daemons and when satellites are disabled (the boot
// registry load errored, so no ConnManager exists to reload into); a
// load-from-disk failure at reload time is a 500 — the live connections are
// left untouched in that case.
func (s *Server) handleSatellitesReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		http.Error(w, "satellite registry reload is global-daemon-only (scoped daemons have no ConnManager)", http.StatusBadRequest)
		return
	}
	fnPtr := s.satelliteReloadFn.Load()
	if fnPtr == nil {
		http.Error(w, "satellites disabled on this daemon (registry failed to load at boot; fix the config and restart groved)", http.StatusConflict)
		return
	}
	summary, err := (*fnPtr)()
	if err != nil {
		http.Error(w, "reload satellite registry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summary)
}

// handleSessionByID handles session-specific operations (path: /api/sessions/{id}/*).
func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	// Parse the session ID and optional action from path
	// Paths: /api/sessions/{id}, /api/sessions/{id}/status, /api/sessions/{id}/end
	path := r.URL.Path[len("/api/sessions/"):]
	parts := splitPath(path)
	if len(parts) == 0 {
		http.Error(w, "session ID required", http.StatusBadRequest)
		return
	}

	sessionID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method == http.MethodDelete {
			// DELETE /api/sessions/{id} — kill the session by sending
			// SIGTERM to its tracked PID and removing the filesystem
			// registry entry. The daemon's normal session collector
			// will pick up the dead PID on its next sweep and emit
			// the appropriate SSE update.
			if err := s.killSession(sessionID); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "killed"})
			return
		}
		if r.Method == http.MethodPatch {
			// PATCH /api/sessions/{id} — partial update (tmux_target, last_sender)
			var req models.SessionPatchRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			if req.TmuxTarget != "" {
				s.engine.Store().ApplyUpdate(store.Update{
					Type:   store.UpdateSessionTmuxTarget,
					Source: "api",
					Payload: &store.SessionTmuxTargetPayload{
						JobID:      sessionID,
						TmuxTarget: req.TmuxTarget,
					},
				})
				// Persist to filesystem registry for restart resilience
				if registry, err := sessions.NewFileSystemRegistry(); err == nil {
					session := s.engine.Store().GetSession(sessionID)
					if session != nil {
						dirName := registryDirectoryForSession(session)
						_ = registry.UpdateFields(dirName, func(m *sessions.SessionMetadata) {
							if registryAttemptMatches(m.AttemptID, session.AttemptID) {
								m.TmuxTarget = req.TmuxTarget
							}
						})
					}
				}
			}
			if req.LastSender != "" || req.LastSenderGroup != "" {
				s.engine.Store().ApplyUpdate(store.Update{
					Type:   store.UpdateSessionLastSender,
					Source: "api",
					Payload: &store.SessionLastSenderPayload{
						JobID:           sessionID,
						LastSender:      req.LastSender,
						LastSenderGroup: req.LastSenderGroup,
					},
				})
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
			return
		}
		// GET /api/sessions/{id} - get single session
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		session := s.engine.Store().GetSession(sessionID)
		if session == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(session)

	case "status":
		// PATCH /api/sessions/{id}/status - update status
		if r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			AttemptID string `json:"attempt_id,omitempty"`
			Status    string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateSessionStatus,
			Source: "api",
			Payload: &store.SessionStatusPayload{
				JobID:     sessionID,
				AttemptID: req.AttemptID,
				Status:    req.Status,
			},
		})

		// Write-through to filesystem crash-recovery metadata so status
		// survives daemon restarts (e.g., "idle" is preserved, not lost).
		if registry, err := sessions.NewFileSystemRegistry(); err == nil {
			session := s.engine.Store().GetSession(sessionID)
			if session != nil && lifecycleRequestMatches(session.AttemptID, req.AttemptID) {
				dirName := registryDirectoryForSession(session)
				_ = registry.UpdateStatus(dirName, req.Status)
			}
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})

	case "end":
		// POST /api/sessions/{id}/end - end session
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			AttemptID string `json:"attempt_id,omitempty"`
			Outcome   string `json:"outcome"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateSessionEnd,
			Source: "api",
			Payload: &store.SessionEndPayload{
				JobID:     sessionID,
				AttemptID: req.AttemptID,
				Outcome:   req.Outcome,
				Reason:    "api_end",
			},
		})
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ended"})

	case "channels":
		// POST /api/sessions/{id}/channels — enable/disable channels
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req models.SessionChannelsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateSessionChannels,
			Source: "api",
			Payload: &store.SessionChannelsPayload{
				JobID:        sessionID,
				Channels:     req.Channels,
				SignalTarget: req.SignalTarget,
			},
		})
		// If channel manager is set, enable/disable channels
		if cm := s.channelManager.Load(); cm != nil {
			if len(req.Channels) > 0 {
				if err := cm.EnableChannel(r.Context(), sessionID, req.Channels...); err != nil {
					s.ulog.Error("Failed to enable channel").Err(err).Log(r.Context())
				}
			} else {
				cm.DisableChannel(r.Context(), sessionID)
			}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})

	case "autonomous":
		// POST /api/sessions/{id}/autonomous — enable/disable autonomous pinger
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req models.SessionAutonomousRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateSessionAutonomous,
			Source: "api",
			Payload: &store.SessionAutonomousPayload{
				JobID: sessionID,
				Autonomous: &models.AutonomousConfig{
					Enabled:     req.Enabled,
					IdleMinutes: req.IdleMinutes,
					Prompt:      req.Prompt,
				},
			},
		})
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})

	case "input":
		// POST /api/sessions/{id}/input — send input text to an interactive agent
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if err := s.SendSessionInput(r.Context(), sessionID, req.Input); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "sent"})

	case "interrupt":
		// POST /api/sessions/{id}/interrupt — send Ctrl+C to interrupt an agent
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := s.SendSessionInterrupt(r.Context(), sessionID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "interrupted"})

	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
}

// lookupRegistryPID consults the global crash-recovery registry for a session's
// confirmed metadata (chiefly its real PID and native session ID). It is the
// bridge that lets a daemon act on a session it did not itself launch — the
// registry is global while session stores are per-scope. Returns nil when no
// entry exists or the registry is unavailable.
func lifecycleRequestMatches(current, incoming string) bool {
	return current == "" || current == incoming
}

func registryAttemptMatches(current, incoming string) bool {
	return current == incoming
}

func registryDirectoryForSession(session *models.Session) string {
	if session == nil {
		return ""
	}
	if session.AttemptID != "" {
		return session.AttemptID
	}
	if session.ClaudeSessionID != "" {
		return session.ClaudeSessionID
	}
	return session.ID
}

func lookupRegistryPID(jobID, attemptID string) *sessions.SessionMetadata {
	registry, err := sessions.NewFileSystemRegistry()
	if err != nil {
		return nil
	}
	key := jobID
	if attemptID != "" {
		key = attemptID
	}
	md, err := registry.Find(key)
	if err != nil {
		return nil
	}
	// New-format rows never broad-fallback from a point lookup and retain an
	// exact field check as armor against corrupt or legacy alias collisions.
	if attemptID != "" && (md.AttemptID != attemptID || md.JobID != jobID) {
		return nil
	}
	return md
}

// killSession terminates a tracked session by sending SIGTERM to its
// recorded PID, removes the filesystem registry entry, and applies a
// session-end update to the in-memory store so SSE subscribers learn
// about the termination immediately. The actual session-collector sweep
// will reconcile the dead PID on its next pass.
//
// Returns an error if the session is unknown or has no PID. Killing a
// process that has already exited is treated as success — the goal is to
// guarantee the session disappears from active tracking.
func (s *Server) killSession(sessionID string) error {
	session := s.engine.Store().GetSession(sessionID)
	if session == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Never signal a PID off a federated session (C8). GetSession is a bare-ID
	// lookup so it already misses composite-keyed remote rows, but a satellite
	// job could share a bare ID with... nothing local — still, guard explicitly:
	// remote control is laptop→satellite only (dispatch, P9); killing here would
	// SIGTERM an unrelated local PID. Proxying to the remote is deferred.
	if session.Origin != "" {
		return fmt.Errorf("session %s belongs to satellite %q; remote control is not supported (dispatch is laptop→satellite only)", sessionID, session.Origin)
	}
	if session.Synthetic {
		return fmt.Errorf("session %s is synthetic (%s) and has no real process intent to end", sessionID, session.Provenance)
	}

	// Resolve the PID to signal. This daemon's record may carry PID 0 when the
	// session was synthesized by the filesystem job-watcher and the agent was
	// actually confirmed against a different scoped daemon. In that case the
	// real PID lives in the global crash-recovery registry, written at confirm
	// time. Without this fallback, killing such a session only closes the PTY
	// and leaves the agent process orphaned.
	pid := session.PID
	if pid <= 0 {
		if md := lookupRegistryPID(sessionID, session.AttemptID); md != nil {
			pid = md.PID
		}
	}

	if pid > 0 {
		// SIGTERM only — let the process clean up. ESRCH (process
		// already gone) is not an error from our perspective.
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("failed to signal pid %d: %w", pid, err)
		}
	}

	// Kill the out-of-process PTY so the treemux NativeAgentPanel gets EOF
	// and auto-closes its pane. Without this the pane stays open even after
	// the agent process exits, requiring a daemon restart to clear the row.
	if tc := s.tuimuxClient.Load(); session.PtyID != "" && tc != nil {
		if err := tc.KillPty(session.PtyID); err != nil {
			s.ulog.Warn("Failed to kill agent PTY on session kill").
				Err(err).
				Field("session_id", sessionID).
				Field("pty_id", session.PtyID).
				Log(context.Background())
		}
	}

	// Remove the crash-recovery state so a daemon restart won't re-resurrect
	// this session as alive. The record itself stays: metadata.json is the
	// index binding this job to its native session and transcript, and killing
	// a session is exactly when the transcript still needs resolving. The
	// directory is named after the native session ID (Claude UUID), falling
	// back to the job ID. When this daemon's record lacks the native ID
	// (filesystem-watcher synthesized record), recover it from the registry so
	// we act on the real directory rather than a non-existent job-ID-named one.
	dirName := registryDirectoryForSession(session)
	if registry, err := sessions.NewFileSystemRegistry(); err == nil {
		_, _ = registry.RemoveRecoveryFilesForJobInScope(sessionID, dirName, s.scope)
	}

	// Mark as interrupted in the in-memory store so SSE subscribers
	// (the embedded hooks panel, etc.) update immediately. The
	// session-collector sweep will eventually reconcile the dead PID,
	// but the eager update keeps the UI snappy.
	s.engine.Store().ApplyUpdate(store.Update{
		Type:   store.UpdateSessionEnd,
		Source: "api",
		Payload: &store.SessionEndPayload{
			JobID:     sessionID,
			AttemptID: session.AttemptID,
			Outcome:   "interrupted",
			Reason:    "api_kill",
		},
	})
	return nil
}

// resolveInputMode determines the input mode for a session's agent UI. The
// effective provider is the session's own recorded provider first (per-job
// providers can differ from the global config), then the workspace's
// flow.interactive_provider, then claude. The default comes from flow's
// agent provider registry (AgentProviderSpec.DefaultInputMode — the single
// declaration of per-provider input modes), overridable via
// [flow.providers.<name>].input_mode. Claude with no config stays "vim",
// the historical behavior.
func (s *Server) resolveInputMode(workDir, sessionProvider string) string {
	var flowCfg struct {
		InteractiveProvider string `toml:"interactive_provider" yaml:"interactive_provider"`
		Providers           map[string]struct {
			InputMode string `toml:"input_mode" yaml:"input_mode"`
		} `toml:"providers" yaml:"providers"`
	}
	if workDir != "" {
		if coreCfg, err := config.LoadFrom(workDir); err == nil {
			_ = coreCfg.UnmarshalExtension("flow", &flowCfg)
		}
	}

	providerName := sessionProvider
	if providerName == "" {
		providerName = flowCfg.InteractiveProvider
	}
	if providerName == "" {
		providerName = "claude"
	}

	inputMode := "vim" // historical default for unknown providers
	if spec, ok := orchestration.LookupAgentProvider(providerName); ok && spec.DefaultInputMode != "" {
		inputMode = spec.DefaultInputMode
	}
	if providerCfg, ok := flowCfg.Providers[providerName]; ok && providerCfg.InputMode != "" {
		inputMode = providerCfg.InputMode
	}
	return inputMode
}

// sessionFromDeliveryState constructs a minimal Session from the persisted
// channel delivery state (state.json). Used as a fallback when the in-memory
// store hasn't been populated yet (e.g., immediately after daemon restart).
func (s *Server) sessionFromDeliveryState(jobID string) *models.Session {
	if info := channels.GetSessionDelivery(jobID); info != nil {
		return &models.Session{
			ID:         jobID,
			Mux:        info.Mux,
			TmuxTarget: info.TmuxTarget,
			PtyID:      info.PtyID,
		}
	}
	return nil
}

// writePtyWithRetry writes bytes to an out-of-process PTY via the tuimux
// daemon. PTY ownership now lives in the standalone tuimux daemon, so this is
// a unix-socket HTTP hop rather than the old zero-hop in-process write. A
// single transient failure is retried once after a 50ms backoff; if it still
// fails the tuimux daemon is unreachable and the SSE fallback (which also
// depends on it) cannot save the session, so we mark the session interrupted
// (not failed — the agent was alive; the route died) and return the error.
func (s *Server) writePtyWithRetry(ctx context.Context, jobID, ptyID string, data []byte) error {
	tc := s.tuimuxClient.Load()
	if tc == nil {
		return fmt.Errorf("tuimux client unavailable")
	}
	err := tc.WritePty(ptyID, data)
	if err != nil {
		time.Sleep(50 * time.Millisecond)
		err = tc.WritePty(ptyID, data)
	}
	if err != nil {
		s.ulog.Warn("PTY write to tuimux daemon failed after retry; marking session interrupted").
			Err(err).
			Field("job_id", jobID).
			Field("pty_id", ptyID).
			Log(ctx)
		attemptID := ""
		if session := s.engine.Store().GetSession(jobID); session != nil {
			attemptID = session.AttemptID
		}
		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateSessionEnd,
			Source: "api",
			Payload: &store.SessionEndPayload{
				JobID:     jobID,
				AttemptID: attemptID,
				Outcome:   "interrupted",
			},
		})
		return fmt.Errorf("tuimux daemon unreachable for session %s: %w", jobID, err)
	}
	return nil
}

// effectiveMux returns the mux to use for routing input/interrupt to a
// session.
//
// A recorded PtyID outranks everything, including an explicit session.Mux: it
// is only ever set when this daemon itself created the PTY on the tuimux
// daemon, so it is a fact about a live route, while Mux is a label that
// history has proven can be wrong. A treemux/tuimux-hosted claw whose delivery
// record was stamped `tmux` by an older `flow agent claw` is exactly that case
// — routing it by the label ran tmux send-keys against a pane that never
// existed. Both out-of-process mux names ("treemux", "tuimux") mean the same
// direct-PTY tier; only a genuine tmux pane routes through send-keys.
// Otherwise we fall back to the legacy implicit inference (TmuxTarget→tmux) so
// pre-upgrade sessions still work until they end naturally.
func effectiveMux(session *models.Session) string {
	if session.PtyID != "" {
		return models.MuxTreemux
	}
	switch session.Mux {
	case models.MuxTreemux, models.MuxTuimux:
		return models.MuxTreemux
	case models.MuxTmux:
		return models.MuxTmux
	case models.MuxNone:
		return models.MuxNone
	}
	if session.TmuxTarget != "" {
		return models.MuxTmux
	}
	return models.MuxNone
}

// resolveSessionForRouting looks up a session by jobID and enriches it with
// persisted delivery state (mux/tmux target/PtyID) when the in-memory store
// session lacks muxing details. This is the shared lookup that input, interrupt
// and capture routing all depend on to reach out-of-process agent PTYs. Returns
// nil if no session can be resolved.
//
// Enrichment is a per-field backfill, never a wholesale overwrite: the store
// entry is the live record and the persisted one can be stale, so delivery
// state only ever fills a blank. When the store knows more than the persisted
// record does, the record is refreshed in place — that self-heals a
// channel-enabled session whose PTY was created (or re-adopted) after its
// delivery state was first written, which is what left inbound Signal messages
// routing to a synthesized tmux target.
func (s *Server) resolveSessionForRouting(jobID string) *models.Session {
	session := s.engine.Store().GetSession(jobID)
	if session == nil {
		return s.sessionFromDeliveryState(jobID)
	}
	info := channels.GetSessionDelivery(jobID)
	if info == nil {
		return session
	}
	if session.PtyID == "" {
		session.PtyID = info.PtyID
	}
	if session.TmuxTarget == "" {
		session.TmuxTarget = info.TmuxTarget
	}
	if session.Mux == "" {
		session.Mux = info.Mux
	}
	channels.SyncSessionDelivery(jobID, session.Mux, session.TmuxTarget, session.PtyID)
	return session
}

// SendSessionInput routes raw input text to an interactive agent session.
// The mux (treemux vs tmux) is read from session.Mux, with a fallback to
// implicit inference for pre-existing sessions. For treemux, it prefers a
// direct PTY write and falls back to an SSE relay when groveterm is
// connected. For tmux, it uses agentstream.SendInput with the resolved
// per-provider input mode (vim wrapping for claude, plain text for others).
func (s *Server) SendSessionInput(ctx context.Context, jobID, rawInput string) error {
	if s.engine == nil {
		return fmt.Errorf("engine not initialized")
	}
	session := s.resolveSessionForRouting(jobID)
	if session == nil {
		return fmt.Errorf("session not found: %s", jobID)
	}
	// Refuse input routing to a federated session (C8): its PTY/tmux target is
	// satellite-side. Remote control is not supported in M2 (dispatch is
	// laptop→satellite only). Defense-in-depth — the bare-ID lookup already
	// misses composite-keyed remote rows.
	if session.Origin != "" {
		return fmt.Errorf("session %s belongs to satellite %q; remote input is not supported (dispatch is laptop→satellite only)", jobID, session.Origin)
	}

	inputMode := s.resolveInputMode(session.WorkingDirectory, session.Provider)
	payload := rawInput + "\r"
	if inputMode == "vim" {
		payload = "\x1bi" + rawInput + "\r"
	}

	mux := effectiveMux(session)

	switch mux {
	case models.MuxTreemux:
		// Tier 1: write to the out-of-process PTY on the tuimux daemon. This is
		// a unix-socket HTTP hop (was a zero-hop in-process write); on a single
		// transient failure we retry once, then mark the session interrupted —
		// because once the tuimux daemon is unreachable the SSE fallback (which
		// also depends on it) cannot recover the session either.
		if session.PtyID != "" && s.tuimuxClient.Load() != nil {
			if err := s.writePtyWithRetry(ctx, jobID, session.PtyID, []byte(payload)); err != nil {
				return err
			}
			s.ulog.Debug("Injected input into agent").
				Field("job_id", jobID).
				Field("mux", mux).
				Field("tier", "direct_pty").
				Field("input_len", len(payload)).
				Log(ctx)
			return nil
		}
		// Tier 2: SSE relay to groveterm
		if s.terminalHub != nil && s.terminalHub.HasConnections() {
			s.engine.Store().ApplyUpdate(store.Update{
				Type:   store.UpdateAgentInput,
				Source: "api",
				Payload: &store.AgentInputPayload{
					JobID: jobID,
					Input: payload,
				},
			})
			s.ulog.Debug("Injected input into agent").
				Field("job_id", jobID).
				Field("mux", mux).
				Field("tier", "sse_relay").
				Field("input_len", len(payload)).
				Log(ctx)
			return nil
		}
		return fmt.Errorf("treemux route unavailable for session %s (no live PTY, no connected terminal)", jobID)

	case models.MuxTmux:
		engine, socket, err := resolveTmuxRoute(ctx, session)
		if err != nil {
			return err
		}
		if err := agentstream.SendInput(ctx, session.TmuxTarget, rawInput,
			agentstream.WithInputMode(inputMode), agentstream.WithEngine(engine)); err != nil {
			return err
		}
		s.ulog.Debug("Injected input into agent").
			Field("job_id", jobID).
			Field("mux", mux).
			Field("tier", "tmux").
			Field("tmux_target", session.TmuxTarget).
			Field("tmux_socket", socket).
			Field("input_len", len(rawInput)).
			Log(ctx)
		return nil

	default:
		return fmt.Errorf("unknown or missing mux for session %s", jobID)
	}
}

// SendSessionInterrupt routes a SIGINT (Ctrl-C) signal to an interactive
// agent session. Mirrors SendSessionInput's mux-based dispatch: direct PTY
// write of \x03 for treemux, SSE relay fallback, or tmux send-keys C-c.
func (s *Server) SendSessionInterrupt(ctx context.Context, jobID string) error {
	if s.engine == nil {
		return fmt.Errorf("engine not initialized")
	}
	session := s.resolveSessionForRouting(jobID)
	if session == nil {
		return fmt.Errorf("session not found: %s", jobID)
	}
	// Refuse interrupt routing to a federated session (C8): the signal would land
	// on a satellite-side agent we do not control from here. Remote control is
	// laptop→satellite only (dispatch, P9). Defense-in-depth on top of the
	// bare-ID lookup missing composite-keyed remote rows.
	if session.Origin != "" {
		return fmt.Errorf("session %s belongs to satellite %q; remote interrupt is not supported (dispatch is laptop→satellite only)", jobID, session.Origin)
	}

	mux := effectiveMux(session)

	switch mux {
	case models.MuxTreemux:
		if session.PtyID != "" && s.tuimuxClient.Load() != nil {
			if err := s.writePtyWithRetry(ctx, jobID, session.PtyID, []byte{0x03}); err != nil {
				return err
			}
			s.ulog.Debug("Sent interrupt to agent").
				Field("job_id", jobID).
				Field("mux", mux).
				Field("tier", "direct_pty").
				Log(ctx)
			return nil
		}
		if s.terminalHub != nil && s.terminalHub.HasConnections() {
			s.engine.Store().ApplyUpdate(store.Update{
				Type:   store.UpdateAgentInput,
				Source: "api",
				Payload: &store.AgentInputPayload{
					JobID: jobID,
					Input: "\x03",
				},
			})
			s.ulog.Debug("Sent interrupt to agent").
				Field("job_id", jobID).
				Field("mux", mux).
				Field("tier", "sse_relay").
				Log(ctx)
			return nil
		}
		return fmt.Errorf("treemux route unavailable for interrupt on session %s", jobID)

	case models.MuxTmux:
		engine, socket, err := resolveTmuxRoute(ctx, session)
		if err != nil {
			return err
		}
		if err := engine.SendKeys(ctx, session.TmuxTarget, "C-c"); err != nil {
			return err
		}
		s.ulog.Debug("Sent interrupt to agent").
			Field("job_id", jobID).
			Field("mux", mux).
			Field("tier", "tmux").
			Field("tmux_target", session.TmuxTarget).
			Field("tmux_socket", socket).
			Log(ctx)
		return nil

	default:
		return fmt.Errorf("unknown or missing mux for session %s", jobID)
	}
}

// handleSessionIntent handles POST /api/sessions/intent - pre-register session intent.
func (s *Server) handleSessionIntent(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var intent store.SessionIntentPayload
	if err := json.NewDecoder(r.Body).Decode(&intent); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if ns := s.fixtureIntentNamespace(&intent); ns != "" {
		s.ulog.Warn("Refused session intent from a test fixture").
			Field("job_id", intent.JobID).
			Field("title", intent.Title).
			Field("fixture_namespace", ns).
			Field("work_dir", intent.WorkDir).
			Log(r.Context())
		http.Error(w, "session belongs to test fixture namespace "+ns, http.StatusForbidden)
		return
	}

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateSessionIntent,
		Source:  "api",
		Payload: &intent,
	})

	// If the intent includes channels, enable them in the channel manager
	if cm := s.channelManager.Load(); cm != nil && len(intent.Channels) > 0 {
		if err := cm.EnableChannel(r.Context(), intent.JobID, intent.Channels...); err != nil {
			s.ulog.Warn("Failed to enable channel from intent").
				Err(err).
				Field("job_id", intent.JobID).
				Log(r.Context())
		}
	}

	s.ulog.Debug("Session intent registered").Field("job_id", intent.JobID).Log(r.Context())
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "registered", "job_id": intent.JobID})
}

// fixtureIntentNamespace reports the test-fixture namespace an intent belongs
// to, or "" when the intent is ordinary work this daemon should record.
//
// A REAL daemon must never adopt a session whose job lives inside a tend/lab/
// tui-pilot sandbox. Those jobs are fictional — mock binaries, temp-dir plans,
// processes that often never start — and nothing ever ends them, so each one
// parks in the Agents drawer forever ("Env Dump", "pi Headless E2E", jobs from
// /tmp/grove-tend-* that the user could not dismiss). The sandbox env is the
// primary defense (harness.HostIdentityEnv); this is the backstop for older
// harnesses and for any other route by which a fixture finds the real socket.
//
// A FIXTURE daemon keeps recording them: its own socket lives in the same
// namespace, and inside the sandbox those sessions are exactly what the
// scenario is asserting on.
func (s *Server) fixtureIntentNamespace(intent *store.SessionIntentPayload) string {
	if intent == nil || coredaemon.FixtureNamespace(s.socketPath) != "" {
		return ""
	}
	for _, p := range []string{intent.WorkDir, intent.JobFilePath} {
		if ns := coredaemon.FixtureNamespace(p); ns != "" {
			return ns
		}
	}
	return ""
}

// handleSessionConfirm handles POST /api/sessions/confirm - confirm session with PID.
func (s *Server) handleSessionConfirm(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var confirmation store.SessionConfirmationPayload
	if err := json.NewDecoder(r.Body).Decode(&confirmation); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateSessionConfirmation,
		Source:  "api",
		Payload: &confirmation,
	})

	// Persist only when the store accepted this exact (or legacy-empty)
	// lifecycle join. A late confirmation from attempt N must not upgrade the
	// projected row or registry record for attempt N+1.
	current := s.engine.Store().GetSession(confirmation.JobID)
	accepted := current != nil && lifecycleRequestMatches(current.AttemptID, confirmation.AttemptID)

	// Persist the confirmed PID to the GLOBAL filesystem registry
	// (~/.grove/hooks/sessions). Grove runs one daemon per scope, but the
	// daemon that later serves `flow agent list` / `flow plan finish` /
	// `flow agent kill` is often a DIFFERENT (cwd/global-scoped) daemon whose
	// session record was synthesized by the filesystem job-watcher and carries
	// PID 0. Without a shared on-disk PID, that daemon can neither SIGTERM the
	// agent at finish nor reap it when it dies — the orphaned-agent bug. The
	// registry is global, so writing it here lets any scoped daemon recover the
	// real PID via registry.Find(jobID). Best-effort: tracking degrades to the
	// previous behavior on error.
	if accepted {
		s.persistConfirmedSessionToRegistry(&confirmation)
	}

	s.ulog.Debug("Session confirmed").
		Field("job_id", confirmation.JobID).
		Field("pid", confirmation.PID).
		Log(r.Context())
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "confirmed"})
}

// persistConfirmedSessionToRegistry writes the confirmed session (with its real
// PID) to the global crash-recovery registry so that ANY scoped daemon — not
// just the one that ran the agent — can recover the PID to kill or reap it.
// Best-effort: a registry write failure must not fail confirmation.
func (s *Server) persistConfirmedSessionToRegistry(c *store.SessionConfirmationPayload) {
	if c == nil || c.PID <= 0 {
		return
	}
	registry, err := sessions.NewFileSystemRegistry()
	if err != nil {
		return
	}

	md := sessions.SessionMetadata{
		SessionID:       c.JobID,
		AttemptID:       c.AttemptID,
		JobID:           c.JobID,
		ClaudeSessionID: c.NativeID,
		PID:             c.PID,
		TranscriptPath:  c.TranscriptPath,
		Provider:        "claude",
		Status:          "running",
		StartedAt:       time.Now(),
		User:            os.Getenv("USER"),
		// Stamp the owning scope so only this daemon's scope adopts/reaps the
		// session on recovery. Empty == unscoped/global.
		Scope: s.scope,
	}
	// Enrich from the in-memory session record when available (plan, workdir,
	// title, pty, mux) so a daemon restart's crash recovery restores full state.
	if sess := s.engine.Store().GetSession(c.JobID); sess != nil && lifecycleRequestMatches(sess.AttemptID, c.AttemptID) {
		md.AttemptID = sess.AttemptID
		md.StartedAt = sess.StartedAt
		md.PlanName = sess.PlanName
		md.JobTitle = sess.JobTitle
		md.ParentJobID = sess.ParentJobID
		md.WorkingDirectory = sess.WorkingDirectory
		md.JobFilePath = sess.JobFilePath
		md.PtyID = sess.PtyID
		md.Mux = sess.Mux
		md.Type = sess.Type
		if sess.Provider != "" {
			md.Provider = sess.Provider
		}
	}

	if err := registry.Register(md); err != nil {
		s.ulog.Debug("Failed to persist confirmed session to registry").
			Err(err).
			Field("job_id", c.JobID).
			Field("attempt_id", c.AttemptID).
			Log(context.Background())
	}
}

// splitPath splits a URL path by "/" and removes empty parts.
func splitPath(path string) []string {
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// handleStreamState provides Server-Sent Events (SSE) for real-time state updates.
// Clients can subscribe to this endpoint to receive updates whenever the daemon state changes.
//
// Query parameters (all optional, all additive — see stream_bus.go and
// coredaemon.StreamFilter). A subscribe with none of them gets the full
// host-wide stream exactly as it always did, so old clients are unaffected:
//
//	since=<seq>   resume after a sequence number the client already saw. An
//	              exact resume replays the ring and SKIPS the initial snapshot
//	              (the client is continuous). A gapped resume emits a
//	              "stream_gap" control frame plus the snapshot instead, and
//	              replays nothing: the retained tail is older than the snapshot,
//	              so replaying it on top would move the client backwards.
//	types=<globs> comma-separated globs matched against update_type. Control
//	              frames ("stream_gap") always pass; the "initial" snapshot is
//	              ordinary state and IS filtered.
//	paths=<list>  comma-separated workspace path prefixes, applied to the
//	              workspace-bearing frames only. An event whose rows are all
//	              dropped is not written at all.
//
// Both allow-lists are enforced here, before serialization and before the
// write, so a filtered subscriber never pays for the JSON of an event it
// declared no interest in.
func (s *Server) handleStreamState(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	// Ensure the connection supports flushing
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	query := r.URL.Query()
	since, wantsReplay, err := parseSinceCursor(query.Get("since"))
	if err != nil {
		http.Error(w, "invalid since cursor: must be a decimal sequence number", http.StatusBadRequest)
		return
	}
	// ?types= is owned by the glob filter and ?paths= by the StreamFilter. The
	// two grew independently and both claimed types=; globs are the superset (a
	// literal pattern matches exactly, which is what an exact allow-list meant),
	// so the type decision lives in one place and the StreamFilter carries only
	// the path allow-list. Leaving its Types populated would filter twice, with
	// the stricter exact matcher silently overriding a caller's glob.
	types, err := parseTypeFilter(query.Get("types"))
	if err != nil {
		http.Error(w, "invalid types filter: "+err.Error(), http.StatusBadRequest)
		return
	}
	filter := coredaemon.StreamFilter{Paths: coredaemon.ParseStreamFilter(query).Paths}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Advertise the hardened contract. A client that does not see these has
	// reached an older daemon, which ignored since/types and emits no seq.
	w.Header().Set(StreamFeaturesHeader, StreamFeatures)
	w.Header().Set(StreamRingHeader, strconv.Itoa(store.RingSize))

	// A live state stream is a real daemon client even though it is not a
	// terminal WebSocket. Keep auto-started scoped daemons alive for the full
	// HTTP stream lifetime.
	releaseLease := s.terminalHub.AcquireClientLease()
	defer releaseLease()

	// Live-subscriber gauges, read at snapshot time by collectCounters. Tracked
	// here rather than pushed into telemetry so the number cannot drift from
	// reality if a future early-return forgets to decrement.
	s.sseSubscribers.Add(1)
	defer s.sseSubscribers.Add(-1)
	if len(types) > 0 || !filter.IsZero() {
		s.sseSubscribersFiltered.Add(1)
		defer s.sseSubscribersFiltered.Add(-1)
	}

	// Subscribe BEFORE reading the ring or the snapshot. Anything published in
	// between then lands in both places; the lastSent watermark below drops the
	// duplicate. Subscribing after would lose it entirely.
	st := s.engine.Store()
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	// Send initial ping to confirm connection
	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// writeFrame emits one already-serialized frame in SSE form. The bytes go
	// straight to the connection rather than through fmt: a snapshot frame is
	// by far the largest thing this endpoint writes, and %s would copy all of
	// it into a formatting buffer on the way out.
	writeFrame := func(data []byte) {
		_, _ = io.WriteString(w, "data: ")
		_, _ = w.Write(data)
		_, _ = io.WriteString(w, "\n\n")
		flusher.Flush()
	}

	// send serializes a frame this subscriber owns — a control frame, or the
	// copy the path filter pruned for it alone — and writes it.
	send := func(update *apiStateUpdate) {
		data, err := json.Marshal(update)
		if err != nil {
			s.ulog.Error("Failed to marshal update").Err(err).Log(r.Context())
			return
		}
		writeFrame(data)
	}

	// deliver writes one converted live/replayed frame. When the path filter
	// pruned nothing it returns its input pointer unchanged, and that pointer
	// identity is the exact test for "these are the bytes every other
	// subscriber of this sequence gets" — so those go through the frame cache
	// and are marshalled once for all of them. A genuinely pruned copy is this
	// subscriber's alone and is serialized here.
	deliver := func(seq uint64, shared, sent *apiStateUpdate) {
		if sent != shared {
			send(sent)
			return
		}
		data, err := s.frames.marshal(seq, sent)
		if err != nil {
			s.ulog.Error("Failed to marshal update").Err(err).Log(r.Context())
			return
		}
		writeFrame(data)
	}

	var lastSent uint64
	replay, gap := []store.Update(nil), store.ReplayGap{}
	if wantsReplay {
		replay, gap = st.Replay(since)
		if gap.Gapped() {
			// Control frame: never filtered, and deliberately emitted before
			// the snapshot so a client reading frame-by-frame knows to treat
			// what follows as a reconcile rather than a delta.
			send(&apiStateUpdate{
				UpdateType: StreamGapUpdateType,
				Source:     "stream",
				Seq:        gap.Current,
				Payload: apiStreamGap{
					Reason:   gap.Reason,
					Since:    gap.Since,
					Oldest:   gap.Oldest,
					Current:  gap.Current,
					RingSize: store.RingSize,
				},
			})
		}
	}

	// Send current state immediately so client has data right away. The
	// snapshot also carries the current resolved theme so a theme change
	// that happened while the client was disconnected isn't lost. Carry the
	// current boot status on this initial frame too: a client that subscribes
	// after the pre-stream GetBootStatus poll would otherwise miss a boot that
	// finished in between, and never see a Done transition. Sent whenever boot
	// is still in progress, regardless of whether any workspaces exist yet.
	//
	// A client resuming exactly (?since= with no gap) skips this: it already
	// has the state, and the replay below carries it forward.
	//
	// So does a filtered subscriber that did not name "initial": building this
	// frame means marshalling every enriched workspace plus the plan index,
	// which is by far the largest thing this endpoint ever writes. Not
	// producing it is the main saving subscribe-time filtering buys.
	switch {
	case wantsReplay && !gap.Gapped():
		// Nothing to send: the replay below is continuous with what the client
		// already has.
	case !types.matches(coredaemon.StreamTypeInitial):
		telemetry.SSEInitialSkipped.Inc()
	default:
		// Built through the frame cache, keyed by (sequence, path filter). A
		// reconnect storm is many clients arriving inside a few hundred
		// milliseconds, and every one of them would otherwise build and marshal
		// its own copy of the whole enriched-workspace map. Keying on the
		// CURRENT sequence is what makes sharing safe: any store mutation
		// advances it, so a cache hit can only ever be a snapshot of exactly the
		// state this subscriber would have read for itself.
		seq := st.CurrentSeq()
		data, ok, err := s.frames.initial(initialFrameKey(seq, filter.Paths), func() ([]byte, bool, error) {
			state := st.Get()
			themePayload := theming.CurrentPayload()
			boot := s.bootStatus.Load()
			if len(state.Workspaces) == 0 && state.PlanIndex == nil && themePayload == nil && (boot == nil || boot.Done) {
				return nil, false, nil
			}
			workspaces := make([]*models.EnrichedWorkspace, 0, len(state.Workspaces))
			for _, ws := range state.Workspaces {
				workspaces = append(workspaces, ws)
			}
			initialUpdate := &apiStateUpdate{
				Workspaces:        workspaces,
				UpdateType:        coredaemon.StreamTypeInitial,
				Seq:               seq,
				Theme:             themePayload,
				BootPhase:         boot,
				PlanIndexSnapshot: state.PlanIndex,
			}
			sent, keep := applyStreamFilter(filter, initialUpdate)
			if !keep {
				return nil, false, nil
			}
			frame, err := json.Marshal(sent)
			return frame, true, err
		})
		switch {
		case err != nil:
			s.ulog.Error("Failed to marshal initial snapshot").Err(err).Log(r.Context())
		case ok:
			writeFrame(data)
			telemetry.SSEInitialSent.Inc()
		}
	}

	if !gap.Gapped() {
		for _, update := range replay {
			apiUpdate, converted := s.frames.convert(update)
			if !converted || !types.matches(apiUpdate.UpdateType) {
				// Still advance the watermark: the frame was delivered as far
				// as this subscription is concerned, and not advancing would
				// let the live loop re-offer it.
				lastSent = max(lastSent, update.Seq)
				continue
			}
			lastSent = update.Seq
			// The path allow-list applies to replayed frames too: a resume must
			// not hand a subscriber the workspaces it declared out of scope.
			sent, ok := applyStreamFilter(filter, apiUpdate)
			if !ok {
				continue
			}
			deliver(update.Seq, apiUpdate, sent)
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case update := <-ch:
			// Drop what the replay above already delivered on this connection.
			if update.Seq != 0 && update.Seq <= lastSent {
				continue
			}
			lastSent = update.Seq
			// Convert internal store.Update to public API format. Shared across
			// every subscriber that reaches this sequence — the conversion is
			// identical for all of them, and each one used to run it itself.
			apiUpdate, converted := s.frames.convert(update)
			if !converted {
				continue
			}
			telemetry.SSEEventsPublished.Inc()

			// Drop before serializing: a filtered subscriber must not pay for
			// the JSON of an event it declared no interest in.
			if !types.matches(apiUpdate.UpdateType) {
				telemetry.SSEEventsFiltered.Inc()
				continue
			}
			sent, ok := applyStreamFilter(filter, apiUpdate)
			if !ok {
				telemetry.SSEEventsFiltered.Inc()
				continue
			}

			deliver(update.Seq, apiUpdate, sent)
			telemetry.SSEEventsDelivered.Inc()
		}
	}
}

// handleStreamWorkspaceHUD provides Server-Sent Events (SSE) for per-workspace
// HUD state. The workspace path is read from the "path" query parameter.
func (s *Server) handleStreamWorkspaceHUD(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing required 'path' query parameter", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	hudWatcher := watcher.NewHUDWatcher(s.engine.Store(), path)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	out := hudWatcher.Watch(ctx)

	// Send initial ping to confirm the connection is alive.
	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case hud, ok := <-out:
			if !ok {
				return
			}
			data, err := json.Marshal(hud)
			if err != nil {
				s.ulog.Error("Failed to marshal HUD update").Err(err).Log(r.Context())
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// startupExeSig is the size+modtime signature of this daemon's own executable,
// captured at process start. A `groved upgrade` swaps the binary on disk; a
// rebuild overwrites it. Comparing the current on-disk signature against this
// baseline tells us whether the running daemon is stale (an upgrade is waiting)
// WITHOUT comparing commit hashes against an unrelated client repo.
var startupExeSig = currentExeSig()

func currentExeSig() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
}

// handleSystemInfo returns the daemon's version, commit, build date, and
// whether its on-disk binary has changed since startup (upgrade available).
// Reports staleness so the treemux HUD can show daemon version + upgrade badge.
func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	upgradeAvailable := false
	if startupExeSig != "" {
		if cur := currentExeSig(); cur != "" && cur != startupExeSig {
			upgradeAvailable = true
		}
	}
	info := models.SystemInfo{
		Version:          version.Version,
		Commit:           version.Commit,
		BuildDate:        version.BuildDate,
		UpgradeAvailable: upgradeAvailable,
		Scope:            s.scope,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// handleSystemBoot returns the daemon's boot progress. It intentionally does
// NOT depend on the engine or any late-wired dependency so it answers from the
// earliest moment the socket is serving. When bootStatus was never set — the
// default bind-last ordering, where the socket only serves after boot
// completes — it reports Done=true.
func (s *Server) handleSystemBoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := s.bootStatus.Load()
	if status == nil {
		status = &coredaemon.BootStatus{Done: true}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// checkJobSubmitWarnings detects unknown fields in a job submission request.
// It returns a list of warnings about fields that will be ignored.
func (s *Server) checkJobSubmitWarnings(bodyBytes []byte) []string {
	var warnings []string

	// Parse the raw JSON to get field names
	var rawReq map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &rawReq); err != nil {
		// If we can't parse raw JSON, skip warning generation
		return warnings
	}

	// Known fields in JobSubmitRequest struct
	knownFields := map[string]bool{
		"plan_dir":     true,
		"job_file":     true,
		"priority":     true,
		"timeout":      true,
		"env":          true,
		"agent_target": true,
		"satellite":    true, // M2 C11: laptop-side satellite routing field
		"plan_bundle":  true, // M2 C11: shipped plan files for satellite materialize
	}

	// Check for unknown fields
	for fieldName := range rawReq {
		if !knownFields[fieldName] {
			warnings = append(warnings, fmt.Sprintf("unknown field: %s (will be ignored)", fieldName))
		}
	}

	return warnings
}

// handleTerminalStatus returns whether a groveterm instance is connected via WebSocket.
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	connected := s.terminalHub != nil && s.terminalHub.HasConnections()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"connected":%t}`, connected)
}

// writeAgentWrapper writes a one-shot wrapper script that re-execs the given
// interactive shell with `-i -c <script>`. The tuimux ApiClient.CreatePty only
// accepts a single command token (no Args), so this wrapper is how we preserve
// the original `<shell> -i -c <script>` invocation (RC sourcing + env exports)
// out-of-process. The wrapper removes itself before exec so it never lingers.
// It returns the absolute path to the executable wrapper.
func writeAgentWrapper(shell, script string) (string, error) {
	f, err := os.CreateTemp("", "grove-agent-*.sh")
	if err != nil {
		return "", err
	}
	path := f.Name()
	// rm the wrapper, then exec the interactive shell with the agent script.
	// Single-quote the script and escape embedded single quotes so the outer
	// `-c` argument is one literal token.
	escapedScript := strings.ReplaceAll(script, "'", "'\\''")
	content := fmt.Sprintf("#!/bin/sh\nrm -f '%s'\nexec '%s' -i -c '%s'\n", path, shell, escapedScript)
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// handleAgentSpawn handles POST /api/agents/spawn — creates a daemon-owned PTY
// for the agent process and sends an attach event to groveterm via SSE.
func (s *Server) handleAgentSpawn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var payload store.SpawnAgentPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// If the tuimux client is available, create an out-of-process PTY for the
	// agent on the standalone tuimux daemon and send an attach event instead
	// of a spawn event. Owning the PTY out-of-process is what lets the agent
	// process and its pane survive a `groved upgrade`.
	if tc := s.tuimuxClient.Load(); tc != nil && payload.Command != "" {
		// Wrap the agent command in an interactive shell so the user's RC
		// files are sourced (PATH includes nvm, homebrew, etc.). Export
		// env vars inside the script to ensure they survive shell init.
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}

		var script strings.Builder
		// GROVE_SCOPE inherits naturally from the daemon's environment
		// (groved sets os.Environ at startup), so no explicit export needed.
		for k, v := range payload.Env {
			escapedVal := strings.ReplaceAll(v, "'", "'\\''")
			script.WriteString(fmt.Sprintf("export %s='%s'; ", k, escapedVal))
		}
		script.WriteString(payload.Command)
		for _, arg := range payload.Args {
			escapedArg := strings.ReplaceAll(arg, "'", "'\\''")
			script.WriteString(fmt.Sprintf(" '%s'", escapedArg))
		}

		// The tuimux ApiClient.CreatePty takes a single command token (no
		// Args), so reproduce the original `<shell> -i -c <script>` invocation
		// via a tiny self-deleting wrapper script that the tuimux daemon execs
		// directly. The wrapper re-execs the interactive shell with the exact
		// same args, preserving RC sourcing and env-export behavior. It removes
		// itself before exec so no temp file lingers past spawn.
		wrapper, werr := writeAgentWrapper(shell, script.String())
		if werr != nil {
			s.ulog.Error("Failed to write agent PTY wrapper").Err(werr).Log(r.Context())
			http.Error(w, "failed to prepare agent PTY: "+werr.Error(), http.StatusInternalServerError)
			return
		}

		attemptID := payload.Env["GROVE_FLOW_ATTEMPT_ID"]
		createPty := func(c *tuimux.ApiClient) (string, error) {
			return c.CreatePty(
				wrapper,
				payload.WorkDir,
				[]string{"GROVE_PTY=1", "GROVE_TERMINAL=1"},
				40, 120,
				map[string]string{
					"job_id":     payload.JobID,
					"attempt_id": attemptID,
					"plan_name":  payload.PlanName,
					"type":       "agent",
					"origin":     "agent:" + payload.JobID,
					"label":      payload.JobTitle,
					"created_by": "flow",
				},
			)
		}

		ptyID, err := createPty(tc)
		if err != nil {
			// The tuimux client is stale — its paired daemon likely died out
			// from under us (e.g. crashed after boot). Re-ensure the daemon
			// once and retry the same wrapper before giving up. On any
			// unrecoverable failure we degrade to the legacy groveterm relay
			// fallback below rather than hard-failing every spawn for the life
			// of this groved.
			s.ulog.Warn("Failed to create agent PTY session; re-ensuring paired tuimux daemon").
				Err(err).Log(r.Context())
			if p := s.tuimuxReEnsure.Load(); p != nil {
				newClient, rerr := (*p)()
				if rerr != nil || newClient == nil {
					s.ulog.Error("tuimux re-ensure failed during agent spawn").
						Err(rerr).Log(r.Context())
				} else {
					s.tuimuxClient.Store(newClient)
					ptyID, err = createPty(newClient)
					if err != nil {
						s.ulog.Error("Failed to create agent PTY session after tuimux re-ensure").
							Err(err).Log(r.Context())
					} else {
						s.ulog.Info("Re-ensured paired tuimux daemon and recreated agent PTY").
							Log(r.Context())
					}
				}
			}
			if err != nil {
				_ = os.Remove(wrapper)
				// Fall through to the legacy relay fallback below.
			}
		}

		if err == nil {
			// Update the session registry with the PTY ID so re-attachment works.
			if st := s.engine.Store(); st != nil {
				st.SetSessionPtyID(payload.JobID, attemptID, ptyID)
			}

			// Persist PtyID to filesystem registry for restart resilience.
			if reg, err := sessions.NewFileSystemRegistry(); err == nil {
				session := s.engine.Store().GetSession(payload.JobID)
				if session != nil && lifecycleRequestMatches(session.AttemptID, attemptID) {
					dirName := registryDirectoryForSession(session)
					_ = reg.UpdateFields(dirName, func(m *sessions.SessionMetadata) {
						if registryAttemptMatches(m.AttemptID, attemptID) {
							m.PtyID = ptyID
						}
					})
				}
			}

			// Send attach event to groveterm via SSE.
			attachPayload := &store.AttachAgentPayload{
				JobID:     payload.JobID,
				PlanName:  payload.PlanName,
				JobTitle:  payload.JobTitle,
				PtyID:     ptyID,
				WorkDir:   payload.WorkDir,
				Env:       payload.Env,
				AutoSplit: payload.AutoSplit,
			}
			s.engine.Store().ApplyUpdate(store.Update{
				Type:    store.UpdateAttachAgentPane,
				Source:  "api",
				Payload: attachPayload,
			})

			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "attached", "pty_id": ptyID})
			return
		}
	}

	// Fallback: relay spawn request to groveterm (legacy path).
	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateSpawnAgentPane,
		Source:  "api",
		Payload: &payload,
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "spawned"})
}

// handleAgentByID routes /api/agents/{id}/* actions (input, capture,
// capture_response, final_output).
func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	path := r.URL.Path[len("/api/agents/"):]
	parts := splitPath(path)
	if len(parts) < 2 {
		http.Error(w, "agent ID and action required", http.StatusBadRequest)
		return
	}

	agentID := parts[0]
	action := parts[1]

	switch action {
	case "input":
		// POST /api/agents/{id}/input — relay input to groveterm via SSE
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateAgentInput,
			Source: "api",
			Payload: &store.AgentInputPayload{
				JobID: agentID,
				Input: req.Input,
			},
		})

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "sent"})

	case "capture":
		// GET /api/agents/{id}/capture — blocking request that waits for groveterm's response
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Tier 1: native PTY capture. Out-of-process agent PTYs have no
		// addressable tmux/server name, so address them by PtyID (mux-agnostic),
		// symmetric with the direct-PTY input path. On error, fall through to the
		// groveterm SSE round-trip below.
		var nativeErr error
		tc := s.tuimuxClient.Load()
		if session := s.resolveSessionForRouting(agentID); session != nil && session.PtyID != "" && tc != nil {
			screen, err := tc.CapturePty(session.PtyID)
			if err == nil {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, screen)
				return
			}
			nativeErr = err
			s.ulog.Warn("Native CapturePty failed, falling back to groveterm SSE").
				Err(err).
				Field("job_id", agentID).
				Field("pty_id", session.PtyID).
				Log(r.Context())
		}

		// Tier 2: groveterm SSE round-trip. This only works when a groveterm
		// terminal is actually connected to relay the capture; without one the
		// waiter can only ever time out. Skip it (mirroring the input path's
		// HasConnections gate) and surface the native error instead, so headless
		// callers fail fast with the real cause rather than a misleading 5s
		// "groveterm did not respond" timeout.
		if s.terminalHub == nil || !s.terminalHub.HasConnections() {
			if s.serveRetainedFinalOutput(w, r, agentID, "no_terminal_connected") {
				return
			}
			if nativeErr != nil {
				http.Error(w, "capture failed: "+nativeErr.Error(), http.StatusBadGateway)
			} else {
				http.Error(w, "capture unavailable: no PTY for native capture and no terminal connected", http.StatusServiceUnavailable)
			}
			return
		}

		ch := make(chan string, 1)

		s.captureWaitersMu.Lock()
		s.captureWaiters[agentID] = ch
		s.captureWaitersMu.Unlock()

		// Broadcast capture request to groveterm via SSE
		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateCaptureRequest,
			Source: "api",
			Payload: &store.CaptureRequestPayload{
				JobID: agentID,
			},
		})

		// Block until groveterm responds or timeout
		select {
		case text := <-ch:
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, text)
		case <-time.After(5 * time.Second):
			s.captureWaitersMu.Lock()
			delete(s.captureWaiters, agentID)
			s.captureWaitersMu.Unlock()
			// A terminal is connected but no panel answered — the usual shape
			// of an agent that already exited and unmounted its pane.
			if s.serveRetainedFinalOutput(w, r, agentID, "sse_timeout") {
				return
			}
			http.Error(w, "capture timeout: groveterm did not respond", http.StatusGatewayTimeout)
		case <-r.Context().Done():
			s.captureWaitersMu.Lock()
			delete(s.captureWaiters, agentID)
			s.captureWaitersMu.Unlock()
			return
		}

	case "capture_response":
		// POST /api/agents/{id}/capture_response — groveterm sends back screen text
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		s.captureWaitersMu.Lock()
		ch, ok := s.captureWaiters[agentID]
		if ok {
			delete(s.captureWaiters, agentID)
		}
		s.captureWaitersMu.Unlock()

		if ok {
			ch <- string(body)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "received"})

	case "final_output":
		// POST /api/agents/{id}/final_output — groveterm pushes an agent
		// pane's last screen text at the instant its PTY session exits, while
		// the terminal still holds the scrollback. Unsolicited: no waiter is
		// pending, this is purely retained for a capture that arrives later.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, finalOutputMaxBytes))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		s.finalOutputs.Put(agentID, string(body))
		s.ulog.Debug("Retained agent final output").
			Field("job_id", agentID).
			Field("bytes", len(body)).
			Log(r.Context())

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "retained"})

	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
}

// serveRetainedFinalOutput answers a capture request from the death-rattle
// snapshot pushed when the agent's PTY exited. It is the last tier: both live
// tiers require a process and a pane, and once those are gone the terminal
// text explaining the exit exists nowhere else. Reports whether it responded.
func (s *Server) serveRetainedFinalOutput(w http.ResponseWriter, r *http.Request, agentID, reason string) bool {
	if s.finalOutputs == nil {
		return false
	}
	text, ok := s.finalOutputs.Get(agentID)
	if !ok {
		return false
	}

	s.ulog.Info("Serving retained final output for capture").
		Field("job_id", agentID).
		Field("reason", reason).
		Field("bytes", len(text)).
		Log(r.Context())

	w.Header().Set("Content-Type", "text/plain")
	// Callers (and log readers) should be able to tell a post-mortem snapshot
	// from a live screen without diffing content.
	w.Header().Set("X-Grove-Capture-Source", "final_output")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, text)
	return true
}

// apiStateUpdate matches the daemon.StateUpdate type for SSE streaming.
type apiStateUpdate struct {
	Workspaces      []*models.EnrichedWorkspace `json:"workspaces,omitempty"`
	WorkspaceDeltas []*models.WorkspaceDelta    `json:"workspace_deltas,omitempty"`
	Sessions        []*models.Session           `json:"sessions,omitempty"`
	UpdateType      string                      `json:"update_type"`
	// Seq is the store's monotonic sequence for this update — the cursor a
	// client passes back as ?since=. Zero (omitted) on control frames the
	// store never published, and on every frame from a pre-hardening daemon.
	Seq        uint64      `json:"seq,omitempty"`
	Source     string      `json:"source,omitempty"`
	Scanned    int         `json:"scanned,omitempty"`
	ConfigFile string      `json:"config_file,omitempty"`
	Payload    interface{} `json:"payload,omitempty"`
	// Theme stamps the current resolved theme onto the "initial" snapshot so
	// clients reconnecting after a disconnect never miss a theme change.
	Theme *coredaemon.ThemeChangedPayload `json:"theme,omitempty"`
	// BootPhase mirrors StateUpdate.BootPhase so "boot_phase" SSE events carry
	// the typed status in its own field, not the generic Payload. Nil on every
	// other update type (omitempty keeps it off the wire).
	BootPhase         *coredaemon.BootStatus    `json:"boot_phase,omitempty"`
	PlanIndex         *models.PlanIndexDelta    `json:"plan_index,omitempty"`
	PlanIndexSnapshot *models.PlanIndexSnapshot `json:"plan_index_snapshot,omitempty"`
}

// convertToAPIUpdate converts internal store.Update to the public API format,
// stamping the store's sequence number onto the frame. A nil result means the
// update does not reach the wire; that outcome is logged (once per type) by
// noteUnconvertedUpdate rather than being silently swallowed, and intentional
// omissions are declared in apiUpdateSkipList.
func convertToAPIUpdate(u store.Update) *apiStateUpdate {
	out := convertUpdatePayload(u)
	if out == nil {
		noteUnconvertedUpdate(u)
		return nil
	}
	out.Seq = u.Seq
	return out
}

// convertUpdatePayload maps one store update onto its wire shape, or nil when
// there is no mapping. Callers want convertToAPIUpdate.
func convertUpdatePayload(u store.Update) *apiStateUpdate {
	switch u.Type {
	case store.UpdateWorkspaces:
		if wsMap, ok := u.Payload.(map[string]*models.EnrichedWorkspace); ok {
			workspaces := make([]*models.EnrichedWorkspace, 0, len(wsMap))
			for _, ws := range wsMap {
				workspaces = append(workspaces, ws)
			}
			return &apiStateUpdate{
				Workspaces: workspaces,
				UpdateType: "workspaces",
				Source:     u.Source,
				Scanned:    u.Scanned,
			}
		}
	case store.UpdateWorkspacesDelta:
		if deltas, ok := u.Payload.([]*models.WorkspaceDelta); ok {
			return &apiStateUpdate{
				WorkspaceDeltas: deltas,
				UpdateType:      "workspaces_delta",
				Source:          u.Source,
				Scanned:         len(deltas),
			}
		}
	case store.UpdateSessions:
		if sessions, ok := u.Payload.([]*models.Session); ok {
			return &apiStateUpdate{
				Sessions:   sessions,
				UpdateType: "sessions",
				Source:     u.Source,
				Scanned:    len(sessions),
			}
		}
		return &apiStateUpdate{
			UpdateType: "sessions",
			Source:     u.Source,
			Scanned:    u.Scanned,
		}
	case store.UpdatePlanIndexDelta:
		if delta, ok := u.Payload.(*models.PlanIndexDelta); ok {
			return &apiStateUpdate{UpdateType: "plan_index", Source: u.Source, Scanned: u.Scanned, PlanIndex: delta}
		}
	case store.UpdateFocus:
		return &apiStateUpdate{
			UpdateType: "focus",
			Source:     u.Source,
			Scanned:    u.Scanned,
		}
	case store.UpdateConfigReload:
		configFile := ""
		if file, ok := u.Payload.(string); ok {
			configFile = file
		}
		return &apiStateUpdate{
			UpdateType: "config_reload",
			Source:     u.Source,
			ConfigFile: configFile,
		}

	// Theme change — the resolved tui.theme value changed. Without this
	// case the event would be silently dropped before reaching the SSE
	// wire. Payload is *coredaemon.ThemeChangedPayload.
	case store.UpdateThemeChanged:
		return &apiStateUpdate{
			UpdateType: string(store.UpdateThemeChanged),
			Source:     u.Source,
			Payload:    u.Payload,
		}
	case store.UpdateSkillSync:
		return &apiStateUpdate{
			UpdateType: "skill_sync",
			Source:     u.Source,
			Payload:    u.Payload,
		}
	case store.UpdateWatcherStatus:
		return &apiStateUpdate{
			UpdateType: "watcher_status",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Tiered git sweep progress. These carry *models.GitSweepProgress and are
	// passed through untyped (like watcher_status) — a consumer decodes with
	// coredaemon.ParseSweepProgress. Scanned mirrors the payload's Done so a
	// client that only reads the generic envelope still sees progress.
	case store.UpdateSweepStarted, store.UpdateSweepProgress, store.UpdateSweepCompleted:
		return &apiStateUpdate{
			UpdateType: string(u.Type),
			Source:     u.Source,
			Scanned:    u.Scanned,
			Payload:    u.Payload,
		}
	case store.UpdateTaskResult:
		if payload, ok := u.Payload.(*store.TaskResultPayload); ok {
			delta := &models.WorkspaceDelta{
				Path:        payload.Workspace,
				TaskResults: map[string]*models.TaskResult{payload.Verb: payload.Result},
			}
			return &apiStateUpdate{
				WorkspaceDeltas: []*models.WorkspaceDelta{delta},
				UpdateType:      "workspaces_delta",
				Source:          u.Source,
				Scanned:         1,
			}
		}

	// Session lifecycle updates - broadcast as session changes
	case store.UpdateSessionIntent, store.UpdateSessionConfirmation,
		store.UpdateSessionStatus, store.UpdateSessionEnd,
		store.UpdateSessionVerdict, store.UpdateSessionActivity,
		store.UpdateSessionsPruned, store.UpdateSessionTokens:
		return &apiStateUpdate{
			UpdateType: "session",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Note mutation events — broadcast as workspace updates so TUI refreshes
	case store.UpdateNoteEvent:
		return &apiStateUpdate{
			UpdateType: "note_event",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Note index updates — broadcast so TUI can refresh cached metadata
	case store.UpdateNoteIndex:
		return &apiStateUpdate{
			UpdateType: "note_index",
			Source:     u.Source,
		}

	// Job lifecycle updates
	case store.UpdateJobSubmitted, store.UpdateJobStarted,
		store.UpdateJobCompleted, store.UpdateJobFailed, store.UpdateJobCancelled,
		store.UpdateJobPendingUser, store.UpdateJobOrphaned:
		return &apiStateUpdate{
			UpdateType: string(u.Type),
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Nav bindings update
	case store.UpdateNavBindings:
		return &apiStateUpdate{
			UpdateType: "nav_bindings",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Memory index mutation — broadcast so TUIs can show a syncing indicator.
	case store.UpdateMemoryIndex:
		return &apiStateUpdate{
			UpdateType: "memory_index",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Native agent pane relay — pass-through to groveterm via SSE.
	case store.UpdateSpawnAgentPane, store.UpdateAttachAgentPane, store.UpdateAgentInput, store.UpdateCaptureRequest:
		return &apiStateUpdate{
			UpdateType: string(u.Type),
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Sync conflict/quarantine notifications — broadcast so TUIs and
	// notify consumers can surface them.
	case store.UpdateSyncConflict:
		return &apiStateUpdate{
			UpdateType: "sync_conflict",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Forge poller cache change — passthrough so the PRs page, the nav review
	// column and the treemux badge see forge state over SSE. Mirrors
	// sync_conflict. Payload: *store.ForgeStatePayload, carrying only the repos
	// that changed; the daemon keeps no forge state of its own (the projection
	// it does keep, models.ReviewStats, rides workspaces_delta).
	case store.UpdateForgeState:
		return &apiStateUpdate{
			UpdateType: "forge_state",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Satellite connection-health transition (C17) — passthrough so the treemux
	// badge and (P10) `grove status` see it over SSE. Mirrors sync_conflict.
	case store.UpdateSatelliteStatus:
		return &apiStateUpdate{
			UpdateType: "satellite_status",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Assistant supervisor status (spec §3.3) — passthrough so the rail's
	// assistant pane learns about a stopped or backing-off supervisor over the
	// stream it is already subscribed to, rather than by polling.
	case store.UpdateAssistantStatus:
		return &apiStateUpdate{
			UpdateType: "assistant_status",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Satellite federation snapshot (C7/C16) — passthrough so SSE consumers can
	// react to a federated jobs/sessions reconcile. The federated rows also
	// appear in GET /api/jobs and /api/sessions (key-agnostic map-ranges), so
	// treemux/hooks/nvim inherit them with zero per-tool change. Mirrors
	// satellite_status.
	case store.UpdateSatelliteSnapshot:
		return &apiStateUpdate{
			UpdateType: "satellite_snapshot",
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Workflow/subagent lifecycle updates — each keeps its DISTINCT
	// update_type string (the job_* pattern, NOT the collapsed "session"
	// pattern). Dropping a case here silently hides events from SSE
	// consumers; the broadcast is lossy-by-design, so consumers treat
	// these as triggers and reconcile via GET /api/workflows.
	case store.UpdateWorkflowRunDiscovered, store.UpdateWorkflowAgentStarted,
		store.UpdateWorkflowAgentCompleted, store.UpdateWorkflowRunStale,
		store.UpdateWorkflowRunCompleted, store.UpdateWorkflowChildrenSnapshot,
		store.UpdateWorkflowBashStarted:
		// UpdateWorkflowChildrenSnapshot / UpdateWorkflowBashStarted are kept on
		// the wire for consumers that reconcile on workflow frames (web viewer).
		// treemux does NOT read these frames: it surfaces the resulting
		// Session.LiveChildren / Session.Subagents via the full /api/sessions list
		// it re-fetches on the next "session" lifecycle frame (the identical
		// delivery path LiveTokens uses — see the R1 trace).
		return &apiStateUpdate{
			UpdateType: string(u.Type),
			Source:     u.Source,
			Payload:    u.Payload,
		}

	case store.UpdateSubjobReportReady, store.UpdateSubjobJoined:
		return &apiStateUpdate{UpdateType: string(u.Type), Source: u.Source, Payload: u.Payload}

	// Build queue lifecycle updates — distinct update_type strings (the
	// job_*/workflow_* pattern). Per-job build output never passes through
	// here; it streams over GET /api/build/jobs/{id}/stream.
	case store.UpdateBuildQueued, store.UpdateBuildStarted, store.UpdateBuildFinished:
		return &apiStateUpdate{
			UpdateType: string(u.Type),
			Source:     u.Source,
			Payload:    u.Payload,
		}

	// Daemon boot progress — carried in the typed BootPhase field (mirroring
	// StateUpdate.BootPhase), not Payload, so SSE consumers decode it directly.
	case store.UpdateBootPhase:
		if bs, ok := u.Payload.(*coredaemon.BootStatus); ok {
			return &apiStateUpdate{
				UpdateType: "boot_phase",
				Source:     u.Source,
				BootPhase:  bs,
			}
		}
	}
	return nil
}

// handleNoteIndex handles GET /api/notes/index - returns the cached note index.
// Supports optional ?workspace= query parameter to filter by workspace.
func (s *Server) handleNoteIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	wsFilter := r.URL.Query().Get("workspace")
	entries := s.engine.Store().GetNoteIndex(wsFilter)
	s.ulog.Debug("Note index request served").
		Field("workspace", wsFilter).
		Field("entries", len(entries)).
		Log(r.Context())
	w.Header().Set("Content-Type", "application/json")
	writeJSONArrayStreamed(w, entries)
}

// writeJSONArrayStreamed encodes a slice as a JSON array one element at a time.
//
// The obvious `json.NewEncoder(w).Encode(entries)` is a heap hazard at this
// endpoint's scale, and not for the reason it looks like. encoding/json buffers
// the WHOLE value in an internal bytes.Buffer before writing a byte to w, then
// returns that buffer to a package-level sync.Pool — which keeps its grown
// capacity, per P. The unfiltered note index is ~38k entries (~13 MB of JSON,
// ~20 MB once the buffer's doubling growth overshoots), so a handful of
// concurrent or successive requests left one such buffer parked on each of the
// machine's Ps: 205 MB, 32% of the live heap on the 2026-08-13 profile, held by
// a pool the daemon cannot see or drain.
//
// Encoding per element keeps every pooled buffer element-sized. The elements
// are separated by commas and each Encode appends its own newline, which is
// insignificant whitespace inside a JSON array, so the output parses exactly as
// before. A nil or empty slice still writes "[]", never "null" — clients treat
// the response as an array unconditionally.
func writeJSONArrayStreamed[T any](w io.Writer, items []T) {
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	if _, err := bw.WriteString("["); err != nil {
		return
	}
	for i := range items {
		if i > 0 {
			if _, err := bw.WriteString(","); err != nil {
				return
			}
		}
		if err := enc.Encode(items[i]); err != nil {
			return
		}
	}
	if _, err := bw.WriteString("]"); err != nil {
		return
	}
	_ = bw.Flush()
}

// handleNoteEvent handles POST /api/notes/event for incremental note count updates.
func (s *Server) handleNoteEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var event models.NoteEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Pre-parse index entry for created/updated/moved events so the store
	// can upsert the note index without needing filesystem access under the lock.
	if event.IndexEntry == nil && event.Path != "" &&
		event.Event != models.NoteEventDeleted && event.Event != models.NoteEventArchived {
		s.tryAttachIndexEntry(&event)
	}

	s.ulog.Debug("Note event received").
		Field("event", event.Event).
		Field("notespace_id", event.NotespaceID).
		Field("notespace_name", event.NotespaceName).
		Field("path", event.Path).
		Field("has_index", event.IndexEntry != nil).
		Log(r.Context())

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateNoteEvent,
		Source:  "nb",
		Payload: &event,
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// tryAttachIndexEntry attempts to parse frontmatter and attach a NoteIndexEntry to a NoteEvent.
func (s *Server) tryAttachIndexEntry(event *models.NoteEvent) {
	state := s.engine.Store().Get()
	cfg, _ := config.LoadDefault()
	locator := workspace.NewNotebookLocator(cfg)

	for _, ws := range state.Workspaces {
		if ws.WorkspaceNode == nil || ws.Name != event.NotespaceName {
			continue
		}
		contentDirPath, contentDirType := enrichment.ResolveContentDirForPath(event.Path, ws.WorkspaceNode, locator)
		if contentDirPath == "" {
			break
		}
		entry, err := enrichment.IndexSingleNote(event.Path, ws.Name, contentDirPath, contentDirType)
		if err == nil {
			event.IndexEntry = entry
		}
		break
	}
}

// handleGetConfig returns the running configuration as JSON.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	degraded := s.configError()
	if s.runningConfig == nil && degraded == nil {
		http.Error(w, "config not initialized", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if degraded == nil {
		_ = json.NewEncoder(w).Encode(s.runningConfig)
		return
	}
	_ = json.NewEncoder(w).Encode(struct {
		*RunningConfig
		Degraded    bool               `json:"degraded"`
		ConfigError *ConfigDegradation `json:"config_error"`
	}{RunningConfig: s.runningConfig, Degraded: true, ConfigError: degraded})
}

// handleFocus handles GET/POST for focused workspaces.
// POST sets the focus list, GET returns current focus.
func (s *Server) handleFocus(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var req struct {
			Source     string   `json:"source"`
			Paths      []string `json:"paths"`
			TTLSeconds int      `json:"ttl_seconds,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Source == "" {
			req.Source = "default"
		}
		s.engine.Store().SetFocusTTL(req.Source, req.Paths, time.Duration(req.TTLSeconds)*time.Second)
		s.ulog.Debug("Focus updated").Field("source", req.Source).Field("count", len(req.Paths)).Field("ttl_seconds", req.TTLSeconds).Log(r.Context())
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]int{"focused": len(req.Paths)})

	case http.MethodGet:
		focus := s.engine.Store().GetFocus()
		paths := make([]string, 0, len(focus))
		for p := range focus {
			paths = append(paths, p)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][]string{"paths": paths})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRefresh triggers an immediate re-scan. This is synchronous: it blocks
// until the scan completes, so the caller can immediately fetch fresh data
// after receiving the 200 OK response.
//
// With no body (or empty paths), all refreshable collectors do a full re-scan.
// With a JSON body {"paths": [...]}, only those workspaces are git-scanned
// (unknown paths silently skipped) and the fresh enriched workspaces are
// returned as a JSON array — the scoped verify-on-reveal path, bounded to a
// few hundred ms for a handful of repos.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Paths []string `json:"paths"`
	}
	// Tolerate the historical bodyless POST: a decode error (io.EOF on an
	// empty body included) just means "full refresh".
	_ = json.NewDecoder(r.Body).Decode(&req)

	if len(req.Paths) == 0 {
		if s.engine != nil {
			s.engine.Refresh(r.Context())
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}
	fresh, err := s.engine.RefreshPaths(r.Context(), req.Paths)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if fresh == nil {
		fresh = []*models.EnrichedWorkspace{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fresh)
}

func (s *Server) isMaintenanceTarget(target string) bool {
	s.maintenanceMu.RLock()
	defer s.maintenanceMu.RUnlock()
	return s.maintenanceTargets[target]
}

// handleJobs handles POST (submit) and GET (list) for /api/jobs.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && s.rejectSubmitWhenConfigDegraded(w) {
		return
	}
	jr := s.jobRunner.Load()
	if jr == nil {
		http.Error(w, "job runner not initialized", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodPost:
		// Parse request and detect unknown fields for capability warnings
		body := r.Body
		var bodyBytes []byte
		var req models.JobSubmitRequest

		// Read body into a buffer so we can decode it twice
		if data, err := io.ReadAll(body); err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		} else {
			bodyBytes = data
		}

		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		// Check for unknown fields in the JSON
		warnings := s.checkJobSubmitWarnings(bodyBytes)

		// Destructive record-return maintenance rejects new local work on a
		// guest (target "") and new dispatch to a named satellite on the laptop.
		if s.isMaintenanceTarget(req.Satellite) {
			http.Error(w, "job dispatch rejected: record-return maintenance is draining this target", http.StatusLocked)
			return
		}

		// Satellite routing (M2 C1/C3/C10): a Satellite-tagged submit is
		// forwarded to that satellite's own POST /api/jobs over the SSH
		// transport rather than run locally. The satellite gains no new verb.
		if req.Satellite != "" {
			cm := s.satelliteCM.Load()
			if cm == nil {
				http.Error(w, "satellite dispatch unavailable: this daemon has no satellite transport (scoped daemon or empty registry)", http.StatusServiceUnavailable)
				return
			}
			if !cm.HasSatellite(req.Satellite) {
				http.Error(w, fmt.Sprintf("unknown satellite %q", req.Satellite), http.StatusBadRequest)
				return
			}
			info, err := s.forwardJobToSatellite(r.Context(), cm, req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			response := models.JobSubmitResponse{JobInfo: info, Warnings: warnings}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(response)
			return
		}

		info, err := jr.Submit(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		response := models.JobSubmitResponse{
			JobInfo:  info,
			Warnings: warnings,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(response)

	case http.MethodGet:
		statusFilter := r.URL.Query().Get("status")
		jobs := s.engine.Store().GetJobs()
		var results []*models.JobInfo
		for _, j := range jobs {
			if statusFilter == "" || j.Status == statusFilter {
				results = append(results, j)
			}
		}
		if results == nil {
			results = []*models.JobInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(results)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleJobByID handles GET (info), DELETE (cancel), and log sub-routes for /api/jobs/{id}[/logs[/stream]].
func (s *Server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	path := r.URL.Path[len("/api/jobs/"):]
	parts := splitPath(path)
	if len(parts) == 0 {
		http.Error(w, "job ID required", http.StatusBadRequest)
		return
	}
	jobID := parts[0]

	// Route sub-paths: /api/jobs/{id}/logs, /logs/stream, and /artifacts.
	if len(parts) == 2 && parts[1] == "artifacts" {
		s.handleJobArtifacts(w, r, jobID)
		return
	}
	if len(parts) >= 2 && parts[1] == "logs" {
		if len(parts) >= 3 && parts[2] == "stream" {
			s.handleStreamJobLogs(w, r, jobID)
		} else {
			s.handleGetJobLogs(w, r, jobID)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		info := s.engine.Store().GetJob(jobID)
		if info == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)

	case http.MethodDelete:
		jr := s.jobRunner.Load()
		if jr == nil {
			http.Error(w, "job runner not initialized", http.StatusServiceUnavailable)
			return
		}
		if err := jr.Cancel(jobID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "cancelled", "job_id": jobID})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGetJobLogs returns historical log content for a job as a JSON array.
func (s *Server) handleGetJobLogs(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logStreamer == nil {
		http.Error(w, "log streamer not initialized", http.StatusServiceUnavailable)
		return
	}

	info := s.engine.Store().GetJob(jobID)
	if info == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	lines := s.logStreamer.GetLogs(jobID)
	if lines == nil {
		lines = []models.LogLine{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(lines)
}

// handleStreamJobLogs provides SSE streaming of log lines for a specific job.
func (s *Server) handleStreamJobLogs(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logStreamer == nil {
		http.Error(w, "log streamer not initialized", http.StatusServiceUnavailable)
		return
	}

	info := s.engine.Store().GetJob(jobID)
	if info == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Determine the log file path from the job
	logFilePath := resolveLogFilePath(info)
	if logFilePath == "" {
		http.Error(w, "no log file available for this job", http.StatusNotFound)
		return
	}

	history, ch, err := s.logStreamer.Subscribe(jobID, logFilePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer s.logStreamer.Unsubscribe(jobID, ch)

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send connection confirmation
	_, _ = fmt.Fprintf(w, ": connected to job %s log stream\n\n", jobID)
	flusher.Flush()

	// Send historical buffer
	for _, line := range history {
		data, err := json.Marshal(line)
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
	}
	flusher.Flush()

	// Stream new events
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return // Stream closed (job completed)
			}
			switch event.Event {
			case "log":
				if event.Line != nil {
					data, err := json.Marshal(event.Line)
					if err != nil {
						continue
					}
					_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
				}
			case "status":
				data, err := json.Marshal(map[string]string{
					"status": event.Status,
					"error":  event.Error,
				})
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "event: status\ndata: %s\n\n", data)
			}
			flusher.Flush()
		}
	}
}

// handleStreamWorkspaceLogs provides SSE streaming of aggregated workspace logs.
func (s *Server) handleStreamWorkspaceLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.workspaceStreamer == nil {
		http.Error(w, "workspace streamer not initialized", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	replay := 100
	if v := q.Get("replay"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &replay); n != 1 || err != nil {
			replay = 100
		}
	}

	opts := models.LogStreamOptions{
		Scope:     q.Get("scope"),
		Workspace: q.Get("workspace"),
		Level:     q.Get("level"),
		System:    q.Get("system") == "true",
		Replay:    replay,
	}

	history, ch := s.workspaceStreamer.Subscribe(opts)
	defer s.workspaceStreamer.Unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for _, line := range history {
		data, err := json.Marshal(models.LogStreamLine{
			Workspace:     line.Workspace,
			WorkspacePath: line.WorkspacePath,
			Line:          line.Line,
		})
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(models.LogStreamLine{
				Workspace:     line.Workspace,
				WorkspacePath: line.WorkspacePath,
				Line:          line.Line,
			})
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// resolveLogFilePath determines the log file path for a job.
// Prefers the path the runtime stashed on JobInfo at launch; falls back to
// a filename-based guess for jobs that pre-date that stashing.
func resolveLogFilePath(info *models.JobInfo) string {
	if info.LogFilePath != "" {
		return info.LogFilePath
	}
	if info.PlanDir == "" || info.JobFile == "" {
		return ""
	}
	jobName := strings.TrimSuffix(info.JobFile, ".md")
	return filepath.Join(info.PlanDir, ".artifacts", jobName, "job.log")
}

// handleEnvUp handles POST /api/env/up requests.
func (s *Server) handleEnvUp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	em := s.envManager.Load()
	if em == nil {
		http.Error(w, "env manager not initialized", http.StatusServiceUnavailable)
		return
	}

	var req coreenv.EnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	resp, err := em.Up(r.Context(), req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(coreenv.EnvResponse{
			Status: "failed",
			Error:  err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleEnvStatus handles GET /api/env/status requests.
func (s *Server) handleEnvStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	em := s.envManager.Load()
	if em == nil {
		http.Error(w, "env manager not initialized", http.StatusServiceUnavailable)
		return
	}

	worktree := r.URL.Query().Get("worktree")
	if worktree == "" {
		http.Error(w, "worktree query parameter required", http.StatusBadRequest)
		return
	}

	resp := em.Status(worktree)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleEnvDown handles POST /api/env/down requests.
func (s *Server) handleEnvDown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	em := s.envManager.Load()
	if em == nil {
		http.Error(w, "env manager not initialized", http.StatusServiceUnavailable)
		return
	}

	var req coreenv.EnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	resp, err := em.Down(r.Context(), req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(coreenv.EnvResponse{
			Status: "failed",
			Error:  err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleProxyRegister handles POST /api/proxy/register. The global
// (unscoped) daemon owns the *.grove.local route table bound to :8443;
// scoped daemons delegate every Register to it via this RPC. Scoped
// daemons return 400 so a misrouted request fails loudly instead of
// silently painting onto a local map nothing else reads.
func (s *Server) handleProxyRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		http.Error(w, "proxy registration is only served by the global (unscoped) daemon", http.StatusBadRequest)
		return
	}
	em := s.envManager.Load()
	if em == nil {
		http.Error(w, "env manager not initialized", http.StatusServiceUnavailable)
		return
	}
	var req coreenv.ProxyRouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Worktree == "" || req.Route == "" || req.Port <= 0 {
		http.Error(w, "worktree, route, and positive port are required", http.StatusBadRequest)
		return
	}
	em.Proxy.Register(req.Worktree, req.Route, req.Port)
	w.WriteHeader(http.StatusOK)
}

// handleProxyUnregister handles POST /api/proxy/unregister. Drops every
// route keyed by the posted worktree; no-op if nothing was registered.
func (s *Server) handleProxyUnregister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		http.Error(w, "proxy registration is only served by the global (unscoped) daemon", http.StatusBadRequest)
		return
	}
	em := s.envManager.Load()
	if em == nil {
		http.Error(w, "env manager not initialized", http.StatusServiceUnavailable)
		return
	}
	var req coreenv.ProxyUnregisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Worktree == "" {
		http.Error(w, "worktree is required", http.StatusBadRequest)
		return
	}
	em.Proxy.Unregister(req.Worktree)
	w.WriteHeader(http.StatusOK)
}

// handleRepoEnsure handles POST /api/repos/ensure — clone+checkout, single-flighted per URL@version.
func (s *Server) handleRepoEnsure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req models.RepoEnsureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	key := req.URL + "@" + req.Version
	ctx := r.Context()
	v, err, _ := s.repoGroup.Do(key, func() (interface{}, error) {
		mgr, mgrErr := repo.NewManager()
		if mgrErr != nil {
			return nil, mgrErr
		}
		path, commit, ensureErr := mgr.EnsureVersion(ctx, req.URL, req.Version)
		if ensureErr != nil {
			return nil, ensureErr
		}
		return &models.RepoEnsureResponse{WorktreePath: path, Commit: commit}, nil
	})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- Channel Management Handlers ---

// handleChannelSend handles POST /api/channels/send — send a message via a channel.
func (s *Server) handleChannelSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cm := s.channelManager.Load()
	if cm == nil {
		http.Error(w, "channel manager not initialized", http.StatusServiceUnavailable)
		return
	}

	var req models.ChannelSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	sendResp, err := cm.Send(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sendResp)
}

// handleChannelStatus handles GET /api/channels/status — get channel system status.
func (s *Server) handleChannelStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cm := s.channelManager.Load()
	if cm == nil {
		http.Error(w, "channel manager not initialized", http.StatusServiceUnavailable)
		return
	}

	status := cm.Status()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// handleChannelCleanup handles POST /api/channels/cleanup — purge stale routes.
func (s *Server) handleChannelCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cm := s.channelManager.Load()
	if cm == nil {
		http.Error(w, "channel manager not initialized", http.StatusServiceUnavailable)
		return
	}

	purged, err := cm.CleanupOrphans(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(models.ChannelCleanupResponse{Purged: purged})
}

// handleNavBindings handles GET /api/nav/bindings — return current nav binding state.
func (s *Server) handleNavBindings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bindings := s.engine.Store().GetNavBindings()
	if bindings == nil {
		// Serialize the lazy disk fill with mutation handlers. Without this, a
		// startup GET could load the old file, a PUT could publish the new one,
		// and then the GET could install its stale snapshot in the store.
		s.navBindingsMu.Lock()
		bindings = s.engine.Store().GetNavBindings()
		if bindings == nil {
			var err error
			bindings, err = navbindings.Load(navbindings.DefaultPath())
			if err != nil {
				s.navBindingsMu.Unlock()
				http.Error(w, fmt.Sprintf("failed to load nav bindings: %v", err), http.StatusInternalServerError)
				return
			}
			s.engine.Store().ApplyUpdate(store.Update{
				Type:    store.UpdateNavBindings,
				Source:  "api",
				Payload: bindings,
			})
		}
		s.navBindingsMu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bindings)
}

// handleNavConfig handles GET /api/nav/config — return the static nav configuration
// (group prefixes) read from the grove config files. This lets non-nav clients
// resolve group prefix transitions without re-implementing the nav config loader.
func (s *Server) handleNavConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.loadNavConfig()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg)
}

// handleNavGroup handles PUT /api/nav/groups/{group} — update a single group's sessions.
func (s *Server) handleNavGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract group name from path
	group := strings.TrimPrefix(r.URL.Path, "/api/nav/groups/")
	if group == "" {
		http.Error(w, "group name required", http.StatusBadRequest)
		return
	}

	var state models.NavGroupState
	if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	s.navBindingsMu.Lock()
	defer s.navBindingsMu.Unlock()

	// Load current state twice: once as the pre-mutation snapshot (prev) so
	// the validator can tolerate pre-existing rule-3 conflicts, and once as
	// the working copy we mutate and persist.
	sessionsPath := navbindings.DefaultPath()
	prev, err := navbindings.Load(sessionsPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load bindings: %v", err), http.StatusInternalServerError)
		return
	}
	file, err := navbindings.Load(sessionsPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load bindings: %v", err), http.StatusInternalServerError)
		return
	}

	// Apply update
	if group == "default" || group == "" {
		file.Sessions = state.Sessions
	} else {
		if file.Groups == nil {
			file.Groups = make(map[string]models.NavGroupState)
		}
		file.Groups[group] = state
	}

	// No-op short-circuit: nav re-PUTs its current state on every tab focus
	// and group switch. When nothing changed, skip the whole persist +
	// regenerate + ReloadAllServers + SSE train — it costs over a second per
	// call and the broadcast churns every subscribed TUI.
	if reflect.DeepEqual(prev, file) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unchanged"})
		return
	}

	// Validate (load group configs from nav config for prefix conflict detection).
	// Diff-aware against prev so pre-existing rule-3 conflicts don't brick all
	// writes — the user must still be able to edit a file that was persisted
	// before the validator was tightened.
	groupConfigs := s.loadNavGroupConfigs()
	if err := navbindings.ValidateAgainstPrevious(prev, file, groupConfigs); err != nil {
		http.Error(w, fmt.Sprintf("validation failed: %v", err), http.StatusBadRequest)
		return
	}

	// Persist to disk
	if err := navbindings.Save(sessionsPath, file); err != nil {
		http.Error(w, fmt.Sprintf("failed to save bindings: %v", err), http.StatusInternalServerError)
		return
	}

	// Regenerate tmux config
	groupBindings := s.buildGroupBindings(file)
	if err := navbindings.GenerateTmuxConf(groupBindings, paths.BinDir(), paths.CacheDir()); err != nil {
		s.ulog.Warn("Failed to regenerate tmux bindings").Err(err).Log(r.Context())
	}

	// Source-file the freshly generated conf into every running tmux server so
	// standalone tmux sessions pick up the change immediately — not just nav
	// CLI callers. The daemon now owns artifact generation AND the live reload,
	// so a binding edit made inside treemux (which no-ops its own
	// RegenerateBindings) still reaches plain tmux. Resolves the same
	// paths.CacheDir() conf path the generator just wrote.
	coretmux.ReloadAllServers()

	// Update store and broadcast SSE
	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateNavBindings,
		Source:  "api",
		Payload: file,
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// handleNavLockedKeys handles PUT /api/nav/locked-keys — update global locked keys.
func (s *Server) handleNavLockedKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var keys []string
	if err := json.NewDecoder(r.Body).Decode(&keys); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	s.navBindingsMu.Lock()
	defer s.navBindingsMu.Unlock()

	sessionsPath := navbindings.DefaultPath()
	file, err := navbindings.Load(sessionsPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load bindings: %v", err), http.StatusInternalServerError)
		return
	}

	// No-op short-circuit (same rationale as handleNavGroup): nav re-PUTs
	// the unchanged locked-keys list constantly; skip persist + regenerate +
	// reload + broadcast when nothing changed.
	if slices.Equal(file.LockedKeys, keys) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unchanged"})
		return
	}

	file.LockedKeys = keys

	if err := navbindings.Save(sessionsPath, file); err != nil {
		http.Error(w, fmt.Sprintf("failed to save bindings: %v", err), http.StatusInternalServerError)
		return
	}

	// Regenerate tmux config
	groupBindings := s.buildGroupBindings(file)
	if err := navbindings.GenerateTmuxConf(groupBindings, paths.BinDir(), paths.CacheDir()); err != nil {
		s.ulog.Warn("Failed to regenerate tmux bindings").Err(err).Log(r.Context())
	}

	// Source-file the regenerated conf into every running tmux server so a
	// locked-keys change is reflected live in standalone tmux (same rationale
	// as handleNavGroup).
	coretmux.ReloadAllServers()

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateNavBindings,
		Source:  "api",
		Payload: file,
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// handleNavLastAccessedGroup handles PUT /api/nav/last-accessed — update the last-accessed group.
func (s *Server) handleNavLastAccessedGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	s.navBindingsMu.Lock()
	defer s.navBindingsMu.Unlock()

	sessionsPath := navbindings.DefaultPath()
	file, err := navbindings.Load(sessionsPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load bindings: %v", err), http.StatusInternalServerError)
		return
	}

	// No-op short-circuit: skip the write and the SSE broadcast when the
	// last-accessed group hasn't actually changed.
	if file.LastAccessedGroup == req.Group {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unchanged"})
		return
	}

	file.LastAccessedGroup = req.Group

	if err := navbindings.Save(sessionsPath, file); err != nil {
		http.Error(w, fmt.Sprintf("failed to save bindings: %v", err), http.StatusInternalServerError)
		return
	}

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateNavBindings,
		Source:  "api",
		Payload: file,
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// loadNavConfig reads the static nav configuration exposed to clients.
func (s *Server) loadNavConfig() models.NavConfig {
	result := models.NavConfig{Groups: make(map[string]models.NavGroupConfig)}

	cfg, err := config.LoadDefault()
	if err != nil {
		return result
	}

	var navCfg struct {
		Prefix        string   `toml:"prefix" yaml:"prefix"`
		AvailableKeys []string `toml:"available_keys" yaml:"available_keys"`
		Groups        map[string]struct {
			Prefix string `toml:"prefix" yaml:"prefix"`
		} `toml:"groups" yaml:"groups"`
	}
	_ = cfg.UnmarshalExtension("nav", &navCfg)

	defaultPrefix := navCfg.Prefix
	if defaultPrefix == "" {
		defaultPrefix = "<prefix>"
	}
	result.Groups["default"] = models.NavGroupConfig{Prefix: defaultPrefix}
	result.AvailableKeys = append([]string(nil), navCfg.AvailableKeys...)
	for name, g := range navCfg.Groups {
		result.Groups[name] = models.NavGroupConfig{Prefix: g.Prefix}
	}
	return result
}

// loadNavGroupConfigs projects the public config into the validation shape.
func (s *Server) loadNavGroupConfigs() map[string]navbindings.GroupConfig {
	cfg := s.loadNavConfig()
	result := make(map[string]navbindings.GroupConfig, len(cfg.Groups))
	for name, group := range cfg.Groups {
		result[name] = navbindings.GroupConfig{Prefix: group.Prefix}
	}
	return result
}

// buildGroupBindings constructs GroupBinding slice from a NavSessionsFile and the grove config.
func (s *Server) buildGroupBindings(file *models.NavSessionsFile) []navbindings.GroupBinding {
	groupConfigs := s.loadNavGroupConfigs()

	bindings := make([]navbindings.GroupBinding, 0, len(groupConfigs)+1)

	// Default group
	defaultPrefix := "<prefix>"
	if cfg, ok := groupConfigs["default"]; ok {
		defaultPrefix = cfg.Prefix
	}
	bindings = append(bindings, navbindings.GroupBinding{
		Name:     "default",
		Prefix:   defaultPrefix,
		Sessions: file.Sessions,
	})

	// Named groups
	for name, gs := range file.Groups {
		prefix := ""
		if cfg, ok := groupConfigs[name]; ok {
			prefix = cfg.Prefix
		}
		bindings = append(bindings, navbindings.GroupBinding{
			Name:     name,
			Prefix:   prefix,
			Sessions: gs.Sessions,
		})
	}

	return bindings
}
