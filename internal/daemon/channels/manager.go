// Package channels manages external messaging channels for the grove daemon.
// It owns the routing table, ref-counting, and message dispatch.
// The actual channel implementations live in notify/pkg/channels/.
package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/notify/pkg/channels"
	"github.com/grovetools/notify/pkg/channels/ha"
	"github.com/grovetools/notify/pkg/channels/signal"
)

// SignalConfig holds the configuration needed to create a Signal channel.
type SignalConfig struct {
	Enabled     bool
	CLIPath     string
	Account     string
	Allowlist   []string
	Groups      []string
	Contacts    map[string]string
	NamedGroups map[string]string
}

// HAConfig holds the configuration needed to create a Home Assistant channel.
type HAConfig struct {
	Enabled     bool
	WebhookPort int
	// WebhookBind is the interface the inbound webhook listener binds to.
	// Empty means loopback (127.0.0.1); the channel refuses to serve on a
	// broader interface unless the operator sets this explicitly.
	WebhookBind   string
	WebhookSecret string
	// WebhookSecretErr carries a secret-resolution failure (e.g. a failed
	// webhook_secret_command) so the channel fails CLOSED instead of opening
	// an unauthenticated endpoint.
	WebhookSecretErr string
	URL              string
	Token            string
	DefaultSatellite string
}

func (cfg *SignalConfig) ResolveTarget(name string) (id string, isGroup bool) {
	if val, ok := cfg.NamedGroups[name]; ok {
		return val, true
	}
	if val, ok := cfg.Contacts[name]; ok {
		return val, false
	}
	return "", false
}

// Manager manages external messaging channels and routes messages to/from agent sessions.
type Manager struct {
	mu             sync.Mutex
	store          *store.Store
	signalCfg      SignalConfig
	signalChannel  channels.Channel
	haCfg          HAConfig
	haChannel      *ha.Channel
	activeSessions map[string]bool  // jobID → true for sessions with signal enabled
	haActiveSess   map[string]bool  // jobID → true for sessions with ha enabled
	routeTable     map[int64]string // signal timestamp → jobID
	ready          chan struct{}    // closed when signal-cli is ready
	isRunning      bool
	ulog           *logging.UnifiedLogger
	ctx            context.Context
	cancel         context.CancelFunc

	// scope is this daemon's scope (empty for the global daemon). Scoped
	// daemons forward outbound sends to the global daemon and register their
	// socket in routing.json so global can forward inbound replies back.
	scope string
	// socketPath is this daemon's own socket, written into routing.json so
	// the global daemon can dial it for inbound session input.
	socketPath string
	// globalClient is non-nil on scoped daemons. When set, Send() proxies
	// to the global daemon and signal-cli is never spawned locally.
	globalClient daemon.Client

	// SendInput is the function used to inject messages into agent sessions.
	// Set by the server at initialization. It takes a jobID (the server
	// resolves the mux + PTY/tmux target internally).
	SendInput func(ctx context.Context, jobID, message string) error

	// EnsureAssistant, when set, runs one ensure pass of the LOCAL assistant
	// supervisor and reports whether it produced a live chain. Inbound that
	// reached no live agent is, for an ecosystem with a standing assistant
	// claw, mail for the assistant: the claw is gone because its session died,
	// and the supervisor is the thing that brings it back (assistant-pane spec
	// §3.3, §3.4 "ensure on inbound").
	//
	// It is synchronous and returns an error because the sender is owed an
	// answer: a launch that fails is reported back over Signal rather than
	// leaving a phone with silence. Callers on the inbound path always invoke
	// it from the flush goroutine, never inline.
	//
	// Only the daemon that owns the assistant has this wired. When the
	// assistant lives in another (scoped) daemon, ensureAssistant forwards to
	// its socket instead — see default_claw.go.
	EnsureAssistant func(ctx context.Context, reason string) error

	// IsDefaultClawJob reports whether a job belongs to this daemon's
	// assistant, so enabling its channel also registers it as the ecosystem's
	// default claw. Nil on every daemon with no [assistant] block, which is
	// the default.
	IsDefaultClawJob func(jobID string) bool

	// assistantQueue holds inbound parked while the assistant comes up, and
	// assistantFlushing says whether a flush goroutine already owns it. Both
	// are guarded by m.mu; see assistant_queue.go.
	assistantQueue    []queuedInbound
	assistantFlushing bool

	recentInbound    [10]models.InboundRecord
	recentInboundIdx int
	recentInboundLen int
	lastInboundAt    time.Time

	// signalStartErr records why the signal channel failed to start (preflight
	// failure), for Status() reporting when m.signalChannel stays nil. Guarded
	// by m.mu; cleared once the channel starts successfully.
	signalStartErr error
}

// NewManager creates a new ChannelManager. scope is the daemon's scope
// ("" for the global daemon); socketPath is this daemon's own socket path
// (used by scoped daemons to register inbound routes in routing.json).
func NewManager(st *store.Store, cfg SignalConfig, haCfg HAConfig, scope, socketPath string) *Manager {
	return &Manager{
		store:          st,
		signalCfg:      cfg,
		haCfg:          haCfg,
		scope:          scope,
		socketPath:     socketPath,
		activeSessions: make(map[string]bool),
		haActiveSess:   make(map[string]bool),
		routeTable:     make(map[int64]string),
		ulog:           logging.NewUnifiedLogger("groved.channels"),
	}
}

// SetGlobalClient puts the Manager into "proxy mode". Scoped daemons call
// this at boot; thereafter Send() forwards to the global daemon and
// EnableChannel writes this daemon's socket into routing.json instead of
// spawning signal-cli.
func (m *Manager) SetGlobalClient(c daemon.Client) {
	m.mu.Lock()
	m.globalClient = c
	m.mu.Unlock()
}

// Start initializes the channel manager. It loads persisted routes and checks
// for existing sessions that need channels.
func (m *Manager) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)

	m.loadRoutes()

	if m.scope == "" && m.globalClient == nil {
		// Global daemon: hydrate activeSessions from the unified state file.
		// InboundRoutes has jobIDs owned by scoped daemons; QuoteRoutes
		// has jobIDs with recent outbound traffic.
		m.mu.Lock()
		if state, err := loadChannelState(); err == nil {
			for jobID := range state.InboundRoutes {
				m.activeSessions[jobID] = true
			}
			for _, jobID := range state.QuoteRoutes {
				m.activeSessions[jobID] = true
			}
		}
		m.mu.Unlock()
	}
	// Scoped daemons no longer prune synchronously at boot — the store
	// may still be empty. Pruning runs on a background ticker instead.

	// Global daemon: signal-cli is infrastructure, not per-session. Spawn
	// it unconditionally at startup so outbound sends from scoped daemons
	// and inbound Signal replies are always routable. Refcount-based
	// lifecycle was wrong under the cross-daemon model because scoped
	// daemons' claws live in routing.json, not in this daemon's
	// activeSessions — the global daemon can't ref-count something it
	// doesn't own.
	if m.scope == "" && m.globalClient == nil && m.signalCfg.Enabled {
		m.mu.Lock()
		if !m.isRunning {
			m.isRunning = true
			m.ready = make(chan struct{})
			go m.startSignalChannel(m.ctx)
		}
		m.mu.Unlock()
	}

	// Start HA channel if enabled (HA is always local, no proxy mode)
	if m.haCfg.Enabled {
		m.mu.Lock()
		haCh := ha.NewChannel(ha.Config{
			WebhookPort:      m.haCfg.WebhookPort,
			WebhookBind:      m.haCfg.WebhookBind,
			WebhookSecret:    m.haCfg.WebhookSecret,
			WebhookSecretErr: m.haCfg.WebhookSecretErr,
			HAURL:            m.haCfg.URL,
			HAToken:          m.haCfg.Token,
			DefaultSatellite: m.haCfg.DefaultSatellite,
		})
		if err := haCh.Start(m.ctx, m.handleHAInbound); err != nil {
			m.ulog.Error("Failed to start HA channel").Err(err).Log(m.ctx)
		} else {
			m.haChannel = haCh
			m.ulog.Info("HA channel started").
				Field("event", "channel.up").
				Field("webhook_port", m.haCfg.WebhookPort).
				Log(m.ctx)
		}
		m.mu.Unlock()
	}

	// Subscribe to store events for route cleanup and channel rehydration
	go m.watchStoreUpdates(m.ctx)

	// Periodic route cleanup (TTL)
	go m.routeCleanup(m.ctx)

	// Background prune — replaces the former synchronous boot-time prune
	go m.backgroundPrune(m.ctx)

	m.ulog.Info("Channel manager started").
		Field("scope", m.scope).
		Field("active_sessions", len(m.activeSessions)).
		Field("route_table_size", len(m.routeTable)).
		Log(m.ctx)
}

