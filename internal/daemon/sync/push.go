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
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
)

// PushConfig holds the configuration for the push pipeline.
type PushConfig struct {
	BatchSize     int           // Events per push (default 50)
	RetryBackoff  time.Duration // Initial backoff for retries (default 1s)
	MaxRetries    int           // Max push retries (default 3)
	BlobChunkSize int64         // Fixed blob chunk size; default 4MB
	CheckInterval time.Duration // How often to check for outbox items (default 5s)
}

// PushPipeline manages draining the outbox to the server with batching,
// backoff, and blob handling.
type PushPipeline struct {
	db        *DB
	client    *Client
	cfg       PushConfig
	log       *logging.UnifiedLogger
	workspace string
}

// NewPushPipeline constructs a push pipeline for a single workspace.
func NewPushPipeline(db *DB, client *Client, workspace string, log *logging.UnifiedLogger, cfg PushConfig) *PushPipeline {
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 50
	}
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = 1 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.BlobChunkSize == 0 {
		cfg.BlobChunkSize = 4 * 1024 * 1024 // 4MB
	}
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = 5 * time.Second
	}

	return &PushPipeline{
		db:        db,
		client:    client,
		cfg:       cfg,
		log:       log,
		workspace: workspace,
	}
}

// DrainOutbox drains the outbox for the pipeline's workspace, pushing batches
// to the server with retry logic and backoff. Returns the number of documents
// successfully acknowledged.
//
// Note: The workspaceRoot parameter is required to read file content from disk.
// It should be the absolute path to the workspace root directory.
func (p *PushPipeline) DrainOutbox(ctx context.Context, workspaceRoot string) (int, error) {
	var successCount int

	for {
		// Check context before fetching the next batch
		select {
		case <-ctx.Done():
			return successCount, ctx.Err()
		default:
		}

		// Fetch a batch of outbox entries
		entries, err := p.db.ListOutbox(p.workspace, p.cfg.BatchSize)
		if err != nil {
			return successCount, fmt.Errorf("failed to list outbox: %w", err)
		}

		if len(entries) == 0 {
			// Outbox is empty
			break
		}

		// Convert outbox entries to SyncEvents, reading file content from disk
		events := make([]syncproto.SyncEvent, len(entries))
		for i, entry := range entries {
			event := entry.ToSyncEvent()

			// Read file content for document_created and document_updated events
			if entry.EventType == syncproto.EventDocumentCreated ||
				entry.EventType == syncproto.EventDocumentUpdated {
				localPath := filepath.Join(workspaceRoot, syncproto.LocalizePath(entry.Path))
				content, err := os.ReadFile(localPath)
				if err != nil {
					if os.IsNotExist(err) {
						// File was deleted; convert to delete event
						event.Type = syncproto.EventDocumentDeleted
						event.Content = nil
					} else {
						return successCount, fmt.Errorf("failed to read file %s: %w", localPath, err)
					}
				} else {
					event.Content = content
					event.Size = int64(len(content))
				}
			}

			// OCC guard: the server compares base_version against the
			// document head and conflicts on mismatch. Populate it from the
			// local sync record (the version we last saw from the server) —
			// without it every non-hash-equal update is a manufactured
			// conflict (base_version defaults to 0).
			if event.Type == syncproto.EventDocumentUpdated {
				if doc, derr := p.db.GetDocumentByPath(p.workspace, entry.Path); derr == nil && doc != nil {
					event.BaseVersion = doc.LastSyncedVersion
					if event.DocumentID == "" {
						event.DocumentID = doc.DocumentID
					}
				}
			}

			events[i] = event
		}

		// Handle blob uploads for large files
		if p.client.SupportsBlobs() {
			maxInlineSize := p.client.MaxInlineSize()
			blobErrors := make(map[int]error)
			for i := range events {
				if events[i].Size > maxInlineSize {
					// Large file: upload chunks and clear inline content
					if err := p.uploadFileBlobs(ctx, events[i].Path, events[i].Content); err != nil {
						// Mark blob upload failure; will skip this event from push
						p.log.Warn("failed to upload blob").
							Field("path", events[i].Path).
							Err(err).Log(ctx)
						blobErrors[i] = err
						continue
					}
					events[i].Content = nil // Don't send inline content for large files
				}
			}

			// If any blob uploads failed, return early to retry the entire batch
			// (don't push events with missing blobs)
			if len(blobErrors) > 0 {
				return successCount, fmt.Errorf("blob upload failures: %v", blobErrors)
			}
		}

		// Push the batch with retry logic
		var pushResp *syncproto.PushResponse
		var pushErr error

		backoff := p.cfg.RetryBackoff
		for attempt := 0; attempt <= p.cfg.MaxRetries; attempt++ {
			select {
			case <-ctx.Done():
				return successCount, ctx.Err()
			default:
			}

			pushResp, pushErr = p.client.Push(ctx, p.workspace, events)
			if pushErr == nil {
				break
			}

			if attempt < p.cfg.MaxRetries {
				p.log.Debug("push attempt failed, retrying").
					Field("attempt", attempt+1).
					Field("backoff_ms", backoff.Milliseconds()).
					Err(pushErr).Log(ctx)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return successCount, ctx.Err()
				}
				backoff *= 2 // Exponential backoff
			} else {
				return successCount, fmt.Errorf("push failed after %d retries: %w", p.cfg.MaxRetries, pushErr)
			}
		}

		// Process results
		var idsToDelete []int64
		for i, result := range pushResp.Results {
			if i >= len(entries) {
				break
			}

			switch result.Status {
			case syncproto.PushStatusAccepted:
				idsToDelete = append(idsToDelete, entries[i].ID)
				// Update document metadata if this is a new document
				if result.DocumentID != "" && events[i].DocumentID == "" {
					if err := p.db.UpsertDocument(&Document{
						DocumentID:        result.DocumentID,
						Workspace:         p.workspace,
						Path:              events[i].Path,
						ContentHash:       events[i].ContentHash,
						BaseContent:       events[i].Content,
						LastSyncedHash:    events[i].ContentHash,
						LastSyncedVersion: result.Version,
						UpdatedAt:         time.Now(),
					}); err != nil {
						p.log.Warn("failed to update document after push").
							Field("path", events[i].Path).
							Err(err).Log(ctx)
					}
				} else if result.DocumentID != "" {
					// Existing document: the accepted content is the new
					// server head — record it as the last-synced state and
					// merge base, not just the version. (Version-only updates
					// left last_synced_hash stale, which broke the pull
					// pipeline's local-dirtiness check.)
					if err := p.db.MarkDocumentSynced(result.DocumentID, result.Version,
						events[i].ContentHash, events[i].Content); err != nil {
						p.log.Warn("failed to update document after push").
							Field("path", events[i].Path).
							Err(err).Log(ctx)
					}
				}
				successCount++

			case syncproto.PushStatusConflict:
				p.log.Warn("push conflict: base_version was stale").
					Field("path", events[i].Path).
					Field("base_version", events[i].BaseVersion).
					Field("server_version", result.Version).Log(ctx)
				// Push-side diff3 rebase: 3-way merge base_content / local
				// disk / server head. A clean merge rewrites the file and
				// retargets the entry at the new server head so the next
				// drain tick re-pushes it; overlapping hunks (or a transient
				// fetch failure) leave the entry parked exactly as before —
				// one retry per tick, head-of-line blocking, no-progress
				// guard below.
				if events[i].Type == syncproto.EventDocumentUpdated {
					p.rebaseConflictedEntry(ctx, workspaceRoot, entries[i], &result)
				}

			case syncproto.PushStatusRejected:
				p.log.Warn("push rejected").
					Field("path", events[i].Path).
					Field("error", result.Error).Log(ctx)
				// Remove from outbox to prevent spinning
				idsToDelete = append(idsToDelete, entries[i].ID)
			}
		}

		// Delete acknowledged entries from the outbox
		if len(idsToDelete) > 0 {
			if err := p.db.DeleteOutbox(idsToDelete); err != nil {
				p.log.Warn("failed to delete outbox entries").
					Err(err).Log(ctx)
			}
		} else {
			// No-progress guard: every entry in this batch stayed in the
			// outbox (conflicts awaiting merge). Another pass would refetch
			// the same rows and spin hot for the rest of the process's life
			// — yield until the next CheckInterval tick instead so the pull
			// pipeline gets a chance to advance the merge base. Conflicted
			// entries at the head of the queue block later entries until
			// they resolve (ordering is preserved deliberately).
			break
		}

		// Update workspace cursor after successful push
		if pushResp.Cursor > 0 {
			if err := p.db.SetCursor(p.workspace, pushResp.Cursor); err != nil {
				p.log.Warn("failed to update cursor after push").
					Field("workspace", p.workspace).
					Err(err).Log(ctx)
			}
		}
	}

	return successCount, nil
}

