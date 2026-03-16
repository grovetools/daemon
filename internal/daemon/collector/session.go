package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/process"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/sirupsen/logrus"
)

// SessionCollector monitors active sessions using fsnotify for instant detection
// and periodic PID verification for liveness checking.
//
// Phase 2 implementation: The daemon becomes the single source of truth for
// "what sessions are running?" This eliminates redundant scanning of
// ~/.grove/hooks/sessions by multiple tools.
//
// Phase 3 enhancement: Now includes Flow Jobs and OpenCode sessions via
// periodic discovery scans, merged with real-time interactive session tracking.
type SessionCollector struct {
	interval    time.Duration
	sessionsDir string
	logger      *logrus.Entry

	// Reference to the store for accessing cached workspace data
	store *store.Store

	// In-memory session registry for interactive sessions (fsnotify-tracked)
	mu       sync.RWMutex
	registry map[string]*models.Session

	// Additional sessions from Flow Jobs and OpenCode (periodically scanned)
	flowJobsMu       sync.RWMutex
	flowJobsRegistry map[string]*models.Session

	openCodeMu       sync.RWMutex
	openCodeRegistry map[string]*models.Session
}

// NewSessionCollector creates a new SessionCollector with the specified interval.
// If interval is 0, defaults to 2 seconds for PID verification.
func NewSessionCollector(interval time.Duration) *SessionCollector {
	if interval == 0 {
		interval = 2 * time.Second
	}
	return &SessionCollector{
		interval:         interval,
		sessionsDir:      filepath.Join(paths.StateDir(), "hooks", "sessions"),
		logger:           logging.NewLogger("daemon.collector.session"),
		registry:         make(map[string]*models.Session),
		flowJobsRegistry: make(map[string]*models.Session),
		openCodeRegistry: make(map[string]*models.Session),
	}
}

// Name returns the collector's name.
func (c *SessionCollector) Name() string { return "session" }

// Run starts the session monitoring loop with fsnotify watching.
func (c *SessionCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	// Store reference for use in scanFlowJobs
	c.store = st

	// Ensure sessions directory exists
	if err := os.MkdirAll(c.sessionsDir, 0755); err != nil {
		c.logger.WithError(err).Warn("Failed to ensure sessions directory exists")
		// Continue anyway - directory might be created later
	}

	// Setup fsnotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		c.logger.WithError(err).Error("Failed to create fsnotify watcher, falling back to polling only")
		return c.runPollingOnly(ctx, st, updates)
	}
	defer watcher.Close()

	// Watch the sessions directory
	if err := watcher.Add(c.sessionsDir); err != nil {
		c.logger.WithError(err).Warn("Failed to watch sessions directory, falling back to polling only")
		return c.runPollingOnly(ctx, st, updates)
	}

	// Also watch each existing session subdirectory for metadata changes
	c.watchExistingSessionDirs(watcher)

	// Initial scan to populate the registry (including Flow Jobs and OpenCode)
	c.fullScan()
	c.scanFlowJobs()
	c.scanOpenCode()
	c.emitUpdate(updates)

	// PID verification ticker (fast - every interval)
	pidTicker := time.NewTicker(c.interval)
	defer pidTicker.Stop()

	// Flow Jobs and OpenCode scan ticker (slower - every 10 seconds)
	// These are more expensive scans, so we do them less frequently
	scanTicker := time.NewTicker(10 * time.Second)
	defer scanTicker.Stop()

	c.logger.WithField("sessions_dir", c.sessionsDir).Info("Session collector started with fsnotify watching")

	for {
		select {
		case <-ctx.Done():
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			c.handleFsEvent(event, watcher, updates)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			c.logger.WithError(err).Error("Watcher error")

		case <-pidTicker.C:
			// Periodic PID verification for active sessions
			start := time.Now()
			if c.verifyPIDs() {
				c.emitUpdate(updates)
			}
			if d := time.Since(start); d > 100*time.Millisecond {
				c.logger.WithField("duration", d).Warn("Slow PID verification detected")
			}

		case <-scanTicker.C:
			// Periodic scan for Flow Jobs and OpenCode sessions
			start := time.Now()
			flowChanged := c.scanFlowJobs()
			openCodeChanged := c.scanOpenCode()
			if flowChanged || openCodeChanged {
				c.emitUpdate(updates)
			}
			if d := time.Since(start); d > 200*time.Millisecond {
				c.logger.WithField("duration", d).Warn("Slow job/opencode scan detected")
			}
		}
	}
}