// pruneStaleSessions drops any activeSessions entry whose backing store
// session is missing, along with any routeTable entries pointing at pruned
// jobIDs. Persists the updated route file if anything changed.
func (m *Manager) pruneStaleSessions(ctx context.Context) {
	m.ulog.Debug("pruneStaleSessions starting").
		Field("scope", m.scope).
		Field("active_sessions_count", len(m.activeSessions)).
		StructuredOnly().Log(ctx)

	m.mu.Lock()
	stale := make([]string, 0, len(m.activeSessions))
	for jobID := range m.activeSessions {
		sess := m.store.GetSession(jobID)
		if sess == nil {
			m.ulog.Warn("pruneStaleSessions: session missing from store, marking stale").
				Field("job_id", jobID).
				Field("scope", m.scope).
				Log(ctx)
			stale = append(stale, jobID)
		} else {
			m.ulog.Debug("pruneStaleSessions: session found").
				Field("job_id", jobID).
				Field("status", sess.Status).
				StructuredOnly().Log(ctx)
		}
	}
	for _, jobID := range stale {
		delete(m.activeSessions, jobID)
		for ts, id := range m.routeTable {
			if id == jobID {
				delete(m.routeTable, ts)
			}
		}
	}
	m.mu.Unlock()

	for _, jobID := range stale {
		m.ulog.Warn("Pruning stale channel entry — no store session").
			Field("job_id", jobID).
			Log(ctx)
	}
	if len(stale) > 0 {
		m.saveRoutes()
	}
}

// CleanupOrphans purges stale routes and orphaned sessions.
// It verifies that cross-daemon routes point to live sockets and that
// activeSessions correspond to active sessions in the store or routing table.
// Returns the number of purged entries.
func (m *Manager) CleanupOrphans(ctx context.Context) (int, error) {
	purgedCount := 0

	// 1. Clean up stale inbound routes — dead sockets
	stateMu.Lock()
	state, err := loadChannelState()
	changedRoutes := false
	if err == nil {
		for jobID, sock := range state.InboundRoutes {
			if _, err := os.Stat(sock); os.IsNotExist(err) {
				delete(state.InboundRoutes, jobID)
				changedRoutes = true
				purgedCount++
			}
		}
		if changedRoutes {
			_ = saveStateAtomic(state)
		}
	}
	stateMu.Unlock()

	routes := make(map[string]string)
	if state != nil {
		routes = state.InboundRoutes
	}

	// 2. Clean up activeSessions — keep if inbound routes or store says alive
	m.mu.Lock()
	stale := make([]string, 0, len(m.activeSessions))
	for jobID := range m.activeSessions {
		if _, ok := routes[jobID]; ok {
			continue
		}
		if m.store.GetSession(jobID) != nil {
			continue
		}
		stale = append(stale, jobID)
	}

	for _, jobID := range stale {
		delete(m.activeSessions, jobID)
		for ts, id := range m.routeTable {
			if id == jobID {
				delete(m.routeTable, ts)
			}
		}
		purgedCount++
	}
	m.mu.Unlock()

	if len(stale) > 0 {
		m.saveRoutes()
		for _, jobID := range stale {
			m.ulog.Info("Purged orphan channel session").Field("job_id", jobID).Log(ctx)
		}
	}

	return purgedCount, nil
}

// Stop shuts down the channel manager and signal-cli.
func (m *Manager) Stop(ctx context.Context) {
	// Detach mutable resources while holding the manager lock, then perform
	// cancellation, channel shutdown, and persistence without it. saveRoutes
	// takes m.mu to snapshot routeTable; calling it while m.mu was held
	// self-deadlocked every daemon shutdown after channel-state persistence was
	// introduced.
	m.mu.Lock()
	cancel := m.cancel
	signalChannel := m.signalChannel
	haChannel := m.haChannel
	m.cancel = nil
	m.signalChannel = nil
	m.haChannel = nil
	m.isRunning = false
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if signalChannel != nil {
		_ = signalChannel.Stop(ctx)
	}
	if haChannel != nil {
		_ = haChannel.Stop(ctx)
	}

	m.saveRoutes()
	m.ulog.Info("Channel manager stopped").Log(ctx)
}

// EnableChannel enables channels for a session. channelNames selects which
// channels to activate (e.g. ["signal"], ["ha"], ["signal","ha"]). An empty
// or nil list defaults to ["signal"] for backwards compatibility.
func (m *Manager) EnableChannel(_ context.Context, jobID string, channelNames ...string) error {
	if len(channelNames) == 0 {
		channelNames = []string{"signal"}
	}

	for _, ch := range channelNames {
		switch ch {
		case "signal":
			if err := m.enableSignal(jobID); err != nil {
				return err
			}
		case "ha":
			if err := m.enableHA(jobID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown channel %q", ch)
		}
	}
	// Snapshot how to reach this session now that it is channel-enabled.
	// state.json is the only routing datum that survives a daemon restart, and
	// it is read by any daemon whose in-memory store never saw the session, so
	// a claw that never records mux/pty_id here leaves inbound messages with
	// nothing to route by.
	m.saveSessionDelivery(jobID)
	return nil
}

func (m *Manager) enableSignal(jobID string) error {
	m.mu.Lock()

	if !m.signalCfg.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("signal is not enabled in configuration")
	}

	m.activeSessions[jobID] = true
	isProxy := m.globalClient != nil

	m.ulog.Info("EnableChannel invoked").
		Field("channel", "signal").
		Field("job_id", jobID).
		Field("scope", m.scope).
		Field("is_proxy", isProxy).
		Log(m.ctx)

	if !isProxy && !m.isRunning {
		m.isRunning = true
		m.ready = make(chan struct{})
		go m.startSignalChannel(m.ctx)
	}
	m.mu.Unlock()

	if isProxy && !m.isSandboxScope() {
		if err := m.addInboundRoute(jobID); err != nil {
			m.ulog.Warn("Failed to write inbound route").Err(err).Field("job_id", jobID).Log(m.ctx)
		}
	}

	// A claw on an assistant job is the ecosystem's default claw (spec §3.4).
	// Registering it HERE — rather than on a supervisor tick — means the record
	// tracks the chain exactly: the supervisor re-claws after every launch and
	// every handoff, so each new head registers itself the moment it becomes
	// reachable, which is also the moment queued inbound may be delivered.
	if m.IsDefaultClawJob != nil && m.IsDefaultClawJob(jobID) {
		m.markDefaultClawJob(jobID)
	}

	m.ulog.Info("Channel enabled for session").Field("channel", "signal").Field("job_id", jobID).Log(m.ctx)
	return nil
}

