// Package sync implements the client-side sync handler for the daemon.
// This file contains the pull pipeline for receiving and applying remote changes.
package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
)

// PullPipeline handles the pull phase of sync: receiving events from the server
// and applying them to the local workspace. Pulling is gated by per-workspace
// Pull configuration; workspaces with Pull=false get no pull pipeline.
type PullPipeline struct {
	ws       *config.SyncWorkspace
	client   *Client
	db       *DB
	log      *logging.UnifiedLogger
	pollWait time.Duration
}

// NewPullPipeline creates a pull pipeline for a workspace.
func NewPullPipeline(ws *config.SyncWorkspace, client *Client, db *DB, log *logging.UnifiedLogger) *PullPipeline {
	return &PullPipeline{
		ws:       ws,
		client:   client,
		db:       db,
		log:      log,
		pollWait: 30 * time.Second,
	}
}

// RunPullLoop continuously polls for new events from the server and applies them locally.
// It uses long-polling to avoid busy-waiting. The loop respects workspace configuration
// and stops when the context is cancelled.
func (p *PullPipeline) RunPullLoop(ctx context.Context, workspaceRoot string) error {
	p.log.Debug("pull loop starting").Field("workspace", p.ws.Name).Log(ctx)
	defer p.log.Debug("pull loop stopped").Field("workspace", p.ws.Name).Log(ctx)

	// Get the current cursor for this workspace
	cursor, err := p.db.GetWorkspaceCursor(p.ws.Name)
	if err != nil {
		return fmt.Errorf("failed to get workspace cursor: %w", err)
	}

	// Main pull loop
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Poll for new events from the server with long-polling
		resp, err := p.client.PullEvents(ctx, p.ws.Name, cursor, 100, p.pollWait)
		if err != nil {
			p.log.Error("pull failed").Field("workspace", p.ws.Name).Err(err).Log(ctx)
			// Back off and retry
			select {
			case <-time.After(5 * time.Second):
				continue
			case <-ctx.Done():
				return nil
			}
		}

		// Handle snapshot requirement (cursor too old, need resync)
		if resp.SnapshotRequired {
			p.log.Info("snapshot required, resyncing").Field("workspace", p.ws.Name).Log(ctx)
			newCursor, rerr := p.snaphotResync(ctx, workspaceRoot)
			if rerr != nil {
				p.log.Error("snapshot resync failed").Field("workspace", p.ws.Name).Err(rerr).Log(ctx)
				select {
				case <-time.After(10 * time.Second):
					continue
				case <-ctx.Done():
					return nil
				}
			}
			// Resume tailing from the manifest cursor the resync persisted.
			// Resetting to 0 here would put the very next pull below the GC
			// watermark again — an endless 410→resync loop, never converging.
			cursor = newCursor
			continue
		}

		// Apply each event
		if len(resp.Events) > 0 {
			for _, ev := range resp.Events {
				if err := p.applyEvent(ctx, workspaceRoot, &ev); err != nil {
					p.log.Error("failed to apply event").
						Field("workspace", p.ws.Name).
						Field("path", ev.Path).
						Field("type", ev.Type).
						Err(err).
						Log(ctx)
					// On apply error, we don't stop the loop — store the conflict
					// and continue. The user will be notified via UpdateSyncConflict.
				}
			}
			// Advance cursor
			cursor = resp.Cursor
			if err := p.db.UpdateWorkspaceCursor(p.ws.Name, cursor); err != nil {
				p.log.Error("failed to update cursor").Field("workspace", p.ws.Name).Err(err).Log(ctx)
			}
		}
	}
}