// runPollingOnly is the fallback when fsnotify is unavailable
func (c *SessionCollector) runPollingOnly(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	scan := func() {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 200*time.Millisecond {
				c.logger.WithField("duration", d).Warn("Slow session polling scan detected")
			}
		}()

		// Use DiscoverAll to get all session types
		allSessions, err := sessions.DiscoverAll()
		if err != nil {
			c.logger.WithError(err).Debug("Failed to discover sessions in polling mode")
			return
		}

		updates <- store.Update{
			Type:    store.UpdateSessions,
			Source:  "session",
			Scanned: len(allSessions),
			Payload: allSessions,
		}
	}

	scan()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan()
		}
	}
}

// watchExistingSessionDirs adds watchers for all existing session subdirectories
func (c *SessionCollector) watchExistingSessionDirs(watcher *fsnotify.Watcher) {
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			sessionDir := filepath.Join(c.sessionsDir, entry.Name())
			if err := watcher.Add(sessionDir); err != nil {
				c.logger.WithError(err).WithField("dir", sessionDir).Debug("Failed to watch session directory")
			}
		}
	}
}

// handleFsEvent processes filesystem events and updates the registry
func (c *SessionCollector) handleFsEvent(event fsnotify.Event, watcher *fsnotify.Watcher, updates chan<- store.Update) {
	// Ignore events on the sessions directory itself
	if event.Name == c.sessionsDir {
		return
	}

	// Parse the path to get session info
	relPath, err := filepath.Rel(c.sessionsDir, event.Name)
	if err != nil {
		return
	}

	parts := strings.Split(relPath, string(filepath.Separator))
	if len(parts) == 0 {
		return
	}

	dirName := parts[0]

	// Handle directory creation (new session)
	if event.Has(fsnotify.Create) {
		sessionDir := filepath.Join(c.sessionsDir, dirName)
		info, err := os.Stat(sessionDir)
		if err == nil && info.IsDir() {
			// New session directory - watch it for file changes
			if err := watcher.Add(sessionDir); err != nil {
				c.logger.WithError(err).WithField("dir", sessionDir).Debug("Failed to watch new session directory")
			}
			// Try to load the session (it may not have all files yet)
			c.loadSession(dirName)
			c.emitUpdate(updates)
		}
	}

	// Handle file creation/modification within session directory
	if (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)) && len(parts) > 1 {
		filename := parts[1]
		// Reload on metadata.json or pid.lock changes
		if filename == "metadata.json" || filename == "pid.lock" {
			c.loadSession(dirName)
			c.emitUpdate(updates)
		}
	}

	// Handle directory removal
	if event.Has(fsnotify.Remove) && len(parts) == 1 {
		c.mu.Lock()
		delete(c.registry, dirName)
		c.mu.Unlock()
		c.emitUpdate(updates)
	}
}

// fullScan reads all interactive sessions from the filesystem
func (c *SessionCollector) fullScan() {
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		c.logger.WithError(err).Debug("Failed to read sessions directory")
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			c.loadSession(entry.Name())
		}
	}
}

// scanFlowJobs discovers Flow Jobs from workspace directories.
// Returns true if the registry changed.
func (c *SessionCollector) scanFlowJobs() bool {
	// Use cached workspace data from the store to avoid expensive re-discovery
	var flowSessions []*models.Session
	var err error

	if c.store != nil {
		workspaces := c.store.GetWorkspaces()
		if len(workspaces) > 0 {
			// Extract workspace nodes from enriched workspaces
			nodes := make([]*workspace.WorkspaceNode, 0, len(workspaces))
			for _, ws := range workspaces {
				if ws.WorkspaceNode != nil {
					nodes = append(nodes, ws.WorkspaceNode)
				}
			}
			flowSessions, err = sessions.DiscoverFlowJobsWithNodes(nodes)
		}
	}

	// Fallback to full discovery if store is not available or empty
	if flowSessions == nil && err == nil {
		flowSessions, err = sessions.DiscoverFlowJobs()
	}

	if err != nil {
		c.logger.WithError(err).Debug("Failed to discover flow jobs")
		return false
	}

	c.flowJobsMu.Lock()
	defer c.flowJobsMu.Unlock()

	// Check if anything changed
	changed := len(flowSessions) != len(c.flowJobsRegistry)
	if !changed {
		for _, s := range flowSessions {
			existing, ok := c.flowJobsRegistry[s.ID]
			if !ok || existing.Status != s.Status || existing.LastActivity != s.LastActivity {
				changed = true
				break
			}
		}
	}

	// Update registry
	c.flowJobsRegistry = make(map[string]*models.Session)
	for _, s := range flowSessions {
		c.flowJobsRegistry[s.ID] = s
	}

	return changed
}