func (m *Manager) enableHA(jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.haCfg.Enabled {
		return fmt.Errorf("home_assistant is not enabled in configuration")
	}

	m.haActiveSess[jobID] = true

	m.ulog.Info("HA channel enabled for session").Field("job_id", jobID).Log(m.ctx)
	return nil
}

// DisableChannel disables a channel for a session. On scoped daemons
// this removes the routing.json entry. signal-cli lifecycle is no longer
// ref-counted — on the global daemon it runs for the daemon's lifetime
// (started in Start, stopped in Stop). Ref-counting was wrong under
// cross-daemon because scoped claws live in routing.json, not in the
// global daemon's activeSessions.
func (m *Manager) DisableChannel(ctx context.Context, jobID string) {
	m.ulog.Info("DisableChannel invoked").
		Field("job_id", jobID).
		Field("scope", m.scope).
		Log(ctx)

	m.mu.Lock()
	delete(m.activeSessions, jobID)
	delete(m.haActiveSess, jobID)
	m.mu.Unlock()

	// Remove inbound route unconditionally — on scoped daemons this clears
	// the route we registered; on the global daemon this clears stale routes
	// left by scoped daemons that have since stopped.
	if err := m.removeInboundRoute(jobID); err != nil {
		m.ulog.Warn("Failed to remove inbound route").Err(err).Field("job_id", jobID).Log(ctx)
	}

	// Clean up persisted delivery info
	m.removeSessionDelivery(jobID)

	// The assistant's claw going away does not retire the ENDPOINT: the
	// ecosystem still has an assistant, and knowing that is what routes the
	// next inbound message into ensure-on-inbound instead of dropping it.
	m.clearDefaultClawJob(jobID)
}

// Send sends a message via the appropriate channel and records the route.
// On scoped daemons (globalClient != nil), signal sends are forwarded to the
// global daemon which owns signal-cli. HA sends are always local.
func (m *Manager) Send(ctx context.Context, req models.ChannelSendRequest) (*models.ChannelSendResponse, error) {
	if req.Channel == "ha" {
		return m.sendHA(ctx, req)
	}

	m.mu.Lock()
	gc := m.globalClient
	ch := m.signalChannel
	ready := m.ready
	m.mu.Unlock()

	if gc != nil {
		return gc.SendChannelMessage(ctx, req)
	}

	if ch == nil {
		return nil, fmt.Errorf("signal channel is not running")
	}

	// Wait for signal-cli to be ready
	if ready != nil {
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, fmt.Errorf("timeout waiting for signal-cli to be ready")
		}
	}

	// Resolve recipient / group
	recipient := req.Recipient
	groupID := req.GroupID
	if recipient == "" && groupID == "" {
		session := m.store.GetSession(req.JobID)
		logging.NewUnifiedLogger("groved.channels.send").Info("resolving target").
			Field("job_id", req.JobID).
			Field("session_found", session != nil).
			Field("signal_target", func() string {
				if session != nil {
					return session.SignalTarget
				}
				return ""
			}()).
			Field("last_sender", func() string {
				if session != nil {
					return session.LastSender
				}
				return ""
			}()).
			Field("last_sender_group", func() string {
				if session != nil {
					return session.LastSenderGroup
				}
				return ""
			}()).
			Field("configured_groups", len(m.signalCfg.Groups)).
			StructuredOnly().Log(ctx)
		if session != nil && session.SignalTarget != "" {
			id, isGroup := m.signalCfg.ResolveTarget(session.SignalTarget)
			if isGroup {
				groupID = id
			} else if id != "" {
				recipient = id
			}
		} else if session != nil && session.LastSenderGroup != "" {
			groupID = session.LastSenderGroup
		} else if session != nil && session.LastSender != "" {
			recipient = session.LastSender
		} else if len(m.signalCfg.Groups) > 0 {
			groupID = m.signalCfg.Groups[0]
		} else {
			// Broadcast to all allowlisted contacts
			for _, contact := range m.signalCfg.Allowlist {
				taggedMsg := m.tagMessage(req.JobID, req.JobTitle, req.Message)
				result, err := ch.Send(ctx, channels.OutboundMessage{
					Recipient: contact,
					Message:   taggedMsg,
				})
				if err != nil {
					m.ulog.Error("Failed to send").Err(err).Field("recipient", contact).Log(ctx)
					continue
				}
				if result != nil && result.Timestamp > 0 {
					m.recordRoute(result.Timestamp, req.JobID)
				}
			}
			return &models.ChannelSendResponse{Status: "broadcast"}, nil
		}
	}

	logging.NewUnifiedLogger("groved.channels.send").Info("resolved target").
		Field("job_id", req.JobID).
		Field("recipient", recipient).
		Field("group_id", groupID).
		StructuredOnly().Log(ctx)

	taggedMsg := m.tagMessage(req.JobID, req.JobTitle, req.Message)
	result, err := ch.Send(ctx, channels.OutboundMessage{
		Recipient: recipient,
		GroupID:   groupID,
		Message:   taggedMsg,
	})
	if err != nil {
		return nil, fmt.Errorf("send failed: %w", err)
	}

	if result != nil && result.Timestamp > 0 {
		m.recordRoute(result.Timestamp, req.JobID)
	}

	return &models.ChannelSendResponse{
		Timestamp: result.Timestamp,
		Status:    "sent",
	}, nil
}

func (m *Manager) sendHA(ctx context.Context, req models.ChannelSendRequest) (*models.ChannelSendResponse, error) {
	m.mu.Lock()
	ch := m.haChannel
	m.mu.Unlock()

	if ch == nil {
		return nil, fmt.Errorf("ha channel is not running")
	}

	result, err := ch.Send(ctx, channels.OutboundMessage{
		Recipient: req.Recipient,
		Message:   req.Message,
	})
	if err != nil {
		return nil, fmt.Errorf("ha send failed: %w", err)
	}

	return &models.ChannelSendResponse{
		Timestamp: result.Timestamp,
		Status:    "sent",
	}, nil
}

// handleHAInbound routes an inbound HA webhook message to the right session.
func (m *Manager) handleHAInbound(msg channels.InboundMessage) {
	ctx := context.Background()

	m.mu.Lock()
	count := len(m.haActiveSess)

	if count == 0 {
		m.mu.Unlock()
		m.ulog.Warn("HA inbound dropped — no active HA sessions").Log(ctx)
		return
	}

	var targetJobID string
	if count == 1 {
		for id := range m.haActiveSess {
			targetJobID = id
		}
	}
	m.mu.Unlock()

	if targetJobID == "" {
		m.ulog.Warn("HA inbound dropped — multiple active HA sessions").
			Field("count", count).Log(ctx)
		return
	}

	formatted := fmt.Sprintf("[via HA Voice from %s] %s", msg.Source, msg.Message)
	m.ulog.Info("HA inbound routed").
		Field("job_id", targetJobID).
		Field("source", msg.Source).
		Log(ctx)

	if m.SendInput != nil {
		if err := m.SendInput(ctx, targetJobID, formatted); err != nil {
			m.ulog.Error("Failed to deliver HA inbound").Err(err).
				Field("job_id", targetJobID).Log(ctx)
		}
	}
}

