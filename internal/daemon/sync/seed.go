package sync

import (
	"time"

	"github.com/google/uuid"
	"github.com/grovetools/core/pkg/syncproto"
)

// InsertAndEnqueue is the single seeding unit shared by every outbox producer
// that hands over freshly-read file content: the watcher's flush path
// (daemon/internal/daemon/watcher/sync.go) and the anti-entropy reconcile
// pass's walkLocalTree (antientropy.go). Factoring it here guarantees watch
// coverage and reconcile coverage can never disagree about the doc space or
// the quarantine judgement — both call this exact sequence.
//
// The caller owns the Included/size gate (both already hold the DocSpace and
// have the content in hand); this helper takes content and records it:
//
//  1. hash-equal no-op — unchanged content never re-enters the outbox;
//  2. secret quarantine — unless the path is explicitly allow-listed via
//     IsQuarantineOverridden (/api/sync/allow). Quarantined content is never
//     upserted or enqueued; the reason is returned so the caller decides how
//     to surface it (the watcher broadcasts a sync_conflict, the reconcile
//     logs + counts it);
//  3. UpsertDocument (ON CONFLICT, insert-or-update, so re-scans are
//     idempotent) then EnqueueOutbox (document_created for a genuinely-new
//     row, document_updated otherwise).
//
// mtime is the file's modification time from the caller's stat (both callers
// already hold a FileInfo alongside the content) — fidelity metadata carried
// on the outbox row so replicas can restore filesystem timestamps. A zero
// mtime is fine (stat failed / unknown): it rides the wire as "unknown".
//
// Exported because the watcher package imports this sync package.
func InsertAndEnqueue(db *DB, notespace, rel string, content []byte, mtime time.Time) (quarantineReason string, err error) {
	hash := hashContent(content)

	// Self-write suppression, ahead of every row read: content byte-identical
	// to what the pull apply registered before writing this path is the
	// daemon's OWN write observed back through fsnotify (or the reconcile
	// walk), never a user edit. It must neither enqueue (the echo-outbox
	// wedge: base_version can only be stale, the entry parks forever) nor
	// touch the doc row (the apply owns that bookkeeping; upserting here
	// against a row the apply has not updated yet stamps v-era state over
	// it). The doc-row hash-gate below cannot cover this case — it only
	// works once the apply's row update has committed, and this flush may
	// have raced ahead of it. See selfwrite.go.
	if db.MatchesSelfWrite(notespace, rel, hash) {
		return "", nil
	}

	doc, err := db.GetDocumentByPath(notespace, rel)
	if err != nil {
		return "", err
	}

	// Diverged docs (S5) are frozen from BOTH producers — the watcher flush and
	// walkLocalTree both route through here. A diverged doc's local file lags the
	// merged server head on purpose; enqueueing its bytes would clobber that head
	// (finding-6 livelock). It stays frozen until `nb sync adopt` clears the flag
	// (which then lets the next edit flow normally).
	if doc != nil && doc.Diverged {
		return "", nil
	}

	// Hash-equal no-op: unchanged content never re-enters the outbox (the
	// watcher's former hash-gate, now shared with reconcile).
	if doc != nil && doc.ContentHash == hash {
		return "", nil
	}

	// Secret quarantine gate — skipped when the path is explicitly allow-listed
	// (/api/sync/allow → sync_quarantine_override). Consulting the override
	// here, in the shared helper, is what makes the watcher and the reconcile
	// agree: an allow-listed file syncs from either path, and a non-overridden
	// secret is dropped from both. (Before this helper existed, flush never
	// read the override table, so the allow-list was written-but-dead.)
	overridden, err := db.IsQuarantineOverridden(notespace, rel)
	if err != nil {
		return "", err
	}
	if !overridden {
		if reason, found := ScanForSecrets(content); found {
			return reason, nil
		}
	}

	eventType := syncproto.EventDocumentCreated
	documentID := uuid.New().String()
	if doc != nil {
		eventType = syncproto.EventDocumentUpdated
		documentID = doc.DocumentID
	}

	if err := db.UpsertDocument(&Document{
		DocumentID:  documentID,
		Notespace:   notespace,
		Path:        rel,
		ContentHash: hash,
	}); err != nil {
		return "", err
	}

	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID:  documentID,
		Notespace:   notespace,
		EventType:   eventType,
		Path:        rel,
		ContentHash: hash,
		Mtime:       mtime,
	}); err != nil {
		return "", err
	}

	return "", nil
}
