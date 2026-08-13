package server

// The operator surface of W3.5 adoption.
//
// The gate that withholds writes lives in the pull pipeline and the verdict
// lives in the watcher (sync/adoption.go, watcher/sync_lifecycle.go). This file
// is the only way an operator sees or resolves one:
//
//	GET  /api/sync/contested        — what is withheld, and the evidence
//	POST /api/sync/contested/adopt  — adopt one notespace, by id
//
// Adoption is deliberately not a query parameter on the listing endpoint: it is
// the one act here that changes what the daemon writes to disk, and a GET that
// mutates is a GET something eventually retries.

import (
	"encoding/json"
	"fmt"
	"net/http"

	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// SetSyncContested wires the watcher's contested set into the HTTP layer.
//
// Two functions rather than the handler itself, matching every other sync
// seam on this server: the server must not import the watcher, and the pair
// says exactly what this surface may do — read the verdicts, and adopt one.
// Nil-safe: without them the endpoints answer "sync is not configured", which
// is the truthful answer on a daemon whose sync handler never registered.
func (s *Server) SetSyncContested(list func() []syncdb.ContestedNotespace, adopt func(string) (syncdb.ContestedNotespace, string, error)) {
	s.syncContested = list
	s.syncAdoptContested = adopt
}

// contestedResponse is the GET /api/sync/contested payload.
type contestedResponse struct {
	Contested []syncdb.ContestedNotespace `json:"contested"`
}

// adoptContestedResponse is the POST /api/sync/contested/adopt payload: what
// was adopted, and where the durable receipt landed.
type adoptContestedResponse struct {
	Adopted syncdb.ContestedNotespace `json:"adopted"`
	Receipt string                    `json:"receipt"`
}

// handleSyncContested handles GET /api/sync/contested. Read-only: it reports
// the notespaces currently taking no incoming writes and why.
func (s *Server) handleSyncContested(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncContested == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}
	out := contestedResponse{Contested: s.syncContested()}
	if out.Contested == nil {
		out.Contested = []syncdb.ContestedNotespace{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleSyncAdoptContested handles POST /api/sync/contested/adopt with body
// {"notespace_id": "..."}.
//
// The id is required and never inferred, even when exactly one notespace is
// contested: adoption is the operator's statement that two histories are the
// same notes, and a daemon that picks the subject of that statement has made
// the decision itself.
func (s *Server) handleSyncAdoptContested(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncAdoptContested == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		NotespaceID string `json:"notespace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}
	if req.NotespaceID == "" {
		http.Error(w, "notespace_id is required; adoption names the notespace it adopts", http.StatusBadRequest)
		return
	}
	adopted, receipt, err := s.syncAdoptContested(req.NotespaceID)
	if err != nil {
		// A notespace that is not contested is the operator's mistake, not the
		// daemon's failure — 409, so a script can tell it from a broken daemon.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(adoptContestedResponse{Adopted: adopted, Receipt: receipt})
}
