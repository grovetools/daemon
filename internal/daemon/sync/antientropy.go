package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	db              *DB
	client          *Client
	workspace       string
	workspaceRoot   string // Absolute path to the workspace root
	log             *logging.UnifiedLogger
	cfg             AntiEntropyConfig
}

// NewAntiEntropyPass constructs an anti-entropy reconciler.
func NewAntiEntropyPass(db *DB, client *Client, workspace, workspaceRoot string,
	log *logging.UnifiedLogger, cfg AntiEntropyConfig) *AntiEntropyPass {
	if cfg.Interval == 0 {
		cfg.Interval = 1 * time.Hour
	}
	return &AntiEntropyPass{
		db:            db,
		client:        client,
		workspace:     workspace,
		workspaceRoot: workspaceRoot,
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

	// Update the workspace cursor
	if err := a.db.SetCursor(a.workspace, manifest.Cursor); err != nil {
		a.log.Warn("failed to update cursor after anti-entropy").
			Err(err).Log(ctx)
	}

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

	if localHashHex == docSnap.Hash {
		// Hash match: adopt the server's UUID and version, storing content as base
		a.log.Debug("anti-entropy: adopting server state").
			Field("path", docSnap.Path).
			Field("hash", localHashHex).Log(ctx)

		return a.db.UpsertDocument(&Document{
			DocumentID:        docSnap.ID,
			Workspace:         a.workspace,
			Path:              docSnap.Path,
			ContentHash:       localHashHex,
			LastSyncedHash:    localHashHex,
			LastSyncedVersion: docSnap.Version,
			BaseContent:       content, // Store local content as the base for 3-way merge
			UpdatedAt:         time.Now(),
		})
	}

	// Hash mismatch: check if already enqueued (avoid duplicate push)
	existing, err := a.db.GetDocumentByPath(a.workspace, docSnap.Path)
	if err != nil {
		return fmt.Errorf("failed to look up existing document for %s: %w", docSnap.Path, err)
	}

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
	})
	return err
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
