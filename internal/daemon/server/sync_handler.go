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
	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/paths"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// SetSyncDB wires the sync database into the server so /api/sync/* handlers
// can serve it. Called on the global daemon only, from the watcher's deferred
// open (SyncHandler.SetDeferredDB) the first time a sync subscription exists —
// which may be at boot, or later on the config reload a first-ever `grove join`
// triggers. Until then every handler sees a nil DB and reports sync as
// unconfigured, exactly as before.
func (s *Server) SetSyncDB(db *syncdb.DB) {
	s.syncDB.Store(db)
}

// SetSyncDBError exposes non-mutating legacy-schema refusal through status.
func (s *Server) SetSyncDBError(status func() string) { s.syncDBError = status }

// syncDatabase returns the wired sync database, or nil while sync is dormant.
func (s *Server) syncDatabase() *syncdb.DB {
	return s.syncDB.Load()
}

func (s *Server) syncNotespaceName(id string) string {
	if s.syncDatabase() == nil {
		return ""
	}
	if binding, _ := s.syncDatabase().GetNotespaceBinding(id); binding != nil {
		return binding.Name
	}
	return ""
}

// SetSyncKick wires the watcher SyncHandler's anti-entropy kick into the
// server so POST /api/sync/repush can trigger an immediate reconcile pass
// (notespace-scoped, or all notespaces for ""). Nil-safe: without it the
// reset still lands and the hourly anti-entropy tick performs the re-push.
func (s *Server) SetSyncKick(kick func(notespace string)) {
	s.syncKick = kick
}

// SetSyncNotespaceRoots wires configured notebook-root resolution into the
// explicit batch-apply endpoint.
func (s *Server) SetSyncNotespaceRoots(resolve func([]string) (map[string]string, error)) {
	s.syncNotespaceRoots = resolve
}

// SetSyncMaintenance wires the watcher-owned synchronous drain. The server
// owns dispatch rejection and active-job checks; the watcher owns debounce,
// reconcile, and outbox flushing.
func (s *Server) SetSyncMaintenance(begin func(context.Context) error, end func()) {
	s.syncBeginMaintenance = begin
	s.syncEndMaintenance = end
}

// SetSyncSubscriptions wires the watcher SyncHandler's live subscription view
// (server URL + per-notespace pull/mode) into the server so GET
// /api/sync/status can report where each notespace syncs and in which
// direction. A function, not a captured config, so a hot reload is reflected
// without a daemon restart. Nil-safe: without it the status payload simply
// omits the server and direction fields, exactly as it did before.
func (s *Server) SetSyncSubscriptions(subs func() (string, []config.SyncWorkspace)) {
	s.syncSubscriptions = subs
}

// SetSyncAuthFailure wires the watcher SyncHandler's token-rejection state
// into the server so GET /api/sync/status reports the stale-token trap
// (contract §3 P2b) instead of a status that looks merely idle: a rejected
// token stops replication completely while every counter stays plausible.
// Nil-safe: without it the payload omits the field, as it did before.
func (s *Server) SetSyncAuthFailure(auth func() (string, time.Time, bool)) {
	s.syncAuthFailure = auth
}

// syncStatusResponse is the GET /api/sync/status payload.
type syncStatusResponse struct {
	Enabled     bool               `json:"enabled"`
	Degraded    bool               `json:"degraded,omitempty"`
	ConfigError *ConfigDegradation `json:"config_error,omitempty"`
	DBPath      string             `json:"db_path,omitempty"`
	// MachineName/MachineID are this host's identity: the config-held display
	// name (machine.toml, hostname default) and the state-held ULID. They are
	// reported even when sync is disabled — identity does not depend on sync
	// being configured. Surfaces render them together as "name (short id)",
	// never the name alone: names collide across machines restored from one
	// dotfiles repo.
	MachineName string `json:"machine_name,omitempty"`
	MachineID   string `json:"machine_id,omitempty"`
	// OriginID is the per-sync.db install id and dies with that DB — a
	// diagnostic (same MachineID + new OriginID = wiped sync.db), not this
	// machine's identity.
	OriginID string `json:"origin_id,omitempty"`
	// Server is the configured grove-syncd base URL — the "where" behind the
	// counters. Empty when the subscription view is not wired.
	Server string `json:"server,omitempty"`
	// AuthError is the stale-token trap made visible: non-empty when the
	// server is currently rejecting this machine's bearer token (401), in
	// which case NOTHING is replicating no matter how healthy the counters
	// below look. AuthErrorSince marks when the rejection started.
	AuthError         string                `json:"auth_error,omitempty"`
	AuthErrorSince    time.Time             `json:"auth_error_since,omitzero"`
	Documents         int                   `json:"documents"`
	DocumentsDiverged int                   `json:"documents_diverged"`
	OutboxPending     int                   `json:"outbox_pending"`
	OutboxParked      int                   `json:"outbox_parked"`
	Notespaces        []syncNotespaceStatus `json:"notespaces,omitempty"`
	MigrationRequired string                `json:"migration_required,omitempty"`
}

