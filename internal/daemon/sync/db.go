// Package sync implements the daemon-side state store for the notebook sync
// protocol (Phase 0): a dedicated SQLite database at
// ~/.local/share/grove/sync/sync.db holding
//
//   - sync_documents — the document identity map (path ↔ stable document
//     UUID) plus per-document sync state: the current local content hash
//     (hash-gating / echo suppression), the last server-confirmed
//     hash/version, and the 3-way merge base content,
//   - sync_outbox — the durable queue of local changes awaiting push,
//   - sync_state — per-notespace replication cursor + origin id,
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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
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

CREATE TABLE IF NOT EXISTS sync_notespaces (
	notespace_id   TEXT PRIMARY KEY,
	notespace_name TEXT NOT NULL,
	root           TEXT NOT NULL UNIQUE,
	subject        TEXT NOT NULL,
	kind           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sync_documents (
	document_id         TEXT PRIMARY KEY,
	notespace           TEXT NOT NULL,
	path                TEXT NOT NULL,
	content_hash        TEXT NOT NULL DEFAULT '',
	last_synced_hash    TEXT NOT NULL DEFAULT '',
	last_synced_version INTEGER NOT NULL DEFAULT 0,
	base_content        BLOB,
	diverged            INTEGER NOT NULL DEFAULT 0,
	updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(notespace, path)
);

CREATE TABLE IF NOT EXISTS sync_outbox (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	document_id   TEXT NOT NULL DEFAULT '',
	notespace     TEXT NOT NULL,
	event_type    TEXT NOT NULL,
	path          TEXT NOT NULL,
	prev_path     TEXT NOT NULL DEFAULT '',
	content_hash  TEXT NOT NULL DEFAULT '',
	payload       TEXT NOT NULL DEFAULT '',
	base_version  INTEGER NOT NULL DEFAULT 0,
	mtime         INTEGER NOT NULL DEFAULT 0,
	created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	parked        INTEGER NOT NULL DEFAULT 0,
	attempts      INTEGER NOT NULL DEFAULT 0,
	next_retry_at DATETIME,
	park_reason   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sync_state (
	notespace      TEXT PRIMARY KEY,
	cursor         INTEGER NOT NULL DEFAULT 0,
	origin_id      TEXT NOT NULL DEFAULT '',
	last_synced_at DATETIME
);

CREATE TABLE IF NOT EXISTS sync_quarantine_override (
	notespace TEXT NOT NULL,
	path      TEXT NOT NULL,
	PRIMARY KEY (notespace, path)
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
// notespace-relative wire path (syncproto.NormalizePath form).
type Document struct {
	DocumentID        string
	Notespace         string
	Path              string
	ContentHash       string // current local content hash (sha256 hex)
	LastSyncedHash    string // hash last confirmed by the server
	LastSyncedVersion int64  // version last confirmed by the server
	BaseContent       []byte // 3-way merge base (server content at last sync)
	// Diverged (P5, S5) marks a document whose merged server head was pushed
	// but whose local file was deliberately NOT rewritten — the local file
	// lags the server head. A diverged doc is frozen from the push sweep and
	// the watcher flush until the user runs `nb sync adopt`; strict push-only
	// means sync never writes the notespace tree to resolve the lag.
	Diverged  bool
	UpdatedAt time.Time
}

// documentColumns is the shared SELECT column list for document scanners; the
// order must match scanDocumentRow.
const documentColumns = `document_id, notespace, path, content_hash, last_synced_hash, last_synced_version, base_content, diverged, updated_at`

// OutboxEntry is one pending local change awaiting push.
type OutboxEntry struct {
	ID          int64
	DocumentID  string
	Notespace   string
	EventType   string // syncproto.Event* constant
	Path        string
	PrevPath    string // for moved events
	ContentHash string
	Payload     string

	// BaseVersion is the server version this change is based on (the doc's
	// last_synced_version captured at enqueue time). It matters for deleted
	// events (B7): recordDelete destroys the sync_documents row immediately —
	// keeping the row alive would break delete-then-recreate on the
	// UNIQUE(notespace, path) constraint — so the entry itself must carry the
	// OCC base or every delete of a server-known doc pushes base_version 0 and
	// parks as a manufactured conflict forever. Updated/moved events resolve
	// their base from the still-live doc row at drain time instead.
	BaseVersion int64

	// Mtime is the source file's modification time captured by a stat at
	// enqueue time (fidelity metadata for the wire event's Mtime field; never
	// an ordering/OCC input). Zero when the stat failed or the event carries
	// no file (deletes) — zero rides the wire as "unknown" and replicas keep
	// today's behavior. Stored as unix nanoseconds (INTEGER, 0 default).
	Mtime     time.Time
	CreatedAt time.Time

	// Phase 4 parking state. A parked entry stays queued (visible to
	// ListOutbox / CountOutbox — a parked entry IS unsynced) but is skipped by
	// ListOutboxDrainable until NextRetryAt passes. ParkReason is a free-form
	// string ("conflict", "oversize_skipped"; P5 adds "diverged").
	Parked      bool
	Attempts    int
	NextRetryAt time.Time
	ParkReason  string
}

// outboxColumns is the shared SELECT column list for outbox scanners; the
// order must match scanOutboxEntry.
const outboxColumns = `id, document_id, notespace, event_type, path, prev_path, content_hash, payload, base_version, mtime, created_at, parked, attempts, next_retry_at, park_reason`

// scanOutboxEntry scans one outbox row selected via outboxColumns. next_retry_at
// is nullable (an entry only has a retry time once parked); parked is stored as
// an INTEGER 0/1.
func scanOutboxEntry(rows *sql.Rows) (*OutboxEntry, error) {
	var e OutboxEntry
	var parked int64
	var mtimeNanos int64
	var nextRetry sql.NullTime
	if err := rows.Scan(&e.ID, &e.DocumentID, &e.Notespace, &e.EventType, &e.Path,
		&e.PrevPath, &e.ContentHash, &e.Payload, &e.BaseVersion, &mtimeNanos, &e.CreatedAt,
		&parked, &e.Attempts, &nextRetry, &e.ParkReason); err != nil {
		return nil, err
	}
	e.Parked = parked != 0
	e.Mtime = nanosToMtime(mtimeNanos)
	if nextRetry.Valid {
		e.NextRetryAt = nextRetry.Time
	}
	return &e, nil
}

// mtimeToNanos maps a file mtime to its INTEGER unix-nanosecond storage form;
// the zero time (mtime unknown) maps to 0, never to the 1970 epoch.
func mtimeToNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// nanosToMtime is the inverse of mtimeToNanos: 0 means "unknown" and maps back
// to the zero time (time.Unix(0, 0) would be the 1970 epoch, not zero).
func nanosToMtime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// NotespaceState is the per-notespace replication state.
type NotespaceState struct {
	Notespace    string
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
	state, err := InspectSchema(path)
	if err != nil {
		return nil, err
	}
	if state.Legacy {
		return nil, ErrLegacySchema
	}
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

	// The client sync.db has no versioned migration mechanism (the server's
	// embedded-FS migrations in sync/pkg/store are a different database). The
	// CREATE TABLE IF NOT EXISTS schema above never alters an existing table,
	// so columns added after a DB was first created are applied here with
	// idempotent ALTER-swallow guards.
	if err := migrateOutbox(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateDocuments(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	d := &DB{db: db, path: path}
	if err := d.ensureOriginID(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// migrateOutbox brings a pre-existing sync_outbox up to the current schema by
// adding the Phase 4 parking columns to DBs created before they existed. Each
// ALTER is idempotent: on a fresh DB (where the schema const already created
// the columns) SQLite reports "duplicate column name", which is swallowed —
// this is the established net-new pattern for this migration-less client DB.
func migrateOutbox(db *sql.DB) error {
	alters := []string{
		`ALTER TABLE sync_outbox ADD COLUMN parked INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sync_outbox ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sync_outbox ADD COLUMN next_retry_at DATETIME`,
		`ALTER TABLE sync_outbox ADD COLUMN park_reason TEXT NOT NULL DEFAULT ''`,
		// B7: OCC base carried on the entry itself. A deleted event's doc row
		// is destroyed at enqueue time (recordDelete), so the entry is the only
		// place the last-synced version can survive until push.
		`ALTER TABLE sync_outbox ADD COLUMN base_version INTEGER NOT NULL DEFAULT 0`,
		// File mtime captured at enqueue (unix nanoseconds; 0 = unknown) so
		// replicas can restore filesystem timestamps. Fidelity metadata only.
		`ALTER TABLE sync_outbox ADD COLUMN mtime INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range alters {
		if _, err := db.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue // column already present — idempotent no-op
			}
			return fmt.Errorf("failed to migrate sync_outbox: %w", err)
		}
	}
	return nil
}

// migrateDocuments brings a pre-existing sync_documents up to the current
// schema by adding the Phase 5 diverged column to DBs created before it
// existed. Same idempotent ALTER-swallow pattern as migrateOutbox: on a fresh
// DB (where the schema const already created the column) SQLite reports
// "duplicate column name", which is swallowed.
func migrateDocuments(db *sql.DB) error {
	alters := []string{
		`ALTER TABLE sync_documents ADD COLUMN diverged INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range alters {
		if _, err := db.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue // column already present — idempotent no-op
			}
			return fmt.Errorf("failed to migrate sync_documents: %w", err)
		}
	}
	return nil
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

// GetServerEpoch returns the last-seen server epoch persisted in sync_meta,
// or "" when no handshake has recorded one yet (first contact, or every
// handshake so far was against a pre-epoch server).
func (d *DB) GetServerEpoch() (string, error) {
	var epoch string
	err := d.db.QueryRow(`SELECT value FROM sync_meta WHERE key = 'server_epoch'`).Scan(&epoch)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read server epoch: %w", err)
	}
	return epoch, nil
}

// SetServerEpoch persists the server epoch last seen in a capabilities
// handshake. CheckServerEpoch compares it against the next handshake's epoch
// to detect a recreated (fresh, empty) server.
func (d *DB) SetServerEpoch(epoch string) error {
	_, err := d.db.Exec(
		`INSERT INTO sync_meta (key, value) VALUES ('server_epoch', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, epoch)
	if err != nil {
		return fmt.Errorf("failed to persist server epoch: %w", err)
	}
	return nil
}

// ResetForRepush voids a notespace's server-confirmed sync state so the next
// anti-entropy sweep re-pushes every document as a document_created — the
// recovery primitive for a recreated (fresh, empty) server, where local
// "already synced" state points at documents that no longer exist. It
// returns the number of documents reset.
//
// For every NON-diverged document: last_synced_hash/'version are zeroed
// (sweepLocalDocuments then chooses document_created, and the drain-time
// no-op guard no longer drops it) while document_id is KEPT — the server's
// create branch inserts the pushed id fresh, so identities stay stable across
// the recreate. The notespace's non-diverged outbox entries are deleted
// (queued/parked UPDATEs against the dead server are obsolete; the sweep
// re-enqueues from disk) and the pull cursor resets to 0. Diverged documents
// — and their outbox entries, whose Payload may carry an unpushed merge — are
// left untouched: they resolve only via explicit `nb sync adopt`.
func (d *DB) ResetForRepush(notespace string) (int, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin repush reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`UPDATE sync_documents SET last_synced_hash = '', last_synced_version = 0, updated_at = CURRENT_TIMESTAMP
		 WHERE notespace = ? AND diverged = 0`, notespace)
	if err != nil {
		return 0, fmt.Errorf("failed to reset documents for repush in %s: %w", notespace, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to count repush reset in %s: %w", notespace, err)
	}

	if _, err := tx.Exec(
		`DELETE FROM sync_outbox WHERE notespace = ?
		 AND document_id NOT IN (SELECT document_id FROM sync_documents WHERE notespace = ? AND diverged = 1)`,
		notespace, notespace); err != nil {
		return 0, fmt.Errorf("failed to clear outbox for repush in %s: %w", notespace, err)
	}

	if _, err := tx.Exec(
		`UPDATE sync_state SET cursor = 0 WHERE notespace = ?`, notespace); err != nil {
		return 0, fmt.Errorf("failed to reset cursor for repush in %s: %w", notespace, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit repush reset for %s: %w", notespace, err)
	}
	return int(n), nil
}

// ResetForRepushAll runs ResetForRepush over every notespace known to
// sync_documents or sync_state, returning the total documents reset and the
// notespaces touched.
func (d *DB) ResetForRepushAll() (int, []string, error) {
	rows, err := d.db.Query(
		`SELECT notespace FROM sync_documents
		 UNION SELECT notespace FROM sync_state ORDER BY notespace`)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to list notespaces for repush: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var notespaces []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return 0, nil, fmt.Errorf("failed to scan notespace for repush: %w", err)
		}
		notespaces = append(notespaces, ws)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	total := 0
	for _, ws := range notespaces {
		n, err := d.ResetForRepush(ws)
		if err != nil {
			return total, notespaces, err
		}
		total += n
	}
	return total, notespaces, nil
}

// ClearDocumentSyncedState zeroes one document's server-confirmed state
// (last_synced_hash/'version) so the next anti-entropy sweep re-enqueues it
// as a document_created. The per-document edition of ResetForRepush, used by
// the push pipeline's unknown-document self-heal.
func (d *DB) ClearDocumentSyncedState(documentID string) error {
	if _, err := d.db.Exec(
		`UPDATE sync_documents SET last_synced_hash = '', last_synced_version = 0, updated_at = CURRENT_TIMESTAMP
		 WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("failed to clear synced state for %s: %w", documentID, err)
	}
	return nil
}

// GetDocumentByPath returns the document for (notespace, path), or nil when
// the path is untracked.
func (d *DB) GetDocumentByPath(notespace, path string) (*Document, error) {
	return scanDocumentRow(d.db.QueryRow(
		`SELECT `+documentColumns+` FROM sync_documents WHERE notespace = ? AND path = ?`,
		notespace, path).Scan)
}

// GetDocument returns the document by its stable UUID, or nil when unknown.
func (d *DB) GetDocument(documentID string) (*Document, error) {
	return scanDocumentRow(d.db.QueryRow(
		`SELECT `+documentColumns+` FROM sync_documents WHERE document_id = ?`,
		documentID).Scan)
}

// scanDocumentRow scans one sync_documents row selected via documentColumns
// (shared by the single-row QueryRow scanners and the ListDocuments loop, the
// consolidation P4 did for outbox). diverged is stored as an INTEGER 0/1; a
// sql.ErrNoRows from a single-row scan maps to (nil, nil) — untracked path.
func scanDocumentRow(scan func(dest ...any) error) (*Document, error) {
	var doc Document
	var diverged int64
	err := scan(&doc.DocumentID, &doc.Notespace, &doc.Path, &doc.ContentHash,
		&doc.LastSyncedHash, &doc.LastSyncedVersion, &doc.BaseContent, &diverged, &doc.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan sync document: %w", err)
	}
	doc.Diverged = diverged != 0
	return &doc, nil
}

// UpsertDocument inserts a new document or, when the document_id already
// exists, updates its notespace/path/content_hash while preserving the
// last-synced fields and base content (those advance only on server
// confirmation, via SetSynced in Phase 1).
func (d *DB) UpsertDocument(doc *Document) error {
	// The ON CONFLICT clause deliberately leaves last_synced_* AND diverged
	// untouched: those advance only on server confirmation / explicit adopt,
	// never from watcher-side content tracking.
	_, err := d.db.Exec(
		`INSERT INTO sync_documents (document_id, notespace, path, content_hash, last_synced_hash, last_synced_version, base_content, diverged, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(document_id) DO UPDATE SET
			notespace = excluded.notespace,
			path = excluded.path,
			content_hash = excluded.content_hash,
			updated_at = CURRENT_TIMESTAMP`,
		doc.DocumentID, doc.Notespace, doc.Path, doc.ContentHash,
		doc.LastSyncedHash, doc.LastSyncedVersion, doc.BaseContent, boolToInt(doc.Diverged))
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

// ListDocuments returns tracked documents ordered by notespace then path. An
// empty notespace lists every notespace; a non-empty one filters to it. Used
// by the read-only /api/sync/documents introspection endpoint (dev UI).
func (d *DB) ListDocuments(notespace string) ([]*Document, error) {
	query := `SELECT ` + documentColumns + ` FROM sync_documents`
	var args []interface{}
	if notespace != "" {
		query += ` WHERE notespace = ?`
		args = append(args, notespace)
	}
	query += ` ORDER BY notespace ASC, path ASC`

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list sync documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var docs []*Document
	for rows.Next() {
		doc, err := scanDocumentRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
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
		`INSERT INTO sync_outbox (document_id, notespace, event_type, path, prev_path, content_hash, payload, base_version, mtime)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.DocumentID, e.Notespace, e.EventType, e.Path, e.PrevPath, e.ContentHash, e.Payload, e.BaseVersion, mtimeToNanos(e.Mtime))
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
// notespace lists all notespaces; limit <= 0 means no limit.
func (d *DB) ListOutbox(notespace string, limit int) ([]*OutboxEntry, error) {
	query := `SELECT ` + outboxColumns + ` FROM sync_outbox`
	var args []interface{}
	if notespace != "" {
		query += ` WHERE notespace = ?`
		args = append(args, notespace)
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
		e, err := scanOutboxEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan sync outbox entry: %w", err)
		}
		entries = append(entries, e)
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

// UpdateOutboxContentHashForPath repoints every pending document_updated
// outbox entry for a path at rebased/merged content. Used after a 3-way merge
// rewrites the on-disk file (push-side rebase, pull-side merge onto a dirty
// file): the push pipeline reads content from disk at push time, so a parked
// entry carrying the pre-merge hash would fail the server's hash-integrity
// check and be dropped — silently losing the merged edit.
func (d *DB) UpdateOutboxContentHashForPath(notespace, path, hash string) error {
	_, err := d.db.Exec(
		`UPDATE sync_outbox SET content_hash = ? WHERE notespace = ? AND path = ? AND event_type = ?`,
		hash, notespace, path, syncproto.EventDocumentUpdated)
	if err != nil {
		return fmt.Errorf("failed to update outbox content hash for %s/%s: %w", notespace, path, err)
	}
	return nil
}

// UpdateOutboxEntryContent retargets a SINGLE outbox entry (by id) at new
// payload bytes and their hash. The push-side rebase (push.go
// rebaseConflictedEntry) holds the exact entry in hand and must carry the merged
// result as the entry's Payload — DrainOutbox prefers Payload over the disk
// read, so the merged content pushes without the local file ever being written.
// Distinct from the path-scoped, hash-only UpdateOutboxContentHashForPath (still
// used by pull.go applyUpdate).
func (d *DB) UpdateOutboxEntryContent(id int64, payload, contentHash string) error {
	_, err := d.db.Exec(
		`UPDATE sync_outbox SET payload = ?, content_hash = ? WHERE id = ?`,
		payload, contentHash, id)
	if err != nil {
		return fmt.Errorf("failed to update outbox entry %d content: %w", id, err)
	}
	return nil
}

// ParkOutbox marks an outbox entry parked with an exponential/next retry time
// and a reason, incrementing its attempt count. A parked entry stays in the
// outbox (still counted as pending, still visible to the anti-entropy sweep's
// pending-set) but ListOutboxDrainable skips it until nextRetryAt passes. The
// entry unparks implicitly: expiry makes it drainable again, a successful push
// deletes it, and a repeat conflict re-parks with attempts+1 → longer backoff.
func (d *DB) ParkOutbox(id int64, reason string, nextRetryAt time.Time) error {
	_, err := d.db.Exec(
		`UPDATE sync_outbox SET parked = 1, attempts = attempts + 1, next_retry_at = ?, park_reason = ? WHERE id = ?`,
		nextRetryAt, reason, id)
	if err != nil {
		return fmt.Errorf("failed to park sync outbox entry %d: %w", id, err)
	}
	return nil
}

// CountOutbox returns the number of pending outbox entries (parked included —
// a parked entry is still unsynced state).
func (d *DB) CountOutbox() (int, error) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sync_outbox`).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count sync outbox: %w", err)
	}
	return n, nil
}

// CountOutboxParked returns the number of parked outbox entries, for the
// /api/sync/status surface (the grove-status parked line, the S3 assertion).
func (d *DB) CountOutboxParked() (int, error) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sync_outbox WHERE parked = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count parked sync outbox: %w", err)
	}
	return n, nil
}

// CountOutboxForPath returns the number of pending outbox entries for a
// notespace/path (parked included). The adopt endpoint (item 5) uses it to
// refuse adopting past an unpushed merge — adopting to the server head while a
// merged payload is still queued would drop the user's merged-in lines from the
// hub.
func (d *DB) CountOutboxForPath(notespace, path string) (int, error) {
	var n int
	if err := d.db.QueryRow(
		`SELECT COUNT(*) FROM sync_outbox WHERE notespace = ? AND path = ?`,
		notespace, path).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count outbox for %s/%s: %w", notespace, path, err)
	}
	return n, nil
}

// GetState returns the replication state for a notespace, or nil when the
// notespace has never synced.
func (d *DB) GetState(notespace string) (*NotespaceState, error) {
	var st NotespaceState
	var lastSynced sql.NullTime
	err := d.db.QueryRow(
		`SELECT notespace, cursor, origin_id, last_synced_at FROM sync_state WHERE notespace = ?`,
		notespace).Scan(&st.Notespace, &st.Cursor, &st.OriginID, &lastSynced)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read sync state for %s: %w", notespace, err)
	}
	if lastSynced.Valid {
		st.LastSyncedAt = lastSynced.Time
	}
	return &st, nil
}

// ListStates returns the replication state of every notespace that has one.
func (d *DB) ListStates() ([]*NotespaceState, error) {
	rows, err := d.db.Query(`SELECT notespace, cursor, origin_id, last_synced_at FROM sync_state ORDER BY notespace`)
	if err != nil {
		return nil, fmt.Errorf("failed to list sync states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var states []*NotespaceState
	for rows.Next() {
		var st NotespaceState
		var lastSynced sql.NullTime
		if err := rows.Scan(&st.Notespace, &st.Cursor, &st.OriginID, &lastSynced); err != nil {
			return nil, fmt.Errorf("failed to scan sync state: %w", err)
		}
		if lastSynced.Valid {
			st.LastSyncedAt = lastSynced.Time
		}
		states = append(states, &st)
	}
	return states, rows.Err()
}

// SetCursor advances a notespace's replication cursor, stamping the install
// origin id and last-synced time.
func (d *DB) SetCursor(notespace string, cursor int64) error {
	_, err := d.db.Exec(
		`INSERT INTO sync_state (notespace, cursor, origin_id, last_synced_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(notespace) DO UPDATE SET
			cursor = excluded.cursor,
			origin_id = excluded.origin_id,
			last_synced_at = CURRENT_TIMESTAMP`,
		notespace, cursor, d.originID)
	if err != nil {
		return fmt.Errorf("failed to set sync cursor for %s: %w", notespace, err)
	}
	return nil
}

// SetDocumentVersion updates the version and last-synced hash for a document
// after successful push. Used by the push pipeline to record server confirmations.
func (d *DB) SetDocumentVersion(documentID string, version int64) error {
	res, err := d.db.Exec(
		`UPDATE sync_documents SET last_synced_version = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE document_id = ?`,
		version, documentID)
	if err != nil {
		return fmt.Errorf("failed to set document version for %s: %w", documentID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("sync document %s not found", documentID)
	}
	return nil
}

// MarkDocumentSynced records a server-confirmed push of an existing document:
// the pushed content is now the server head, so it becomes the last-synced
// state AND the 3-way merge base. Leaving last_synced_hash/base_content stale
// here makes the pull pipeline misjudge local dirtiness on the next remote
// update.
func (d *DB) MarkDocumentSynced(documentID string, version int64, hash string, content []byte) error {
	res, err := d.db.Exec(
		`UPDATE sync_documents SET last_synced_version = ?, last_synced_hash = ?, base_content = ?, content_hash = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE document_id = ?`,
		version, hash, content, hash, documentID)
	if err != nil {
		return fmt.Errorf("failed to mark document synced for %s: %w", documentID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("sync document %s not found", documentID)
	}
	return nil
}

// ToSyncEvent converts an OutboxEntry to a syncproto.SyncEvent for push,
// reading the document content and populating the event fields.
func (e *OutboxEntry) ToSyncEvent() syncproto.SyncEvent {
	return syncproto.SyncEvent{
		Type:        e.EventType,
		NotespaceID: syncproto.NotespaceID(e.Notespace),
		DocumentID:  e.DocumentID,
		Path:        e.Path,
		PrevPath:    e.PrevPath,
		ContentHash: e.ContentHash,
		// The enqueue-time OCC base (B7). Non-zero only for deleted events,
		// whose doc row no longer exists at drain time; DrainOutbox overwrites
		// or backfills it from the live doc row for updated/moved events.
		BaseVersion: e.BaseVersion,
		// The enqueue-time file mtime (fidelity metadata; zero = unknown).
		// DrainOutbox refreshes it from a stat when it re-reads disk content.
		Mtime: e.Mtime,
		// Content and Size are populated by the caller
	}
}

// IsQuarantineOverridden reports whether a notespace/path pair has been
// explicitly overridden to allow syncing despite secret quarantine matches.
func (d *DB) IsQuarantineOverridden(notespace, path string) (bool, error) {
	var count int
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM sync_quarantine_override WHERE notespace = ? AND path = ?`,
		notespace, path).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check quarantine override for %s/%s: %w", notespace, path, err)
	}
	return count > 0, nil
}

// SetQuarantineOverride adds a notespace/path to the quarantine override list,
// allowing it to sync despite secret pattern matches.
func (d *DB) SetQuarantineOverride(notespace, path string) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO sync_quarantine_override (notespace, path) VALUES (?, ?)`,
		notespace, path)
	if err != nil {
		return fmt.Errorf("failed to set quarantine override for %s/%s: %w", notespace, path, err)
	}
	return nil
}

// RemoveQuarantineOverride removes a notespace/path from the override list.
func (d *DB) RemoveQuarantineOverride(notespace, path string) error {
	_, err := d.db.Exec(
		`DELETE FROM sync_quarantine_override WHERE notespace = ? AND path = ?`,
		notespace, path)
	if err != nil {
		return fmt.Errorf("failed to remove quarantine override for %s/%s: %w", notespace, path, err)
	}
	return nil
}

// GetNotespaceCursor retrieves the current pull cursor for a notespace.
func (d *DB) GetNotespaceCursor(notespace string) (int64, error) {
	state, err := d.GetState(notespace)
	if err != nil {
		return 0, err
	}
	if state == nil {
		return 0, nil
	}
	return state.Cursor, nil
}

// UpdateNotespaceCursor updates the pull cursor for a notespace.
func (d *DB) UpdateNotespaceCursor(notespace string, cursor int64) error {
	return d.SetCursor(notespace, cursor)
}

// InsertDocument inserts a new document into sync_documents.
func (d *DB) InsertDocument(doc *Document) error {
	_, err := d.db.Exec(
		`INSERT INTO sync_documents (document_id, notespace, path, content_hash, last_synced_hash, last_synced_version, base_content, diverged)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.DocumentID, doc.Notespace, doc.Path, doc.ContentHash, doc.LastSyncedHash, doc.LastSyncedVersion, doc.BaseContent, boolToInt(doc.Diverged))
	if err != nil {
		return fmt.Errorf("failed to insert sync document %s: %w", doc.DocumentID, err)
	}
	return nil
}

// UpdateDocument updates an existing document in sync_documents.
func (d *DB) UpdateDocument(doc *Document) error {
	_, err := d.db.Exec(
		`UPDATE sync_documents SET content_hash = ?, last_synced_hash = ?, last_synced_version = ?, base_content = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE document_id = ?`,
		doc.ContentHash, doc.LastSyncedHash, doc.LastSyncedVersion, doc.BaseContent, doc.DocumentID)
	if err != nil {
		return fmt.Errorf("failed to update sync document %s: %w", doc.DocumentID, err)
	}
	return nil
}

// AdoptDocument records a server document in the local database, marking it as
// synced. Used during snapshot/anti-entropy reconciliation when local and
// remote hashes match. The hash-verified local content becomes the 3-way merge
// base: version, last_synced_hash, and base_content roll together (mirroring
// MarkDocumentSynced) — rolling the version alone leaves a stale merge base,
// the phantom-conflict trap.
func (d *DB) AdoptDocument(notespace, path, documentID string, version int64, hash string, content []byte) error {
	// diverged = 0: adopting the server head IS the resolution of divergence.
	// The reconcile self-heal path (antientropy.go reconcileDocument's hash-equal
	// branch) and `nb sync adopt` both land here, so a disk drift back to
	// hash-equal-with-server clears the diverged flag for free.
	_, err := d.db.Exec(
		`UPDATE sync_documents SET document_id = ?, last_synced_version = ?, last_synced_hash = ?, content_hash = ?, base_content = ?, diverged = 0, updated_at = CURRENT_TIMESTAMP
		 WHERE notespace = ? AND path = ?`,
		documentID, version, hash, hash, content, notespace, path)
	if err != nil {
		return fmt.Errorf("failed to adopt document %s: %w", documentID, err)
	}
	return nil
}

// PendingReturnPush reports the first adopted path that still has an unpushed
// local change queued, or "" when the batch is clear to apply. Adopting past
// an unpushed push would drop the user's local lines from the hub, so the
// apply boundary refuses — the same policy handleSyncAdopt applies to a single
// document. It is what makes ReconcileReturnEscrow's outbox purge safe: by the
// time reconcile runs, the only queued events for these paths are echoes of
// the writes the apply itself just made.
func (d *DB) PendingReturnPush(m ReturnManifest) (string, error) {
	for _, op := range m.Operations {
		var n int
		if err := d.db.QueryRow(
			`SELECT COUNT(*) FROM sync_outbox
			 WHERE document_id = ? OR (notespace = ? AND (path = ? OR (? != '' AND path = ?)))`,
			op.DocumentID, op.Notespace, op.Path, op.PreviousPath, op.PreviousPath).Scan(&n); err != nil {
			return "", fmt.Errorf("failed to count outbox for %s/%s: %w", op.Notespace, op.Path, err)
		}
		if n > 0 {
			return op.Notespace + "/" + op.Path, nil
		}
	}
	return "", nil
}

// ReconcileReturnEscrow atomically advances the laptop identity map to an
// explicitly adopted, hash-verified server generation. It also removes stale
// queued events for each adopted identity/path so the watcher cannot echo the
// pre-adoption operation back to the server. Callers must have cleared
// PendingReturnPush first, so this purge can only discard the apply's own
// echoes and never an unpushed user edit.
func (d *DB) ReconcileReturnEscrow(e ReturnEscrow) error {
	if err := e.Manifest.Validate(); err != nil {
		return err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, op := range e.Manifest.Operations {
		if _, err = tx.Exec(`DELETE FROM sync_outbox WHERE document_id = ? OR (notespace = ? AND (path = ? OR path = ?))`,
			op.DocumentID, op.Notespace, op.Path, op.PreviousPath); err != nil {
			return fmt.Errorf("clear adopted outbox for %s: %w", op.DocumentID, err)
		}
		if op.Type == "delete" {
			if _, err = tx.Exec(`DELETE FROM sync_documents WHERE document_id = ?`, op.DocumentID); err != nil {
				return fmt.Errorf("delete adopted identity %s: %w", op.DocumentID, err)
			}
			continue
		}
		content, ok := e.Content[op.DocumentID]
		if !ok {
			return fmt.Errorf("adopted content missing for %s", op.DocumentID)
		}
		// A watcher event racing the filesystem commit may have minted a fresh
		// local identity for the adopted destination. The server identity wins.
		if _, err = tx.Exec(`DELETE FROM sync_documents WHERE notespace = ? AND path = ? AND document_id != ?`, op.Notespace, op.Path, op.DocumentID); err != nil {
			return fmt.Errorf("clear raced local identity at %s/%s: %w", op.Notespace, op.Path, err)
		}
		_, err = tx.Exec(`INSERT INTO sync_documents
			(document_id, notespace, path, content_hash, last_synced_hash, last_synced_version, base_content, diverged, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
			ON CONFLICT(document_id) DO UPDATE SET notespace=excluded.notespace, path=excluded.path,
			content_hash=excluded.content_hash, last_synced_hash=excluded.last_synced_hash,
			last_synced_version=excluded.last_synced_version, base_content=excluded.base_content,
			diverged=0, updated_at=CURRENT_TIMESTAMP`, op.DocumentID, op.Notespace, op.Path,
			op.HeadHash, op.HeadHash, op.HeadVersion, content)
		if err != nil {
			return fmt.Errorf("adopt identity %s: %w", op.DocumentID, err)
		}
	}
	return tx.Commit()
}

// RemapDocument rewrites a document's identity from a locally-minted id to the
// id the server confirmed for the same path (B8): when a create (or an update
// under a lost id) lands on a path the server already tracks, the server
// answers with the EXISTING document id, and the local row — plus every queued
// outbox entry still carrying the stale id — must adopt it or every subsequent
// push dangles off an id the server never learned. Same-row UPDATE, so the
// UNIQUE(notespace, path) constraint is untouched; the PRIMARY KEY constraint
// still fails if newID is already tracked at another path, which the caller
// surfaces rather than papering over.
func (d *DB) RemapDocument(oldID, newID string) error {
	if oldID == newID {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin document remap: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE sync_documents SET document_id = ?, updated_at = CURRENT_TIMESTAMP WHERE document_id = ?`,
		newID, oldID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("failed to remap sync document %s -> %s: %w", oldID, newID, err)
	}
	if _, err := tx.Exec(
		`UPDATE sync_outbox SET document_id = ? WHERE document_id = ?`,
		newID, oldID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("failed to remap outbox entries %s -> %s: %w", oldID, newID, err)
	}
	return tx.Commit()
}

// RetypeOutboxEntry rewrites a single outbox entry's event type in place. Used
// by the B8 create-conflict adoption: once the local row has adopted the
// server identity, the queued document_created must re-push as a
// document_updated (drain resolves base_version and document_id from the live
// doc row for updates) instead of re-colliding with the occupied path.
func (d *DB) RetypeOutboxEntry(id int64, eventType string) error {
	_, err := d.db.Exec(
		`UPDATE sync_outbox SET event_type = ? WHERE id = ?`, eventType, id)
	if err != nil {
		return fmt.Errorf("failed to retype sync outbox entry %d: %w", id, err)
	}
	return nil
}

// MarkDiverged sets the diverged flag on a document (P5, S5): the push-side
// rebase produced a merged server head that the local file does NOT match, and
// strict push-only forbids rewriting the file. A diverged doc is frozen from the
// push sweep (antientropy.go) and the watcher flush (InsertAndEnqueue) until the
// user runs `nb sync adopt`.
func (d *DB) MarkDiverged(documentID string) error {
	if _, err := d.db.Exec(
		`UPDATE sync_documents SET diverged = 1, updated_at = CURRENT_TIMESTAMP WHERE document_id = ?`,
		documentID); err != nil {
		return fmt.Errorf("failed to mark document %s diverged: %w", documentID, err)
	}
	return nil
}

// ClearDiverged clears the diverged flag on a document. AdoptDocument already
// clears it as part of rolling the merge base; this is the standalone verb for
// callers that only need the flag cleared.
func (d *DB) ClearDiverged(documentID string) error {
	if _, err := d.db.Exec(
		`UPDATE sync_documents SET diverged = 0, updated_at = CURRENT_TIMESTAMP WHERE document_id = ?`,
		documentID); err != nil {
		return fmt.Errorf("failed to clear diverged on document %s: %w", documentID, err)
	}
	return nil
}

// CountDocumentsDiverged returns the number of documents in the diverged state,
// for the /api/sync/status surface (the diverged line / adopt prompt).
func (d *DB) CountDocumentsDiverged() (int, error) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sync_documents WHERE diverged = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count diverged sync documents: %w", err)
	}
	return n, nil
}

// boolToInt maps a Go bool to the 0/1 INTEGER storage the sync schema uses for
// boolean columns (diverged, parked).
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// InsertOutboxEntry inserts an outbox entry (alias for EnqueueOutbox).
func (d *DB) InsertOutboxEntry(e *OutboxEntry) error {
	_, err := d.EnqueueOutbox(e)
	return err
}

// MovePrefix updates all documents under a prefix, renaming the directory.
// For example, moving "plans/" to "archived/plans/" updates all docs under the prefix.
func (d *DB) MovePrefix(notespace, oldPrefix, newPrefix string) error {
	// Normalize prefixes to end with /
	if len(oldPrefix) > 0 && oldPrefix[len(oldPrefix)-1] != '/' {
		oldPrefix += "/"
	}
	if len(newPrefix) > 0 && newPrefix[len(newPrefix)-1] != '/' {
		newPrefix += "/"
	}

	_, err := d.db.Exec(
		`UPDATE sync_documents SET path = ? || SUBSTR(path, ?) WHERE notespace = ? AND path LIKE ?`,
		newPrefix, len(oldPrefix)+1, notespace, oldPrefix+"%")
	if err != nil {
		return fmt.Errorf("failed to move prefix %s to %s: %w", oldPrefix, newPrefix, err)
	}
	return nil
}

// DeletePrefix removes all documents under a path prefix.
func (d *DB) DeletePrefix(notespace, prefix string) error {
	// Normalize prefix to end with /
	if len(prefix) > 0 && prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}

	_, err := d.db.Exec(
		`DELETE FROM sync_documents WHERE notespace = ? AND (path = ? OR path LIKE ?)`,
		notespace, prefix[:len(prefix)-1], prefix+"%")
	if err != nil {
		return fmt.Errorf("failed to delete prefix %s: %w", prefix, err)
	}
	return nil
}