// rebaseConflictedEntry implements the push-side diff3 rebase for a parked
// Conflict entry: fetch the server head (HistoryBlob), 3-way merge it with
// the local disk content over the stored merge base (frontmatter per-key LWW,
// body line-based diff3), and on a clean merge rewrite the file and roll the
// doc's merge base + the outbox hash forward so the next drain re-pushes the
// merged content with the server head as base_version.
//
// Returns true only when a clean rebase landed. Every failure mode leaves the
// entry parked untouched: transient errors (head fetch, file read) get no
// artifact; an overlapping merge writes a conflict artifact once per
// divergence (same format/location as the pull side's).
func (p *PushPipeline) rebaseConflictedEntry(ctx context.Context, workspaceRoot string, entry *OutboxEntry, result *syncproto.PushResult) bool {
	docID := result.DocumentID
	if docID == "" {
		docID = entry.DocumentID
	}
	if docID == "" || result.Version == 0 {
		// Server didn't identify the head; nothing to rebase onto. Transient.
		return false
	}

	doc, err := p.db.GetDocumentByPath(p.workspace, entry.Path)
	if err != nil || doc == nil {
		return false
	}

	// Read the local disk content BEFORE the network fetch: the post-merge
	// re-verify below compares against this snapshot, so any local edit that
	// lands mid-rebase aborts this attempt (next tick retries).
	localPath := filepath.Join(workspaceRoot, syncproto.LocalizePath(entry.Path))
	localContent, err := os.ReadFile(localPath)
	if err != nil {
		return false
	}
	localHash := hashContent(localContent)

	serverContent, err := p.client.HistoryBlob(ctx, p.workspace, docID, result.Version)
	if err != nil {
		// Transient: stay parked, no artifact, retry next tick.
		p.log.Debug("rebase: failed to fetch server head").
			Field("path", entry.Path).
			Field("server_version", result.Version).
			Err(err).Log(ctx)
		return false
	}

	// Frontmatter merges per-key (LWW-map semantics — never conflicts);
	// only overlapping body hunks park the document.
	mergedVals := mergeValues(
		parseFrontmatter(doc.BaseContent),
		parseFrontmatter(localContent),
		parseFrontmatter(serverContent))
	mergedBody, clean := diff3Merge(
		extractBody(doc.BaseContent),
		extractBody(localContent),
		extractBody(serverContent))
	if !clean {
		p.recordConflictArtifact(ctx, entry.Path, docID, localContent)
		return false
	}
	merged := reconstructDocument(mergedVals, frontmatterKeys(localContent), mergedBody)

	// A newer local edit mid-rebase invalidates the merge inputs: abort this
	// attempt without touching anything; the next tick rebases the new state.
	current, err := os.ReadFile(localPath)
	if err != nil || hashContent(current) != localHash {
		p.log.Debug("rebase: local file changed mid-rebase, aborting").
			Field("path", entry.Path).Log(ctx)
		return false
	}

	if err := writeFile(localPath, merged); err != nil {
		p.log.Warn("rebase: failed to write merged content").
			Field("path", entry.Path).Err(err).Log(ctx)
		return false
	}

	// The server head becomes the merge base (and base_version for the
	// re-push — DrainOutbox reads it from LastSyncedVersion); the merged
	// content is the new local state. ContentHash must track the merged
	// bytes so the watcher's hash gate suppresses the echo of our write.
	mergedHash := hashContent(merged)
	doc.ContentHash = mergedHash
	doc.LastSyncedHash = hashContent(serverContent)
	doc.LastSyncedVersion = result.Version
	doc.BaseContent = serverContent
	if err := p.db.UpdateDocument(doc); err != nil {
		p.log.Warn("rebase: failed to update document record").
			Field("path", entry.Path).Err(err).Log(ctx)
		return false
	}
	if err := p.db.UpdateOutboxContentHashForPath(p.workspace, entry.Path, mergedHash); err != nil {
		p.log.Warn("rebase: failed to update outbox entry").
			Field("path", entry.Path).Err(err).Log(ctx)
		return false
	}

	p.log.Info("rebased conflicted push onto server head").
		Field("path", entry.Path).
		Field("base_version", result.Version).Log(ctx)
	return true
}