// recordInbound appends an inbound routing decision to the circular buffer.
// Must be called with m.mu NOT held (it acquires the lock internally).
func (m *Manager) recordInbound(sender, strategy, targetJob, errMsg string, delivered bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastInboundAt = time.Now()
	m.recentInbound[m.recentInboundIdx] = models.InboundRecord{
		Timestamp: m.lastInboundAt,
		Sender:    sender,
		Strategy:  strategy,
		TargetJob: targetJob,
		Delivered: delivered,
		Error:     errMsg,
	}
	m.recentInboundIdx = (m.recentInboundIdx + 1) % len(m.recentInbound)
	if m.recentInboundLen < len(m.recentInbound) {
		m.recentInboundLen++
	}
}

// Status returns the current status of the channel system.
func (m *Manager) Status() *models.ChannelStatusResponse {
	m.mu.Lock()
	defer m.mu.Unlock()

	resp := &models.ChannelStatusResponse{
		SignalCLIRunning: m.isRunning,
		ActiveRoutes:     len(m.routeTable),
		RefCount:         len(m.activeSessions),
		SignalEnabled:    m.signalCfg.Enabled,
	}

	if m.signalChannel != nil {
		st := m.signalChannel.Status()
		resp.SignalIsAlive = st.IsAlive
		resp.SignalRestartCount = st.RestartCount
		resp.SignalStopped = st.Stopped
		resp.SignalLastError = st.LastError
		if !st.LastRestartAt.IsZero() {
			resp.SignalLastRestart = &st.LastRestartAt
		}
	} else if m.signalStartErr != nil {
		// Channel never started (preflight failure); surface why.
		resp.SignalLastError = m.signalStartErr.Error()
	}

	if !m.lastInboundAt.IsZero() {
		resp.LastInboundTimestamp = &m.lastInboundAt
	}

	records := make([]models.InboundRecord, 0, m.recentInboundLen)
	start := m.recentInboundIdx - m.recentInboundLen
	if start < 0 {
		start += len(m.recentInbound)
	}
	for i := 0; i < m.recentInboundLen; i++ {
		idx := (start + i) % len(m.recentInbound)
		records = append(records, m.recentInbound[idx])
	}
	resp.RecentInbound = records

	return resp
}

// getActiveSessionIDs returns the union of in-memory activeSessions and
// routing.json inbound routes. This gives the global daemon a complete
// view of all channel-enabled sessions, including those on scoped daemons.
func (m *Manager) getActiveSessionIDs() map[string]bool {
	active := make(map[string]bool)
	m.mu.Lock()
	for id := range m.activeSessions {
		active[id] = true
	}
	m.mu.Unlock()

	if state, err := loadChannelState(); err == nil {
		for id := range state.InboundRoutes {
			active[id] = true
		}
	}
	return active
}

// handleInbound routes an inbound message to the correct agent session.
func (m *Manager) handleInbound(msg channels.InboundMessage) {
	ctx := context.Background()
	text := msg.Message
	var targetJobID string
	var resolvedVia string

	// Build unified active set (local activeSessions + routing.json)
	activeIDs := m.getActiveSessionIDs()

	m.mu.Lock()

	quoteID := int64(0)
	if msg.Quote != nil {
		quoteID = msg.Quote.ID
	}
	m.ulog.Debug("Inbound signal message").
		Field("source", msg.Source).
		Field("text_len", len(text)).
		Field("quote_id", quoteID).
		Field("active_sessions", len(activeIDs)).
		Field("route_table_size", len(m.routeTable)).
		Log(ctx)

	// 0. Handle !commands (meta-commands from user)
	if strings.HasPrefix(text, "!") {
		m.mu.Unlock()
		go m.handleCommand(ctx, msg.Source, msg.GroupID, text, activeIDs)
		return
	}

	// 1. Check for Quote (Reply)
	if msg.Quote != nil {
		if jobID, exists := m.routeTable[msg.Quote.ID]; exists {
			targetJobID = jobID
			resolvedVia = "quote"
		} else {
			// Stale route — try extracting tag from quoted text
			targetJobID = m.extractTagFromText(msg.Quote.Text, activeIDs)
			if targetJobID != "" {
				resolvedVia = "quote_tag_fallback"
			}
			m.ulog.Debug("Quote route miss").
				Field("quote_id", msg.Quote.ID).
				Field("recovered_job_id", targetJobID).
				Log(ctx)
		}
	}

	// 2. Fresh Message — Check for @tag
	if targetJobID == "" && strings.HasPrefix(text, "@") {
		tag, rest := parseTag(text)
		targetJobID = m.resolveTagFrom(tag, activeIDs)
		if targetJobID != "" {
			text = rest
			resolvedVia = "tag"
		}
	}

	// 3. Fallback routing — use unified active set
	if targetJobID == "" {
		count := len(activeIDs)
		if count == 1 {
			for id := range activeIDs {
				targetJobID = id
			}
			resolvedVia = "single_active_fallback"
		} else {
			m.mu.Unlock()
			m.routeUnresolved(ctx, msg, text, activeIDs)
			return
		}
	}

	m.mu.Unlock()

	m.deliverInbound(ctx, targetJobID, resolvedVia, msg, text, false)
}

// routeUnresolved handles inbound the cascade could not address: nothing
// active, or several claws active and no quote or @tag to choose between them.
//
// Before phase 3 both cases ended in a drop. They now end at the ecosystem's
// DEFAULT CLAW when it has one (spec §3.4): ad-hoc feature claws keep their
// @tag/quote discipline, and the standing assistant catches everything else, so
// an untagged "make me a plan for X" texted from a phone lands on exactly the
// agent whose job is making plans. A drop is still the answer for an ecosystem
// that never opted in.
func (m *Manager) routeUnresolved(ctx context.Context, msg channels.InboundMessage, text string, activeIDs map[string]bool) {
	claw := LoadDefaultClaw()

	if jobID, ok := m.liveDefaultClaw(); ok {
		m.ulog.Info("Inbound routed to the default claw").
			Field("job_id", jobID).
			Field("plan", claw.Plan).
			Field("active_sessions", len(activeIDs)).
			Log(ctx)
		m.deliverInbound(ctx, jobID, "default_claw", msg, text, false)
		return
	}

	// The ecosystem has an assistant, it just is not up. That is mail, not an
	// error: wake the supervisor, park the message, deliver it on attach.
	if claw.IsEndpoint() {
		m.queueForAssistant(msg, text, "inbound_no_assistant")
		return
	}

	if len(activeIDs) > 1 {
		m.ulog.Warn("Inbound message unroutable — multiple active agents").
			Field("active_sessions", len(activeIDs)).
			Log(ctx)
		m.recordInbound(msg.Source, "dropped", "", "multiple active agents", false)
		m.replyWithAgentList(msg.Source, msg.GroupID, activeIDs)
		return
	}
	m.ulog.Warn("Inbound message dropped — no active agents").Log(ctx)
	m.recordInbound(msg.Source, "dropped", "", "no active agents", false)
	m.pokeAssistant("inbound_no_agents")
}