// snaphotResync fetches the manifest snapshot and reconciles it with local state.
// Hash-equal files are adopted in place; divergent files are re-fetched.
// It returns the manifest cursor it persisted, which the pull loop must adopt
// as its resume point.
func (p *PullPipeline) snaphotResync(ctx context.Context, workspaceRoot string) (int64, error) {
	p.log.Debug("fetching snapshot manifest").Field("workspace", p.ws.Name).Log(ctx)

	manifest, err := p.client.Snapshot(ctx, p.ws.Name)
	if err != nil {
		return 0, fmt.Errorf("snapshot fetch failed: %w", err)
	}

	p.log.Debug("snapshot received").Field("workspace", p.ws.Name).Field("documents", len(manifest.Documents)).Log(ctx)

	// Reconcile: for each document in the manifest, check if we have a hash match
	for _, doc := range manifest.Documents {
		localDoc, _ := p.db.GetDocumentByPath(p.ws.Name, doc.Path)

		adopted := false
		if localDoc != nil && localDoc.ContentHash == doc.Hash {
			// Hash match: adopt the server UUID and version in place. The
			// hash-verified on-disk content becomes the merge base — version,
			// last_synced_hash, and base_content roll together (rolling the
			// version alone leaves a stale merge base → phantom conflicts).
			// If disk no longer matches the tracked hash, fall through and
			// re-fetch the server head like any divergent document.
			content, rerr := readFile(p.joinPath(workspaceRoot, doc.Path))
			if rerr == nil && hashContent(content) == doc.Hash {
				p.log.Debug("adopting hash-equal document").Field("path", doc.Path).Log(ctx)
				if err := p.db.AdoptDocument(p.ws.Name, doc.Path, doc.ID, doc.Version, doc.Hash, content); err != nil {
					p.log.Error("failed to adopt document").Field("path", doc.Path).Err(err).Log(ctx)
				}
				adopted = true
			}
		}
		if !adopted {
			// Missing or divergent document: fetch the head content and
			// materialize it NOW. The cursor jump below skips these
			// documents' historical events, so anything not fetched here
			// would never arrive (documents pushed before a client's first
			// sync were silently lost when this branch only logged).
			content, err := p.client.HistoryBlob(ctx, p.ws.Name, doc.ID, doc.Version)
			if err != nil {
				p.log.Error("snapshot content fetch failed").Field("path", doc.Path).Err(err).Log(ctx)
				continue
			}
			ev := &syncproto.SyncEvent{
				Type:        syncproto.EventDocumentCreated,
				DocumentID:  doc.ID,
				Path:        doc.Path,
				Version:     doc.Version,
				ContentHash: doc.Hash,
				Content:     content,
				// The manifest's fidelity mtime, so snapshot hydration (a
				// satellite's first materialization) restores file timestamps
				// the same way event replay does. Zero = unknown (old server).
				Mtime: doc.Mtime,
			}
			if localDoc != nil {
				ev.Type = syncproto.EventDocumentUpdated
			}
			if err := p.applyEvent(ctx, workspaceRoot, ev); err != nil {
				p.log.Error("snapshot apply failed").Field("path", doc.Path).Err(err).Log(ctx)
			}
		}
	}

	// Update the cursor to the manifest's snapshot cursor
	if err := p.db.UpdateWorkspaceCursor(p.ws.Name, manifest.Cursor); err != nil {
		return 0, fmt.Errorf("failed to update cursor after snapshot: %w", err)
	}

	return manifest.Cursor, nil
}

// applyEvent applies a single event: creates, updates, moves, or deletes a document.
// Merge conflicts during update are recorded as conflict artifacts.
func (p *PullPipeline) applyEvent(ctx context.Context, workspaceRoot string, ev *syncproto.SyncEvent) error {
	switch ev.Type {
	case syncproto.EventDocumentCreated:
		return p.applyCreate(ctx, workspaceRoot, ev)
	case syncproto.EventDocumentUpdated:
		return p.applyUpdate(ctx, workspaceRoot, ev)
	case syncproto.EventDocumentMoved:
		return p.applyMove(ctx, workspaceRoot, ev)
	case syncproto.EventDocumentDeleted:
		return p.applyDelete(ctx, workspaceRoot, ev)
	case syncproto.EventPrefixMoved:
		return p.applyPrefixMove(ctx, workspaceRoot, ev)
	case syncproto.EventPrefixDeleted:
		return p.applyPrefixDelete(ctx, workspaceRoot, ev)
	default:
		return fmt.Errorf("unknown event type: %s", ev.Type)
	}
}

