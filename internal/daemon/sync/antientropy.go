package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

// AntiEntropyConfig holds configuration for the anti-entropy reconciliation pass.
type AntiEntropyConfig struct {
	Interval time.Duration // How often to run (default 1 hour)
}

// AntiEntropyPass reconciles the local filesystem against the server manifest,
// updating sync state for hash-equal files and enqueueing divergent ones for push.
type AntiEntropyPass struct {
	db            *DB
	client        *Client
	workspace     string
	workspaceRoot string    // Absolute path to the workspace root
	space         *DocSpace // exclusion + routing policy; drives walkLocalTree
	log           *logging.UnifiedLogger
	cfg           AntiEntropyConfig
}

// NewAntiEntropyPass constructs an anti-entropy reconciler. space is the same
// per-workspace DocSpace the watcher built (NewDocSpace(sub)), so the tree-walk
// reconcile and the live watcher judge the doc space identically. A nil space
// falls back to compiled defaults.
func NewAntiEntropyPass(db *DB, client *Client, workspace, workspaceRoot string,
	space *DocSpace, log *logging.UnifiedLogger, cfg AntiEntropyConfig,
) *AntiEntropyPass {
	if cfg.Interval == 0 {
		cfg.Interval = 1 * time.Hour
	}
	if space == nil {
		space = NewDocSpace(nil)
	}
	return &AntiEntropyPass{
		db:            db,
		client:        client,
		workspace:     workspace,
		workspaceRoot: workspaceRoot,
		space:         space,
		log:           log,
		cfg:           cfg,
	}
}

// Run performs a single anti-entropy reconciliation pass: fetches the server
// snapshot and diffs against the local filesystem, updating sync state for
// matching files and enqueueing divergent ones.
func (a *AntiEntropyPass) Run(ctx context.Context) error {
	manifest, err := a.client.Snapshot(ctx, a.workspace)
	if err != nil {
		return fmt.Errorf("failed to fetch snapshot for anti-entropy: %w", err)
	}

	a.log.Debug("anti-entropy pass: fetched manifest").
		Field("document_count", len(manifest.Documents)).
		Field("cursor", manifest.Cursor).Log(ctx)

	// Build a set of documents in the manifest for fast lookup
	manifestByPath := make(map[string]*syncproto.DocumentSnapshot)
	for i := range manifest.Documents {
		manifestByPath[manifest.Documents[i].Path] = &manifest.Documents[i]
	}

	// Reconcile each document in the manifest
	for _, docSnap := range manifest.Documents {
		if err := a.reconcileDocument(ctx, &docSnap); err != nil {
			a.log.Warn("failed to reconcile document").
				Field("path", docSnap.Path).
				Err(err).Log(ctx)
			// Continue processing other documents on error
		}
	}

	// Push-side reconcile sweep (defect #14): the manifest loop above only
	// adopts SERVER state — local documents created or edited while the
	// daemon was down produced no watcher events and no outbox entries, so
	// without this sweep they stay invisible to sync forever. It runs AFTER
	// the adopt pass so a stale-hash false-dirty (defect #13) gets repaired
	// by adoption instead of being pointlessly re-pushed.
	if err := a.sweepLocalDocuments(ctx); err != nil {
		return fmt.Errorf("anti-entropy push sweep failed: %w", err)
	}

	// Tree-walk reconcile (Phase 3): disk→db. The two passes above only
	// reconcile documents already known to the server (reconcileDocument) or
	// already tracked in sync.db (sweepLocalDocuments). A file that exists on
	// disk but has no sync.db row and is absent from the manifest — the whole
	// tree on first subscription (hydration), or anything the watcher missed
	// forever after — is invisible to both. walkLocalTree is the disk→db half:
	// it enumerates the tree through the same DocSpace the watcher uses and
	// seeds genuinely-new files via the shared InsertAndEnqueue. Ordered last
	// so already-adopted/tracked rows are settled before the walk's existence
	// check runs (the create-storm guard).
	if err := a.walkLocalTree(ctx); err != nil {
		return fmt.Errorf("anti-entropy tree walk failed: %w", err)
	}

	// NOTE: anti-entropy must NOT touch the pull cursor — it is push-side
	// reconciliation. Writing manifest.Cursor here both masked a dead pull
	// loop (cursor advanced with zero events applied) and skipped any event
	// between the manifest fetch and the write. Cursor ownership belongs to
	// the pull loop and its snapshot resync only.
	return nil
}

