// Package server provides the HTTP server for the grove daemon.
package server

import (
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

	"syscall"
	"time"

	"github.com/grovetools/core/config"
	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/tmux"
	navbindings "github.com/grovetools/nav/pkg/bindings"
	"github.com/grovetools/daemon/internal/daemon/channels"
	daemonpty "github.com/grovetools/daemon/internal/daemon/pty"
	daemonweb "github.com/grovetools/daemon/web"
	"github.com/grovetools/daemon/internal/daemon/engine"
	daemonenv "github.com/grovetools/daemon/internal/daemon/env"
	"github.com/grovetools/daemon/internal/daemon/jobrunner"
	"github.com/grovetools/daemon/internal/daemon/logstreamer"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/watcher"
	"github.com/grovetools/daemon/internal/enrichment"
	"github.com/grovetools/memory/pkg/memory"
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
	jobRunner     *jobrunner.JobRunner
	logStreamer   *logstreamer.LogStreamer
	envManager     *daemonenv.Manager
	channelManager *channels.Manager

	// PTY session manager for daemon-owned PTY sessions.
	ptyManager *daemonpty.Manager

	// Memory store + embedder are wired via SetMemoryStore so /api/memory/*
	// handlers can serve the same instance the MemoryHandler watcher uses.
	memStore    memory.DocumentStore
	memEmbedder *memory.Embedder
	memDBPath   string

	// captureWaiters holds pending GET /api/agents/{id}/capture requests.
	// The HTTP handler blocks on the channel until groveterm sends the
	// capture response via POST /api/agents/{id}/capture_response.
	captureWaitersMu sync.Mutex
	captureWaiters   map[string]chan string

	// terminalHub routes WebSocket messages for multi-attach
	// (Primary/Follower groveterm instances).
	terminalHub *TerminalHub
}