// applyCreate writes a new document to the local filesystem.
func (p *PullPipeline) applyCreate(ctx context.Context, workspaceRoot string, ev *syncproto.SyncEvent) error {
	// Fetch content if blob-tier
	content := ev.Content
	if len(content) == 0 && ev.ContentHash != "" {
		var err error
		content, err = p.client.FetchBlob(ctx, ev.ContentHash)
		if err != nil {
			return fmt.Errorf("failed to fetch blob: %w", err)
		}
	}

	// Write to disk, restoring the origin's file mtime when the event carries
	// one (zero = old server/client: keep the write time, as before).
	filePath := p.joinPath(workspaceRoot, ev.Path)
	if err := writeFile(filePath, content, ev.Mtime); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Record in sync DB
	doc := &Document{
		DocumentID:        ev.DocumentID,
		Workspace:         p.ws.Name,
		Path:              ev.Path,
		ContentHash:       ev.ContentHash,
		LastSyncedVersion: ev.Version,
		LastSyncedHash:    ev.ContentHash,
		BaseContent:       content,
	}
	if err := p.db.InsertDocument(doc); err != nil {
		return fmt.Errorf("failed to record document: %w", err)
	}

	p.log.Debug("created document").Field("path", ev.Path).Field("id", ev.DocumentID).Log(ctx)
	return nil
}

