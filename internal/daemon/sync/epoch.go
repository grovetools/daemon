package sync

import (
	"context"
	gosync "sync"

	"github.com/grovetools/core/logging"
)

// epochMu serializes CheckServerEpoch across the callers that can race it
// (the transport connect and each workspace's anti-entropy pass), so an epoch
// change triggers exactly one full reset instead of one per goroutine.
var epochMu gosync.Mutex

// CheckServerEpoch reconciles the server epoch received in a capabilities
// handshake against the last-seen epoch persisted in sync_meta. It closes the
// push-side gap the pull side covers with 410/snapshot_required: a recreated
// server (fresh, empty DB — the disposable-VM redeploy) mints a new epoch,
// and without this check a push-only client never re-pushes documents its
// sync.db says are already synced — every edit enqueues an UPDATE the empty
// server rejects as "unknown document".
//
// The stored-vs-received matrix:
//
//   - received == ""        → pre-epoch server: nothing to compare, no-op.
//   - stored == received    → same server: no-op.
//   - stored == ""          → first contact (or first post-upgrade handshake):
//     record the epoch, no re-push.
//   - stored != received    → the server was recreated: void every
//     workspace's synced state (ResetForRepush) so the next anti-entropy
//     sweep re-pushes the full document set as creates with stable ids,
//     then record the new epoch.
//
// Returns whether a full re-push reset was performed.
func CheckServerEpoch(ctx context.Context, db *DB, received string, log *logging.UnifiedLogger) (bool, error) {
	if received == "" {
		return false, nil
	}

	epochMu.Lock()
	defer epochMu.Unlock()

	stored, err := db.GetServerEpoch()
	if err != nil {
		return false, err
	}
	if stored == received {
		return false, nil
	}
	if stored == "" {
		return false, db.SetServerEpoch(received)
	}

	if log != nil {
		log.Warn("sync server epoch changed — server was recreated, re-pushing all documents").
			Field("stored_epoch", stored).
			Field("server_epoch", received).Log(ctx)
	}
	docs, workspaces, err := db.ResetForRepushAll()
	if err != nil {
		return false, err
	}
	if log != nil {
		log.Warn("reset local sync state for full re-push").
			Field("documents_reset", docs).
			Field("workspaces", len(workspaces)).Log(ctx)
	}
	return true, db.SetServerEpoch(received)
}