// New creates a new Server instance.
func New(logger *logrus.Entry) *Server {
	return &Server{
		logger:         logger,
		captureWaiters: make(map[string]chan string),
		terminalHub:    NewTerminalHub(logger),
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

// SetJobRunner sets the job runner for the server.
func (s *Server) SetJobRunner(jr *jobrunner.JobRunner) {
	s.jobRunner = jr
}

// SetLogStreamer sets the log streamer for the server.
func (s *Server) SetLogStreamer(ls *logstreamer.LogStreamer) {
	s.logStreamer = ls
}

// SetEnvManager sets the environment manager for the server.
func (s *Server) SetEnvManager(m *daemonenv.Manager) {
	s.envManager = m
}

// SetChannelManager sets the channel manager for the server.
func (s *Server) SetChannelManager(m *channels.Manager) {
	s.channelManager = m
}

// SetPtyManager sets the PTY session manager for the server.
func (s *Server) SetPtyManager(m *daemonpty.Manager) {
	s.ptyManager = m
}

// ListenAndServe starts the daemon on the given unix socket path.
// If httpPort > 0, also listens on localhost:httpPort for browser access
// (web terminal viewer, API debugging). It blocks until the server stops.
func (s *Server) ListenAndServe(socketPath string, httpPort ...int) error {
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
	mux.HandleFunc("/api/workspace/hud/stream", s.handleStreamWorkspaceHUD)
	mux.HandleFunc("/api/config", s.handleGetConfig)
	mux.HandleFunc("/api/focus", s.handleFocus)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/notes/index", s.handleNoteIndex)
	mux.HandleFunc("/api/notes/event", s.handleNoteEvent)
	// Environment management endpoints
	mux.HandleFunc("/api/env/up", s.handleEnvUp)
	mux.HandleFunc("/api/env/down", s.handleEnvDown)
	mux.HandleFunc("/api/env/status", s.handleEnvStatus)
	// Job management endpoints
	mux.HandleFunc("/api/jobs/", s.handleJobByID)
	mux.HandleFunc("/api/jobs", s.handleJobs)
	// Channel management endpoints
	mux.HandleFunc("/api/channels/send", s.handleChannelSend)
	mux.HandleFunc("/api/channels/status", s.handleChannelStatus)
	// Memory search endpoints
	mux.HandleFunc("/api/memory/search", s.handleMemorySearch)
	mux.HandleFunc("/api/memory/coverage", s.handleMemoryCoverage)
	mux.HandleFunc("/api/memory/status", s.handleMemoryStatus)
	// Treemux multi-attach WebSocket endpoint
	mux.HandleFunc("/api/treemux/ws", s.HandleTerminalWS)
	// Treemux SSE stream for web viewers
	mux.HandleFunc("/api/treemux/stream", s.handleTerminalStream)
	// Static web viewer files
	mux.Handle("/web/treemux/", http.StripPrefix("/web/treemux/", daemonweb.TreemuxFileServer()))

	// PTY session management endpoints
	mux.HandleFunc("/api/pty/create", s.handlePtyCreate)
	mux.HandleFunc("/api/pty/list", s.handlePtyList)
	mux.HandleFunc("/api/pty/kill/", s.handlePtyKill)
	mux.HandleFunc("/api/pty/attach/", s.handlePtyAttach)

	// Nav bindings endpoints
	mux.HandleFunc("/api/nav/bindings", s.handleNavBindings)
	mux.HandleFunc("/api/nav/config", s.handleNavConfig)
	mux.HandleFunc("/api/nav/groups/", s.handleNavGroup)
	mux.HandleFunc("/api/nav/locked-keys", s.handleNavLockedKeys)
	mux.HandleFunc("/api/nav/last-accessed", s.handleNavLastAccessedGroup)
	// System endpoints
	mux.HandleFunc("/api/system/treemux-status", s.handleTerminalStatus)
	// Native agent pane relay endpoints
	mux.HandleFunc("/api/agents/spawn", s.handleAgentSpawn)
	mux.HandleFunc("/api/agents/", s.handleAgentByID)

	handler := h2c.NewHandler(mux, &http2.Server{})

	s.server = &http.Server{
		Handler: handler,
	}

	// Optionally start a TCP listener for browser access (web terminal viewer).
	if len(httpPort) > 0 && httpPort[0] > 0 {
		port := httpPort[0]
		go func() {
			addr := fmt.Sprintf("localhost:%d", port)
			s.logger.WithField("addr", addr).Info("HTTP server listening (web terminal viewer)")
			tcpServer := &http.Server{
				Addr:    addr,
				Handler: handler,
			}
			if err := tcpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logger.WithError(err).Error("HTTP server failed")
			}
		}()
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
			json.NewEncoder(w).Encode(map[string]string{"status": "killed"})
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
			}
			if req.LastSender != "" {
				s.engine.Store().ApplyUpdate(store.Update{
					Type:   store.UpdateSessionLastSender,
					Source: "api",
					Payload: &store.SessionLastSenderPayload{
						JobID:      sessionID,
						LastSender: req.LastSender,
					},
				})
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
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

		// Write-through to filesystem crash-recovery metadata so status
		// survives daemon restarts (e.g., "idle" is preserved, not lost).
		if registry, err := sessions.NewFileSystemRegistry(); err == nil {
			// Look up the native session ID for the filesystem directory name
			session := s.engine.Store().GetSession(sessionID)
			dirName := sessionID
			if session != nil && session.ClaudeSessionID != "" {
				dirName = session.ClaudeSessionID
			}
			_ = registry.UpdateStatus(dirName, req.Status)
		}

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
				JobID:    sessionID,
				Channels: req.Channels,
			},
		})
		// If channel manager is set, enable/disable channels
		if s.channelManager != nil {
			if len(req.Channels) > 0 {
				if err := s.channelManager.EnableChannel(r.Context(), sessionID); err != nil {
					s.logger.WithError(err).Error("Failed to enable channel")
				}
			} else {
				s.channelManager.DisableChannel(r.Context(), sessionID)
			}
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})

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
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})

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

		session := s.engine.Store().GetSession(sessionID)
		if session == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if session.TmuxTarget == "" {
			http.Error(w, "session has no tmux target", http.StatusBadRequest)
			return
		}

		tmuxClient, err := tmux.NewClient()
		if err != nil {
			http.Error(w, fmt.Sprintf("tmux not available: %v", err), http.StatusInternalServerError)
			return
		}

		// Determine input mode from project config
		inputMode := s.resolveInputMode(session.WorkingDirectory)

		ctx := r.Context()
		if inputMode == "vim" {
			if err := tmuxClient.SendKeys(ctx, session.TmuxTarget, "Escape", "i", req.Input); err != nil {
				http.Error(w, fmt.Sprintf("failed to send input: %v", err), http.StatusInternalServerError)
				return
			}
		} else {
			if err := tmuxClient.SendKeys(ctx, session.TmuxTarget, req.Input); err != nil {
				http.Error(w, fmt.Sprintf("failed to send input: %v", err), http.StatusInternalServerError)
				return
			}
		}

		// Send Enter to submit
		if err := tmuxClient.SendKeys(ctx, session.TmuxTarget, "C-m"); err != nil {
			http.Error(w, fmt.Sprintf("failed to send submit key: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "sent"})

	case "interrupt":
		// POST /api/sessions/{id}/interrupt — send Ctrl+C to interrupt an agent
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		session := s.engine.Store().GetSession(sessionID)
		if session == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if session.TmuxTarget == "" {
			http.Error(w, "session has no tmux target", http.StatusBadRequest)
			return
		}

		tmuxClient, err := tmux.NewClient()
		if err != nil {
			http.Error(w, fmt.Sprintf("tmux not available: %v", err), http.StatusInternalServerError)
			return
		}

		if err := tmuxClient.SendKeys(r.Context(), session.TmuxTarget, "C-c"); err != nil {
			http.Error(w, fmt.Sprintf("failed to send interrupt: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "interrupted"})

	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
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

	if session.PID > 0 {
		// SIGTERM only — let the process clean up. ESRCH (process
		// already gone) is not an error from our perspective.
		if err := syscall.Kill(session.PID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("failed to signal pid %d: %w", session.PID, err)
		}
	}

	// Remove the crash-recovery directory so a daemon restart won't
	// re-resurrect this session as alive. The directory is named after
	// the native session ID (Claude UUID), falling back to the job ID.
	dirName := sessionID
	if session.ClaudeSessionID != "" {
		dirName = session.ClaudeSessionID
	}
	if registry, err := sessions.NewFileSystemRegistry(); err == nil {
		_ = registry.Unregister(dirName)
	}

	// Mark as interrupted in the in-memory store so SSE subscribers
	// (the embedded hooks panel, etc.) update immediately. The
	// session-collector sweep will eventually reconcile the dead PID,
	// but the eager update keeps the UI snappy.
	s.engine.Store().ApplyUpdate(store.Update{
		Type:   store.UpdateSessionEnd,
		Source: "api",
		Payload: &store.SessionEndPayload{
			JobID:   sessionID,
			Outcome: "interrupted",
		},
	})

	s.logger.WithField("session_id", sessionID).Info("Session killed via API")
	return nil
}

// resolveInputMode reads the grove config for a workspace to determine if vim input mode is active.
func (s *Server) resolveInputMode(workDir string) string {
	if workDir == "" {
		return "vim" // default
	}

	coreCfg, err := config.LoadFrom(workDir)
	if err != nil {
		return "vim"
	}

	var flowCfg struct {
		InteractiveProvider string `toml:"interactive_provider" yaml:"interactive_provider"`
		Providers           map[string]struct {
			InputMode string `toml:"input_mode" yaml:"input_mode"`
		} `toml:"providers" yaml:"providers"`
	}
	coreCfg.UnmarshalExtension("flow", &flowCfg)

	providerName := "claude"
	if flowCfg.InteractiveProvider != "" {
		providerName = flowCfg.InteractiveProvider
	}
	if providerCfg, ok := flowCfg.Providers[providerName]; ok && providerCfg.InputMode != "" {
		return providerCfg.InputMode
	}

	return "vim"
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

	// If the intent includes channels, enable them in the channel manager
	if s.channelManager != nil && len(intent.Channels) > 0 {
		for range intent.Channels {
			if err := s.channelManager.EnableChannel(r.Context(), intent.JobID); err != nil {
				s.logger.WithError(err).WithField("job_id", intent.JobID).Warn("Failed to enable channel from intent")
			}
		}
	}

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
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	s.logger.WithField("path", path).Debug("HUD SSE client connected")

	for {
		select {
		case <-r.Context().Done():
			s.logger.WithField("path", path).Debug("HUD SSE client disconnected")
			return
		case hud, ok := <-out:
			if !ok {
				return
			}
			data, err := json.Marshal(hud)
			if err != nil {
				s.logger.WithError(err).Error("Failed to marshal HUD update")
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleTerminalStatus returns whether a groveterm instance is connected via WebSocket.
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	connected := s.terminalHub != nil && s.terminalHub.HasConnections()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"connected":%t}`, connected)
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

	// If the PTY manager is available, create a daemon-owned PTY for the agent
	// and send an attach event instead of a spawn event. This lets the agent
	// process survive terminal restarts.
	if s.ptyManager != nil && payload.Command != "" {
		// Wrap the agent command in an interactive shell so the user's RC
		// files are sourced (PATH includes nvm, homebrew, etc.). Export
		// env vars inside the script to ensure they survive shell init.
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}

		var script strings.Builder
		for k, v := range payload.Env {
			escapedVal := strings.ReplaceAll(v, "'", "'\\''")
			script.WriteString(fmt.Sprintf("export %s='%s'; ", k, escapedVal))
		}
		script.WriteString(payload.Command)
		for _, arg := range payload.Args {
			escapedArg := strings.ReplaceAll(arg, "'", "'\\''")
			script.WriteString(fmt.Sprintf(" '%s'", escapedArg))
		}

		sess, err := s.ptyManager.Create(daemonpty.CreateRequest{
			CWD:       payload.WorkDir,
			Command:   shell,
			Args:      []string{"-i", "-c", script.String()},
			Origin:    "agent:" + payload.JobID,
			Label:     payload.JobTitle,
			CreatedBy: "flow",
			Labels: map[string]string{
				"job_id":    payload.JobID,
				"plan_name": payload.PlanName,
				"type":      "agent",
			},
		})
		if err != nil {
			s.logger.WithError(err).Error("Failed to create agent PTY session")
			http.Error(w, "failed to create agent PTY: "+err.Error(), http.StatusInternalServerError)
			return
		}

		ptyID := sess.Metadata().ID

		// Update the session registry with the PTY ID so re-attachment works.
		if st := s.engine.Store(); st != nil {
			st.SetSessionPtyID(payload.JobID, ptyID)
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
		json.NewEncoder(w).Encode(map[string]string{"status": "attached", "pty_id": ptyID})
		return
	}

	// Fallback: relay spawn request to groveterm (legacy path).
	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateSpawnAgentPane,
		Source:  "api",
		Payload: &payload,
	})

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "spawned"})
}

// handleAgentByID routes /api/agents/{id}/* actions (input, capture, capture_response).
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
		json.NewEncoder(w).Encode(map[string]string{"status": "sent"})

	case "capture":
		// GET /api/agents/{id}/capture — blocking request that waits for groveterm's response
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
			io.WriteString(w, text)
		case <-time.After(5 * time.Second):
			s.captureWaitersMu.Lock()
			delete(s.captureWaiters, agentID)
			s.captureWaitersMu.Unlock()
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
		json.NewEncoder(w).Encode(map[string]string{"status": "received"})

	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
}

// apiStateUpdate matches the daemon.StateUpdate type for SSE streaming.
type apiStateUpdate struct {
	Workspaces      []*models.EnrichedWorkspace `json:"workspaces,omitempty"`
	WorkspaceDeltas []*models.WorkspaceDelta    `json:"workspace_deltas,omitempty"`
	Sessions        []*models.Session           `json:"sessions,omitempty"`
	UpdateType      string                      `json:"update_type"`
	Source          string                      `json:"source,omitempty"`
	Scanned         int                         `json:"scanned,omitempty"`
	ConfigFile      string                      `json:"config_file,omitempty"`
	Payload         interface{}                 `json:"payload,omitempty"`
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
	case store.UpdateWatcherStatus:
		return &apiStateUpdate{
			UpdateType: "watcher_status",
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
		store.UpdateJobPendingUser:
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
	s.logger.WithFields(logrus.Fields{
		"workspace": wsFilter,
		"entries":   len(entries),
	}).Debug("Note index request served")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
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

	s.logger.WithFields(logrus.Fields{
		"event":       event.Event,
		"workspace":   event.Workspace,
		"path":        event.Path,
		"has_index":   event.IndexEntry != nil,
	}).Debug("Note event received")

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateNoteEvent,
		Source:  "nb",
		Payload: &event,
	})

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// tryAttachIndexEntry attempts to parse frontmatter and attach a NoteIndexEntry to a NoteEvent.
func (s *Server) tryAttachIndexEntry(event *models.NoteEvent) {
	state := s.engine.Store().Get()
	cfg, _ := config.LoadDefault()
	locator := workspace.NewNotebookLocator(cfg)

	for _, ws := range state.Workspaces {
		if ws.WorkspaceNode == nil || ws.Name != event.Workspace {
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

// handleRefresh triggers an immediate re-scan of all refreshable collectors.
// This is synchronous: it blocks until the scan completes, so the caller can
// immediately fetch fresh data after receiving the 200 OK response.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine != nil {
		s.engine.Refresh(r.Context())
	}
	w.WriteHeader(http.StatusOK)
}

// handleJobs handles POST (submit) and GET (list) for /api/jobs.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.jobRunner == nil {
		http.Error(w, "job runner not initialized", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var req models.JobSubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		info, err := s.jobRunner.Submit(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(info)

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
		json.NewEncoder(w).Encode(results)

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

	// Route sub-paths: /api/jobs/{id}/logs and /api/jobs/{id}/logs/stream
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
		json.NewEncoder(w).Encode(info)

	case http.MethodDelete:
		if s.jobRunner == nil {
			http.Error(w, "job runner not initialized", http.StatusServiceUnavailable)
			return
		}
		if err := s.jobRunner.Cancel(jobID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "cancelled", "job_id": jobID})

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
	json.NewEncoder(w).Encode(lines)
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
	fmt.Fprintf(w, ": connected to job %s log stream\n\n", jobID)
	flusher.Flush()

	// Send historical buffer
	for _, line := range history {
		data, err := json.Marshal(line)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
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
					fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
				}
			case "status":
				data, err := json.Marshal(map[string]string{
					"status": event.Status,
					"error":  event.Error,
				})
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: status\ndata: %s\n\n", data)
			}
			flusher.Flush()
		}
	}
}

// resolveLogFilePath determines the log file path for a job.
// For now, it constructs the path from the plan directory and job file.
// TODO: This should be stored in JobInfo when the job starts.
func resolveLogFilePath(info *models.JobInfo) string {
	if info.PlanDir == "" || info.JobFile == "" {
		return ""
	}
	// Convention: job logs are stored in .artifacts/<job-name>/job.log within the plan dir
	jobName := strings.TrimSuffix(info.JobFile, ".md")
	return filepath.Join(info.PlanDir, ".artifacts", jobName, "job.log")
}

// handleEnvUp handles POST /api/env/up requests.
func (s *Server) handleEnvUp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.envManager == nil {
		http.Error(w, "env manager not initialized", http.StatusServiceUnavailable)
		return
	}

	var req coreenv.EnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	resp, err := s.envManager.Up(r.Context(), req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(coreenv.EnvResponse{
			Status: "failed",
			Error:  err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleEnvStatus handles GET /api/env/status requests.
func (s *Server) handleEnvStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.envManager == nil {
		http.Error(w, "env manager not initialized", http.StatusServiceUnavailable)
		return
	}

	worktree := r.URL.Query().Get("worktree")
	if worktree == "" {
		http.Error(w, "worktree query parameter required", http.StatusBadRequest)
		return
	}

	resp := s.envManager.Status(worktree)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleEnvDown handles POST /api/env/down requests.
func (s *Server) handleEnvDown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.envManager == nil {
		http.Error(w, "env manager not initialized", http.StatusServiceUnavailable)
		return
	}

	var req coreenv.EnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	resp, err := s.envManager.Down(r.Context(), req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(coreenv.EnvResponse{
			Status: "failed",
			Error:  err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// --- Channel Management Handlers ---

// handleChannelSend handles POST /api/channels/send — send a message via a channel.
func (s *Server) handleChannelSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.channelManager == nil {
		http.Error(w, "channel manager not initialized", http.StatusServiceUnavailable)
		return
	}

	var req models.ChannelSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	sendResp, err := s.channelManager.Send(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sendResp)
}

// handleChannelStatus handles GET /api/channels/status — get channel system status.
func (s *Server) handleChannelStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.channelManager == nil {
		http.Error(w, "channel manager not initialized", http.StatusServiceUnavailable)
		return
	}

	status := s.channelManager.Status()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// handleNavBindings handles GET /api/nav/bindings — return current nav binding state.
func (s *Server) handleNavBindings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bindings := s.engine.Store().GetNavBindings()
	if bindings == nil {
		// Load from disk if not yet in store
		var err error
		bindings, err = navbindings.Load(navbindings.DefaultPath())
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to load nav bindings: %v", err), http.StatusInternalServerError)
			return
		}
		// Cache in store
		s.engine.Store().ApplyUpdate(store.Update{
			Type:    store.UpdateNavBindings,
			Source:  "api",
			Payload: bindings,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(bindings)
}

// handleNavConfig handles GET /api/nav/config — return the static nav configuration
// (group prefixes) read from the grove config files. This lets non-nav clients
// resolve group prefix transitions without re-implementing the nav config loader.
func (s *Server) handleNavConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	groupConfigs := s.loadNavGroupConfigs()
	cfg := models.NavConfig{Groups: make(map[string]models.NavGroupConfig, len(groupConfigs))}
	for name, gc := range groupConfigs {
		cfg.Groups[name] = models.NavGroupConfig{Prefix: gc.Prefix}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
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
		s.logger.WithError(err).Warn("Failed to regenerate tmux bindings")
	}

	// Update store and broadcast SSE
	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateNavBindings,
		Source:  "api",
		Payload: file,
	})

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
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

	sessionsPath := navbindings.DefaultPath()
	file, err := navbindings.Load(sessionsPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load bindings: %v", err), http.StatusInternalServerError)
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
		s.logger.WithError(err).Warn("Failed to regenerate tmux bindings")
	}

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    store.UpdateNavBindings,
		Source:  "api",
		Payload: file,
	})

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
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

	sessionsPath := navbindings.DefaultPath()
	file, err := navbindings.Load(sessionsPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load bindings: %v", err), http.StatusInternalServerError)
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
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// loadNavGroupConfigs reads the grove config to get group prefix configurations for validation.
func (s *Server) loadNavGroupConfigs() map[string]navbindings.GroupConfig {
	result := make(map[string]navbindings.GroupConfig)

	cfg, err := config.LoadDefault()
	if err != nil {
		return result
	}

	// Extract nav config from the grove config
	var navCfg struct {
		Prefix string `toml:"prefix" yaml:"prefix"`
		Groups map[string]struct {
			Prefix string `toml:"prefix" yaml:"prefix"`
		} `toml:"groups" yaml:"groups"`
	}
	cfg.UnmarshalExtension("nav", &navCfg)

	if navCfg.Prefix != "" {
		result["default"] = navbindings.GroupConfig{Prefix: navCfg.Prefix}
	} else {
		result["default"] = navbindings.GroupConfig{Prefix: "<prefix>"}
	}

	for name, g := range navCfg.Groups {
		result[name] = navbindings.GroupConfig{Prefix: g.Prefix}
	}

	return result
}

// buildGroupBindings constructs GroupBinding slice from a NavSessionsFile and the grove config.
func (s *Server) buildGroupBindings(file *models.NavSessionsFile) []navbindings.GroupBinding {
	groupConfigs := s.loadNavGroupConfigs()

	var bindings []navbindings.GroupBinding

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