// applyUpdate applies a remote content update with 3-way merge conflict detection.
// If a conflict is detected, a conflict artifact is written and UpdateSyncConflict is emitted.
func (p *PullPipeline) applyUpdate(ctx context.Context, workspaceRoot string, ev *syncproto.SyncEvent) error {
	// Fetch content if blob-tier
	content := ev.Content
	if len(content) == 0 && ev.ContentHash != "" {
		var err error
		content, err = p.client.FetchBlob(ctx, ev.ContentHash)
		if err != nil {
			return fmt.Errorf("failed to fetch blob: %w", err)
		}
	}

	doc, err := p.db.GetDocumentByPath(p.ws.Name, ev.Path)
	if err != nil {
		return fmt.Errorf("failed to look up document: %w", err)
	}
	if doc == nil {
		return fmt.Errorf("document not found: %s", ev.Path)
	}

	// Read current local content
	filePath := p.joinPath(workspaceRoot, ev.Path)
	localContent, err := readFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read local file: %w", err)
	}

	// Fast-forward only when the local file matches the last SERVER-confirmed
	// content (no unpushed local edit to preserve), or already equals the
	// incoming remote content. Comparing against doc.ContentHash here is the
	// silent-data-loss bug: the watcher updates ContentHash on every local
	// save, so a dirty-but-tracked file looks "clean" and the unpushed edit
	// gets overwritten by the remote version.
	localHash := hashContent(localContent)
	if localHash == doc.LastSyncedHash || localHash == ev.ContentHash {
		// Fast-forward: disk becomes exactly the remote content, so the
		// origin's mtime is restored with it (zero mtime = keep write time).
		if err := writeFile(filePath, content, ev.Mtime); err != nil {
			return fmt.Errorf("failed to write file: %w", err)
		}
		doc.ContentHash = ev.ContentHash
		doc.LastSyncedVersion = ev.Version
		doc.LastSyncedHash = ev.ContentHash
		doc.BaseContent = content
		return p.db.UpdateDocument(doc)
	}

	// 3-way merge of remote onto the dirty local file: frontmatter merges
	// per-key (LWW-map semantics — frontmatter never conflicts, by design;
	// see mergeValues), the body goes through line-based diff3. Disjoint
	// edits from both sides compose; only overlapping body hunks conflict.
	baseVals := parseFrontmatter(doc.BaseContent)
	localVals := parseFrontmatter(localContent)
	remoteVals := parseFrontmatter(content)

	merged := mergeValues(baseVals, localVals, remoteVals)

	baseBody := extractBody(doc.BaseContent)
	localBody := extractBody(localContent)
	remoteBody := extractBody(content)

	mergedBody, clean := diff3Merge(baseBody, localBody, remoteBody)
	if !clean {
		// Overlapping body hunks: CONFLICT — keep local, record an artifact.
		p.log.Info("merge conflict detected").Field("path", ev.Path).Log(ctx)
		if err := p.recordConflict(ctx, workspaceRoot, ev.Path, ev.DocumentID, localContent); err != nil {
			return fmt.Errorf("failed to record conflict: %w", err)
		}
		// TODO: emit UpdateSyncConflict SSE
		return nil
	}

	// Clean merge: both sides' edits land on disk. The remote head becomes
	// the merge base / last-synced state; ContentHash tracks the merged bytes
	// (disk truth — the local edit inside them is still unpushed).
	// Merged bytes are neither side's file verbatim, so no fidelity mtime
	// applies — the merge is a genuinely-new local modification (write time).
	mergedContent := reconstructDocument(merged, frontmatterKeys(localContent), mergedBody)
	if err := writeFile(filePath, mergedContent, time.Time{}); err != nil {
		return fmt.Errorf("failed to write merged file: %w", err)
	}

	mergedHash := hashContent(mergedContent)
	doc.ContentHash = mergedHash
	doc.LastSyncedVersion = ev.Version
	doc.LastSyncedHash = ev.ContentHash
	doc.BaseContent = content

	// Retarget any parked push of the pre-merge local edit at the merged
	// content: the push pipeline reads disk content at push time, so a stale
	// entry hash would fail the server's hash-integrity check and be dropped
	// — silently losing the local half of the merge. (base_version comes from
	// LastSyncedVersion above, so the re-push carries the new head.)
	if err := p.db.UpdateOutboxContentHashForPath(p.ws.Name, ev.Path, mergedHash); err != nil {
		p.log.Warn("failed to retarget outbox after merge").Field("path", ev.Path).Err(err).Log(ctx)
	}
	return p.db.UpdateDocument(doc)
}

// applyMove renames a document locally and updates the database.
func (p *PullPipeline) applyMove(ctx context.Context, workspaceRoot string, ev *syncproto.SyncEvent) error {
	doc, err := p.db.GetDocumentByPath(p.ws.Name, ev.PrevPath)
	if err != nil {
		return fmt.Errorf("failed to look up document: %w", err)
	}
	if doc == nil {
		return fmt.Errorf("document not found at prev_path: %s", ev.PrevPath)
	}

	oldPath := p.joinPath(workspaceRoot, ev.PrevPath)
	newPath := p.joinPath(workspaceRoot, ev.Path)

	if err := moveFile(oldPath, newPath); err != nil {
		return fmt.Errorf("failed to move file: %w", err)
	}
	// Restore the origin's mtime after the rename (a move event carries the
	// moved file's stat; rename alone preserves only the replica's old mtime).
	// Best-effort, fidelity only.
	if !ev.Mtime.IsZero() {
		_ = os.Chtimes(newPath, ev.Mtime, ev.Mtime)
	}

	doc.Path = ev.Path
	doc.LastSyncedVersion = ev.Version
	return p.db.MoveDocument(doc.DocumentID, ev.Path)
}

