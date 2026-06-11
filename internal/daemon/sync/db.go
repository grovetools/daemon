// Package sync implements the daemon-side state store for the notebook sync
// protocol (Phase 0): a dedicated SQLite database at
// ~/.local/share/grove/sync/sync.db holding
//
//   - sync_documents — the document identity map (path ↔ stable document
//     UUID) plus per-document sync state: the current local content hash
//     (hash-gating / echo suppression), the last server-confirmed
//     hash/version, and the 3-way merge base content,
//   - sync_outbox — the durable queue of local changes awaiting push,
//   - sync_state — per-workspace replication cursor + origin id,
//   - sync_meta — install-wide keys, most importantly the persistent
//     origin id used for echo suppression.
//
// The database is owned exclusively by the global daemon; scoped daemons
// proxy /api/sync/* to the global daemon, mirroring the memory.db pattern.
// Phase 0 is notebook-read-only and serverless: this package records local
// state only — nothing here writes to the notebook tree or talks to a
// server.
package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/grovetools/core/pkg/paths"
	_ "github.com/mattn/go-sqlite3" // sqlite driver (same driver memory.db uses)
)

// schema is idempotent: every statement is IF NOT EXISTS so Open can run it
// on every boot. Phase 0 ships the sync-shaped schema in full so Phase 1
// lands without a migration.
const schema = `
CREATE TABLE IF NOT EXISTS sync_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sync_documents (
	document_id         TEXT PRIMARY KEY,
	workspace           TEXT NOT NULL,
	path                TEXT NOT NULL,
	content_hash        TEXT NOT NULL DEFAULT '',
	last_synced_hash    TEXT NOT NULL DEFAULT '',
	last_synced_version INTEGER NOT NULL DEFAULT 0,
	base_content        BLOB,
	updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(workspace, path)
);

CREATE TABLE IF NOT EXISTS sync_outbox (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	document_id  TEXT NOT NULL DEFAULT '',
	workspace    TEXT NOT NULL,
	event_type   TEXT NOT NULL,
	path         TEXT NOT NULL,
	prev_path    TEXT NOT NULL DEFAULT '',
	content_hash TEXT NOT NULL DEFAULT '',
	payload      TEXT NOT NULL DEFAULT '',
	created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sync_state (
	workspace      TEXT PRIMARY KEY,
	cursor         INTEGER NOT NULL DEFAULT 0,
	origin_id      TEXT NOT NULL DEFAULT '',
	last_synced_at DATETIME
);
`

// DB wraps the sync SQLite database.
type DB struct {
	db       *sql.DB
	path     string
	originID string
}

// Document is one row of sync_documents: the identity map entry and sync
// state for a single notebook document. Path is the slash-normalized
// workspace-relative wire path (syncproto.NormalizePath form).
type Document struct {
	DocumentID        string
	Workspace         string
	Path              string
	ContentHash       string // current local content hash (sha256 hex)
	LastSyncedHash    string // hash last confirmed by the server
	LastSyncedVersion int64  // version last confirmed by the server
	BaseContent       []byte // 3-way merge base (server content at last sync)
	UpdatedAt         time.Time
}

// OutboxEntry is one pending local change awaiting push.
type OutboxEntry struct {
	ID          int64
	DocumentID  string
	Workspace   string
	EventType   string // syncproto.Event* constant
	Path        string
	PrevPath    string // for moved events
	ContentHash string
	Payload     string
	CreatedAt   time.Time
}

// WorkspaceState is the per-workspace replication state.
type WorkspaceState struct {
	Workspace    string
	Cursor       int64
	OriginID     string
	LastSyncedAt time.Time
}

// DefaultDBPath returns the canonical sync database location,
// <data-dir>/sync/sync.db (~/.local/share/grove/sync/sync.db by default,
// honoring GROVE_HOME/XDG overrides for hermetic tests).
func DefaultDBPath() string {
	return filepath.Join(paths.DataDir(), "sync", "sync.db")
}

