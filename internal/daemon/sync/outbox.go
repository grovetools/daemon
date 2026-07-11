package sync

import (
	"fmt"
	"strings"
	"time"

	"github.com/grovetools/core/pkg/syncproto"
)

// ListOutboxDrainable returns the outbox entries that DrainOutbox may push
// right now, in FIFO (id ASC) order, honoring both parking and the prefix/doc
// barrier rules (S3 + F7).
//
// Parking (S3): a parked entry whose next_retry_at is still in the future is
// skipped so one conflicted document cannot spin or head-of-line-block the rest
// of the queue.
//
// Barriers (F7): the single-FIFO ordering guarantee that the server's
// applyPrefixMove/applyPrefixDelete rely on (they mutate many rows per event)
// must survive parking — an entry may not be drained ahead of an earlier
// still-blocked entry that:
//
//	(a) shares its document_id, or
//	(b) is a prefix op (prefix_moved / prefix_deleted) whose path prefix covers
//	    it, or
//	(c) — the reverse direction — the candidate is itself a prefix op and an
//	    earlier blocked entry falls under the candidate's own prefix.
//
// Blocking is transitive: a candidate blocked by any of the above is itself
// added to the blocked sets, so a chain (parked A → B shares A's doc → C shares
// B's doc) holds all the way down. The prefix-covering predicate cannot be
// expressed cheaply in SQL, so the SELECT stays a plain FIFO scan and the
// filter runs in Go.
//
// limit caps the number of returned (drainable) entries; entries are scanned in
// full id order regardless so earlier barriers are always observed.
func (d *DB) ListOutboxDrainable(workspace string, limit int, now time.Time) ([]*OutboxEntry, error) {
	query := `SELECT ` + outboxColumns + ` FROM sync_outbox`
	var args []interface{}
	if workspace != "" {
		query += ` WHERE workspace = ?`
		args = append(args, workspace)
	}
	query += ` ORDER BY id ASC`

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list drainable sync outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var all []*OutboxEntry
	for rows.Next() {
		e, err := scanOutboxEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan sync outbox entry: %w", err)
		}
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Blocked sets accumulate as the scan walks FIFO order.
	blockedDocIDs := map[string]bool{} // document_id of a blocked entry
	blockedPaths := map[string]bool{}  // path/prev_path of a blocked entry
	var barrierPrefixes []string       // path/prev_path of a blocked prefix op

	block := func(e *OutboxEntry) {
		if e.DocumentID != "" {
			blockedDocIDs[e.DocumentID] = true
		}
		if e.Path != "" {
			blockedPaths[e.Path] = true
		}
		if e.PrevPath != "" {
			blockedPaths[e.PrevPath] = true
		}
		if isPrefixOp(e.EventType) {
			if e.Path != "" {
				barrierPrefixes = append(barrierPrefixes, e.Path)
			}
			if e.PrevPath != "" {
				barrierPrefixes = append(barrierPrefixes, e.PrevPath)
			}
		}
	}

	isBlocked := func(e *OutboxEntry) bool {
		// (a) shares the document_id of an earlier blocked entry.
		if e.DocumentID != "" && blockedDocIDs[e.DocumentID] {
			return true
		}
		// (b) falls under an earlier blocked prefix op.
		if pathUnderAnyPrefix(e.Path, barrierPrefixes) ||
			(e.PrevPath != "" && pathUnderAnyPrefix(e.PrevPath, barrierPrefixes)) {
			return true
		}
		// (c) reverse: the candidate is a prefix op that covers an earlier
		// blocked entry's path — it must not overtake an update inside its
		// subtree.
		if isPrefixOp(e.EventType) {
			for p := range blockedPaths {
				if pathUnderPrefix(p, e.Path) || (e.PrevPath != "" && pathUnderPrefix(p, e.PrevPath)) {
					return true
				}
			}
		}
		return false
	}

	var drainable []*OutboxEntry
	for _, e := range all {
		// Skippable: parked and its retry time has not yet arrived. It blocks
		// later entries per the barrier rules but is never drained now.
		if e.Parked && e.NextRetryAt.After(now) {
			block(e)
			continue
		}
		if isBlocked(e) {
			block(e) // transitive: propagate the block downstream
			continue
		}
		drainable = append(drainable, e)
		if limit > 0 && len(drainable) >= limit {
			break
		}
	}
	return drainable, nil
}

func isPrefixOp(eventType string) bool {
	return eventType == syncproto.EventPrefixMoved || eventType == syncproto.EventPrefixDeleted
}

// pathUnderPrefix reports whether path is at or below prefix. Semantics mirror
// the server's store.MatchesPrefix (sync/pkg/store/sqlite.go) — duplicated
// (not imported) to avoid a client→server package dependency.
func pathUnderPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func pathUnderAnyPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if pathUnderPrefix(path, prefix) {
			return true
		}
	}
	return false
}
