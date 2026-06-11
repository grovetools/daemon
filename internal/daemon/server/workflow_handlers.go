package server

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// unixOnly wraps a handler to reject requests that did not arrive over the
// unix socket listener. The daemon serves one mux on both the 0600 unix
// socket and an optional unauthenticated localhost TCP listener (web
// viewer); transcript-bearing endpoints must only be reachable on the
// former. The listener network is read from the request's local address
// (http.LocalAddrContextKey); anything other than "unix" gets 403.
func unixOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && addr.Network() != "unix" {
			http.Error(w, "forbidden: endpoint available over the unix socket only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// handleWorkflowEvent handles POST /api/workflows/event — the ingestion
// endpoint for hook-forwarded (and any other producer's) workflow lifecycle
// events. The event is folded into State.WorkflowRuns and broadcast with a
// distinct workflow_* SSE update_type.
func (s *Server) handleWorkflowEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var ev models.WorkflowEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	updateType, ok := store.UpdateTypeForWorkflowKind(ev.Kind)
	if !ok {
		http.Error(w, "unknown event kind", http.StatusBadRequest)
		return
	}
	// Stamp server-side when the producer didn't: the daemon is the clock
	// for sources that have none (the journal carries no timestamps).
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if ev.Source == "" {
		ev.Source = models.WorkflowSourceHooks
	}

	s.engine.Store().ApplyUpdate(store.Update{
		Type:    updateType,
		Source:  ev.Source,
		Payload: &store.WorkflowEventPayload{Event: ev},
	})

	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// workflowsSnapshot is the GET /api/workflows response shape: aggregated
// run state keyed by run ID, plus run-less subagents (ad-hoc Agent-tool
// spawns and not-yet-attributed workflow agents) keyed by session key.
type workflowsSnapshot struct {
	Runs  map[string]*models.WorkflowRunState    `json:"runs"`
	Adhoc map[string]map[string]*models.Subagent `json:"adhoc,omitempty"`
}

// handleGetWorkflows handles GET /api/workflows — the snapshot consumers
// reconcile against, since the SSE broadcast is lossy-by-design.
func (s *Server) handleGetWorkflows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}

	snapshot := workflowsSnapshot{
		Runs:  s.engine.Store().GetWorkflowRuns(),
		Adhoc: s.engine.Store().GetAdhocSubagents(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}