// applyDelete removes a document, or marks it for revival if there are local unpushed edits.
func (p *PullPipeline) applyDelete(ctx context.Context, workspaceRoot string, ev *syncproto.SyncEvent) error {
	doc, err := p.db.GetDocumentByPath(p.ws.Name, ev.Path)
	if err != nil {
		return fmt.Errorf("failed to look up document: %w", err)
	}
	if doc == nil {
		// Already gone: idempotent
		return nil
	}

	// Read current local content
	filePath := p.joinPath(workspaceRoot, ev.Path)
	localContent, err := readFile(filePath)
	if err != nil {
		// File already deleted locally
		return p.db.DeleteDocument(doc.DocumentID)
	}

	// Check for local edits: if local hash != base, this is edit-wins-over-delete
	if hashContent(localContent) != doc.LastSyncedHash {
		p.log.Info("edit-wins-over-delete: local edits preserved").
			Field("path", ev.Path).
			Field("id", doc.DocumentID).
			Log(ctx)

		// Synthesize a document_updated event to push the local content back
		hash := hashContent(localContent)
		outboxEv := &OutboxEntry{
			DocumentID:  doc.DocumentID,
			Workspace:   p.ws.Name,
			EventType:   syncproto.EventDocumentUpdated,
			Path:        ev.Path,
			ContentHash: hash,
			Payload:     string(localContent),
			Mtime:       statMtime(filePath),
		}
		return p.db.InsertOutboxEntry(outboxEv)
	}

	// No local edits: safe to delete
	filePath = p.joinPath(workspaceRoot, ev.Path)
	if err := deleteFile(filePath); err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return p.db.DeleteDocument(doc.DocumentID)
}

// applyPrefixMove moves a directory and updates all documents under it.
func (p *PullPipeline) applyPrefixMove(ctx context.Context, workspaceRoot string, ev *syncproto.SyncEvent) error {
	// Rename the directory on disk
	oldPath := p.joinPath(workspaceRoot, ev.PrevPath)
	newPath := p.joinPath(workspaceRoot, ev.Path)

	if err := moveFile(oldPath, newPath); err != nil {
		return fmt.Errorf("failed to move prefix: %w", err)
	}

	// Update all documents under this prefix in the database
	return p.db.MovePrefix(p.ws.Name, ev.PrevPath, ev.Path)
}

// applyPrefixDelete deletes a directory.
func (p *PullPipeline) applyPrefixDelete(ctx context.Context, workspaceRoot string, ev *syncproto.SyncEvent) error {
	path := p.joinPath(workspaceRoot, ev.Path)
	if err := deleteDir(path); err != nil {
		return fmt.Errorf("failed to delete prefix: %w", err)
	}

	// Delete all documents under this prefix in the database
	return p.db.DeletePrefix(p.ws.Name, ev.Path)
}

// recordConflict writes a conflict artifact to disk at ~/.local/state/grove/sync/conflicts/.
// Emits an UpdateSyncConflict SSE event for TUI display.
func (p *PullPipeline) recordConflict(ctx context.Context, workspaceRoot, path, docID string, localContent []byte) error {
	// Create conflicts directory: ~/.local/state/grove/sync/conflicts/{workspace}/
	conflictDir := filepath.Join(paths.StateDir(), "sync", "conflicts", p.ws.Name)
	if err := os.MkdirAll(conflictDir, 0o700); err != nil {
		return fmt.Errorf("failed to create conflict directory: %w", err)
	}

	// Write conflict artifact: {path}.{uuid}.conflict.md
	conflictFile := filepath.Join(conflictDir, fmt.Sprintf("%s.%s.conflict.md", path, docID))
	if err := writeFile(conflictFile, localContent, time.Time{}); err != nil {
		return fmt.Errorf("failed to write conflict artifact: %w", err)
	}

	p.log.Info("conflict recorded").Field("workspace", p.ws.Name).Field("path", path).Field("artifact", conflictFile).Log(ctx)
	// TODO: Emit store.UpdateSyncConflict SSE event
	return nil
}

func (p *PullPipeline) joinPath(root, path string) string {
	// Use filepath.Join for proper path handling across OS
	return filepath.Join(root, filepath.FromSlash(path))
}