// recordConflictArtifact writes a conflict artifact for an unmergeable
// push-side divergence — same format and location as the pull pipeline's
// recordConflict — unless one already exists for this document (the entry is
// retried every tick; the artifact is written once per divergence).
func (p *PushPipeline) recordConflictArtifact(ctx context.Context, path, docID string, localContent []byte) {
	conflictDir := filepath.Join(paths.StateDir(), "sync", "conflicts", p.workspace)
	conflictFile := filepath.Join(conflictDir, fmt.Sprintf("%s.%s.conflict.md", path, docID))
	if _, err := os.Stat(conflictFile); err == nil {
		return // already recorded for this divergence
	}
	if err := writeFile(conflictFile, localContent); err != nil {
		p.log.Warn("failed to write conflict artifact").
			Field("path", path).Err(err).Log(ctx)
		return
	}
	p.log.Info("conflict recorded").
		Field("workspace", p.workspace).
		Field("path", path).
		Field("artifact", conflictFile).Log(ctx)
}

// uploadFileBlobs chunks and compresses a large file, uploading each chunk
// to the blob tier and computing the content hash. The chunks are fixed 4MB
// segments, compressed with zstd. After all chunks are uploaded, the content
// hash is computed and stored for the document.
func (p *PushPipeline) uploadFileBlobs(ctx context.Context, path string, content []byte) error {
	// Compute the full-file hash for the blob reference
	hash := sha256.Sum256(content)
	hashHex := hex.EncodeToString(hash[:])

	// Blob contract v1: ONE blob per document version, payload = the RAW
	// content, addressed by sha256(payload) == the event content_hash. The
	// server verifies hash(bytes)==key on upload (422 otherwise) and every
	// party can verify end-to-end. Compression and multi-chunk manifests
	// are protocol vNext — earlier schemes (zstd chunks keyed by compressed
	// hash, then zstd-whole keyed by raw hash) either orphaned blobs or
	// failed server-side integrity verification.
	if err := p.client.PushBlob(ctx, hashHex, content); err != nil {
		return fmt.Errorf("failed to upload blob for %s: %w", path, err)
	}
	p.log.Debug("uploaded blob").
		Field("path", path).
		Field("blob_hash", hashHex).
		Field("size", len(content)).Log(ctx)

	p.log.Debug("uploaded all blob chunks").
		Field("path", path).
		Field("content_hash", hashHex).Log(ctx)
	return nil
}

// RunPushLoop starts a long-running goroutine that periodically drains the
// outbox. It blocks until the context is cancelled. The workspaceRoot parameter
// is required to read file content from disk.
func (p *PushPipeline) RunPushLoop(ctx context.Context, workspaceRoot string) error {
	ticker := time.NewTicker(p.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final drain before shutdown
			if _, err := p.DrainOutbox(ctx, workspaceRoot); err != nil && err != context.Canceled {
				p.log.Error("final push drain failed").Err(err).Log(ctx)
			}
			return ctx.Err()

		case <-ticker.C:
			count, err := p.DrainOutbox(ctx, workspaceRoot)
			if err != nil {
				if err == context.Canceled {
					return err
				}
				p.log.Error("push drain error").Err(err).Log(ctx)
				// Continue polling on error
			} else if count > 0 {
				p.log.Debug("drained outbox entries").
					Field("count", count).
					Field("workspace", p.workspace).Log(ctx)
			}
		}
	}
}
