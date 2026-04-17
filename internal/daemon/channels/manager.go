// Package channels manages external messaging channels for the grove daemon.
// It owns the routing table, ref-counting, and message dispatch.
// The actual channel implementations live in notify/pkg/channels/.
package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/logging"
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
	activeSessions map[string]bool // jobID → true for sessions with signal enabled
	routeTable     map[int64]string // signal timestamp → jobID
	ready          chan struct{}     // closed when signal-cli is ready
	isRunning      bool
	ulog           *logging.UnifiedLogger
	ctx            context.Context
	cancel         context.CancelFunc

	// SendInput is the function used to inject messages into tmux sessions.
	// Set by the server at initialization.
	SendInput func(ctx context.Context, tmuxTarget, message string) error
}

// NewManager creates a new ChannelManager.
func NewManager(st *store.Store, cfg SignalConfig) *Manager {
	return &Manager{
		store:          st,
		signalCfg:      cfg,
		activeSessions: make(map[string]bool),
		routeTable:     make(map[int64]string),
		ulog:           logging.NewUnifiedLogger("groved.channels"),
	}
}

// Start initializes the channel manager. It loads persisted routes and checks
// for existing sessions that need channels.
func (m *Manager) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)

	m.loadRoutes()

	// Subscribe to session end events for route cleanup
	go m.watchSessionEnds(m.ctx)

	// Periodic route cleanup (TTL)
	go m.routeCleanup(m.ctx)

	m.ulog.Info("Channel manager started").Log(m.ctx)
}

// Stop shuts down the channel manager and signal-cli.
func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}

	if m.signalChannel != nil {
		m.signalChannel.Stop(ctx)
		m.signalChannel = nil
		m.isRunning = false
	}

	m.saveRoutes()
	m.ulog.Info("Channel manager stopped").Log(ctx)
}

// EnableChannel enables a channel for a session. Starts signal-cli if needed.
func (m *Manager) EnableChannel(_ context.Context, jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.signalCfg.Enabled {
		return fmt.Errorf("signal is not enabled in configuration")
	}

	m.activeSessions[jobID] = true

	if !m.isRunning {
		m.isRunning = true
		m.ready = make(chan struct{})
		go m.startSignalChannel(m.ctx) // Use manager's long-lived context, not request context
	}

	m.ulog.Info("Channel enabled for session").Field("job_id", jobID).Log(m.ctx)
	return nil
}

// DisableChannel disables a channel for a session. Stops signal-cli if no sessions remain.
func (m *Manager) DisableChannel(ctx context.Context, jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.activeSessions, jobID)

	if len(m.activeSessions) == 0 && m.signalChannel != nil {
		m.signalChannel.Stop(ctx)
		m.signalChannel = nil
		m.isRunning = false
		m.ulog.Info("Signal channel stopped (no active sessions)").Log(ctx)
	}
}

// Send sends a message via the signal channel and records the route.
func (m *Manager) Send(ctx context.Context, req models.ChannelSendRequest) (*models.ChannelSendResponse, error) {
	m.mu.Lock()
	ch := m.signalChannel
	ready := m.ready
	m.mu.Unlock()

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

// Status returns the current status of the channel system.
func (m *Manager) Status() *models.ChannelStatusResponse {
	m.mu.Lock()
	defer m.mu.Unlock()

	return &models.ChannelStatusResponse{
		SignalCLIRunning: m.isRunning,
		ActiveRoutes:     len(m.routeTable),
		RefCount:         len(m.activeSessions),
	}
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
			// Reply with active agent list
			m.replyWithAgentList(msg.Source)
			return
		} else {
			m.mu.Unlock()
			m.ulog.Warn("Inbound message dropped — no active agents").Log(ctx)
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

	// Route to agent
	session := m.store.GetSession(targetJobID)
	if session == nil {
		m.ulog.Warn("Target session not found in store").Field("job_id", targetJobID).Log(ctx)
		return
	}
	if session.TmuxTarget == "" {
		m.ulog.Warn("Target session has empty TmuxTarget").
			Field("job_id", targetJobID).
			Field("session_status", session.Status).
			Field("pty_id", session.PtyID).
			Log(ctx)
		return
	}

	if m.SendInput == nil {
		m.ulog.Error("SendInput not wired on Manager — message dropped").
			Field("job_id", targetJobID).
			Log(ctx)
		return
	}

	taggedText := fmt.Sprintf("[via Signal] %s", text)
	m.ulog.Info("Injecting signal message into agent").
		Field("job_id", targetJobID).
		Field("tmux_target", session.TmuxTarget).
		Field("pty_id", session.PtyID).
		Field("input_len", len(taggedText)).
		Log(ctx)
	if err := m.SendInput(ctx, session.TmuxTarget, taggedText); err != nil {
		m.ulog.Error("Failed to inject signal message into agent").
			Err(err).
			Field("job_id", targetJobID).
			Field("tmux_target", session.TmuxTarget).
			Log(ctx)
		return
	}
	m.ulog.Success("Signal message injected").Field("job_id", targetJobID).Log(ctx)
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
		ch.Send(context.Background(), channels.OutboundMessage{
			Recipient: recipient,
			Message:   msg,
		})
	}
}

// parseTag extracts "@tag rest" from a message.
func parseTag(text string) (tag, rest string) {
	text = strings.TrimPrefix(text, "@")
	parts := strings.SplitN(text, " ", 2)
	tag = parts[0]
	if len(parts) > 1 {
		rest = parts[1]
	}
	return
}

// watchSessionEnds listens for session end events and cleans up routes.
func (m *Manager) watchSessionEnds(ctx context.Context) {
	ch := m.store.Subscribe()
	defer m.store.Unsubscribe(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case u := <-ch:
			if u.Type == store.UpdateSessionEnd {
				if payload, ok := u.Payload.(*store.SessionEndPayload); ok {
					m.DisableChannel(ctx, payload.JobID)
					m.cleanupRoutesForJob(payload.JobID)
				}
			}
		}
	}
}

// cleanupRoutesForJob removes all route entries for a specific job.
func (m *Manager) cleanupRoutesForJob(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for ts, id := range m.routeTable {
		if id == jobID {
			delete(m.routeTable, ts)
		}
	}
	go m.saveRoutes()
}

// routeCleanup periodically purges stale routes older than 7 days.
func (m *Manager) routeCleanup(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	cutoff := time.Duration(7 * 24 * time.Hour)

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

// Persistence

func (m *Manager) routeFilePath() string {
	return filepath.Join(paths.StateDir(), "channels", "signal_routes.json")
}

func (m *Manager) loadRoutes() {
	data, err := os.ReadFile(m.routeFilePath())
	if err != nil {
		return // File doesn't exist yet
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	json.Unmarshal(data, &m.routeTable)
}

func (m *Manager) saveRoutes() {
	m.mu.Lock()
	data, err := json.Marshal(m.routeTable)
	m.mu.Unlock()
	if err != nil {
		return
	}

	dir := filepath.Dir(m.routeFilePath())
	os.MkdirAll(dir, 0755)
	os.WriteFile(m.routeFilePath(), data, 0644)
}
