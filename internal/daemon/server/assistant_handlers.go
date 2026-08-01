package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/assistant"
)

// SetAssistantSupervisor wires the daemon-side assistant supervisor
// (assistant-pane spec §3.3) onto the server. Nil-safe by construction: an
// ecosystem with no [assistant] block never calls this, and the handlers below
// answer 503/last-known-status instead of panicking.
func (s *Server) SetAssistantSupervisor(sup *assistant.Supervisor) {
	s.assistantSupervisor.Store(sup)
}

// handleAssistantEnsure handles POST /api/assistant/ensure — the pane-focus
// trigger of the ensure-running loop. It is a REQUEST, not a restart: a chain
// that is already live is left alone and reported as live.
//
// `?force=1` re-arms a tripped circuit breaker. The rail pane never sends it —
// a UI that silently re-armed the breaker would defeat the thing whose whole
// job is to make a crash loop visible — so forcing is an explicit operator act.
func (s *Server) handleAssistantEnsure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sup := s.assistantSupervisor.Load()
	if sup == nil {
		http.Error(w, "assistant supervisor not initialized", http.StatusServiceUnavailable)
		return
	}

	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "api"
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	status, err := sup.Ensure(r.Context(), reason, force)
	if err != nil {
		// The caller is a pane that wants to show WHY, so the reason travels
		// in the body. A disabled supervisor is a configuration answer (412),
		// a tripped breaker is a refusal to act (409), anything else is a
		// continuation that failed (500).
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, assistant.ErrDisabled):
			code = http.StatusPreconditionFailed
		case errors.Is(err, assistant.ErrStopped):
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// handleAssistantStatus handles GET /api/assistant/status — the supervisor's
// state without triggering anything.
func (s *Server) handleAssistantStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Prefer the live supervisor; fall back to the last snapshot the store
	// received, so a daemon whose supervisor is disabled still answers with
	// something truthful rather than an error.
	if sup := s.assistantSupervisor.Load(); sup != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sup.Status())
		return
	}
	var status *models.AssistantStatus
	if s.engine != nil {
		status = s.engine.Store().GetAssistantStatus()
	}
	if status == nil {
		status = &models.AssistantStatus{State: models.AssistantStateDisabled}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}