// scanOpenCode discovers OpenCode sessions.
// Returns true if the registry changed.
func (c *SessionCollector) scanOpenCode() bool {
	openCodeSessions, err := sessions.DiscoverOpenCodeSessions()
	if err != nil {
		c.logger.WithError(err).Debug("Failed to discover OpenCode sessions")
		return false
	}

	c.openCodeMu.Lock()
	defer c.openCodeMu.Unlock()

	// Check if anything changed
	changed := len(openCodeSessions) != len(c.openCodeRegistry)
	if !changed {
		for _, s := range openCodeSessions {
			existing, ok := c.openCodeRegistry[s.ID]
			if !ok || existing.Status != s.Status || existing.LastActivity != s.LastActivity {
				changed = true
				break
			}
		}
	}

	// Update registry
	c.openCodeRegistry = make(map[string]*models.Session)
	for _, s := range openCodeSessions {
		c.openCodeRegistry[s.ID] = s
	}

	return changed
}

// loadSession reads a session from disk and updates the registry
func (c *SessionCollector) loadSession(dirName string) {
	sessionDir := filepath.Join(c.sessionsDir, dirName)
	pidFile := filepath.Join(sessionDir, "pid.lock")
	metadataFile := filepath.Join(sessionDir, "metadata.json")

	// Read PID
	pidContent, err := os.ReadFile(pidFile)
	if err != nil {
		return // Session not ready yet
	}

	var pid int
	if _, err := parseIntFromBytes(pidContent, &pid); err != nil {
		return
	}

	// Read metadata
	metadataContent, err := os.ReadFile(metadataFile)
	if err != nil {
		return // Session not ready yet
	}

	var metadata sessions.SessionMetadata
	if err := parseMetadata(metadataContent, &metadata); err != nil {
		return
	}

	// Determine effective IDs
	sessionID := metadata.SessionID
	claudeSessionID := metadata.ClaudeSessionID
	if claudeSessionID == "" {
		claudeSessionID = dirName
	}
	if sessionID == "" {
		sessionID = dirName
	}

	// Check liveness
	// PID=0 means the session was pre-registered (intent) but not yet confirmed
	status := "running"
	var endedAt *time.Time
	lastActivity := metadata.StartedAt

	if pid == 0 {
		// Session intent registered but not yet confirmed with actual PID
		status = "pending"
	} else if !process.IsProcessAlive(pid) {
		status = "interrupted"
		now := time.Now()
		endedAt = &now
		lastActivity = now
	}

	session := &models.Session{
		ID:               sessionID,
		Type:             metadata.Type,
		ClaudeSessionID:  claudeSessionID,
		PID:              pid,
		Repo:             metadata.Repo,
		Branch:           metadata.Branch,
		WorkingDirectory: metadata.WorkingDirectory,
		User:             metadata.User,
		Status:           status,
		StartedAt:        metadata.StartedAt,
		LastActivity:     lastActivity,
		EndedAt:          endedAt,
		JobTitle:         metadata.JobTitle,
		PlanName:         metadata.PlanName,
		JobFilePath:      metadata.JobFilePath,
		Provider:         metadata.Provider,
	}

	c.mu.Lock()
	// Use dirName as key since that's how we track in the filesystem
	c.registry[dirName] = session
	c.mu.Unlock()
}

// verifyPIDs checks if active sessions are still running
// Returns true if any session status changed
func (c *SessionCollector) verifyPIDs() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	changed := false
	for dirName, session := range c.registry {
		// Only check sessions we think are active (skip pending - PID=0)
		if session.Status == "running" || session.Status == "idle" || session.Status == "pending_user" {
			// Skip sessions with PID=0 - they're still pending confirmation
			if session.PID == 0 {
				continue
			}
			if !process.IsProcessAlive(session.PID) {
				c.logger.WithFields(logrus.Fields{
					"session_id": session.ID,
					"pid":        session.PID,
				}).Debug("Session process died")

				session.Status = "interrupted"
				now := time.Now()
				session.EndedAt = &now
				session.LastActivity = now
				changed = true

				// Optionally clean up the session directory for dead processes
				// In Phase 5 (Lifecycle), this will trigger completion logic
				go c.cleanupDeadSession(dirName, session)
			}
		}
	}
	return changed
}

