// Package server provides the HTTP server for the grove daemon.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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

// Server manages the daemon's HTTP server over a Unix socket.
type Server struct {
	logger        *logrus.Entry
	server        *http.Server
	engine        *engine.Engine
	runningConfig *RunningConfig
}

// New creates a new Server instance.
func New(logger *logrus.Entry) *Server {
	return &Server{
		logger: logger,
	}
}

// SetEngine sets the collector engine for the server.
func (s *Server) SetEngine(eng *engine.Engine) {
	s.engine = eng
}

// SetRunningConfig sets the running configuration for the server.
func (s *Server) SetRunningConfig(cfg *RunningConfig) {
	s.runningConfig = cfg
}

// ListenAndServe starts the daemon on the given unix socket path.
// It blocks until the server stops or fails.
func (s *Server) ListenAndServe(socketPath string) error {
	// Cleanup stale socket
	if _, err := os.Stat(socketPath); err == nil {
		if err := os.Remove(socketPath); err != nil {
			return fmt.Errorf("failed to remove stale socket: %w", err)
		}
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}

	// Set restrictive permissions on socket
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// State API endpoints
	mux.HandleFunc("/api/state", s.handleGetState)
	mux.HandleFunc("/api/workspaces", s.handleGetWorkspaces)
	// Session endpoints - order matters! Most specific routes first.
	mux.HandleFunc("/api/sessions/intent", s.handleSessionIntent)
	mux.HandleFunc("/api/sessions/confirm", s.handleSessionConfirm)
	mux.HandleFunc("/api/sessions/", s.handleSessionByID)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/stream", s.handleStreamState)
	mux.HandleFunc("/api/config", s.handleGetConfig)
	mux.HandleFunc("/api/focus", s.handleFocus)

	s.server = &http.Server{
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}

	s.logger.WithField("socket", socketPath).Info("Daemon listening")
	return s.server.Serve(listener)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("Shutting down server...")
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

// handleGetState returns the complete daemon state as JSON.
func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	state := s.engine.Store().Get()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

// handleGetWorkspaces returns all enriched workspaces as JSON.
func (s *Server) handleGetWorkspaces(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	workspaces := s.engine.Store().GetWorkspaces()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(workspaces)
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
	json.NewEncoder(w).Encode(sessions)
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
		json.NewEncoder(w).Encode(session)

	case "status":
		// PATCH /api/sessions/{id}/status - update status
		if r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateSessionStatus,
			Source: "api",
			Payload: &store.SessionStatusPayload{
				JobID:  sessionID,
				Status: req.Status,
			},
		})
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})

	case "end":
		// POST /api/sessions/{id}/end - end session
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Outcome string `json:"outcome"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		s.engine.Store().ApplyUpdate(store.Update{
			Type:   store.UpdateSessionEnd,
			Source: "api",
			Payload: &store.SessionEndPayload{
				JobID:   sessionID,
				Outcome: req.Outcome,
			},
		})
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ended"})

	default:
		http.Error(w, "unknown action", http.StatusNotFound)
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

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateSessionIntent,
		Source:  "api",
		Payload: &intent,
	})

	s.logger.WithField("job_id", intent.JobID).Debug("Session intent registered")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "registered", "job_id": intent.JobID})
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

	s.logger.WithFields(logrus.Fields{
		"job_id": confirmation.JobID,
		"pid":    confirmation.PID,
	}).Debug("Session confirmed")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "confirmed"})
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

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe to store updates
	ch := s.engine.Store().Subscribe()
	defer s.engine.Store().Unsubscribe(ch)

	// Send initial ping to confirm connection
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	s.logger.Debug("SSE client connected")

	// Send current state immediately so client has data right away
	state := s.engine.Store().Get()
	if len(state.Workspaces) > 0 {
		workspaces := make([]*models.EnrichedWorkspace, 0, len(state.Workspaces))
		for _, ws := range state.Workspaces {
			workspaces = append(workspaces, ws)
		}
		initialUpdate := &apiStateUpdate{
			Workspaces: workspaces,
			UpdateType: "initial",
		}
		if data, err := json.Marshal(initialUpdate); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}

	for {
		select {
		case <-r.Context().Done():
			s.logger.Debug("SSE client disconnected")
			return
		case update := <-ch:
			// Convert internal store.Update to public API format
			apiUpdate := convertToAPIUpdate(update)
			if apiUpdate == nil {
				continue
			}

			data, err := json.Marshal(apiUpdate)
			if err != nil {
				s.logger.WithError(err).Error("Failed to marshal update")
				continue
			}
			// SSE format: "data: {json}\n\n"
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// apiStateUpdate matches the daemon.StateUpdate type for SSE streaming.
type apiStateUpdate struct {
	Workspaces []*models.EnrichedWorkspace `json:"workspaces,omitempty"`
	Sessions   []*models.Session           `json:"sessions,omitempty"`
	UpdateType string                      `json:"update_type"`
	Source     string                      `json:"source,omitempty"`
	Scanned    int                         `json:"scanned,omitempty"`
	ConfigFile string                      `json:"config_file,omitempty"`
	Payload    interface{}                 `json:"payload,omitempty"`
}

// convertToAPIUpdate converts internal store.Update to the public API format.
func convertToAPIUpdate(u store.Update) *apiStateUpdate {
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
	case store.UpdateSkillSync:
		return &apiStateUpdate{
			UpdateType: "skill_sync",
			Source:     u.Source,
			Payload:    u.Payload,
		}
	// Session lifecycle updates - broadcast as session changes
	case store.UpdateSessionIntent, store.UpdateSessionConfirmation,
		store.UpdateSessionStatus, store.UpdateSessionEnd:
		return &apiStateUpdate{
			UpdateType: "session",
			Source:     u.Source,
			Payload:    u.Payload,
		}
	}
	return nil
}

// handleGetConfig returns the running configuration as JSON.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if s.runningConfig == nil {
		http.Error(w, "config not initialized", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.runningConfig)
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
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		s.engine.Store().SetFocus(req.Paths)
		s.logger.WithField("count", len(req.Paths)).Debug("Focus updated")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]int{"focused": len(req.Paths)})

	case http.MethodGet:
		focus := s.engine.Store().GetFocus()
		paths := make([]string, 0, len(focus))
		for p := range focus {
			paths = append(paths, p)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string][]string{"paths": paths})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
