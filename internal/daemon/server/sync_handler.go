// Sync HTTP handlers — expose sync.db state via /api/sync/*. The routes are
// served on the 0600 unix socket ONLY (sync state is content-adjacent
// metadata that must not leak via the unauthenticated localhost TCP
// listener). sync.db is owned by the global daemon; scoped daemons proxy
// /api/sync/* to the global daemon's unix socket, mirroring the memory.db
// scoped-proxy pattern.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/grovetools/core/pkg/paths"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// SetSyncDB wires the sync database into the server so /api/sync/* handlers
// can serve it. Called from cmd/groved.go on the global daemon only, and
// only when sync is configured (the dark gate).
func (s *Server) SetSyncDB(db *syncdb.DB) {
	s.syncDB = db
}

// syncStatusResponse is the GET /api/sync/status payload.
type syncStatusResponse struct {
	Enabled       bool                  `json:"enabled"`
	DBPath        string                `json:"db_path,omitempty"`
	OriginID      string                `json:"origin_id,omitempty"`
	Documents     int                   `json:"documents"`
	OutboxPending int                   `json:"outbox_pending"`
	Workspaces    []syncWorkspaceStatus `json:"workspaces,omitempty"`
}

type syncWorkspaceStatus struct {
	Name         string    `json:"name"`
	Cursor       int64     `json:"cursor"`
	LastSyncedAt time.Time `json:"last_synced_at,omitzero"`
}

// handleSyncStatus handles GET /api/sync/status.
func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Scoped daemons never open sync.db — forward to the global daemon.
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}

	out := syncStatusResponse{}
	if s.syncDB != nil {
		out.Enabled = true
		out.DBPath = s.syncDB.Path()
		out.OriginID = s.syncDB.OriginID()
		if n, err := s.syncDB.CountDocuments(); err == nil {
			out.Documents = n
		}
		if n, err := s.syncDB.CountOutbox(); err == nil {
			out.OutboxPending = n
		}
		if states, err := s.syncDB.ListStates(); err == nil {
			for _, st := range states {
				out.Workspaces = append(out.Workspaces, syncWorkspaceStatus{
					Name:         st.Workspace,
					Cursor:       st.Cursor,
					LastSyncedAt: st.LastSyncedAt,
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// forwardSyncToGlobal replays an /api/sync/* request against the global
// daemon's unix socket. Unlike the memory proxy (typed core client methods),
// sync forwarding is a raw HTTP relay so Phase 0 needs no core client
// surface for an API that is still dark.
func (s *Server) forwardSyncToGlobal(w http.ResponseWriter, r *http.Request) {
	socketPath := paths.SocketPath()
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://groved"+r.URL.RequestURI(), r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("forward sync request failed: %v", err), http.StatusInternalServerError)
		return
	}
	req.Header = r.Header.Clone()

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("forward sync request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