// reconcileDocument compares a manifest entry against the local filesystem:
// if hashes match, adopt the server's UUID/version; if divergent, enqueue for push.
func (a *AntiEntropyPass) reconcileDocument(ctx context.Context, docSnap *syncproto.DocumentSnapshot) error {
	localPath := filepath.Join(a.workspaceRoot, syncproto.LocalizePath(docSnap.Path))

	// Check if the local file exists
	content, err := os.ReadFile(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			// File was deleted locally but exists on server: not an error in anti-entropy
			// (the local deletion will be picked up by the watcher when it next runs)
			a.log.Debug("anti-entropy: local file missing").
				Field("path", docSnap.Path).Log(ctx)
			return nil
		}
		return fmt.Errorf("failed to read local file %s: %w", localPath, err)
	}

	// Compute local content hash
	hash := sha256.Sum256(content)
	localHashHex := hex.EncodeToString(hash[:])

	existing, err := a.db.GetDocumentByPath(a.workspace, docSnap.Path)
	if err != nil {
		return fmt.Errorf("failed to look up existing document for %s: %w", docSnap.Path, err)
	}

	if localHashHex == docSnap.Hash {
		// Already fully adopted: quiet no-op (a stale last_synced_hash here
		// is what made each hourly pass re-adopt the same documents forever).
		if existing != nil && existing.DocumentID == docSnap.ID &&
			existing.LastSyncedVersion == docSnap.Version &&
			existing.LastSyncedHash == docSnap.Hash &&
			existing.ContentHash == localHashHex &&
			bytes.Equal(existing.BaseContent, content) {
			return nil
		}

		// Hash match: adopt the server's UUID and version. Version,
		// last_synced_hash, and base_content roll together (defect #13) —
		// the UpsertDocument previously used here only refreshed
		// content_hash on conflict, leaving last_synced_hash/base_content
		// stale: permanent false-dirty, an hourly self-sustaining re-adopt
		// loop, and the stale-merge-base phantom-conflict trap reintroduced
		// via the anti-entropy path.
		a.log.Debug("anti-entropy: adopting server state").
			Field("path", docSnap.Path).
			Field("hash", localHashHex).Log(ctx)

		if existing == nil {
			return a.db.InsertDocument(&Document{
				DocumentID:        docSnap.ID,
				Workspace:         a.workspace,
				Path:              docSnap.Path,
				ContentHash:       localHashHex,
				LastSyncedHash:    localHashHex,
				LastSyncedVersion: docSnap.Version,
				BaseContent:       content, // Local content is the base for 3-way merge
			})
		}
		return a.db.AdoptDocument(a.workspace, docSnap.Path, docSnap.ID, docSnap.Version, localHashHex, content)
	}

	// Hash mismatch: check if already enqueued (avoid duplicate push)
	if existing != nil && existing.ContentHash != docSnap.Hash {
		// Already tracked locally with different content; will be pushed on next sync
		a.log.Debug("anti-entropy: divergent file already tracked").
			Field("path", docSnap.Path).
			Field("local_hash", existing.ContentHash).
			Field("server_hash", docSnap.Hash).Log(ctx)
		return nil
	}

	// New divergent file: enqueue for push
	a.log.Info("anti-entropy: new divergent file, enqueueing for push").
		Field("path", docSnap.Path).
		Field("local_hash", localHashHex).
		Field("server_hash", docSnap.Hash).Log(ctx)

	_, err = a.db.EnqueueOutbox(&OutboxEntry{
		DocumentID:  docSnap.ID,
		Workspace:   a.workspace,
		EventType:   syncproto.EventDocumentUpdated,
		Path:        docSnap.Path,
		ContentHash: localHashHex,
		Mtime:       statMtime(localPath),
	})
	return err
}