// deliverInbound tags a message with its channel provenance and delivers it to
// targetJobID, forwarding across daemons when the session lives elsewhere.
//
// fromQueue marks a message that already came out of the ensure-on-inbound
// buffer. Such a message must never be re-queued on failure — that is a loop —
// so its sender gets the error instead.
func (m *Manager) deliverInbound(ctx context.Context, targetJobID, resolvedVia string, msg channels.InboundMessage, text string, fromQueue bool) {
	m.ulog.Info("Inbound message routed").
		Field("job_id", targetJobID).
		Field("resolved_via", resolvedVia).
		Log(ctx)

	// Update LastSender (and LastSenderGroup if from a group)
	m.store.ApplyUpdate(store.Update{
		Type:   store.UpdateSessionLastSender,
		Source: "channels",
		Payload: &store.SessionLastSenderPayload{
			JobID:           targetJobID,
			LastSender:      msg.Source,
			LastSenderGroup: msg.GroupID,
		},
	})

	// Cross-daemon routing: if routing.json maps this jobID to another
	// daemon's socket, forward the input there instead of looking it up
	// in our local store.
	// Format context tag: resolve phone to name if possible
	senderLabel := msg.Source
	for name, phone := range m.signalCfg.Contacts {
		if phone == msg.Source {
			senderLabel = name
			break
		}
	}
	var taggedText string
	if msg.GroupID != "" {
		taggedText = fmt.Sprintf("[via Signal from %s] %s", senderLabel, text)
	} else {
		taggedText = fmt.Sprintf("[via Signal from %s] %s", senderLabel, text)
	}

	if sockPath, ok := m.lookupInboundRoute(targetJobID); ok && sockPath != "" && sockPath != m.socketPath {
		if staleRoute, err := m.forwardSessionInput(ctx, sockPath, targetJobID, taggedText); err != nil {
			log := m.ulog.Warn("Cross-daemon inbound forward failed").
				Err(err).
				Field("job_id", targetJobID).
				Field("socket", sockPath).
				Field("stale_route", staleRoute)
			log.Log(ctx)
			// Only a route that cannot lead anywhere is worth discarding. A
			// delivery failure from a daemon that does own the session (its
			// recorded mux is wrong, its PTY is gone) is a delivery problem;
			// dropping the route on top of it would also lose the address.
			if staleRoute {
				_ = m.removeInboundRoute(targetJobID)
			}
			m.recordInbound(msg.Source, resolvedVia, targetJobID, err.Error(), false)
			m.onUndeliverable(targetJobID, msg, text, fromQueue, err)
			return
		}
		m.ulog.Success("Signal message forwarded to scoped daemon").
			Field("job_id", targetJobID).
			Field("socket", sockPath).
			Log(ctx)
		m.recordInbound(msg.Source, resolvedVia, targetJobID, "", true)
		return
	}

	// Route to agent.
	//
	// Note: we intentionally do NOT prune on a local store miss anymore.
	// Under the cross-daemon model the global daemon's store is
	// ephemeral — sessions live in scoped daemons, or in the global
	// daemon but get repopulated lazily by hooks. A transient store miss
	// immediately after daemon restart is normal and doesn't mean the
	// session is dead. SendInput handles its own "session not found"
	// error; we log and move on without mutating activeSessions. The
	// periodic routeCleanup handles genuinely stale entries via TTL.
	if m.SendInput == nil {
		m.ulog.Error("SendInput not wired on Manager — message dropped").
			Field("job_id", targetJobID).
			Log(ctx)
		m.recordInbound(msg.Source, resolvedVia, targetJobID, "SendInput not wired", false)
		return
	}

	session := m.store.GetSession(targetJobID)
	injectLog := m.ulog.Info("Injecting signal message into agent").
		Field("job_id", targetJobID).
		Field("input_len", len(taggedText))
	if session != nil {
		injectLog = injectLog.
			Field("mux", session.Mux).
			Field("tmux_target", session.TmuxTarget).
			Field("pty_id", session.PtyID)
	} else {
		injectLog = injectLog.Field("store_entry", "missing")
	}
	injectLog.Log(ctx)

	if err := m.SendInput(ctx, targetJobID, taggedText); err != nil {
		m.ulog.Error("Failed to inject signal message into agent").
			Err(err).
			Field("job_id", targetJobID).
			Log(ctx)
		m.recordInbound(msg.Source, resolvedVia, targetJobID, err.Error(), false)
		m.onUndeliverable(targetJobID, msg, text, fromQueue, err)
		return
	}
	m.ulog.Success("Signal message injected").Field("job_id", targetJobID).Log(ctx)
	m.recordInbound(msg.Source, resolvedVia, targetJobID, "", true)
}

// onUndeliverable handles a message that was addressed correctly and still did
// not arrive. A claw whose session no longer accepts input is a dead agent with
// a live route; when it is the ASSISTANT's, that is the exact condition
// ensure-on-inbound exists for, so the message goes back into the queue behind
// a supervisor poke rather than being lost. For any other agent there is
// nothing to restart, so the poke stays the best-effort nudge it was.
func (m *Manager) onUndeliverable(targetJobID string, msg channels.InboundMessage, text string, fromQueue bool, cause error) {
	if !fromQueue {
		if claw := LoadDefaultClaw(); claw.IsEndpoint() && claw.JobID == targetJobID {
			// The registration is a fossil: the session behind it is gone.
			// Retire it so the flush waits for a genuinely new claw.
			m.clearDefaultClawJob(targetJobID)
			m.queueForAssistant(msg, text, "inbound_undeliverable")
			return
		}
		m.pokeAssistant("inbound_undeliverable")
		return
	}
	m.replyOverSignal(msg.Source, msg.GroupID,
		fmt.Sprintf("Assistant could not receive your message: %v", cause))
}

// pokeAssistant asks the assistant supervisor to ensure a live assistant, off
// the inbound path. Best-effort in both directions: no supervisor means no
// poke, and a poke never delays or fails the message it rode in on.
func (m *Manager) pokeAssistant(reason string) {
	if m.EnsureAssistant == nil {
		return
	}
	go func() {
		if err := m.EnsureAssistant(context.Background(), reason); err != nil {
			m.ulog.Warn("Assistant ensure failed").Err(err).
				Field("reason", reason).Log(context.Background())
		}
	}()
}

