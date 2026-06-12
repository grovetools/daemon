package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

// PushConfig holds the configuration for the push pipeline.
type PushConfig struct {
	BatchSize      int           // Events per push (default 50)
	RetryBackoff   time.Duration // Initial backoff for retries (default 1s)
	MaxRetries     int           // Max push retries (default 3)
	BlobChunkSize  int64         // Fixed blob chunk size; default 4MB
	CheckInterval  time.Duration // How often to check for outbox items (default 5s)
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
					// Update version for existing document
					if err := p.db.SetDocumentVersion(result.DocumentID, result.Version); err != nil {
						p.log.Warn("failed to update document version").
							Field("path", events[i].Path).
							Err(err).Log(ctx)
					}
				}
				successCount++

			case syncproto.PushStatusConflict:
				p.log.Info("push conflict: base_version was stale").
					Field("path", events[i].Path).Log(ctx)
				// Leave the entry in the outbox for manual resolution or retry
				// The conflict will be broadcast via UpdateSyncConflict in watcher

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

// uploadFileBlobs chunks and compresses a large file, uploading each chunk
// to the blob tier and computing the content hash. The chunks are fixed 4MB
// segments, compressed with zstd. After all chunks are uploaded, the content
// hash is computed and stored for the document.
func (p *PushPipeline) uploadFileBlobs(ctx context.Context, path string, content []byte) error {
	// Compute the full-file hash for the blob reference
	hash := sha256.Sum256(content)
	hashHex := hex.EncodeToString(hash[:])

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return fmt.Errorf("failed to create zstd encoder: %w", err)
	}
	defer encoder.Close()

	// Upload fixed 4MB chunks
	chunkSize := p.cfg.BlobChunkSize
	for offset := int64(0); offset < int64(len(content)); offset += chunkSize {
		end := offset + chunkSize
		if end > int64(len(content)) {
			end = int64(len(content))
		}

		chunk := content[offset:end]
		compressed := encoder.EncodeAll(chunk, nil)

		// Compute chunk hash for dedup
		chunkHash := sha256.Sum256(compressed)
		chunkHashHex := hex.EncodeToString(chunkHash[:])

		if err := p.client.PushBlob(ctx, chunkHashHex, compressed); err != nil {
			return fmt.Errorf("failed to upload blob chunk for %s at offset %d: %w", path, offset, err)
		}

		p.log.Debug("uploaded blob chunk").
			Field("path", path).
			Field("chunk_hash", chunkHashHex).
			Field("original_size", len(chunk)).
			Field("compressed_size", len(compressed)).Log(ctx)

		encoder.Reset(nil) // Reset for next chunk
	}

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