type syncNotespaceStatus struct {
	NotespaceID   string                    `json:"notespace_id"`
	NotespaceName string                    `json:"notespace_name,omitempty"`
	Cursor        int64                     `json:"cursor"`
	LastSyncedAt  time.Time                 `json:"last_synced_at,omitzero"`
	Hydration     *syncdb.HydrationProgress `json:"hydration,omitempty"`
	// Pull and Mode mirror the subscription this notespace syncs under
	// (config.SyncWorkspace): Pull=false is push-only, Mode filters what is
	// subscribed. Zero values when the notespace has sync.db state but no
	// matching subscription (e.g. a subscription removed from sync.toml).
	Pull bool   `json:"pull,omitempty"`
	Mode string `json:"mode,omitempty"`
	// Role is the subscription's relationship: satellite, peer, or registry.
	// Empty is a legacy role-less entry (push-only) — or a notespace with no
	// matching subscription at all.
	Role string `json:"role,omitempty"`
	// Contested is the watcher adoption gate's live verdict. A contested
	// notespace has neither transport, so surface the reason and the directions
	// withheld instead of letting an old LastSyncedAt make it look idle.
	Contested bool     `json:"contested,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Withheld  []string `json:"withheld,omitempty"`
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

	// Machine identity is reported unconditionally: it exists whether or not
	// sync is configured, and the ID is minted here if this is the first
	// process on the host to ask for it.
	out := syncStatusResponse{
		MachineName: config.ResolveMachineName(),
		MachineID:   machine.ID(),
		Degraded:    s.configError() != nil,
		ConfigError: s.configError(),
	}
	// Reported outside the Enabled branch on purpose: a token rejection is
	// exactly the state in which sync.db can look fine and nothing replicates.
	if s.syncAuthFailure != nil {
		if detail, since, failing := s.syncAuthFailure(); failing {
			out.AuthError = detail
			out.AuthErrorSince = since
		}
	}
	if s.syncDBError != nil {
		out.MigrationRequired = s.syncDBError()
		if out.MigrationRequired != "" {
			out.Degraded = true
		}
	}
	if s.syncDatabase() != nil {
		out.Enabled = true
		out.DBPath = s.syncDatabase().Path()
		out.OriginID = s.syncDatabase().OriginID()
		if n, err := s.syncDatabase().CountDocuments(); err == nil {
			out.Documents = n
		}
		if n, err := s.syncDatabase().CountDocumentsDiverged(); err == nil {
			out.DocumentsDiverged = n
		}
		// OutboxPending is the TOTAL count, parked included — a parked entry is
		// still unsynced state, so hiding it would let waitForSync (and the
		// cluster harness) pass while data is stuck. OutboxParked splits out the
		// parked subset for the grove-status parked line and the S3 assertion.
		if n, err := s.syncDatabase().CountOutbox(); err == nil {
			out.OutboxPending = n
		}
		if n, err := s.syncDatabase().CountOutboxParked(); err == nil {
			out.OutboxParked = n
		}
		// Subscription overlay: the rows are still driven by sync.db state
		// (a subscription with no state row does not appear, as before) —
		// the config only annotates them with direction and mode.
		subsByName := map[string]config.SyncWorkspace{}
		if s.syncSubscriptions != nil {
			server, subs := s.syncSubscriptions()
			out.Server = server
			for _, sub := range subs {
				subsByName[sub.Name] = sub
			}
		}
		contestedByID := map[string]syncdb.ContestedNotespace{}
		if s.syncContested != nil {
			for _, entry := range s.syncContested() {
				contestedByID[entry.NotespaceID] = entry
			}
		}
		if states, err := s.syncDatabase().ListStates(); err == nil {
			for _, st := range states {
				ws := syncNotespaceStatus{
					NotespaceID:  st.Notespace,
					Cursor:       st.Cursor,
					LastSyncedAt: st.LastSyncedAt,
					Hydration:    syncdb.HydrationStatus(st.Notespace),
				}
				if binding, _ := s.syncDatabase().GetNotespaceBinding(st.Notespace); binding != nil {
					ws.NotespaceName = binding.Name
				} else {
					// Fresh v2 databases always have a binding before state. The
					// fallback keeps status diagnostic for synthetic/test rows only.
					ws.NotespaceName = st.Notespace
				}
				if sub, ok := subsByName[ws.NotespaceName]; ok {
					ws.Pull = sub.Pull
					ws.Mode = sub.Mode
					ws.Role = sub.Role
				}
				if contested, ok := contestedByID[ws.NotespaceID]; ok {
					ws.Contested = true
					ws.Reason = contested.Reason
					ws.Withheld = []string{"push", "pull"}
				}
				out.Notespaces = append(out.Notespaces, ws)
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
	NotespaceID    string `json:"notespace_id"`
	NotespaceName  string `json:"notespace_name,omitempty"`
	Path           string `json:"path"`
	Version        int64  `json:"version"`
	ContentHash    string `json:"content_hash"`
	LastSyncedHash string `json:"last_synced_hash"`
	IsDirty        bool   `json:"is_dirty"`
	Diverged       bool   `json:"diverged"`
}

// handleSyncDocuments handles GET /api/sync/documents[?notespace=W], returning
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
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	docs, err := s.syncDatabase().ListDocuments(r.URL.Query().Get("notespace_id"))
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list documents: %v", err), http.StatusInternalServerError)
		return
	}

	out := make([]syncDocumentResponse, 0, len(docs))
	for _, doc := range docs {
		out = append(out, syncDocumentResponse{
			DocumentID:     doc.DocumentID,
			NotespaceID:    doc.Notespace,
			NotespaceName:  s.syncNotespaceName(doc.Notespace),
			Path:           doc.Path,
			Version:        doc.LastSyncedVersion,
			ContentHash:    doc.ContentHash,
			LastSyncedHash: doc.LastSyncedHash,
			IsDirty:        doc.ContentHash != doc.LastSyncedHash,
			Diverged:       doc.Diverged,
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
	ID            int64     `json:"id"`
	DocumentID    string    `json:"document_id"`
	NotespaceID   string    `json:"notespace_id"`
	NotespaceName string    `json:"notespace_name,omitempty"`
	EventType     string    `json:"event_type"`
	Path          string    `json:"path"`
	PrevPath      string    `json:"prev_path,omitempty"`
	ContentHash   string    `json:"content_hash"`
	CreatedAt     time.Time `json:"created_at"`
	Parked        bool      `json:"parked,omitempty"`
	Attempts      int       `json:"attempts,omitempty"`
	NextRetryAt   time.Time `json:"next_retry_at,omitzero"`
	ParkReason    string    `json:"park_reason,omitempty"`
}

// handleSyncOutbox handles GET /api/sync/outbox[?notespace=W], returning the
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
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	entries, err := s.syncDatabase().ListOutbox(r.URL.Query().Get("notespace_id"), 0)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list outbox: %v", err), http.StatusInternalServerError)
		return
	}

	out := make([]syncOutboxResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, syncOutboxResponse{
			ID:            e.ID,
			DocumentID:    e.DocumentID,
			NotespaceID:   e.Notespace,
			NotespaceName: s.syncNotespaceName(e.Notespace),
			EventType:     e.EventType,
			Path:          e.Path,
			PrevPath:      e.PrevPath,
			ContentHash:   e.ContentHash,
			CreatedAt:     e.CreatedAt,
			Parked:        e.Parked,
			Attempts:      e.Attempts,
			NextRetryAt:   e.NextRetryAt,
			ParkReason:    e.ParkReason,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// syncActivityResponse is one entry of the GET /api/sync/activity payload: a
// terminal transfer outcome from the capped sync_activity feed — an outgoing
// change the server answered, or an incoming event applied (or refused)
// locally. Distinct from /api/sync/history, which is a per-document version
// history proxied to the server; this is the machine-local "what moved
// recently, in which direction" feed behind the Notebook Sync history page.
type syncActivityResponse struct {
	ID            int64     `json:"id"`
	NotespaceID   string    `json:"notespace_id"`
	NotespaceName string    `json:"notespace_name,omitempty"`
	Direction     string    `json:"direction"`
	EventType     string    `json:"event_type"`
	Path          string    `json:"path"`
	PrevPath      string    `json:"prev_path,omitempty"`
	DocumentID    string    `json:"document_id,omitempty"`
	Result        string    `json:"result"`
	Detail        string    `json:"detail,omitempty"`
	Version       int64     `json:"version,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
}

