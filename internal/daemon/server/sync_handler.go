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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/grovetools/core/config"
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
	OutboxParked  int                   `json:"outbox_parked"`
	Workspaces    []syncWorkspaceStatus `json:"workspaces,omitempty"`
}

type syncWorkspaceStatus struct {
	Name         string                    `json:"name"`
	Cursor       int64                     `json:"cursor"`
	LastSyncedAt time.Time                 `json:"last_synced_at,omitzero"`
	Hydration    *syncdb.HydrationProgress `json:"hydration,omitempty"`
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
		// OutboxPending is the TOTAL count, parked included — a parked entry is
		// still unsynced state, so hiding it would let waitForSync (and the
		// cluster harness) pass while data is stuck. OutboxParked splits out the
		// parked subset for the grove-status parked line and the S3 assertion.
		if n, err := s.syncDB.CountOutbox(); err == nil {
			out.OutboxPending = n
		}
		if n, err := s.syncDB.CountOutboxParked(); err == nil {
			out.OutboxParked = n
		}
		if states, err := s.syncDB.ListStates(); err == nil {
			for _, st := range states {
				out.Workspaces = append(out.Workspaces, syncWorkspaceStatus{
					Name:         st.Workspace,
					Cursor:       st.Cursor,
					LastSyncedAt: st.LastSyncedAt,
					Hydration:    syncdb.HydrationStatus(st.Workspace),
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// syncDocumentResponse is one entry of the GET /api/sync/documents payload:
// per-document sync state for the dev-UI document matrix. IsDirty is the
// LastSyncedHash comparison the pull pipeline relies on (a clean cell that
// should read dirty was the visible symptom of the fast-forward clobber bug).
type syncDocumentResponse struct {
	DocumentID     string `json:"document_id"`
	Workspace      string `json:"workspace"`
	Path           string `json:"path"`
	Version        int64  `json:"version"`
	ContentHash    string `json:"content_hash"`
	LastSyncedHash string `json:"last_synced_hash"`
	IsDirty        bool   `json:"is_dirty"`
}

// handleSyncDocuments handles GET /api/sync/documents[?workspace=W], returning
// per-document sync state with a computed is_dirty flag. Read-only.
func (s *Server) handleSyncDocuments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Scoped daemons never open sync.db — forward to the global daemon.
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDB == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	docs, err := s.syncDB.ListDocuments(r.URL.Query().Get("workspace"))
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list documents: %v", err), http.StatusInternalServerError)
		return
	}

	out := make([]syncDocumentResponse, 0, len(docs))
	for _, doc := range docs {
		out = append(out, syncDocumentResponse{
			DocumentID:     doc.DocumentID,
			Workspace:      doc.Workspace,
			Path:           doc.Path,
			Version:        doc.LastSyncedVersion,
			ContentHash:    doc.ContentHash,
			LastSyncedHash: doc.LastSyncedHash,
			IsDirty:        doc.ContentHash != doc.LastSyncedHash,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// syncOutboxResponse is one entry of the GET /api/sync/outbox payload: a
// change parked in the local push queue. Payload is omitted (it can carry the
// full document body) — this view drives the "parked" matrix indicator, not a
// content diff.
type syncOutboxResponse struct {
	ID          int64     `json:"id"`
	DocumentID  string    `json:"document_id"`
	Workspace   string    `json:"workspace"`
	EventType   string    `json:"event_type"`
	Path        string    `json:"path"`
	PrevPath    string    `json:"prev_path,omitempty"`
	ContentHash string    `json:"content_hash"`
	CreatedAt   time.Time `json:"created_at"`
	Parked      bool      `json:"parked,omitempty"`
	Attempts    int       `json:"attempts,omitempty"`
	NextRetryAt time.Time `json:"next_retry_at,omitzero"`
	ParkReason  string    `json:"park_reason,omitempty"`
}

// handleSyncOutbox handles GET /api/sync/outbox[?workspace=W], returning the
// pending push queue in insertion order. Read-only.
func (s *Server) handleSyncOutbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDB == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	entries, err := s.syncDB.ListOutbox(r.URL.Query().Get("workspace"), 0)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list outbox: %v", err), http.StatusInternalServerError)
		return
	}

	out := make([]syncOutboxResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, syncOutboxResponse{
			ID:          e.ID,
			DocumentID:  e.DocumentID,
			Workspace:   e.Workspace,
			EventType:   e.EventType,
			Path:        e.Path,
			PrevPath:    e.PrevPath,
			ContentHash: e.ContentHash,
			CreatedAt:   e.CreatedAt,
			Parked:      e.Parked,
			Attempts:    e.Attempts,
			NextRetryAt: e.NextRetryAt,
			ParkReason:  e.ParkReason,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// syncConflictResponse is one entry of the GET /api/sync/conflicts payload: a
// conflict artifact on disk plus the 3-way-merge base recovered from sync.db.
// BaseContent is the "base" leg of the conflict inspector's diff; the "local"
// leg is ArtifactContent and the "server head" leg is fetched separately.
type syncConflictResponse struct {
	Workspace       string `json:"workspace"`
	Path            string `json:"path"`        // original wire path of the conflicted document
	DocumentID      string `json:"document_id"` // parsed from the artifact filename
	Artifact        string `json:"artifact"`    // artifact filename, workspace-relative (slash form)
	ArtifactContent string `json:"artifact_content"`
	BaseContent     string `json:"base_content,omitempty"` // 3-way base from sync_documents, when resolvable
}

// handleSyncConflicts handles GET /api/sync/conflicts[?workspace=W]. It scans
// the on-disk conflict store (StateDir/sync/conflicts/<workspace>/) written by
// the pull pipeline (pull.go recordConflict: <path>.<document_id>.conflict.md)
// and, for each artifact, recovers the base content from the matching
// sync_documents row. Read-only.
func (s *Server) handleSyncConflicts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDB == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	filter := r.URL.Query().Get("workspace")
	root := filepath.Join(paths.StateDir(), "sync", "conflicts")
	out := make([]syncConflictResponse, 0)

	wsEntries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		// No conflicts have ever been recorded — empty list, not an error.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read conflicts dir: %v", err), http.StatusInternalServerError)
		return
	}

	for _, wsEntry := range wsEntries {
		if !wsEntry.IsDir() {
			continue
		}
		workspace := wsEntry.Name()
		if filter != "" && workspace != filter {
			continue
		}
		wsDir := filepath.Join(root, workspace)
		walkErr := filepath.WalkDir(wsDir, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() || !strings.HasSuffix(de.Name(), ".conflict.md") {
				return nil
			}
			rel, err := filepath.Rel(wsDir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)

			// Artifact name is "<path>.<document_id>.conflict.md"; the
			// document id is the segment after the final dot of the stem.
			stem := strings.TrimSuffix(rel, ".conflict.md")
			idx := strings.LastIndex(stem, ".")
			if idx < 0 {
				return nil // unparseable name — skip rather than guess
			}
			origPath, docID := stem[:idx], stem[idx+1:]

			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}

			resp := syncConflictResponse{
				Workspace:       workspace,
				Path:            origPath,
				DocumentID:      docID,
				Artifact:        rel,
				ArtifactContent: string(content),
			}
			if doc, derr := s.syncDB.GetDocument(docID); derr == nil && doc != nil {
				resp.BaseContent = string(doc.BaseContent)
			}
			out = append(out, resp)
			return nil
		})
		if walkErr != nil {
			http.Error(w, fmt.Sprintf("failed to scan conflicts: %v", walkErr), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleSyncAllow handles POST /api/sync/allow, which adds a workspace/path
// to the quarantine override list, allowing it to sync despite secret pattern
// matches. Request body: {"workspace": "...", "path": "..."}.
func (s *Server) handleSyncAllow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Scoped daemons forward to global daemon
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}

	if s.syncDB == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Workspace string `json:"workspace"`
		Path      string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if req.Workspace == "" || req.Path == "" {
		http.Error(w, "workspace and path are required", http.StatusBadRequest)
		return
	}

	if err := s.syncDB.SetQuarantineOverride(req.Workspace, req.Path); err != nil {
		http.Error(w, fmt.Sprintf("failed to set quarantine override: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleSyncDisallowQuarantine handles DELETE /api/sync/allow, which removes
// a workspace/path from the quarantine override list.
func (s *Server) handleSyncDisallowQuarantine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Scoped daemons forward to global daemon
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}

	if s.syncDB == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Workspace string `json:"workspace"`
		Path      string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if req.Workspace == "" || req.Path == "" {
		http.Error(w, "workspace and path are required", http.StatusBadRequest)
		return
	}

	if err := s.syncDB.RemoveQuarantineOverride(req.Workspace, req.Path); err != nil {
		http.Error(w, fmt.Sprintf("failed to remove quarantine override: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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

// historyClient builds a sync client from the configured sync settings for
// user-initiated history/restore proxying. Constructed per request — these
// are rare, human-driven calls and the handshake doubles as a liveness check.
func (s *Server) historyClient(ctx context.Context) (*syncdb.Client, error) {
	cfg, err := config.LoadSyncConfig()
	if err != nil || cfg == nil {
		return nil, fmt.Errorf("sync is not configured")
	}
	return syncdb.NewClientFromConfig(ctx, cfg, "", s.syncDB.OriginID(), "", nil)
}

// handleSyncHistory handles GET /api/sync/history?workspace=W&path=P by
// proxying to grove-syncd's /sync/history with the daemon-held token.
func (s *Server) handleSyncHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDB == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	workspace, path := q.Get("workspace"), q.Get("path")
	if workspace == "" || path == "" {
		http.Error(w, "workspace and path are required", http.StatusBadRequest)
		return
	}

	client, err := s.historyClient(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	entries, err := client.History(r.Context(), workspace, path)
	if err != nil {
		http.Error(w, fmt.Sprintf("history fetch failed: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

// handleSyncRestore handles GET /api/sync/restore?workspace=W&path=P&version=V.
// The daemon resolves the document id from sync.db and returns the raw
// historical content; the caller (nb) writes the file as a user-initiated
// edit, which re-enters sync as a normal new head version.
func (s *Server) handleSyncRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDB == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	workspace, path, versionStr := q.Get("workspace"), q.Get("path"), q.Get("version")
	if workspace == "" || path == "" || versionStr == "" {
		http.Error(w, "workspace, path and version are required", http.StatusBadRequest)
		return
	}
	version, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}

	doc, err := s.syncDB.GetDocumentByPath(workspace, path)
	if err != nil {
		http.Error(w, fmt.Sprintf("document lookup failed: %v", err), http.StatusInternalServerError)
		return
	}
	if doc == nil {
		http.Error(w, "document is not tracked by sync", http.StatusNotFound)
		return
	}

	client, err := s.historyClient(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	content, err := client.HistoryBlob(r.Context(), workspace, doc.DocumentID, version)
	if err != nil {
		http.Error(w, fmt.Sprintf("restore fetch failed: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(content)
}