// cleanupDeadSession handles cleanup for sessions with dead processes
func (c *SessionCollector) cleanupDeadSession(dirName string, session *models.Session) {
	sessionDir := filepath.Join(c.sessionsDir, dirName)

	// For non-flow sessions (claude_session without job file), clean up immediately
	if session.JobFilePath == "" && session.Type != "interactive_agent" && session.Type != "agent" {
		// Wait a moment for any final file writes
		time.Sleep(2 * time.Second)
		os.RemoveAll(sessionDir)
		return
	}

	// For flow jobs, the cleanup is handled by the discovery/completion logic
	// Just log for now - Phase 5 will add auto-completion
}

// emitUpdate sends the merged registry state to the update channel
func (c *SessionCollector) emitUpdate(updates chan<- store.Update) {
	allSessions := c.getAllSessionsMerged()

	updates <- store.Update{
		Type:    store.UpdateSessions,
		Source:  "session",
		Scanned: len(allSessions),
		Payload: allSessions,
	}
}

// getAllSessionsMerged returns all sessions from all sources, with deduplication.
// Interactive sessions take priority over Flow Jobs (they have live PID info).
func (c *SessionCollector) getAllSessionsMerged() []*models.Session {
	merged := make(map[string]*models.Session)

	// 1. Add OpenCode sessions first (lowest priority)
	c.openCodeMu.RLock()
	for id, s := range c.openCodeRegistry {
		sCopy := *s
		merged[id] = &sCopy
	}
	c.openCodeMu.RUnlock()

	// 2. Add Flow Jobs (medium priority - more metadata)
	c.flowJobsMu.RLock()
	for id, s := range c.flowJobsRegistry {
		if existing, ok := merged[id]; ok {
			// Merge: keep Flow Job metadata but update with existing info
			sCopy := *s
			if existing.Provider != "" && sCopy.Provider == "" {
				sCopy.Provider = existing.Provider
			}
			merged[id] = &sCopy
		} else {
			sCopy := *s
			merged[id] = &sCopy
		}
	}
	c.flowJobsMu.RUnlock()

	// 3. Add interactive sessions (highest priority - live PID tracking)
	c.mu.RLock()
	for _, s := range c.registry {
		sCopy := *s
		if existing, ok := merged[sCopy.ID]; ok {
			// Interactive session updates existing entry
			existing.PID = sCopy.PID
			existing.Status = sCopy.Status
			existing.LastActivity = sCopy.LastActivity
			existing.ClaudeSessionID = sCopy.ClaudeSessionID
			if sCopy.Provider != "" {
				existing.Provider = sCopy.Provider
			}
		} else {
			merged[sCopy.ID] = &sCopy
		}
	}
	c.mu.RUnlock()

	// Convert to slice
	result := make([]*models.Session, 0, len(merged))
	for _, s := range merged {
		result = append(result, s)
	}
	return result
}

// GetSessions returns a copy of the current session list from all sources (for direct access)
func (c *SessionCollector) GetSessions() []*models.Session {
	return c.getAllSessionsMerged()
}

// GetSession returns a specific session by ID from any source
func (c *SessionCollector) GetSession(id string) *models.Session {
	// Check interactive sessions first (most authoritative for live state)
	c.mu.RLock()
	for _, s := range c.registry {
		if s.ID == id {
			sCopy := *s
			c.mu.RUnlock()
			return &sCopy
		}
	}
	c.mu.RUnlock()

	// Check flow jobs
	c.flowJobsMu.RLock()
	if s, ok := c.flowJobsRegistry[id]; ok {
		sCopy := *s
		c.flowJobsMu.RUnlock()
		return &sCopy
	}
	c.flowJobsMu.RUnlock()

	// Check OpenCode sessions
	c.openCodeMu.RLock()
	if s, ok := c.openCodeRegistry[id]; ok {
		sCopy := *s
		c.openCodeMu.RUnlock()
		return &sCopy
	}
	c.openCodeMu.RUnlock()

	return nil
}

// parseIntFromBytes parses a PID from byte content using fmt.Sscanf
func parseIntFromBytes(content []byte, result *int) (int, error) {
	s := strings.TrimSpace(string(content))
	_, err := fmt.Sscanf(s, "%d", result)
	return *result, err
}

// parseMetadata unmarshals JSON metadata
func parseMetadata(content []byte, result *sessions.SessionMetadata) error {
	return json.Unmarshal(content, result)
}