// handleSyncActivity handles GET /api/sync/activity[?notespace_id=W&limit=N],
// returning recent transfer outcomes newest-first. Read-only.
func (s *Server) handleSyncActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	entries, err := s.syncDatabase().ListActivity(r.URL.Query().Get("notespace_id"), limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list activity: %v", err), http.StatusInternalServerError)
		return
	}

	out := make([]syncActivityResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, syncActivityResponse{
			ID:            e.ID,
			NotespaceID:   e.Notespace,
			NotespaceName: s.syncNotespaceName(e.Notespace),
			Direction:     e.Direction,
			EventType:     e.EventType,
			Path:          e.Path,
			PrevPath:      e.PrevPath,
			DocumentID:    e.DocumentID,
			Result:        e.Result,
			Detail:        e.Detail,
			Version:       e.Version,
			OccurredAt:    e.OccurredAt,
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
	NotespaceID   string `json:"notespace_id"`
	NotespaceName string `json:"notespace_name,omitempty"`
	Path          string `json:"path"`        // original wire path of the conflicted document
	DocumentID    string `json:"document_id"` // parsed from the artifact filename
	// Kind is what went wrong: "merge" (the historical case) or one of the
	// named kinds, e.g. "registry_foreign_write". It is parsed back out of the
	// artifact filename, because this endpoint is artifact-backed and the
	// filename is the only thing that outlives the SSE broadcast.
	Kind            string `json:"kind,omitempty"`
	Artifact        string `json:"artifact"` // artifact filename, notespace-relative (slash form)
	ArtifactContent string `json:"artifact_content"`
	BaseContent     string `json:"base_content,omitempty"` // 3-way base from sync_documents, when resolvable
}

// handleSyncConflicts handles GET /api/sync/conflicts[?notespace=W]. It scans
// the on-disk conflict store (StateDir/sync/conflicts/<notespace>/) written by
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
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	filter := r.URL.Query().Get("notespace_id")
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
		notespace := wsEntry.Name()
		if filter != "" && notespace != filter {
			continue
		}
		wsDir := filepath.Join(root, notespace)
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

			// The writer and this reader share one naming scheme
			// (syncdb.ParseConflictArtifactName), so the kind a conflict was
			// broadcast with is the kind this endpoint reports.
			origPath, docID, kind, ok := syncdb.ParseConflictArtifactName(rel)
			if !ok {
				return nil // unparseable name — skip rather than guess
			}

			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}

			resp := syncConflictResponse{
				NotespaceID:     notespace,
				NotespaceName:   s.syncNotespaceName(notespace),
				Path:            origPath,
				DocumentID:      docID,
				Kind:            kind,
				Artifact:        rel,
				ArtifactContent: string(content),
			}
			if doc, derr := s.syncDatabase().GetDocument(docID); derr == nil && doc != nil {
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

// handleSyncAllow handles POST /api/sync/allow, which adds a notespace/path
// to the quarantine override list, allowing it to sync despite secret pattern
// matches. Request body: {"notespace": "...", "path": "..."}.
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

	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		NotespaceID string `json:"notespace_id"`
		Path        string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if req.NotespaceID == "" || req.Path == "" {
		http.Error(w, "notespace and path are required", http.StatusBadRequest)
		return
	}

	if err := s.syncDatabase().SetQuarantineOverride(req.NotespaceID, req.Path); err != nil {
		http.Error(w, fmt.Sprintf("failed to set quarantine override: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleSyncDisallowQuarantine handles DELETE /api/sync/allow, which removes
// a notespace/path from the quarantine override list.
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

	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		NotespaceID string `json:"notespace_id"`
		Path        string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if req.NotespaceID == "" || req.Path == "" {
		http.Error(w, "notespace and path are required", http.StatusBadRequest)
		return
	}

	if err := s.syncDatabase().RemoveQuarantineOverride(req.NotespaceID, req.Path); err != nil {
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
	// Same DeviceID/OriginID split as the watcher's transport client: the
	// machine ULID identifies the host across sync.db rebuilds. The server
	// may keep discarding it — rendezvous stays dumb.
	return syncdb.NewClientFromConfig(ctx, cfg, machine.ID(), s.syncDatabase().OriginID(), "", nil)
}

// handleSyncHistory handles GET /api/sync/history?notespace=W&path=P by
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
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	notespace, path := q.Get("notespace_id"), q.Get("path")
	if notespace == "" || path == "" {
		http.Error(w, "notespace and path are required", http.StatusBadRequest)
		return
	}

	client, err := s.historyClient(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	entries, err := client.History(r.Context(), notespace, path)
	if err != nil {
		http.Error(w, fmt.Sprintf("history fetch failed: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

// handleSyncRestore handles GET /api/sync/restore?notespace=W&path=P&version=V.
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
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	notespace, path, versionStr := q.Get("notespace_id"), q.Get("path"), q.Get("version")
	if notespace == "" || path == "" || versionStr == "" {
		http.Error(w, "notespace, path and version are required", http.StatusBadRequest)
		return
	}
	version, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}

	doc, err := s.syncDatabase().GetDocumentByPath(notespace, path)
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
	content, err := client.HistoryBlob(r.Context(), notespace, doc.DocumentID, version)
	if err != nil {
		http.Error(w, fmt.Sprintf("restore fetch failed: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(content)
}

// handleSyncAdopt handles POST /api/sync/adopt with body {"notespace","path"}.
// It is the ONLY sanctioned path by which a diverged document's local file is
// brought to the server head — and even here the daemon does NOT write the
// notespace tree: it fetches the head content, rolls the sync.db merge base +
// clears the diverged flag, and returns the raw head bytes; the CLI (nb sync
// adopt) performs the local os.WriteFile as a user-initiated edit. Modeled on
// handleSyncAllow (unix-only, global-scope forwarded). Distinct from
// handleSyncRestore, which is version-explicit history playback; adopt is
// head-fetch + metadata roll.
func (s *Server) handleSyncAdopt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Scoped daemons never open sync.db — forward to the global daemon.
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		NotespaceID string `json:"notespace_id"`
		Path        string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}
	if req.NotespaceID == "" || req.Path == "" {
		http.Error(w, "notespace and path are required", http.StatusBadRequest)
		return
	}

	// Must be a tracked document (mirrors handleSyncRestore). AdoptDocument does
	// not error on zero rows affected, so this existence check is load-bearing.
	doc, err := s.syncDatabase().GetDocumentByPath(req.NotespaceID, req.Path)
	if err != nil {
		http.Error(w, fmt.Sprintf("document lookup failed: %v", err), http.StatusInternalServerError)
		return
	}
	if doc == nil {
		http.Error(w, "document is not tracked by sync", http.StatusNotFound)
		return
	}

	// Refuse to adopt past an unpushed merge: if any outbox entry still exists
	// for this path (e.g. the merged payload has not yet drained), adopting to
	// the server head would drop the user's merged-in lines from the hub. In the
	// normal flow the parked entry drains before the user reaches for adopt.
	if n, err := s.syncDatabase().CountOutboxForPath(req.NotespaceID, req.Path); err != nil {
		http.Error(w, fmt.Sprintf("outbox lookup failed: %v", err), http.StatusInternalServerError)
		return
	} else if n > 0 {
		http.Error(w, "a pending push exists for this document; wait for it to drain before adopting", http.StatusConflict)
		return
	}

	// Resolve the server head: there is no head-content endpoint, so compose the
	// existing read-only calls — snapshot to find the document's current
	// version, then HistoryBlob at that version (head versions retain content in
	// the events table, the same mechanism handleSyncRestore relies on).
	client, err := s.historyClient(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	manifest, err := client.Snapshot(r.Context(), req.NotespaceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("snapshot fetch failed: %v", err), http.StatusBadGateway)
		return
	}
	var snap *snapshotDoc
	for i := range manifest.Documents {
		if manifest.Documents[i].Path == req.Path {
			d := manifest.Documents[i]
			snap = &snapshotDoc{ID: d.ID, Version: d.Version, Hash: d.Hash}
			break
		}
	}
	if snap == nil {
		http.Error(w, "document is not present in the server snapshot", http.StatusNotFound)
		return
	}
	content, err := client.HistoryBlob(r.Context(), req.NotespaceID, snap.ID, snap.Version)
	if err != nil {
		http.Error(w, fmt.Sprintf("head fetch failed: %v", err), http.StatusBadGateway)
		return
	}

	// Roll the merge base to the head and clear diverged. Ordering note: the DB
	// says "clean" before the CLI writes the file; if the CLI write then fails,
	// the next reconcile sweep sees diskHash != last_synced_hash and re-enqueues
	// — self-healing, acceptable.
	if err := s.syncDatabase().AdoptDocument(req.NotespaceID, req.Path, snap.ID, snap.Version, snap.Hash, content); err != nil {
		http.Error(w, fmt.Sprintf("adopt failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Hash", snap.Hash)
	_, _ = w.Write(content)
}

// snapshotDoc is the subset of a snapshot document entry the adopt handler
// needs (id/version/hash), decoupling the handler from the wire struct.
type snapshotDoc struct {
	ID      string
	Version int64
	Hash    string
}

type syncIncomingResponse struct {
	Manifest       syncdb.ReturnManifest `json:"manifest"`
	Clean          bool                  `json:"clean"`
	EscrowPath     string                `json:"escrow_path,omitempty"`
	EscrowVerified bool                  `json:"escrow_verified"`
}

func requestedReturnNotespaces(r *http.Request) ([]string, error) {
	raw := r.URL.Query().Get("notespace_ids")
	var out []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("an explicit non-empty notespaces list is required")
	}
	return out, nil
}

func (s *Server) currentReturnManifest(ctx context.Context, notespaces []string) (syncdb.ReturnManifest, error) {
	client, err := s.historyClient(ctx)
	if err != nil {
		return syncdb.ReturnManifest{}, err
	}
	return syncdb.BuildReturnManifest(ctx, client, s.syncDatabase(), notespaces)
}

func returnEscrowDir(satellite string) (string, error) {
	if satellite == "" || filepath.Base(satellite) != satellite {
		return "", fmt.Errorf("a valid satellite name is required")
	}
	return filepath.Join(paths.StateDir(), "satellites", satellite, "record-return"), nil
}

func findVerifiedReturnEscrow(dir, generation string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].IsDir() {
			continue
		}
		p := filepath.Join(dir, entries[i].Name())
		if syncdb.VerifyReturnEscrow(p, generation) == nil {
			return p
		}
	}
	return ""
}

// handleSyncIncoming is read-only: it compares server heads against laptop
// tracked state and never writes notebook files.
func (s *Server) handleSyncIncoming(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}
	notespaces, err := requestedReturnNotespaces(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m, err := s.currentReturnManifest(r.Context(), notespaces)
	if err != nil {
		http.Error(w, fmt.Sprintf("incoming manifest failed: %v", err), http.StatusBadGateway)
		return
	}
	resp := syncIncomingResponse{Manifest: m, Clean: len(m.Operations) == 0}
	if dir, derr := returnEscrowDir(r.URL.Query().Get("satellite")); derr == nil {
		resp.EscrowPath = findVerifiedReturnEscrow(dir, m.Generation)
		resp.EscrowVerified = resp.EscrowPath != ""
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleSyncEscrow accepts a reviewed manifest, recomputes it against current
// local/server state (stale review refusal), then durably writes verified head
// content under the laptop satellite state directory.
func (s *Server) handleSyncEscrow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Satellite string                `json:"satellite"`
		Manifest  syncdb.ReturnManifest `json:"manifest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := req.Manifest.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	current, err := s.currentReturnManifest(r.Context(), req.Manifest.Notespaces)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := syncdb.ValidateReviewedManifest(req.Manifest, current); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	dir, err := returnEscrowDir(req.Satellite)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client, err := s.historyClient(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	path, err := syncdb.WriteReturnEscrow(r.Context(), client, req.Manifest, dir)
	if err != nil {
		http.Error(w, fmt.Sprintf("escrow failed: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"path": path, "generation": req.Manifest.Generation, "verified": true})
}

type syncApplyResult struct {
	Schema     string                   `json:"schema"`
	Generation string                   `json:"generation"`
	EscrowPath string                   `json:"escrow_path,omitempty"`
	Counts     syncdb.ReturnApplyCounts `json:"counts"`
	Outcome    string                   `json:"outcome"`
}

// handleSyncApply is the user-authorized laptop write boundary. It rechecks
// the reviewed generation, writes and verifies its durable escrow, validates
// and stages the whole filesystem batch, then advances sync identity state.
func (s *Server) handleSyncApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDatabase() == nil || s.syncNotespaceRoots == nil {
		http.Error(w, "sync apply is not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Satellite string                `json:"satellite"`
		Manifest  syncdb.ReturnManifest `json:"manifest"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if _, err := returnEscrowDir(req.Satellite); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := req.Manifest.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	current, err := s.currentReturnManifest(r.Context(), req.Manifest.Notespaces)
	if err != nil {
		http.Error(w, fmt.Sprintf("cannot recheck incoming generation: %v", err), http.StatusBadGateway)
		return
	}
	if err = syncdb.ValidateReviewedManifest(req.Manifest, current); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	result := syncApplyResult{Schema: "grove.record-return-apply/v1", Generation: current.Generation, Outcome: "clean"}
	if len(current.Operations) == 0 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	// Refuse to adopt past an unpushed local change, mirroring handleSyncAdopt:
	// the adopted server head would otherwise overwrite lines the hub has never
	// seen. The local hash preconditions cannot catch this on their own — the
	// manifest's base_hash tracks the watcher-updated content hash, so a
	// locally-edited-but-unpushed file still matches.
	if pending, pendErr := s.syncDatabase().PendingReturnPush(current); pendErr != nil {
		http.Error(w, fmt.Sprintf("outbox lookup failed: %v", pendErr), http.StatusInternalServerError)
		return
	} else if pending != "" {
		http.Error(w, fmt.Sprintf("a pending push exists for %s; wait for it to drain before applying", pending), http.StatusConflict)
		return
	}
	roots, err := s.syncNotespaceRoots(current.Notespaces)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dir, _ := returnEscrowDir(req.Satellite)
	client, err := s.historyClient(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("sync server unavailable: %v", err), http.StatusBadGateway)
		return
	}
	escrowPath, err := syncdb.WriteReturnEscrow(r.Context(), client, current, dir)
	if err != nil {
		http.Error(w, fmt.Sprintf("escrow failed: %v", err), http.StatusInternalServerError)
		return
	}
	// Escrow creation may fetch several blobs. Recheck once more after it so a
	// server generation change or disconnect during that window fails before
	// the first notebook mutation. The verified escrow remains recoverable.
	final, err := s.currentReturnManifest(r.Context(), current.Notespaces)
	if err != nil {
		http.Error(w, fmt.Sprintf("cannot recheck incoming generation (verified escrow retained at %s): %v", escrowPath, err), http.StatusBadGateway)
		return
	}
	if err = syncdb.ValidateReviewedManifest(current, final); err != nil {
		http.Error(w, fmt.Sprintf("%v (verified escrow retained at %s)", err, escrowPath), http.StatusConflict)
		return
	}
	counts, err := syncdb.ApplyReturnEscrow(escrowPath, current.Generation, syncdb.ReturnApplyOptions{
		NotespaceRoots: roots,
		Reconcile:      s.syncDatabase().ReconcileReturnEscrow,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("apply failed (verified escrow retained at %s): %v", escrowPath, err), http.StatusConflict)
		return
	}
	for _, ws := range current.Notespaces {
		if s.syncKick != nil {
			s.syncKick(ws)
		}
	}
	result.EscrowPath, result.Counts, result.Outcome = escrowPath, counts, "applied"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func nonTerminalJob(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "abandoned", "error", "stopped":
		return false
	}
	return true
}

// handleSyncMaintenance establishes the destructive drain barrier. A named
// target blocks laptop dispatch; target "" blocks guest-local submits.
func (s *Server) handleSyncMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	var req struct {
		Action string `json:"action"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "enter":
		s.maintenanceMu.Lock()
		s.maintenanceTargets[req.Target] = true
		s.maintenanceMu.Unlock()
		active := 0
		if s.engine != nil {
			for _, j := range s.engine.Store().GetJobs() {
				if req.Target == "" {
					if j.Origin == "" && nonTerminalJob(j.Status) {
						active++
					}
				} else if j.Origin == req.Target && nonTerminalJob(j.Status) {
					active++
				}
			}
		}
		if active > 0 {
			http.Error(w, fmt.Sprintf("%d managed job(s) are still active", active), http.StatusConflict)
			return
		}
		if s.syncBeginMaintenance == nil {
			http.Error(w, "sync drain is unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := s.syncBeginMaintenance(r.Context()); err != nil {
			http.Error(w, fmt.Sprintf("sync drain failed: %v", err), http.StatusConflict)
			return
		}
	case "leave":
		s.maintenanceMu.Lock()
		delete(s.maintenanceTargets, req.Target)
		s.maintenanceMu.Unlock()
		if s.syncEndMaintenance != nil {
			s.syncEndMaintenance()
		}
	case "status":
	default:
		http.Error(w, "action must be enter, status, or leave", http.StatusBadRequest)
		return
	}
	pending, parked, diverged := 0, 0, 0
	if s.syncDatabase() != nil {
		pending, _ = s.syncDatabase().CountOutbox()
		parked, _ = s.syncDatabase().CountOutboxParked()
		diverged, _ = s.syncDatabase().CountDocumentsDiverged()
	}
	s.maintenanceMu.RLock()
	draining := s.maintenanceTargets[req.Target]
	s.maintenanceMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"draining": draining, "outbox_pending": pending, "outbox_parked": parked, "documents_diverged": diverged})
}

// handleSyncRepush handles POST /api/sync/repush with optional body
// {"notespace": "<name>"} (absent/empty = all notespaces). It is the manual
// edition of the automatic server-epoch recovery: it voids the
// server-confirmed sync state of every non-diverged document
// (ResetForRepush — last_synced_* zeroed, document ids kept, obsolete outbox
// entries dropped, pull cursor reset) and kicks an immediate anti-entropy
// pass, whose sweep re-pushes the full document set as document_created
// events. For when a sync server was recreated out-of-band or local state is
// otherwise suspected stale. Modeled on handleSyncAdopt (unix-only,
// global-scope forwarded).
func (s *Server) handleSyncRepush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Scoped daemons never open sync.db — forward to the global daemon.
	if s.scope != "" {
		s.forwardSyncToGlobal(w, r)
		return
	}
	if s.syncDatabase() == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}

	// The body is optional: absent/empty selects all notespaces.
	var req struct {
		NotespaceID string `json:"notespace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	var (
		notespaces []string
		reset      int
		err        error
	)
	if req.NotespaceID == "" {
		reset, notespaces, err = s.syncDatabase().ResetForRepushAll()
	} else {
		reset, err = s.syncDatabase().ResetForRepush(req.NotespaceID)
		notespaces = []string{req.NotespaceID}
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("repush reset failed: %v", err), http.StatusInternalServerError)
		return
	}
	if notespaces == nil {
		notespaces = []string{}
	}

	// Convert the reset into pushes now rather than at the next hourly tick.
	if s.syncKick != nil {
		s.syncKick(req.NotespaceID)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"notespaces":      notespaces,
		"documents_reset": reset,
	})
}
