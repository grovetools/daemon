package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grovetools/core/logging"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
)

const (
	// oversizeRetryInterval is the fixed park duration for an entry whose
	// content exceeds the server's blob ceiling. Exponential backoff is
	// meaningless for a size condition; a long, flat retry is enough — the
	// cheap disk re-read at push time self-heals a shrunk file or a raised
	// server ceiling.
	oversizeRetryInterval = 1 * time.Hour

	// conflictBackoffCap bounds the exponential backoff applied to a parked
	// conflict entry so attempts never stretch past ~10 minutes.
	conflictBackoffCap = 10 * time.Minute

	// rejectedRetryInterval is the fixed park duration for an entry the server
	// rejected outright (B8). Rejects used to be silently DELETED "to prevent
	// spinning", which is how the occupied-path create vanished forever; now
	// that those cases are structured accepts/conflicts, a reject means a
	// truly malformed push — park it long and flat so operators can see it on
	// /api/sync/outbox instead of losing it.
	rejectedRetryInterval = 1 * time.Hour

	// unknownDocSelfHealMaxAttempts caps how many times an "unknown document"
	// reject self-heals (clear the doc's synced state, drop the entry, let the
	// sweep re-enqueue a create) before falling back to parking. The reject
	// means the server holds NEITHER the id NOR the path — the recreated-
	// server signature — so a create must succeed; the cap is the loop guard
	// for anything pathological.
	unknownDocSelfHealMaxAttempts = 2
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
	notespace string

	// OnOversizeSkipped, when non-nil, is invoked for each outbox entry
	// dropped because its content exceeds the server's advertised blob
	// ceiling. It lets the watcher surface the skip (e.g. broadcast a
	// SyncConflictPayload) without coupling the push pipeline to the store.
	// Phase 4 will generalize this into a parked-entry surfacing hook.
	OnOversizeSkipped func(notespace, path string, size, limit int64)

	// OnDiverged, when non-nil, is invoked when a push-side rebase produces a
	// merged server head that the local file does not match (S5): the merged
	// content is pushed but the notespace file is deliberately left untouched.
	// It lets the watcher surface the diverged disposition (broadcast a
	// SyncConflictPayload{Kind:"diverged"}) so the user knows to `nb sync adopt`.
	// Same decoupling pattern as OnOversizeSkipped.
	OnDiverged func(notespace, path string)
	OnConflict func(kind, notespace, path, documentID, detail string)
}

// NewPushPipeline constructs a push pipeline for a single notespace.
func NewPushPipeline(db *DB, client *Client, notespace string, log *logging.UnifiedLogger, cfg PushConfig) *PushPipeline {
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
		notespace: notespace,
	}
}

