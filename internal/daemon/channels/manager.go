// Package channels manages external messaging channels for the grove daemon.
// It owns the routing table, ref-counting, and message dispatch.
// The actual channel implementations live in notify/pkg/channels/.
package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/grovetools/notify/pkg/channels/signal"
)

// SignalConfig holds the configuration needed to create a Signal channel.
type SignalConfig struct {
	Enabled   bool
	CLIPath   string
	Account   string
	Allowlist []string
}

// Manager manages external messaging channels and routes messages to/from agent sessions.
type Manager struct {
	mu             sync.Mutex
	store          *store.Store
	signalCfg      SignalConfig
	signalChannel  channels.Channel
	activeSessions map[string]bool  // jobID → true for sessions with signal enabled
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

	recentInbound    [10]models.InboundRecord
	recentInboundIdx int
	recentInboundLen int
	lastInboundAt    time.Time
}

// NewManager creates a new ChannelManager. scope is the daemon's scope
// ("" for the global daemon); socketPath is this daemon's own socket path
// (used by scoped daemons to register inbound routes in routing.json).
func NewManager(st *store.Store, cfg SignalConfig, scope, socketPath string) *Manager {
	return &Manager{
		store:          st,
		signalCfg:      cfg,
		scope:          scope,
		socketPath:     socketPath,
		activeSessions: make(map[string]bool),
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
	m.mu.Lock()
	stale := make([]string, 0, len(m.activeSessions))
	for jobID := range m.activeSessions {
		if m.store.GetSession(jobID) == nil {
			stale = append(stale, jobID)
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
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}

	if m.signalChannel != nil {
		_ = m.signalChannel.Stop(ctx)
		m.signalChannel = nil
		m.isRunning = false
	}

	m.saveRoutes()
	m.ulog.Info("Channel manager stopped").Log(ctx)
}

// EnableChannel enables a channel for a session. Starts signal-cli if needed.
// On a scoped daemon (globalClient != nil) it registers this daemon's socket
// in routing.json instead of spawning signal-cli locally.
func (m *Manager) EnableChannel(_ context.Context, jobID string) error {
	m.mu.Lock()

	if !m.signalCfg.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("signal is not enabled in configuration")
	}

	m.activeSessions[jobID] = true
	isProxy := m.globalClient != nil

	if !isProxy && !m.isRunning {
		m.isRunning = true
		m.ready = make(chan struct{})
		go m.startSignalChannel(m.ctx) // Use manager's long-lived context, not request context
	}
	m.mu.Unlock()

	if isProxy && !m.isSandboxScope() {
		if err := m.addInboundRoute(jobID); err != nil {
			m.ulog.Warn("Failed to write inbound route").Err(err).Field("job_id", jobID).Log(m.ctx)
		}
	}

	m.ulog.Info("Channel enabled for session").Field("job_id", jobID).Log(m.ctx)
	return nil
}

// DisableChannel disables a channel for a session. On scoped daemons
// this removes the routing.json entry. signal-cli lifecycle is no longer
// ref-counted — on the global daemon it runs for the daemon's lifetime
// (started in Start, stopped in Stop). Ref-counting was wrong under
// cross-daemon because scoped claws live in routing.json, not in the
// global daemon's activeSessions.
func (m *Manager) DisableChannel(ctx context.Context, jobID string) {
	m.mu.Lock()
	delete(m.activeSessions, jobID)
	isProxy := m.globalClient != nil
	m.mu.Unlock()

	if isProxy {
		if err := m.removeInboundRoute(jobID); err != nil {
			m.ulog.Warn("Failed to remove inbound route").Err(err).Field("job_id", jobID).Log(ctx)
		}
	}
}

// Send sends a message via the signal channel and records the route.
// On scoped daemons (globalClient != nil), this is forwarded to the global
// daemon which owns signal-cli.
func (m *Manager) Send(ctx context.Context, req models.ChannelSendRequest) (*models.ChannelSendResponse, error) {
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

	// Resolve recipient
	recipient := req.Recipient
	if recipient == "" {
		// Check LastSender for this session
		session := m.store.GetSession(req.JobID)
		if session != nil && session.LastSender != "" {
			recipient = session.LastSender
		} else {
			// Broadcast to all allowlisted contacts
			for _, contact := range m.signalCfg.Allowlist {
				taggedMsg := m.tagMessage(req.JobID, req.Message)
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

	taggedMsg := m.tagMessage(req.JobID, req.Message)
	result, err := ch.Send(ctx, channels.OutboundMessage{
		Recipient: recipient,
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
	}

	if m.signalChannel != nil {
		st := m.signalChannel.Status()
		resp.SignalIsAlive = st.IsAlive
		resp.SignalRestartCount = st.RestartCount
		if !st.LastRestartAt.IsZero() {
			resp.SignalLastRestart = &st.LastRestartAt
		}
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

// handleInbound routes an inbound message to the correct agent session.
func (m *Manager) handleInbound(msg channels.InboundMessage) {
	ctx := context.Background()
	text := msg.Message
	var targetJobID string
	var resolvedVia string

	m.mu.Lock()

	quoteID := int64(0)
	if msg.Quote != nil {
		quoteID = msg.Quote.ID
	}
	m.ulog.Debug("Inbound signal message").
		Field("source", msg.Source).
		Field("text_len", len(text)).
		Field("quote_id", quoteID).
		Field("active_sessions", len(m.activeSessions)).
		Field("route_table_size", len(m.routeTable)).
		Log(ctx)

	// 1. Check for Quote (Reply)
	if msg.Quote != nil {
		if jobID, exists := m.routeTable[msg.Quote.ID]; exists {
			targetJobID = jobID
			resolvedVia = "quote"
		} else {
			// Stale route — try extracting tag from quoted text
			targetJobID = m.extractTagFromText(msg.Quote.Text)
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
		targetJobID = m.resolveTag(tag)
		if targetJobID != "" {
			text = rest
			resolvedVia = "tag"
		}
	}

	// 3. Fallback routing
	if targetJobID == "" {
		count := len(m.activeSessions)
		if count == 1 {
			for id := range m.activeSessions {
				targetJobID = id
			}
			resolvedVia = "single_active_fallback"
		} else if count > 1 {
			m.mu.Unlock()
			m.ulog.Warn("Inbound message unroutable — multiple active agents").
				Field("active_sessions", count).
				Log(ctx)
			m.recordInbound(msg.Source, "dropped", "", "multiple active agents", false)
			m.replyWithAgentList(msg.Source)
			return
		} else {
			m.mu.Unlock()
			m.ulog.Warn("Inbound message dropped — no active agents").Log(ctx)
			m.recordInbound(msg.Source, "dropped", "", "no active agents", false)
			return
		}
	}

	m.mu.Unlock()

	m.ulog.Info("Inbound message routed").
		Field("job_id", targetJobID).
		Field("resolved_via", resolvedVia).
		Log(ctx)

	// Update LastSender
	m.store.ApplyUpdate(store.Update{
		Type:   store.UpdateSessionLastSender,
		Source: "channels",
		Payload: &store.SessionLastSenderPayload{
			JobID:      targetJobID,
			LastSender: msg.Source,
		},
	})

	// Cross-daemon routing: if routing.json maps this jobID to another
	// daemon's socket, forward the input there instead of looking it up
	// in our local store.
	if sockPath, ok := m.lookupInboundRoute(targetJobID); ok && sockPath != "" && sockPath != m.socketPath {
		taggedText := fmt.Sprintf("[via Signal] %s", text)
		if err := m.forwardSessionInput(ctx, sockPath, targetJobID, taggedText); err != nil {
			m.ulog.Warn("Cross-daemon inbound forward failed — purging route").
				Err(err).
				Field("job_id", targetJobID).
				Field("socket", sockPath).
				Log(ctx)
			_ = m.removeInboundRoute(targetJobID)
			m.recordInbound(msg.Source, resolvedVia, targetJobID, err.Error(), false)
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
	taggedText := fmt.Sprintf("[via Signal] %s", text)
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
		return
	}
	m.ulog.Success("Signal message injected").Field("job_id", targetJobID).Log(ctx)
	m.recordInbound(msg.Source, resolvedVia, targetJobID, "", true)
}

// startSignalChannel starts the signal-cli daemon process.
func (m *Manager) startSignalChannel(ctx context.Context) {
	ch := signal.NewChannel(signal.Config{
		CLIPath:   m.signalCfg.CLIPath,
		Account:   m.signalCfg.Account,
		Allowlist: m.signalCfg.Allowlist,
	})

	if err := ch.Start(ctx, m.handleInbound); err != nil {
		m.ulog.Error("Failed to start Signal channel").Err(err).Log(ctx)
		m.mu.Lock()
		m.isRunning = false
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	m.signalChannel = ch
	close(m.ready) // Signal that we're ready
	m.mu.Unlock()

	m.ulog.Info("Signal channel started").Log(ctx)
}

// tagMessage prepends a session tag to outbound messages.
func (m *Manager) tagMessage(jobID, message string) string {
	session := m.store.GetSession(jobID)
	if session != nil && session.JobTitle != "" {
		return fmt.Sprintf("[%s] %s", session.JobTitle, message)
	}
	return message
}

// recordRoute stores a timestamp→jobID mapping and persists to disk.
func (m *Manager) recordRoute(timestamp int64, jobID string) {
	m.mu.Lock()
	m.routeTable[timestamp] = jobID
	m.mu.Unlock()
	m.saveRoutes()
}

// resolveTag matches a tag against active session titles/IDs.
func (m *Manager) resolveTag(tag string) string {
	tag = strings.ToLower(tag)
	for id := range m.activeSessions {
		session := m.store.GetSession(id)
		if session == nil {
			continue
		}
		if strings.EqualFold(session.JobTitle, tag) || strings.EqualFold(session.ID, tag) {
			return id
		}
	}
	return ""
}

// extractTagFromText tries to find a [tag] in quoted text.
func (m *Manager) extractTagFromText(text string) string {
	if idx := strings.Index(text, "["); idx >= 0 {
		if end := strings.Index(text[idx:], "]"); end > 0 {
			tag := text[idx+1 : idx+end]
			return m.resolveTag(tag)
		}
	}
	return ""
}

// replyWithAgentList sends a Signal message listing active agents.
func (m *Manager) replyWithAgentList(recipient string) {
	m.mu.Lock()
	ch := m.signalChannel
	var agents []string
	for id := range m.activeSessions {
		session := m.store.GetSession(id)
		if session != nil {
			agents = append(agents, fmt.Sprintf("  @%s", session.JobTitle))
		}
	}
	m.mu.Unlock()

	if ch != nil {
		msg := "Multiple agents active. Reply to a specific message or use @tag:\n" + strings.Join(agents, "\n")
		_, _ = ch.Send(context.Background(), channels.OutboundMessage{
			Recipient: recipient,
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
					// Don't disable channels for sessions that have channels
					// in their frontmatter — the frontmatter is the source of
					// truth for claw state. The session collector may emit
					// spurious ends (e.g., PID=0 after daemon restart) but the
					// agent is still running.
					session := m.store.GetSession(payload.JobID)
					if session != nil && hasSignalChannel(session.Channels) {
						continue
					}
					m.DisableChannel(ctx, payload.JobID)
					m.cleanupRoutesForJob(payload.JobID)
				}
			case store.UpdateSessionTmuxTarget:
				// Persist delivery info to state.json for restart resilience
				if payload, ok := u.Payload.(*store.SessionTmuxTargetPayload); ok {
					if m.isActive(payload.JobID) {
						m.saveSessionDelivery(payload.JobID)
					}
				}
			case store.UpdateJobsDiscovered:
				jobs, ok := u.Payload.([]*models.JobInfo)
				if !ok {
					continue
				}
				for _, job := range jobs {
					if hasSignalChannel(job.Channels) && !m.isActive(job.ID) {
						if err := m.EnableChannel(ctx, job.ID); err == nil {
							m.ulog.Info("Rehydrated channel from discovered job").
								Field("job_id", job.ID).
								Log(ctx)
						}
					}
				}
			}
		}
	}
}

func hasSignalChannel(channels []string) bool {
	for _, ch := range channels {
		if ch == "signal" {
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

// saveSessionDelivery persists a session's mux/target to state.json.
func (m *Manager) saveSessionDelivery(jobID string) {
	session := m.store.GetSession(jobID)
	if session == nil {
		return
	}
	if session.Mux == "" && session.TmuxTarget == "" && session.PtyID == "" {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil {
		return
	}
	state.SessionDelivery[jobID] = SessionDeliveryInfo{
		Mux:        session.Mux,
		TmuxTarget: session.TmuxTarget,
		PtyID:      session.PtyID,
	}
	_ = saveStateAtomic(state)
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
	return m.activeSessions[jobID]
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
	InboundRoutes   map[string]string              `json:"inbound_routes"`            // jobID → socketPath
	QuoteRoutes     map[int64]string               `json:"quote_routes"`              // timestamp → jobID
	SessionDelivery map[string]SessionDeliveryInfo  `json:"session_delivery,omitempty"` // jobID → delivery info
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
func (m *Manager) forwardSessionInput(ctx context.Context, socketPath, jobID, input string) error {
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
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://unix/api/sessions/"+jobID+"/input", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("scoped daemon returned %s", resp.Status)
	}
	return nil
}