// startSignalChannel starts the signal-cli daemon process.
func (m *Manager) startSignalChannel(ctx context.Context) {
	ch := signal.NewChannel(signal.Config{
		CLIPath:   m.signalCfg.CLIPath,
		Account:   m.signalCfg.Account,
		Allowlist: m.signalCfg.Allowlist,
		Groups:    m.signalCfg.Groups,
	})

	if err := ch.Start(ctx, m.handleInbound); err != nil {
		m.ulog.Error("Failed to start Signal channel").Err(err).
			Field("event", "channel.disabled").Log(ctx)
		m.mu.Lock()
		m.isRunning = false
		m.signalStartErr = err
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	m.signalChannel = ch
	m.signalStartErr = nil
	close(m.ready) // Signal that we're ready
	m.mu.Unlock()

	m.ulog.Info("Signal channel started").Log(ctx)
}

// tagMessage prepends a session tag to outbound messages.
// jobTitle is an explicit title passed across the daemon boundary;
// falls back to the store lookup if empty.
func (m *Manager) tagMessage(jobID, jobTitle, message string) string {
	if jobTitle != "" {
		return fmt.Sprintf("[%s] %s", jobTitle, message)
	}
	session := m.store.GetSession(jobID)
	if session != nil && session.JobTitle != "" {
		return fmt.Sprintf("[%s] %s", session.JobTitle, message)
	}
	return message
}

// handleCommand processes !commands sent via Signal.
func (m *Manager) handleCommand(ctx context.Context, sender, groupID, text string, activeIDs map[string]bool) {
	cmd := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(text)), "!")
	cmd = strings.SplitN(cmd, " ", 2)[0]

	var reply string
	switch cmd {
	case "claws", "agents", "status":
		if len(activeIDs) == 0 {
			reply = "No active claws."
		} else {
			lines := []string{fmt.Sprintf("Active claws (%d):", len(activeIDs))}
			for id := range activeIDs {
				title := id
				if session := m.store.GetSession(id); session != nil && session.JobTitle != "" {
					title = session.JobTitle
				} else if idx := strings.LastIndex(id, "-"); idx > 0 {
					title = id[:idx]
				}
				lines = append(lines, fmt.Sprintf("  @%s", title))
			}
			reply = strings.Join(lines, "\n")
		}
	case "health":
		status := "alive"
		if m.signalChannel != nil {
			st := m.signalChannel.Status()
			if !st.IsAlive {
				status = fmt.Sprintf("dead (restarts: %d)", st.RestartCount)
			}
		}
		reply = fmt.Sprintf("Signal: %s\nClaws: %d\nQuote routes: %d", status, len(activeIDs), len(m.routeTable))
	case "unclaw":
		parts := strings.SplitN(cmd+" "+strings.SplitN(text, " ", 2)[1], " ", 2)
		tag := strings.TrimSpace(parts[1])
		if tag == "" {
			reply = "Usage: !unclaw <agent>"
		} else {
			targetID := m.resolveTagFrom(strings.TrimPrefix(tag, "@"), activeIDs)
			if targetID == "" {
				reply = fmt.Sprintf("No active claw matching '%s'", tag)
			} else {
				m.DisableChannel(ctx, targetID)
				m.cleanupRoutesForJob(targetID)
				if session := m.store.GetSession(targetID); session != nil && session.JobFilePath != "" {
					stripClawFrontmatter(session.JobFilePath)
				}
				title := targetID
				if idx := strings.LastIndex(targetID, "-"); idx > 0 {
					title = targetID[:idx]
				}
				reply = fmt.Sprintf("Unclawed @%s", title)
			}
		}
	default:
		reply = "Commands: !claws, !health, !unclaw <agent>"
	}

	m.ulog.Info("handling !command").
		Field("cmd", cmd).
		Field("reply_len", len(reply)).
		Field("sender", sender).
		StructuredOnly().Log(ctx)

	m.mu.Lock()
	ch := m.signalChannel
	m.mu.Unlock()
	if ch != nil && reply != "" {
		sendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := ch.Send(sendCtx, channels.OutboundMessage{Recipient: sender, GroupID: groupID, Message: reply})
		if err != nil {
			m.ulog.Error("!command reply failed").Err(err).Field("cmd", cmd).StructuredOnly().Log(ctx)
		} else {
			m.ulog.Info("!command reply sent").Field("cmd", cmd).StructuredOnly().Log(ctx)
		}
	} else {
		m.ulog.Warn("!command reply skipped").Field("ch_nil", ch == nil).Field("reply_empty", reply == "").StructuredOnly().Log(ctx)
	}
}

// recordRoute stores a timestamp→jobID mapping and persists to disk.
func (m *Manager) recordRoute(timestamp int64, jobID string) {
	m.mu.Lock()
	m.routeTable[timestamp] = jobID
	m.mu.Unlock()
	m.saveRoutes()
}

// resolveTagFrom matches a tag against session titles/IDs in the given set.
// When the store has no entry (scoped sessions on the global daemon), it
// falls back to matching the tag against the job ID string itself.
func (m *Manager) resolveTagFrom(tag string, activeIDs map[string]bool) string {
	tag = strings.ToLower(tag)
	for id := range activeIDs {
		session := m.store.GetSession(id)
		if session != nil {
			title := session.JobTitle
			m.ulog.Debug("tag match attempt (store)").
				Field("tag", tag).
				Field("id", id).
				Field("job_title", title).
				StructuredOnly().Log(m.ctx)
			if strings.EqualFold(title, tag) || strings.EqualFold(id, tag) {
				return id
			}
			continue
		}
		// Fallback for scoped sessions missing from the global store
		idLower := strings.ToLower(id)
		m.ulog.Debug("tag match attempt (id fallback)").
			Field("tag", tag).
			Field("id", id).
			StructuredOnly().Log(m.ctx)
		if idLower == tag || strings.HasPrefix(idLower, tag+"-") {
			return id
		}
	}
	return ""
}

// extractTagFromText tries to find a [tag] in quoted text.
func (m *Manager) extractTagFromText(text string, activeIDs map[string]bool) string {
	if idx := strings.Index(text, "["); idx >= 0 {
		if end := strings.Index(text[idx:], "]"); end > 0 {
			tag := text[idx+1 : idx+end]
			return m.resolveTagFrom(tag, activeIDs)
		}
	}
	return ""
}

// replyWithAgentList sends a Signal message listing active agents.
func (m *Manager) replyWithAgentList(recipient, groupID string, activeIDs map[string]bool) {
	m.mu.Lock()
	ch := m.signalChannel
	m.mu.Unlock()

	var agents []string
	for id := range activeIDs {
		title := id
		if session := m.store.GetSession(id); session != nil && session.JobTitle != "" {
			title = session.JobTitle
		} else if idx := strings.LastIndex(id, "-"); idx > 0 {
			title = id[:idx]
		}
		agents = append(agents, fmt.Sprintf("  @%s", title))
	}

	if ch != nil {
		msg := "Multiple agents active. Reply to a specific message or use @tag:\n" + strings.Join(agents, "\n")
		_, _ = ch.Send(context.Background(), channels.OutboundMessage{
			Recipient: recipient,
			GroupID:   groupID,
			Message:   msg,
		})
	}
}