// Open opens (creating if necessary) the sync database at path, applies the
// schema, and ensures the persistent per-install origin id exists.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create sync db directory: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sync db: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to apply sync db schema: %w", err)
	}

	d := &DB{db: db, path: path}
	if err := d.ensureOriginID(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the underlying database.
func (d *DB) Close() error {
	return d.db.Close()
}

// Path returns the database file path.
func (d *DB) Path() string {
	return d.path
}

// OriginID returns the persistent per-install origin id (echo suppression
// dedup key, distinct from user identity). Generated once on first Open.
func (d *DB) OriginID() string {
	return d.originID
}

func (d *DB) ensureOriginID() error {
	var origin string
	err := d.db.QueryRow(`SELECT value FROM sync_meta WHERE key = 'origin_id'`).Scan(&origin)
	switch {
	case err == sql.ErrNoRows:
		origin = uuid.New().String()
		if _, err := d.db.Exec(`INSERT INTO sync_meta (key, value) VALUES ('origin_id', ?)`, origin); err != nil {
			return fmt.Errorf("failed to persist origin id: %w", err)
		}
	case err != nil:
		return fmt.Errorf("failed to read origin id: %w", err)
	}
	d.originID = origin
	return nil
}

// GetDocumentByPath returns the document for (workspace, path), or nil when
// the path is untracked.
func (d *DB) GetDocumentByPath(workspace, path string) (*Document, error) {
	return d.scanDocument(d.db.QueryRow(
		`SELECT document_id, workspace, path, content_hash, last_synced_hash, last_synced_version, base_content, updated_at
		 FROM sync_documents WHERE workspace = ? AND path = ?`, workspace, path))
}

// GetDocument returns the document by its stable UUID, or nil when unknown.
func (d *DB) GetDocument(documentID string) (*Document, error) {
	return d.scanDocument(d.db.QueryRow(
		`SELECT document_id, workspace, path, content_hash, last_synced_hash, last_synced_version, base_content, updated_at
		 FROM sync_documents WHERE document_id = ?`, documentID))
}

func (d *DB) scanDocument(row *sql.Row) (*Document, error) {
	var doc Document
	err := row.Scan(&doc.DocumentID, &doc.Workspace, &doc.Path, &doc.ContentHash,
		&doc.LastSyncedHash, &doc.LastSyncedVersion, &doc.BaseContent, &doc.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan sync document: %w", err)
	}
	return &doc, nil
}

// UpsertDocument inserts a new document or, when the document_id already
// exists, updates its workspace/path/content_hash while preserving the
// last-synced fields and base content (those advance only on server
// confirmation, via SetSynced in Phase 1).
func (d *DB) UpsertDocument(doc *Document) error {
	_, err := d.db.Exec(
		`INSERT INTO sync_documents (document_id, workspace, path, content_hash, last_synced_hash, last_synced_version, base_content, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(document_id) DO UPDATE SET
			workspace = excluded.workspace,
			path = excluded.path,
			content_hash = excluded.content_hash,
			updated_at = CURRENT_TIMESTAMP`,
		doc.DocumentID, doc.Workspace, doc.Path, doc.ContentHash,
		doc.LastSyncedHash, doc.LastSyncedVersion, doc.BaseContent)
	if err != nil {
		return fmt.Errorf("failed to upsert sync document %s: %w", doc.DocumentID, err)
	}
	return nil
}

// MoveDocument updates a document's path in place (same UUID, new path) —
// the rename-detection write path fed by nb's typed move events.
func (d *DB) MoveDocument(documentID, newPath string) error {
	res, err := d.db.Exec(
		`UPDATE sync_documents SET path = ?, updated_at = CURRENT_TIMESTAMP WHERE document_id = ?`,
		newPath, documentID)
	if err != nil {
		return fmt.Errorf("failed to move sync document %s: %w", documentID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("sync document %s not found", documentID)
	}
	return nil
}

// DeleteDocument removes a document from the identity map.
func (d *DB) DeleteDocument(documentID string) error {
	if _, err := d.db.Exec(`DELETE FROM sync_documents WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("failed to delete sync document %s: %w", documentID, err)
	}
	return nil
}

// CountDocuments returns the number of tracked documents.
func (d *DB) CountDocuments() (int, error) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sync_documents`).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count sync documents: %w", err)
	}
	return n, nil
}

// EnqueueOutbox appends a pending change to the outbox and returns its id.
func (d *DB) EnqueueOutbox(e *OutboxEntry) (int64, error) {
	res, err := d.db.Exec(
		`INSERT INTO sync_outbox (document_id, workspace, event_type, path, prev_path, content_hash, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.DocumentID, e.Workspace, e.EventType, e.Path, e.PrevPath, e.ContentHash, e.Payload)
	if err != nil {
		return 0, fmt.Errorf("failed to enqueue sync outbox entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to read outbox insert id: %w", err)
	}
	return id, nil
}

// ListOutbox returns pending outbox entries in insertion order. An empty
// workspace lists all workspaces; limit <= 0 means no limit.
func (d *DB) ListOutbox(workspace string, limit int) ([]*OutboxEntry, error) {
	query := `SELECT id, document_id, workspace, event_type, path, prev_path, content_hash, payload, created_at
		 FROM sync_outbox`
	var args []interface{}
	if workspace != "" {
		query += ` WHERE workspace = ?`
		args = append(args, workspace)
	}
	query += ` ORDER BY id ASC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list sync outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*OutboxEntry
	for rows.Next() {
		var e OutboxEntry
		if err := rows.Scan(&e.ID, &e.DocumentID, &e.Workspace, &e.EventType, &e.Path,
			&e.PrevPath, &e.ContentHash, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan sync outbox entry: %w", err)
		}
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}

// DeleteOutbox removes acknowledged outbox entries by id.
func (d *DB) DeleteOutbox(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin outbox delete: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM sync_outbox WHERE id = ?`, id); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed to delete sync outbox entry %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// CountOutbox returns the number of pending outbox entries.
func (d *DB) CountOutbox() (int, error) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sync_outbox`).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count sync outbox: %w", err)
	}
	return n, nil
}

// GetState returns the replication state for a workspace, or nil when the
// workspace has never synced.
func (d *DB) GetState(workspace string) (*WorkspaceState, error) {
	var st WorkspaceState
	var lastSynced sql.NullTime
	err := d.db.QueryRow(
		`SELECT workspace, cursor, origin_id, last_synced_at FROM sync_state WHERE workspace = ?`,
		workspace).Scan(&st.Workspace, &st.Cursor, &st.OriginID, &lastSynced)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read sync state for %s: %w", workspace, err)
	}
	if lastSynced.Valid {
		st.LastSyncedAt = lastSynced.Time
	}
	return &st, nil
}

