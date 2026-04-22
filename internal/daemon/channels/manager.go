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
	activeSessions map[string]bool // jobID → true for sessions with signal enabled
	routeTable     map[int64]string // signal timestamp → jobID
	ready          chan struct{}     // closed when signal-cli is ready
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
		// Global daemon: the local session store is ephemeral (empty
		// immediately after restart; sessions live in scoped daemons OR
		// get re-registered lazily by hooks on the next agent activity).
		// Rebuild activeSessions from persisted state so cross-daemon
		// claw survives a global-daemon restart.
		//
		// Two sources:
		//   1. routing.json — jobIDs owned by scoped daemons (cross-daemon).
		//   2. routeTable — jobIDs that had recent outbound traffic (local
		//      sessions whose store entry will re-register via hooks).
		m.mu.Lock()
		if routes, err := readInboundRoutes(); err == nil {
			for jobID := range routes {
				m.activeSessions[jobID] = true
			}
		}
		for _, jobID := range m.routeTable {
			m.activeSessions[jobID] = true
		}
		m.mu.Unlock()
	} else {
		// Scoped / proxy daemon: activeSessions tracks local sessions.
		// The store is authoritative — prune any activeSessions entry
		// whose session has gone missing.
		m.pruneStaleSessions(m.ctx)
	}

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

	// Subscribe to session end events for route cleanup
	go m.watchSessionEnds(m.ctx)

	// Periodic route cleanup (TTL)
	go m.routeCleanup(m.ctx)

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
	var stale []string
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

// pruneSession removes a single jobID from activeSessions and purges any
// routeTable entries pointing at it. Used as inline self-healing when a
// routed inbound message resolves a jobID that no longer has a store
// session.
func (m *Manager) pruneSession(jobID string) {
	m.mu.Lock()
	delete(m.activeSessions, jobID)
	for ts, id := range m.routeTable {
		if id == jobID {
			delete(m.routeTable, ts)
		}
	}
	m.mu.Unlock()
	m.saveRoutes()
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

	if isProxy {
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
			return
		}
		m.ulog.Success("Signal message forwarded to scoped daemon").
			Field("job_id", targetJobID).
			Field("socket", sockPath).
			Log(ctx)
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

// --- Cross-daemon inbound routing (routing.json) ---
//
// routing.json maps jobID → absolute socketPath of the scoped daemon that
// owns that session. Scoped daemons write entries when a user enables the
// Signal channel on a session; the global daemon reads entries when an
// inbound Signal message resolves to that jobID and forwards SendInput
// across daemons via HTTP over the scoped socket.
//
// Writes use a tmp-file + atomic rename to avoid torn reads from the
// global daemon's inbound goroutine.

func inboundRouteFile() string {
	return filepath.Join(paths.StateDir(), "channels", "routing.json")
}

func readInboundRoutes() (map[string]string, error) {
	data, err := os.ReadFile(inboundRouteFile())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	if len(data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// writeInboundRoutesAtomic writes the routing.json atomically via a
// tmp-file + rename so readers never observe a torn file.
func writeInboundRoutesAtomic(routes map[string]string) error {
	path := inboundRouteFile()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".routing-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// inboundRouteMu serializes read-modify-write cycles across goroutines in
// the same daemon. Cross-daemon concurrency is covered by atomic rename.
var inboundRouteMu sync.Mutex

func (m *Manager) addInboundRoute(jobID string) error {
	if m.socketPath == "" {
		return fmt.Errorf("manager has no socketPath; cannot register inbound route")
	}
	inboundRouteMu.Lock()
	defer inboundRouteMu.Unlock()
	routes, err := readInboundRoutes()
	if err != nil {
		return err
	}
	routes[jobID] = m.socketPath
	return writeInboundRoutesAtomic(routes)
}

func (m *Manager) removeInboundRoute(jobID string) error {
	inboundRouteMu.Lock()
	defer inboundRouteMu.Unlock()
	routes, err := readInboundRoutes()
	if err != nil {
		return err
	}
	if _, ok := routes[jobID]; !ok {
		return nil
	}
	delete(routes, jobID)
	return writeInboundRoutesAtomic(routes)
}

func (m *Manager) lookupInboundRoute(jobID string) (string, bool) {
	routes, err := readInboundRoutes()
	if err != nil {
		return "", false
	}
	sock, ok := routes[jobID]
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
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("scoped daemon returned %s", resp.Status)
	}
	return nil
}
