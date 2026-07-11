package sync

import (
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
// Exported because the watcher package imports this sync package.
func InsertAndEnqueue(db *DB, workspace, rel string, content []byte) (quarantineReason string, err error) {
	hash := hashContent(content)

	doc, err := db.GetDocumentByPath(workspace, rel)
	if err != nil {
		return "", err
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
	overridden, err := db.IsQuarantineOverridden(workspace, rel)
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
		Workspace:   workspace,
		Path:        rel,
		ContentHash: hash,
	}); err != nil {
		return "", err
	}

	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID:  documentID,
		Workspace:   workspace,
		EventType:   eventType,
		Path:        rel,
		ContentHash: hash,
	}); err != nil {
		return "", err
	}

	return "", nil
}