// sweepLocalDocuments is the push-side half of reconciliation (defect #14):
// the live watcher is the only producer of outbox events, so a document
// created or edited while the daemon was down sits in sync_documents with a
// disk hash that never matches last_synced_hash and no outbox entry. The
// sweep enqueues such documents — document_created when the document has
// never been synced (empty last_synced_hash / version 0), document_updated
// otherwise — refreshing content_hash from disk. Documents with pending
// outbox entries are never touched: their change is already queued, and
// parked conflicts at the head of the line are intentional (the pull
// pipeline owns the merge).
func (a *AntiEntropyPass) sweepLocalDocuments(ctx context.Context) error {
	docs, err := a.db.ListDocuments(a.workspace)
	if err != nil {
		return fmt.Errorf("failed to list documents for push sweep: %w", err)
	}

	pending, err := a.db.ListOutbox(a.workspace, 0)
	if err != nil {
		return fmt.Errorf("failed to list outbox for push sweep: %w", err)
	}
	pendingIDs := make(map[string]bool, len(pending))
	pendingPaths := make(map[string]bool, len(pending))
	for _, e := range pending {
		if e.DocumentID != "" {
			pendingIDs[e.DocumentID] = true
		}
		pendingPaths[e.Path] = true
		if e.PrevPath != "" {
			pendingPaths[e.PrevPath] = true
		}
	}

	for _, doc := range docs {
		if pendingIDs[doc.DocumentID] || pendingPaths[doc.Path] {
			continue
		}

		// Diverged docs (S5) are frozen from the push sweep: their disk hash
		// deliberately differs from last_synced_hash (the local file lags the
		// merged server head), so re-enqueueing here would clobber the merged
		// head — the finding-6 livelock. They stay frozen until `nb sync adopt`.
		if doc.Diverged {
			continue
		}

		localPath := filepath.Join(a.workspaceRoot, syncproto.LocalizePath(doc.Path))
		content, err := os.ReadFile(localPath) //nolint:gosec // G304: path from tracked notebook tree
		if err != nil {
			if os.IsNotExist(err) {
				a.sweepMissingFile(ctx, doc)
			} else {
				a.log.Warn("push sweep: failed to read local file").
					Field("path", doc.Path).Err(err).Log(ctx)
			}
			continue
		}

		sum := sha256.Sum256(content)
		diskHash := hex.EncodeToString(sum[:])
		if diskHash == doc.LastSyncedHash {
			// Disk matches the server-confirmed state (including false-dirty
			// rows just repaired by the adopt pass): nothing to push.
			continue
		}

		// Secret quarantine: the same gate as the watcher's flush path —
		// nothing reaches the outbox if it matches a secret heuristic.
		if reason, found := ScanForSecrets(content); found {
			a.log.Warn("push sweep: document matches secret heuristic, not queued").
				Field("workspace", a.workspace).
				Field("path", doc.Path).
				Field("heuristic", reason).Log(ctx)
			continue
		}

		eventType := syncproto.EventDocumentUpdated
		if doc.LastSyncedHash == "" || doc.LastSyncedVersion == 0 {
			eventType = syncproto.EventDocumentCreated
		}

		// Refresh content_hash from disk (UpsertDocument's conflict clause
		// preserves the last-synced fields, which advance only on server
		// confirmation).
		if diskHash != doc.ContentHash {
			if err := a.db.UpsertDocument(&Document{
				DocumentID:  doc.DocumentID,
				Workspace:   a.workspace,
				Path:        doc.Path,
				ContentHash: diskHash,
			}); err != nil {
				a.log.Warn("push sweep: failed to refresh content hash").
					Field("path", doc.Path).Err(err).Log(ctx)
				continue
			}
		}

		a.log.Info("push sweep: enqueueing local change the watcher missed").
			Field("path", doc.Path).
			Field("event", eventType).
			Field("hash", diskHash).Log(ctx)

		if _, err := a.db.EnqueueOutbox(&OutboxEntry{
			DocumentID:  doc.DocumentID,
			Workspace:   a.workspace,
			EventType:   eventType,
			Path:        doc.Path,
			ContentHash: diskHash,
			Mtime:       statMtime(localPath),
		}); err != nil {
			a.log.Warn("push sweep: failed to enqueue outbox entry").
				Field("path", doc.Path).Err(err).Log(ctx)
		}
	}
	return nil
}

// sweepMissingFile handles a tracked document whose file is gone from disk:
// a previously-synced document deleted while the daemon was down gets a
// document_deleted event and its row dropped, mirroring the watcher's
// recordDelete. A never-synced row with no file has nothing to replicate.
func (a *AntiEntropyPass) sweepMissingFile(ctx context.Context, doc *Document) {
	if doc.LastSyncedHash == "" && doc.LastSyncedVersion == 0 {
		return
	}
	a.log.Info("push sweep: tracked file missing on disk, enqueueing delete").
		Field("path", doc.Path).Log(ctx)
	if _, err := a.db.EnqueueOutbox(&OutboxEntry{
		DocumentID: doc.DocumentID,
		Workspace:  a.workspace,
		EventType:  syncproto.EventDocumentDeleted,
		Path:       doc.Path,
	}); err != nil {
		a.log.Warn("push sweep: failed to enqueue delete").
			Field("path", doc.Path).Err(err).Log(ctx)
		return
	}
	if err := a.db.DeleteDocument(doc.DocumentID); err != nil {
		a.log.Warn("push sweep: failed to drop deleted document").
			Field("path", doc.Path).Err(err).Log(ctx)
	}
}