// parseTag extracts "@tag rest" from a message. Splits on the first
// whitespace character (space, newline, etc.) so tags typed on a phone
// with a newline separator still route correctly.
func parseTag(text string) (tag, rest string) {
	text = strings.TrimPrefix(text, "@")
	idx := strings.IndexAny(text, " \t\n\r")
	if idx < 0 {
		return text, ""
	}
	return text[:idx], strings.TrimLeftFunc(text[idx:], func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

// watchStoreUpdates listens for session lifecycle and job discovery events.
func (m *Manager) watchStoreUpdates(ctx context.Context) {
	ch := m.store.Subscribe()
	defer m.store.Unsubscribe(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case u := <-ch:
			switch u.Type {
			case store.UpdateSessionEnd:
				if payload, ok := u.Payload.(*store.SessionEndPayload); ok {
					session := m.store.GetSession(payload.JobID)
					isTerminal := payload.Outcome == "completed" || payload.Outcome == "failed" ||
						payload.Outcome == "interrupted" || payload.Outcome == "abandoned"

					m.ulog.Info("watchStoreUpdates: received UpdateSessionEnd").
						Field("job_id", payload.JobID).
						Field("outcome", payload.Outcome).
						Field("is_terminal", isTerminal).
						Field("has_session", session != nil).
						Field("has_channels", session != nil && hasChannel(session.Channels)).
						Log(ctx)

					if session != nil && hasChannel(session.Channels) && !isTerminal {
						m.ulog.Info("watchStoreUpdates: ignoring spurious session end").
							Field("job_id", payload.JobID).
							Log(ctx)
						continue
					}

					m.ulog.Info("watchStoreUpdates: disabling channel due to session end").
						Field("job_id", payload.JobID).
						Log(ctx)
					m.DisableChannel(ctx, payload.JobID)
					m.cleanupRoutesForJob(payload.JobID)
					if isTerminal && session != nil && session.JobFilePath != "" {
						stripClawFrontmatter(session.JobFilePath)
					}
				}
			case store.UpdateSessionTmuxTarget:
				// Persist delivery info to state.json for restart resilience.
				if payload, ok := u.Payload.(*store.SessionTmuxTargetPayload); ok {
					if m.isActive(payload.JobID) {
						m.saveSessionDeliveryTmuxTarget(payload.JobID, payload.TmuxTarget)
					}
				}
			case store.UpdateJobsDiscovered:
				jobs, ok := u.Payload.([]*models.JobInfo)
				if !ok {
					continue
				}
				for _, job := range jobs {
					if !hasChannel(job.Channels) || m.isActive(job.ID) {
						continue
					}
					// Only rehydrate channels for jobs that are actually running
					if job.Status != "running" && job.Status != "idle" && job.Status != "pending_user" {
						continue
					}
					if err := m.EnableChannel(ctx, job.ID, job.Channels...); err == nil {
						m.ulog.Info("Rehydrated channel from discovered job").
							Field("job_id", job.ID).
							Field("status", job.Status).
							Log(ctx)
					}
				}
			}
		}
	}
}

func hasChannel(chList []string) bool {
	for _, ch := range chList {
		if ch == "signal" || ch == "ha" {
			return true
		}
	}
	return false
}

// isSandboxScope returns true if this daemon's scope looks like a test
// sandbox temp directory. Test-spawned daemons must not write to the
// host's routing.json or they hijack inbound message delivery.
func (m *Manager) isSandboxScope() bool {
	return strings.Contains(m.scope, "/grove-tend-") ||
		strings.HasPrefix(m.scope, os.TempDir()) ||
		strings.HasPrefix(m.scope, "/private/var/folders/") ||
		strings.HasPrefix(m.scope, "/tmp/")
}

// saveSessionDelivery persists a session's mux/target to state.json. Called
// when a channel is enabled: state.json is what routing falls back to after a
// restart (and on any daemon whose in-memory store never saw the session), so
// the record has to be written from the live session — mux and pty_id
// included — the moment the session becomes reachable over a channel.
func (m *Manager) saveSessionDelivery(jobID string) {
	if m.store == nil {
		return
	}
	session := m.store.GetSession(jobID)
	if session == nil {
		return
	}
	if session.Mux == "" && session.TmuxTarget == "" && session.PtyID == "" {
		return
	}
	writeSessionDelivery(jobID, SessionDeliveryInfo{
		Mux:        session.Mux,
		TmuxTarget: session.TmuxTarget,
		PtyID:      session.PtyID,
	}, false)
}

// saveSessionDeliveryTmuxTarget records a tmux target against a session's
// delivery state without disturbing what is already known about how to reach
// it.
//
// The old version of this wrote {mux: "tmux", pty_id: ""} unconditionally, so
// a single `flow agent claw` — which stamped a synthesized tmux pane name onto
// every agent, including out-of-process ones — silently demoted a live
// treemux/tuimux session's persisted route to a tmux pane that never existed.
// Inbound Signal messages then ran send-keys against nothing. The tmux target
// is now additive: mux and pty_id come from the live session, fall back to
// whatever was already recorded, and only settle on "tmux" when a tmux target
// really is all anyone knows.
func (m *Manager) saveSessionDeliveryTmuxTarget(jobID, tmuxTarget string) {
	var mux, ptyID string
	if m.store != nil {
		if session := m.store.GetSession(jobID); session != nil {
			mux, ptyID = session.Mux, session.PtyID
		}
	}

	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil {
		return
	}
	prev := state.SessionDelivery[jobID]
	if ptyID == "" {
		ptyID = prev.PtyID
	}
	if mux == "" {
		mux = prev.Mux
	}
	if mux == "" && ptyID == "" {
		mux = models.MuxTmux
	}
	next := SessionDeliveryInfo{Mux: mux, TmuxTarget: tmuxTarget, PtyID: ptyID}
	if next == prev {
		return
	}
	state.SessionDelivery[jobID] = next
	_ = saveStateAtomic(state)
}

// SyncSessionDelivery refreshes an EXISTING delivery record with a session's
// live route. Routing calls it with what the in-memory store knows, so a
// channel-enabled session whose PTY was created — or re-adopted after an
// upgrade — later than its delivery record heals itself instead of routing
// forever by a stale mux label. It never creates a record: a session with no
// channel enabled has no business in state.json, and DisableChannel is what
// takes records out again.
func SyncSessionDelivery(jobID, mux, tmuxTarget, ptyID string) {
	writeSessionDelivery(jobID, SessionDeliveryInfo{
		Mux:        mux,
		TmuxTarget: tmuxTarget,
		PtyID:      ptyID,
	}, true)
}

// writeSessionDelivery persists info for jobID, skipping the write when
// nothing changed. With requireExisting set it only refreshes a record that is
// already there.
func writeSessionDelivery(jobID string, info SessionDeliveryInfo, requireExisting bool) {
	if info == (SessionDeliveryInfo{}) {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil {
		return
	}
	prev, ok := state.SessionDelivery[jobID]
	if requireExisting && !ok {
		return
	}
	if ok && prev == info {
		return
	}
	state.SessionDelivery[jobID] = info
	_ = saveStateAtomic(state)
}

// removeSessionDelivery removes persisted delivery info for a job ID.
func (m *Manager) removeSessionDelivery(jobID string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil {
		return
	}
	if _, ok := state.SessionDelivery[jobID]; ok {
		delete(state.SessionDelivery, jobID)
		_ = saveStateAtomic(state)
	}
}

// GetSessionDelivery returns persisted delivery info for a job ID.
func GetSessionDelivery(jobID string) *SessionDeliveryInfo {
	state, err := loadChannelState()
	if err != nil {
		return nil
	}
	info, ok := state.SessionDelivery[jobID]
	if !ok {
		return nil
	}
	return &info
}

func (m *Manager) isActive(jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeSessions[jobID] || m.haActiveSess[jobID]
}

// backgroundPrune periodically prunes stale sessions instead of at boot.
func (m *Manager) backgroundPrune(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pruneStaleSessions(ctx)
		}
	}
}

// cleanupRoutesForJob removes all route entries for a specific job.
func (m *Manager) cleanupRoutesForJob(jobID string) {
	m.ulog.Info("cleanupRoutesForJob executing").
		Field("job_id", jobID).
		Log(m.ctx)
	m.mu.Lock()
	for ts, id := range m.routeTable {
		if id == jobID {
			delete(m.routeTable, ts)
		}
	}
	isProxy := m.globalClient != nil
	m.mu.Unlock()
	go m.saveRoutes()
	if isProxy {
		_ = m.removeInboundRoute(jobID)
	}
}