// DrainOutbox drains the outbox for the pipeline's notespace, pushing batches
// to the server with retry logic and backoff. Returns the number of documents
// successfully acknowledged.
//
// Note: The notespaceRoot parameter is required to read file content from disk.
// It should be the absolute path to the notespace root directory.
func (p *PushPipeline) DrainOutbox(ctx context.Context, notespaceRoot string) (int, error) {
	var successCount int

	for {
		// Check context before fetching the next batch
		select {
		case <-ctx.Done():
			return successCount, ctx.Err()
		default:
		}

		// Fetch a batch of drainable outbox entries. ListOutboxDrainable skips
		// parked entries whose retry time is still in the future and enforces
		// the doc/prefix barrier rules (F7), so a parked conflict isolates only
		// itself instead of head-of-line-blocking the queue (S3).
		now := time.Now()
		entries, err := p.db.ListOutboxDrainable(p.notespace, p.cfg.BatchSize, now)
		if err != nil {
			return successCount, fmt.Errorf("failed to list outbox: %w", err)
		}

		if len(entries) == 0 {
			// Outbox is empty
			break
		}

		// Convert outbox entries to SyncEvents. The entry may carry its own
		// bytes in Payload (the S5 push-only source: the rebase merges the
		// server head and retargets the entry's Payload so push never re-reads
		// — nor writes — the notespace file); otherwise content comes from disk.
		// A no-op update (push-content hash == the last-synced hash) is dropped
		// client-side (S4) so an adoption-shaped edit dies here instead of
		// round-tripping to the server's inline-size rejection.
		keptEntries := make([]*OutboxEntry, 0, len(entries))
		keptEvents := make([]syncproto.SyncEvent, 0, len(entries))
		var noopIDs []int64
		for _, entry := range entries {
			event := entry.ToSyncEvent()

			// Resolve content for document_created and document_updated events.
			if entry.EventType == syncproto.EventDocumentCreated ||
				entry.EventType == syncproto.EventDocumentUpdated {
				if entry.Payload != "" {
					// Payload-carried bytes exist regardless of disk — no
					// missing-file → delete conversion on this path.
					event.Content = []byte(entry.Payload)
					event.Size = int64(len(entry.Payload))
				} else {
					localPath := filepath.Join(notespaceRoot, syncproto.LocalizePath(entry.Path))
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
						// The pushed bytes are the disk bytes as of NOW, so the
						// wire hash must be recomputed from them: keeping the
						// enqueue-time entry.ContentHash makes the server's
						// validateContent reject the push ("content does not
						// match content_hash") forever whenever the file changed
						// between enqueue and drain — every retry re-reads fresh
						// bytes but re-sends the frozen hash (B9).
						event.ContentHash = hashContent(content)
						// Refresh the fidelity mtime alongside them (the
						// enqueue-time stat is stale for the same reason). Zero
						// on a stat race — the entry's enqueue-time mtime then
						// stands.
						if mtime := statMtime(localPath); !mtime.IsZero() {
							event.Mtime = mtime
						}
					}
				}
			}

			// OCC guard + no-op drop for updates. The server compares
			// base_version against the document head and conflicts on mismatch;
			// populate it from the local sync record (the version we last saw
			// from the server) — without it every non-hash-equal update is a
			// manufactured conflict (base_version defaults to 0).
			if event.Type == syncproto.EventDocumentUpdated ||
				event.Type == syncproto.EventDocumentCreated {
				if doc, derr := p.db.GetDocumentByPath(p.notespace, entry.Path); derr == nil && doc != nil {
					if event.Type == syncproto.EventDocumentUpdated {
						event.BaseVersion = doc.LastSyncedVersion
						if event.DocumentID == "" {
							event.DocumentID = doc.DocumentID
						}
					}
					// No-op drop (S4): compute the hash of the content actually
					// being pushed (not the possibly-stale entry.ContentHash
					// against fresh disk bytes). If it already equals the
					// server-confirmed head, the change is a no-op — delete it
					// rather than round-trip to the server. Applies to creates
					// too: a doc row at this path with the same synced hash
					// means the server already holds this exact document (e.g.
					// after a pull raced the enqueue), so the create would only
					// re-collide with the occupied path.
					if hashContent(event.Content) == doc.LastSyncedHash {
						noopIDs = append(noopIDs, entry.ID)
						continue
					}
				}
			}

			// OCC base for deletes and moves (B7). A deleted entry already
			// carries the base captured at enqueue time (recordDelete stamps
			// doc.LastSyncedVersion before destroying the doc row, so the row
			// is gone by now). Moved entries — and updates converted to
			// deletes above when the file vanished — resolve at drain time
			// from the still-live doc row (MoveDocument keeps the row, already
			// repointed at entry.Path). Without a base the server's
			// applyDelete/applyMove reject every event against a server-known
			// doc as a conflict, permanently.
			if event.Type == syncproto.EventDocumentDeleted ||
				event.Type == syncproto.EventDocumentMoved {
				if event.BaseVersion == 0 {
					if doc, derr := p.db.GetDocumentByPath(p.notespace, entry.Path); derr == nil && doc != nil {
						event.BaseVersion = doc.LastSyncedVersion
						if event.DocumentID == "" {
							event.DocumentID = doc.DocumentID
						}
					}
				}
			}

			keptEntries = append(keptEntries, entry)
			keptEvents = append(keptEvents, event)
		}

		// Drop the no-op updates from the outbox. A batch that was entirely
		// no-ops must loop (fetch fresh work) rather than push zero events —
		// mirror the oversize path's empty-batch continue. No infinite loop:
		// the dropped entries no longer requeue.
		if len(noopIDs) > 0 {
			if err := p.db.DeleteOutbox(noopIDs); err != nil {
				p.log.Warn("failed to delete no-op outbox entries").Err(err).Log(ctx)
			}
		}
		entries = keptEntries
		events := keptEvents
		if len(events) == 0 {
			continue
		}

		// Handle blob uploads for large files, with per-entry oversize
		// disposition. A file larger than the server's advertised blob ceiling
		// is NOT a batch error (that livelocked the whole outbox on one file:
		// oversize → truncated upload → hash mismatch → forever): it is skipped
		// with a warning, surfaced, and dropped so the batch keeps flowing.
		// Transient blob-upload failures still fail the batch for a retry.
		//
		// The entries/events slices stay index-aligned as they are rebuilt —
		// the result loop below maps pushResp.Results[i] ↔ entries[i].
		if p.client.SupportsBlobs() {
			maxInline := p.client.MaxInlineSize()
			maxBlob := p.client.MaxBlobSize()

			keptEntries := make([]*OutboxEntry, 0, len(entries))
			keptEvents := make([]syncproto.SyncEvent, 0, len(events))
			var skippedIDs []int64
			for i := range events {
				switch {
				case events[i].Size <= maxInline:
					// Inline path, unchanged.
				case maxBlob > 0 && events[i].Size > maxBlob:
					// Oversize: exceeds the server blob ceiling. Decide → collect
					// the ID → dispose below; do NOT keep the entry/event.
					skippedIDs = append(skippedIDs, entries[i].ID)
					p.log.Warn("oversize file skipped by sync").
						Field("path", events[i].Path).
						Field("size", events[i].Size).
						Field("limit", maxBlob).Log(ctx)
					if p.OnOversizeSkipped != nil {
						p.OnOversizeSkipped(p.notespace, events[i].Path, events[i].Size, maxBlob)
					}
					continue
				default:
					// Between the inline threshold and the blob ceiling (or an
					// old server that advertised no ceiling): upload the blob and
					// clear inline content. A transient upload failure keeps
					// today's batch-error return so the batch retries.
					if err := p.uploadFileBlobs(ctx, events[i].Path, events[i].Content); err != nil {
						p.log.Warn("failed to upload blob").
							Field("path", events[i].Path).
							Err(err).Log(ctx)
						return successCount, fmt.Errorf("blob upload failed for %s: %w", events[i].Path, err)
					}
					events[i].Content = nil // Don't send inline content for large files
				}
				keptEntries = append(keptEntries, entries[i])
				keptEvents = append(keptEvents, events[i])
			}

			// Phase 4: park oversize entries instead of deleting them, so the
			// surfaced-not-deleted disposition is uniform (a parked entry is
			// still counted as unsynced and stays visible on /api/sync/outbox).
			// A fixed long retry — exponential backoff is meaningless for a size
			// condition; DrainOutbox re-reads disk content at push time, so a
			// shrunk file or a raised server ceiling self-heals on the next
			// retry.
			for _, id := range skippedIDs {
				if err := p.db.ParkOutbox(id, "oversize_skipped", now.Add(oversizeRetryInterval)); err != nil {
					p.log.Warn("failed to park oversize outbox entry").
						Field("id", id).Err(err).Log(ctx)
				}
			}

			entries = keptEntries
			events = keptEvents

			// The whole batch was oversize skips: those entries are deleted, so
			// the next ListOutbox returns fresh work — loop rather than push an
			// empty batch (no infinite loop, since the skips no longer requeue).
			if len(events) == 0 {
				continue
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

			pushResp, pushErr = p.client.Push(ctx, p.notespace, events)
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
		parkedCount := 0
		for i, result := range pushResp.Results {
			if i >= len(entries) {
				break
			}

			switch result.Status {
			case syncproto.PushStatusAccepted:
				idsToDelete = append(idsToDelete, entries[i].ID)
				// Accepted deletes and moves (reachable since B7 — with
				// base_version 0 they always conflicted) must not fall into the
				// content-roll below: a delete's doc row is already gone
				// client-side (recordDelete), and a move pushes no content, so
				// MarkDocumentSynced would warn on the missing row / wipe the
				// merge base with nil bytes. A move only advances the version.
				if events[i].Type == syncproto.EventDocumentDeleted {
					successCount++
					break
				}
				if events[i].Type == syncproto.EventDocumentMoved {
					if result.DocumentID != "" {
						if err := p.db.SetDocumentVersion(result.DocumentID, result.Version); err != nil {
							p.log.Warn("failed to update document after move push").
								Field("path", events[i].Path).
								Err(err).Log(ctx)
						}
					}
					successCount++
					break
				}
				// B8: the server confirmed a DIFFERENT identity than the one
				// pushed — a create (or a lost-id update) absorbed hash-equal
				// into the existing doc at that path. Re-map the local row
				// (and any queued entries) onto the server's id before
				// rolling sync state; without this, MarkDocumentSynced below
				// targets an id the local db doesn't hold and the row stays
				// divorced at version 0.
				if result.DocumentID != "" && events[i].DocumentID != "" &&
					events[i].DocumentID != result.DocumentID {
					if err := p.db.RemapDocument(events[i].DocumentID, result.DocumentID); err != nil {
						p.log.Warn("failed to adopt server document identity").
							Field("path", events[i].Path).
							Field("local_id", events[i].DocumentID).
							Field("server_id", result.DocumentID).
							Err(err).Log(ctx)
					}
				}
				// Update document metadata if this is a new document
				if result.DocumentID != "" && events[i].DocumentID == "" {
					if err := p.db.UpsertDocument(&Document{
						DocumentID:        result.DocumentID,
						Notespace:         p.notespace,
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
					//
					// Diverged doc (S5): the accepted content is the merged
					// payload, not the local file. Roll last_synced_* +
					// base_content to it, but do NOT let MarkDocumentSynced
					// overwrite content_hash (which tracks the disk file, still
					// lagging) or clear the diverged flag — the file stays
					// diverged until `nb sync adopt`.
					if doc, derr := p.db.GetDocumentByPath(p.notespace, events[i].Path); derr == nil && doc != nil && doc.Diverged {
						doc.LastSyncedVersion = result.Version
						doc.LastSyncedHash = events[i].ContentHash
						doc.BaseContent = events[i].Content
						if err := p.db.UpdateDocument(doc); err != nil {
							p.log.Warn("failed to roll diverged document after push").
								Field("path", events[i].Path).Err(err).Log(ctx)
						}
					} else if err := p.db.MarkDocumentSynced(result.DocumentID, result.Version,
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
				// retargets the entry at the new server head; overlapping hunks
				// (or a transient fetch failure) leave the content untouched and
				// write a conflict artifact. Either way (P4) the entry is parked
				// with an exponential backoff below rather than retried every
				// tick — a clean rebase re-pushes the merged content after the
				// backoff; a failed one just backs off instead of spinning.
				// The rebase retargets the entry's Payload at the merged server
				// head and advances the doc's merge base; it NEVER writes the
				// notespace file (S5). When the merge diverges from disk it
				// flags the doc diverged and the entry parks with reason
				// "diverged" (free-form park_reason, no schema change) so the
				// user knows to `nb sync adopt`.
				// Created events run the same machinery (B8): a conflicted
				// create means the path is live on the server under an id the
				// client lost track of. The rebase re-maps the local row onto
				// the server identity, retypes the entry to document_updated,
				// and — with no common ancestor to merge over — either
				// composes cleanly or adopts the server head wholesale with
				// the doc marked diverged (never silent loss).
				reason := "conflict"
				if events[i].Type == syncproto.EventDocumentUpdated ||
					events[i].Type == syncproto.EventDocumentCreated {
					if _, diverged := p.rebaseConflictedEntry(ctx, notespaceRoot, entries[i], &result); diverged {
						reason = "diverged"
						if p.OnDiverged != nil {
							p.OnDiverged(p.notespace, events[i].Path)
						}
					}
				}
				if err := p.db.ParkOutbox(entries[i].ID, reason, now.Add(p.conflictBackoff(entries[i].Attempts))); err != nil {
					p.log.Warn("failed to park conflicted outbox entry").
						Field("path", events[i].Path).Err(err).Log(ctx)
				} else {
					parkedCount++
				}

			case syncproto.PushStatusRejected:
				// Unknown-document self-heal: the server holds neither the
				// pushed id nor the path — the recreated-server signature (a
				// fresh, empty DB rejects every UPDATE from a client whose
				// sync.db says the doc is synced). Parking would strand the
				// change for the flat backoff and then reject again forever;
				// instead void this doc's server-confirmed state and drop the
				// entry, so the next anti-entropy sweep re-enqueues it as a
				// document_created (which the empty server accepts, preserving
				// the stable document id). Attempts-capped as a loop guard —
				// at/over the cap it parks like any other reject.
				if strings.Contains(result.Error, "unknown document") &&
					entries[i].Attempts < unknownDocSelfHealMaxAttempts {
					p.log.Warn("push rejected as unknown document — self-healing for create re-push").
						Field("path", events[i].Path).
						Field("document_id", entries[i].DocumentID).
						Field("attempts", entries[i].Attempts).
						Field("error", result.Error).Log(ctx)
					if err := p.selfHealUnknownDocument(entries[i]); err != nil {
						p.log.Warn("failed to self-heal unknown-document reject; parking").
							Field("path", events[i].Path).Err(err).Log(ctx)
					} else {
						// Deleting the entry is the drain-loop progress signal
						// (same as an ack); the sweep owns the re-enqueue.
						idsToDelete = append(idsToDelete, entries[i].ID)
						break
					}
				}
				p.log.Warn("push rejected").
					Field("path", events[i].Path).
					Field("error", result.Error).Log(ctx)
				// Park with a long flat backoff instead of silently deleting
				// (B8: the silent delete is how an occupied-path create
				// vanished forever — those cases are structured
				// accepts/conflicts now, so a reject means a truly malformed
				// push that deserves operator visibility on
				// /api/sync/outbox). Parking counts as progress for the
				// no-progress guard below, and a parked entry drops out of
				// ListOutboxDrainable until its retry time, so this cannot
				// reintroduce the hot-spin regression.
				if err := p.db.ParkOutbox(entries[i].ID, "rejected", now.Add(rejectedRetryInterval)); err != nil {
					p.log.Warn("failed to park rejected outbox entry").
						Field("path", events[i].Path).Err(err).Log(ctx)
				} else {
					parkedCount++
				}
			}
		}

		// Delete acknowledged entries from the outbox
		if len(idsToDelete) > 0 {
			if err := p.db.DeleteOutbox(idsToDelete); err != nil {
				p.log.Warn("failed to delete outbox entries").
					Err(err).Log(ctx)
			}
		}

		// No-progress guard: break only when this batch neither deleted nor
		// parked anything. Parking IS progress — a parked entry drops out of
		// the next ListOutboxDrainable call (its retry time is in the future),
		// so the loop terminates naturally without refetching the same rows and
		// spinning hot (the ~2,300 log-lines/sec regression). When something
		// parked, loop again to drain the entries the parked one is no longer
		// head-of-line-blocking.
		if len(idsToDelete) == 0 && parkedCount == 0 {
			break
		}

		// Update notespace cursor after successful push
		if pushResp.Cursor > 0 {
			if err := p.db.SetCursor(p.notespace, pushResp.Cursor); err != nil {
				p.log.Warn("failed to update cursor after push").
					Field("notespace", p.notespace).
					Err(err).Log(ctx)
			}
		}
	}

	return successCount, nil
}

// selfHealUnknownDocument voids the server-confirmed state of the document an
// "unknown document" reject targeted (last_synced_hash/'version → empty/0),
// so the next anti-entropy sweep re-enqueues it as a document_created and the
// drain-time no-op guard cannot drop it. The caller deletes the rejected
// outbox entry on success. A missing doc row (e.g. the entry was a move whose
// row was repointed, then deleted) is not an error: with no row there is no
// stale synced state to void, and the tree walk re-seeds the file if it still
// exists.
func (p *PushPipeline) selfHealUnknownDocument(entry *OutboxEntry) error {
	docID := entry.DocumentID
	if docID == "" {
		doc, err := p.db.GetDocumentByPath(p.notespace, entry.Path)
		if err != nil {
			return err
		}
		if doc == nil {
			return nil
		}
		docID = doc.DocumentID
	}
	return p.db.ClearDocumentSyncedState(docID)
}

// conflictBackoff returns the exponential retry delay for a conflict entry that
// has already been parked `attempts` times: RetryBackoff << attempts, capped at
// conflictBackoffCap. The shift is guarded against overflow (a large attempts
// count shifts the duration negative).
func (p *PushPipeline) conflictBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	d := p.cfg.RetryBackoff << attempts
	if d <= 0 || d > conflictBackoffCap {
		return conflictBackoffCap
	}
	return d
}

// rebaseConflictedEntry implements the push-side diff3 rebase for a parked
// Conflict entry, under STRICT PUSH-ONLY (S5): fetch the server head
// (HistoryBlob — a read-only shadow read), 3-way merge it with the local disk
// content over the stored merge base (frontmatter per-key LWW, body line-based
// diff3), and on a clean merge RETARGET THE OUTBOX ENTRY at the merged bytes
// (carried as Payload) and roll the doc's merge base forward — WITHOUT ever
// writing the notespace file. The local file is left as the user last saved it;
// when the merge differs from disk the doc enters the `diverged` state, resolved
// only by an explicit `nb sync adopt`.
//
// Returns (rebased, diverged). rebased is true only when a clean rebase landed;
// diverged is true when the merged head differs from the on-disk bytes (the
// normal case when the merge did anything). Every failure mode returns
// (false, false) and leaves the entry parked untouched: transient errors (head
// fetch, file read) get no artifact; an overlapping merge writes a conflict
// artifact once per divergence (same format/location as the pull side's).
//
// B8 additions: when the server identified the head under a DIFFERENT
// document id than the local row carries (a create — or an update under a
// lost id — collided with the live doc at this path), the local row and its
// queued entries are re-mapped onto the server identity first, and a
// conflicted document_created is retyped to document_updated after the
// retarget. When the doc has no common ancestor at all (never synced: version
// 0, empty base), an overlapping merge cannot resolve by retrying, so the
// server head is adopted as the merged state wholesale — server content
// preserved, local file untouched and captured in the conflict artifact, doc
// marked diverged for `nb sync adopt`.
//
// There is NO mid-rebase re-read guard: nothing is written, so there is nothing
// to guard. A local edit that lands mid-rebase is simply the next local state of
// a now-diverged doc, held (like every diverged edit) until adopt.
func (p *PushPipeline) rebaseConflictedEntry(ctx context.Context, notespaceRoot string, entry *OutboxEntry, result *syncproto.PushResult) (rebased, diverged bool) {
	docID := result.DocumentID
	if docID == "" {
		docID = entry.DocumentID
	}
	if docID == "" || result.Version == 0 {
		// Server didn't identify the head; nothing to rebase onto. Transient.
		return false, false
	}

	doc, err := p.db.GetDocumentByPath(p.notespace, entry.Path)
	if err != nil || doc == nil {
		return false, false
	}

	// Read the local disk content — reading is fine, writing is not (S5). It is
	// the "local" leg of the 3-way merge; the file is never modified.
	localPath := filepath.Join(notespaceRoot, syncproto.LocalizePath(entry.Path))
	localContent, err := os.ReadFile(localPath)
	if err != nil {
		return false, false
	}
	localHash := hashContent(localContent)

	serverContent, err := p.client.HistoryBlob(ctx, p.notespace, docID, result.Version)
	if err != nil {
		// Transient: stay parked, no artifact, retry next tick.
		p.log.Debug("rebase: failed to fetch server head").
			Field("path", entry.Path).
			Field("server_version", result.Version).
			Err(err).Log(ctx)
		return false, false
	}

	if notespacepkg.IsIdentityStamp(entry.Path) && hashContent(serverContent) != localHash {
		detail := "identity stamp differs from the registered local identity; automatic merge is forbidden"
		_, _ = WriteRegistrationConflict(p.notespace, detail)
		if p.OnConflict != nil {
			p.OnConflict(ConflictKindRegistration, p.notespace, entry.Path, docID, detail)
		}
		return false, false
	}

	// B8 identity adoption: the server answered with a different document id
	// than the local row carries — a create (or an update under a lost id)
	// collided with the live doc at this path. Re-map the local row and every
	// queued outbox entry onto the server's id before converging content;
	// pushing under the locally-minted id can never succeed.
	if doc.DocumentID != docID {
		if err := p.db.RemapDocument(doc.DocumentID, docID); err != nil {
			p.log.Warn("rebase: failed to adopt server document identity").
				Field("path", entry.Path).
				Field("local_id", doc.DocumentID).
				Field("server_id", docID).
				Err(err).Log(ctx)
			return false, false
		}
		doc.DocumentID = docID
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
	var merged []byte
	switch {
	case clean:
		merged = reconstructDocument(mergedVals, frontmatterKeys(localContent), mergedBody)
	case doc.LastSyncedVersion == 0 && len(doc.BaseContent) == 0:
		// No common ancestor (B8: a recreate over an orphaned server doc, or
		// an edit under a never-synced row). The overlap can never resolve by
		// retrying — there is no merge base and never will be — so instead of
		// parking an unresolvable conflict forever, adopt the SERVER HEAD as
		// the merged state: the server keeps its content, the local file
		// (untouched, S5) diverges, the local bytes are captured in the
		// conflict artifact, and the user arbitrates via `nb sync adopt`.
		// Nothing is lost on either side.
		p.recordConflictArtifact(ctx, entry.Path, docID, localContent)
		merged = append([]byte(nil), serverContent...)
	default:
		p.recordConflictArtifact(ctx, entry.Path, docID, localContent)
		return false, false
	}
	mergedHash := hashContent(merged)

	// Retarget THIS entry at the merged bytes (carried as Payload): DrainOutbox
	// prefers Payload over the disk read, so the merged content pushes with the
	// advanced LastSyncedVersion as base_version — without the file being
	// touched.
	if err := p.db.UpdateOutboxEntryContent(entry.ID, string(merged), mergedHash); err != nil {
		p.log.Warn("rebase: failed to retarget outbox entry").
			Field("path", entry.Path).Err(err).Log(ctx)
		return false, false
	}

	// A conflicted create has adopted the server identity above, so its
	// re-push must be a document_updated (drain resolves document_id and
	// base_version from the now-remapped doc row) — re-pushing it as a create
	// would just re-collide with the occupied path. When the merged bytes
	// equal the adopted server head, drain's no-op drop retires the entry
	// without a round trip.
	if entry.EventType == syncproto.EventDocumentCreated {
		if err := p.db.RetypeOutboxEntry(entry.ID, syncproto.EventDocumentUpdated); err != nil {
			p.log.Warn("rebase: failed to retype created entry to updated").
				Field("path", entry.Path).Err(err).Log(ctx)
			return false, false
		}
		entry.EventType = syncproto.EventDocumentUpdated
	}

	// Advance the doc record: the server head becomes the merge base (and
	// base_version for the re-push — DrainOutbox reads it from
	// LastSyncedVersion). content_hash tracks the DISK file (localHash), NOT the
	// merged bytes: content_hash means "hash of the local file", and the local
	// file no longer tracks the push.
	doc.ContentHash = localHash
	doc.LastSyncedHash = hashContent(serverContent)
	doc.LastSyncedVersion = result.Version
	doc.BaseContent = serverContent
	if err := p.db.UpdateDocument(doc); err != nil {
		p.log.Warn("rebase: failed to update document record").
			Field("path", entry.Path).Err(err).Log(ctx)
		return false, false
	}

	// Diverged when the merged head differs from disk (true whenever the merge
	// pulled in remote lines). The equality case means disk already equals the
	// merge — no divergence, nothing to adopt.
	diverged = mergedHash != localHash
	if diverged {
		if err := p.db.MarkDiverged(doc.DocumentID); err != nil {
			p.log.Warn("rebase: failed to mark document diverged").
				Field("path", entry.Path).Err(err).Log(ctx)
		}
	}

	p.log.Info("rebased conflicted push onto server head (strict push-only)").
		Field("path", entry.Path).
		Field("base_version", result.Version).
		Field("diverged", diverged).Log(ctx)
	return true, diverged
}

// recordConflictArtifact writes a conflict artifact for an unmergeable
// push-side divergence — same format and location as the pull pipeline's
// recordConflict — unless one already exists for this document (the entry is
// retried every tick; the artifact is written once per divergence).
func (p *PushPipeline) recordConflictArtifact(ctx context.Context, path, docID string, localContent []byte) {
	conflictDir := filepath.Join(paths.StateDir(), "sync", "conflicts", p.notespace)
	conflictFile := filepath.Join(conflictDir, fmt.Sprintf("%s.%s.conflict.md", path, docID))
	if _, err := os.Stat(conflictFile); err == nil {
		return // already recorded for this divergence
	}
	if err := writeFile(conflictFile, localContent, time.Time{}); err != nil {
		p.log.Warn("failed to write conflict artifact").
			Field("path", path).Err(err).Log(ctx)
		return
	}
	p.log.Info("conflict recorded").
		Field("notespace", p.notespace).
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
// outbox. It blocks until the context is cancelled. The notespaceRoot parameter
// is required to read file content from disk.
func (p *PushPipeline) RunPushLoop(ctx context.Context, notespaceRoot string) error {
	ticker := time.NewTicker(p.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final drain before shutdown
			if _, err := p.DrainOutbox(ctx, notespaceRoot); err != nil && err != context.Canceled {
				p.log.Error("final push drain failed").Err(err).Log(ctx)
			}
			return ctx.Err()

		case <-ticker.C:
			count, err := p.DrainOutbox(ctx, notespaceRoot)
			if err != nil {
				if err == context.Canceled {
					return err
				}
				p.log.Error("push drain error").Err(err).Log(ctx)
				// Continue polling on error
			} else if count > 0 {
				p.log.Debug("drained outbox entries").
					Field("count", count).
					Field("notespace", p.notespace).Log(ctx)
			}
		}
	}
}