// walkLocalTree is the disk→db reconcile pass (Phase 3, finding #1): it walks
// the workspace tree through the shared DocSpace and seeds any included file
// that has no sync.db row via InsertAndEnqueue — the same path the watcher's
// flush uses, so a create/quarantine judgement is identical on both. On an
// empty sync.db this pass IS hydration (every file is new); on a populated one
// it catches whatever the live watcher missed.
//
// A file that already has a row is skipped: content drift on tracked documents
// belongs to sweepLocalDocuments, not here. That skip is the create-storm
// guard — a partial sync.db meeting a full tree must never re-enqueue content
// that already synced. The walk reuses P2's DocSpace.WalkTree (walk.go): the
// exact prune-aware enumeration the recursive watch uses, so coverage cannot
// diverge. Reconcile-seeded entries flow through EnqueueOutbox like any other,
// so P4's per-doc outbox parking applies to them uniformly with no P3 rework.
func (a *AntiEntropyPass) walkLocalTree(ctx context.Context) error {
	start := time.Now()
	var scanned, enqueued, quarantined int64
	setHydrationProgress(HydrationProgress{
		Workspace: a.workspace,
		Running:   true,
		StartedAt: start,
	})

	walkErr := a.space.WalkTree(a.workspaceRoot, nil, func(abs, rel string, de fs.DirEntry) error {
		scanned++
		// Long hydration must stay cancellable without stat-ing ctx every file.
		if scanned%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}

		doc, err := a.db.GetDocumentByPath(a.workspace, rel)
		if err != nil {
			a.log.Warn("hydration: failed to look up document").
				Field("path", rel).Err(err).Log(ctx)
			return nil
		}
		// Create-storm guard: a row already exists → tracked. Drift is
		// sweepLocalDocuments' job; this pass only seeds genuinely-new files.
		if doc != nil {
			return nil
		}

		// Size gate before reading (mirror flush: never read an oversize file).
		info, err := de.Info()
		if err != nil {
			return nil // raced with a delete between readdir and stat
		}
		if a.space.Route(rel, info.Size()) == RouteSkip {
			return nil
		}

		content, err := os.ReadFile(abs) //nolint:gosec // G304: path from tracked notebook tree
		if err != nil {
			if !os.IsNotExist(err) {
				a.log.Warn("hydration: failed to read file").
					Field("path", rel).Err(err).Log(ctx)
			}
			return nil
		}

		reason, err := InsertAndEnqueue(a.db, a.workspace, rel, content, info.ModTime())
		if err != nil {
			a.log.Warn("hydration: failed to enqueue file").
				Field("path", rel).Err(err).Log(ctx)
			return nil
		}
		if reason != "" {
			// Reconcile LOGS + COUNTS quarantine; no Store broadcast — that is
			// the watcher's job (addendum #8). Nothing reaches the outbox.
			a.log.Warn("hydration: secret quarantined, not queued").
				Field("workspace", a.workspace).
				Field("path", rel).
				Field("heuristic", reason).Log(ctx)
			quarantined++
		} else {
			enqueued++
		}

		// Periodic progress line + registry update on a long hydration.
		if scanned%1000 == 0 {
			rate := filesPerSec(scanned, time.Since(start))
			a.log.Info("hydration progress").
				Field("workspace", a.workspace).
				Field("scanned", scanned).
				Field("enqueued", enqueued).
				Field("quarantined", quarantined).
				Field("files_per_sec", rate).Log(ctx)
			setHydrationProgress(HydrationProgress{
				Workspace:   a.workspace,
				Running:     true,
				Scanned:     scanned,
				Enqueued:    enqueued,
				Quarantined: quarantined,
				StartedAt:   start,
				FilesPerSec: rate,
			})
		}
		return nil
	})

	finished := time.Now()
	setHydrationProgress(HydrationProgress{
		Workspace:   a.workspace,
		Running:     false,
		Scanned:     scanned,
		Enqueued:    enqueued,
		Quarantined: quarantined,
		StartedAt:   start,
		FinishedAt:  finished,
		FilesPerSec: filesPerSec(scanned, finished.Sub(start)),
	})

	if walkErr != nil {
		return walkErr
	}

	a.log.Info("hydration pass complete").
		Field("workspace", a.workspace).
		Field("scanned", scanned).
		Field("enqueued", enqueued).
		Field("quarantined", quarantined).
		Field("duration_ms", finished.Sub(start).Milliseconds()).Log(ctx)
	return nil
}

// filesPerSec is the hydration rate, guarding a zero/negative elapsed.
func filesPerSec(n int64, elapsed time.Duration) float64 {
	sec := elapsed.Seconds()
	if sec <= 0 {
		return 0
	}
	return float64(n) / sec
}

// RunAntiEntropyLoop starts a long-running goroutine that periodically runs
// the anti-entropy pass. It blocks until the context is cancelled.
func (a *AntiEntropyPass) RunAntiEntropyLoop(ctx context.Context) error {
	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := a.Run(ctx); err != nil {
				a.log.Error("anti-entropy pass failed").Err(err).Log(ctx)
				// Continue polling on error
			}
		}
	}
}