// routeCleanup periodically purges stale routes older than 7 days.
func (m *Manager) routeCleanup(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	cutoff := 7 * 24 * time.Hour

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			now := time.Now().UnixMilli()
			for ts := range m.routeTable {
				if time.Duration(now-ts)*time.Millisecond > cutoff {
					delete(m.routeTable, ts)
				}
			}
			m.mu.Unlock()
			m.saveRoutes()
		}
	}
}

// --- Unified channel state persistence (channels/state.json) ---
//
// state.json consolidates the formerly separate routing.json (cross-daemon
// inbound routes) and signal_routes.json (quote-reply routing) into a
// single atomic file. Writes use tmp-file + rename.

// SessionDeliveryInfo holds mux delivery state for a channel-enabled session.
type SessionDeliveryInfo struct {
	Mux        string `json:"mux,omitempty"`
	TmuxTarget string `json:"tmux_target,omitempty"`
	PtyID      string `json:"pty_id,omitempty"`
}

// ChannelState is the on-disk representation of all channel routing state.
type ChannelState struct {
	InboundRoutes   map[string]string              `json:"inbound_routes"`             // jobID → socketPath
	QuoteRoutes     map[int64]string               `json:"quote_routes"`               // timestamp → jobID
	SessionDelivery map[string]SessionDeliveryInfo `json:"session_delivery,omitempty"` // jobID → delivery info
	DefaultClaw     *DefaultClawInfo               `json:"default_claw,omitempty"`     // the ecosystem's standing assistant claw
}

func stateFilePath() string {
	return filepath.Join(paths.StateDir(), "channels", "state.json")
}

func loadChannelState() (*ChannelState, error) {
	data, err := os.ReadFile(stateFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &ChannelState{
				InboundRoutes:   map[string]string{},
				QuoteRoutes:     map[int64]string{},
				SessionDelivery: map[string]SessionDeliveryInfo{},
			}, nil
		}
		return nil, err
	}
	var state ChannelState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.InboundRoutes == nil {
		state.InboundRoutes = map[string]string{}
	}
	if state.QuoteRoutes == nil {
		state.QuoteRoutes = map[int64]string{}
	}
	if state.SessionDelivery == nil {
		state.SessionDelivery = map[string]SessionDeliveryInfo{}
	}
	return &state, nil
}

// saveStateAtomic writes the unified state file atomically via tmp + rename.
func saveStateAtomic(state *ChannelState) error {
	path := stateFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: daemon state directory
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// stateMu serializes read-modify-write cycles within the same daemon.
var stateMu sync.Mutex

func (m *Manager) loadRoutes() {
	state, err := loadChannelState()
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routeTable = state.QuoteRoutes
}

func (m *Manager) saveRoutes() {
	stateMu.Lock()
	defer stateMu.Unlock()

	state, err := loadChannelState()
	if err != nil {
		state = &ChannelState{InboundRoutes: map[string]string{}, QuoteRoutes: map[int64]string{}}
	}

	m.mu.Lock()
	state.QuoteRoutes = m.routeTable
	m.mu.Unlock()

	_ = saveStateAtomic(state)
}

func (m *Manager) addInboundRoute(jobID string) error {
	m.ulog.Info("addInboundRoute executing").
		Field("job_id", jobID).
		Field("socket", m.socketPath).
		Field("is_sandbox", m.isSandboxScope()).
		Log(m.ctx)
	if m.socketPath == "" {
		return fmt.Errorf("manager has no socketPath; cannot register inbound route")
	}
	if m.isSandboxScope() {
		m.mu.Lock()
		m.activeSessions[jobID] = true
		m.mu.Unlock()
		return nil
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil {
		return err
	}
	state.InboundRoutes[jobID] = m.socketPath
	return saveStateAtomic(state)
}

func (m *Manager) removeInboundRoute(jobID string) error {
	m.ulog.Info("removeInboundRoute executing").
		Field("job_id", jobID).
		Log(m.ctx)
	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil {
		return err
	}
	if _, ok := state.InboundRoutes[jobID]; !ok {
		return nil
	}
	delete(state.InboundRoutes, jobID)
	return saveStateAtomic(state)
}

func (m *Manager) lookupInboundRoute(jobID string) (string, bool) {
	state, err := loadChannelState()
	if err != nil {
		return "", false
	}
	sock, ok := state.InboundRoutes[jobID]
	return sock, ok
}

// forwardSessionInput POSTs /api/sessions/{jobID}/input to the given scoped
// daemon socket. Used by the global daemon to hand inbound Signal messages
// to the scoped daemon that actually owns the session's PTY.
//
// The first return value reports whether the failure means the ROUTE is wrong
// (unreachable daemon, or one that has never heard of the session) as opposed
// to a live daemon failing to deliver — only the former should cost the
// session its route.
func (m *Manager) forwardSessionInput(ctx context.Context, socketPath, jobID, input string) (bool, error) {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 10 * time.Second,
	}
	body, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://unix/api/sessions/"+jobID+"/input", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		// The socket is unreachable: the scoped daemon is gone, so the route is
		// what's wrong.
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		// Carry the scoped daemon's own explanation across the hop. Without it
		// the operator sees a bare "returned 500 Internal Server Error" and has
		// to go read the other daemon's log to learn that, say, the session's
		// recorded tmux pane does not exist.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		reason := strings.TrimSpace(string(detail))
		if reason == "" {
			reason = resp.Status
		}
		// A 404 means that daemon does not have the session — the route is
		// stale. Anything else is a live daemon failing to deliver: keep the
		// route so the next message still reaches the daemon that owns the
		// session.
		stale := resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone
		return stale, fmt.Errorf("scoped daemon at %s returned %s: %s", socketPath, resp.Status, reason)
	}
	return false, nil
}

// stripClawFrontmatter removes channels and autonomous blocks from a job
// file's YAML frontmatter so the claw is not rehydrated on daemon restart.
func stripClawFrontmatter(filePath string) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	s := string(content)

	// Remove channels line (e.g. "channels: [signal]" or "channels:\n  - signal")
	if idx := strings.Index(s, "\nchannels:"); idx >= 0 {
		end := idx + 1
		line := s[end:]
		lineEnd := strings.Index(line, "\n")
		if lineEnd >= 0 {
			end += lineEnd + 1
			remaining := s[end:]
			for strings.HasPrefix(remaining, "- ") || strings.HasPrefix(remaining, "  - ") {
				nextLine := strings.Index(remaining, "\n")
				if nextLine >= 0 {
					end += nextLine + 1
					remaining = s[end:]
				} else {
					end += len(remaining)
					break
				}
			}
		} else {
			end += len(line)
		}
		s = s[:idx+1] + s[end:]
	}

	// Remove signal_target line
	if idx := strings.Index(s, "\nsignal_target:"); idx >= 0 {
		end := idx + 1
		line := s[end:]
		lineEnd := strings.Index(line, "\n")
		if lineEnd >= 0 {
			end += lineEnd + 1
		} else {
			end += len(line)
		}
		s = s[:idx+1] + s[end:]
	}

	// Remove autonomous block
	if idx := strings.Index(s, "autonomous:\n"); idx >= 0 {
		end := idx
		lines := strings.Split(s[idx:], "\n")
		end += len(lines[0]) + 1
		for i := 1; i < len(lines); i++ {
			if strings.HasPrefix(lines[i], "  ") {
				end += len(lines[i]) + 1
			} else {
				break
			}
		}
		s = s[:idx] + s[end:]
	}

	if s != string(content) {
		_ = os.WriteFile(filePath, []byte(s), 0o600) //nolint:gosec // G306: job file permissions
	}
}