// ListStates returns the replication state of every workspace that has one.
func (d *DB) ListStates() ([]*WorkspaceState, error) {
	rows, err := d.db.Query(`SELECT workspace, cursor, origin_id, last_synced_at FROM sync_state ORDER BY workspace`)
	if err != nil {
		return nil, fmt.Errorf("failed to list sync states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var states []*WorkspaceState
	for rows.Next() {
		var st WorkspaceState
		var lastSynced sql.NullTime
		if err := rows.Scan(&st.Workspace, &st.Cursor, &st.OriginID, &lastSynced); err != nil {
			return nil, fmt.Errorf("failed to scan sync state: %w", err)
		}
		if lastSynced.Valid {
			st.LastSyncedAt = lastSynced.Time
		}
		states = append(states, &st)
	}
	return states, rows.Err()
}

// SetCursor advances a workspace's replication cursor, stamping the install
// origin id and last-synced time.
func (d *DB) SetCursor(workspace string, cursor int64) error {
	_, err := d.db.Exec(
		`INSERT INTO sync_state (workspace, cursor, origin_id, last_synced_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(workspace) DO UPDATE SET
			cursor = excluded.cursor,
			origin_id = excluded.origin_id,
			last_synced_at = CURRENT_TIMESTAMP`,
		workspace, cursor, d.originID)
	if err != nil {
		return fmt.Errorf("failed to set sync cursor for %s: %w", workspace, err)
	}
	return nil
}
