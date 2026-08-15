// Sync activity feed — the durable "what moved, when" journal behind
// GET /api/sync/activity. The outbox records only what is still owed
// (rows are deleted on ack) and the SSE ring dies with the process, so
// without this table a completed transfer leaves no trace a UI can show.
// One row per terminal transfer outcome: an outgoing entry acked, parked
// or rejected by the server, or an incoming event applied (or refused)
// locally. Capped at activityCap rows, pruned inside the insert.

package sync

import (
	"database/sql"
	"fmt"
	"time"
)

// Activity directions. Outgoing rows are written by the push pipeline when
// the server answers; incoming rows by the pull pipeline when a foreign
// event is applied (echo suppression is server-side, so every pulled event
// is genuinely foreign).
const (
	ActivityOutgoing = "outgoing"
	ActivityIncoming = "incoming"
)

// Activity results. The healthy pair is synced/applied; everything else is
// an issue the feed exists to surface.
const (
	ActivityResultSynced   = "synced"   // outgoing change accepted by the server
	ActivityResultApplied  = "applied"  // incoming event applied to the local tree
	ActivityResultConflict = "conflict" // divergence recorded (artifact written / entry parked)
	ActivityResultDiverged = "diverged" // push-side merge left the local file lagging (S5)
	ActivityResultRejected = "rejected" // server refused the push; entry parked
	ActivityResultRequeued = "requeued" // unknown-document self-heal: re-enqueued as a create
	ActivityResultError    = "error"    // incoming apply failed locally
)

// activityCap bounds sync_activity. Pruned on insert (the
// pruneExpiredFocusLocked prune-on-access model, not a background sweep):
// the feed is a recency surface, not an audit log, and a bounded table
// keeps the migration-less client DB from growing without limit.
const activityCap = 1000

// ActivityEntry is one row of sync_activity: a terminal outcome for one
// document transfer, in either direction.
type ActivityEntry struct {
	ID         int64
	Notespace  string
	Direction  string // ActivityOutgoing | ActivityIncoming
	EventType  string // syncproto.Event* vocabulary
	Path       string
	PrevPath   string // for moved events
	DocumentID string
	Result     string // ActivityResult* vocabulary
	Detail     string // free-form: park reason, apply error, conflict kind
	Version    int64  // server version when the outcome carries one
	OccurredAt time.Time
}

// RecordActivity appends one activity row and prunes the table to
// activityCap. Callers treat failure as diagnostic (log-and-continue): the
// feed must never be the reason a transfer outcome is not committed.
func (d *DB) RecordActivity(e *ActivityEntry) error {
	_, err := d.db.Exec(
		`INSERT INTO sync_activity (notespace, direction, event_type, path, prev_path, document_id, result, detail, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Notespace, e.Direction, e.EventType, e.Path, e.PrevPath, e.DocumentID, e.Result, e.Detail, e.Version)
	if err != nil {
		return fmt.Errorf("failed to record sync activity: %w", err)
	}
	// Prune by id distance from the head: ids are AUTOINCREMENT so this is a
	// single indexed range delete, and MAX(id) on an empty table cannot be
	// reached from here (the insert above just succeeded).
	if _, err := d.db.Exec(
		`DELETE FROM sync_activity WHERE id <= (SELECT MAX(id) FROM sync_activity) - ?`,
		activityCap); err != nil {
		return fmt.Errorf("failed to prune sync activity: %w", err)
	}
	return nil
}

// ListActivity returns activity entries newest-first. An empty notespace
// lists all notespaces; limit <= 0 means no limit (the table is capped at
// activityCap anyway).
func (d *DB) ListActivity(notespace string, limit int) ([]*ActivityEntry, error) {
	query := `SELECT id, notespace, direction, event_type, path, prev_path, document_id, result, detail, version, occurred_at
		 FROM sync_activity`
	var args []interface{}
	if notespace != "" {
		query += ` WHERE notespace = ?`
		args = append(args, notespace)
	}
	query += ` ORDER BY id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list sync activity: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*ActivityEntry
	for rows.Next() {
		e, err := scanActivityEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan sync activity entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func scanActivityEntry(rows *sql.Rows) (*ActivityEntry, error) {
	var e ActivityEntry
	if err := rows.Scan(&e.ID, &e.Notespace, &e.Direction, &e.EventType, &e.Path,
		&e.PrevPath, &e.DocumentID, &e.Result, &e.Detail, &e.Version, &e.OccurredAt); err != nil {
		return nil, err
	}
	return &e, nil
}
