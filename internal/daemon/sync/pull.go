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
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/registry"
	"github.com/grovetools/core/pkg/syncproto"
)

// PullPipeline handles the pull phase of sync: receiving events from the server
// and applying them to the local notespace. Pulling is gated by per-notespace
// Pull configuration; notespaces with Pull=false get no pull pipeline.
type PullPipeline struct {
	ws       *config.SyncWorkspace
	client   *Client
	db       *DB
	log      *logging.UnifiedLogger
	pollWait time.Duration

	// OwnMachineID is this host's durable identity (core/pkg/machine). It is
	// the input to the own-note guard below and is only consulted for a
	// role = "registry" subscription; empty disables the guard, which is
	// exactly the pre-identity behavior.
	OwnMachineID string

	// OnRegistryForeignWrite is called when the guard drops an inbound event
	// for this machine's own registry note. The watcher wires it to
	// broadcastConflict; nil is a legal no-op, and the artifact is written
	// either way. Plain-typed (no store import) for the same reason
	// PushPipeline.OnOversizeSkipped is: the pipeline must not depend on the
	// daemon's store package.
	OnRegistryForeignWrite func(notespace, path, detail string)
	// OnConflict surfaces artifact-backed pull/identity conflicts to SSE.
	OnConflict func(kind, notespace, path, documentID, detail string)

	// OnContested is the W3.5 adoption seam's caller: the gate below has found
	// that this batch would write over un-synced local notes. The watcher wires
	// it to SyncHandler.MarkContested, which tears the pull pipeline down until
	// the operator adopts. nil is a legal no-op — the batch is still withheld
	// and the evidence is still written, which is what makes the gate safe in
	// the pipeline's own tests.
	OnContested func(notespaceID string, evidence AdoptionEvidence)

	// missingRootReported is the root whose refusal has already been reported
	// for the current episode (see refuseMissingRoot). Touched only from the
	// pull goroutine.
	missingRootReported string
	// adoptionSettled records that the operator's ADOPTION receipt has been
	// seen for this root, which is the only thing that retires the gate for the
	// life of the pipeline. A clean batch settles nothing — no batch is the
	// tree. Touched only from the pull goroutine. See guardAdoption.
	adoptionSettled bool
	// adoptionReported keeps a withheld batch from re-announcing itself. The
	// pull loop retries on a timer and teardown is not instantaneous, so
	// without it one contest would write a fresh artifact and a fresh SSE
	// update every retry — the same once-per-episode discipline
	// missingRootReported applies to a missing root.
	adoptionReported bool
}

// NewPullPipeline creates a pull pipeline for a notespace.
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
// It uses long-polling to avoid busy-waiting. The loop respects notespace configuration
// and stops when the context is cancelled.
func (p *PullPipeline) RunPullLoop(ctx context.Context, notespaceRoot string) error {
	p.log.Debug("pull loop starting").Field("notespace", p.ws.Name).Log(ctx)
	defer p.log.Debug("pull loop stopped").Field("notespace", p.ws.Name).Log(ctx)

	// Get the current cursor for this notespace
	cursor, err := p.db.GetNotespaceCursor(p.ws.Name)
	if err != nil {
		return fmt.Errorf("failed to get notespace cursor: %w", err)
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
			p.log.Error("pull failed").Field("notespace", p.ws.Name).Err(err).Log(ctx)
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
			p.log.Info("snapshot required, resyncing").Field("notespace", p.ws.Name).Log(ctx)
			newCursor, rerr := p.snaphotResync(ctx, notespaceRoot)
			if rerr != nil {
				p.log.Error("snapshot resync failed").Field("notespace", p.ws.Name).Err(rerr).Log(ctx)
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
			// Root-must-exist, checked ONCE per apply batch (W3.2). The cursor
			// deliberately does not advance on refusal: the events are still
			// owed to this machine and must replay unchanged once the operator
			// materializes the recorded root.
			if err := p.refuseMissingRoot(ctx, notespaceRoot); err != nil {
				select {
				case <-time.After(30 * time.Second):
					continue
				case <-ctx.Done():
					return nil
				}
			}
			// The adoption gate (W3.5), checked with the same per-batch timing
			// and the same cursor discipline as the root precondition: a
			// contested notespace takes NO writes, and the events stay owed to
			// this machine until the operator adopts.
			if err := p.guardAdoption(ctx, notespaceRoot, IncomingFromEvents(resp.Events)); err != nil {
				select {
				case <-time.After(30 * time.Second):
					continue
				case <-ctx.Done():
					return nil
				}
			}
			for _, ev := range resp.Events {
				if err := p.applyEvent(ctx, notespaceRoot, &ev); err != nil {
					p.log.Error("failed to apply event").
						Field("notespace", p.ws.Name).
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
			if err := p.db.UpdateNotespaceCursor(p.ws.Name, cursor); err != nil {
				p.log.Error("failed to update cursor").Field("notespace", p.ws.Name).Err(err).Log(ctx)
			}
		}
	}
}

// snaphotResync fetches the manifest snapshot and reconciles it with local state.
// Hash-equal files are adopted in place; divergent files are re-fetched.
// It returns the manifest cursor it persisted, which the pull loop must adopt
// as its resume point.
func (p *PullPipeline) snaphotResync(ctx context.Context, notespaceRoot string) (int64, error) {
	// Hydration is the largest incoming apply there is; refuse it whole rather
	// than materializing a notespace root one document at a time.
	if err := p.refuseMissingRoot(ctx, notespaceRoot); err != nil {
		return 0, err
	}
	p.log.Debug("fetching snapshot manifest").Field("notespace", p.ws.Name).Log(ctx)

	manifest, err := p.client.Snapshot(ctx, p.ws.Name)
	if err != nil {
		return 0, fmt.Errorf("snapshot fetch failed: %w", err)
	}

	p.log.Debug("snapshot received").Field("notespace", p.ws.Name).Field("documents", len(manifest.Documents)).Log(ctx)

	// Hydration onto a tree that already holds notes is precisely the W3.5
	// case, and the manifest is the richest input the gate ever gets: every
	// document the server holds, with its hash, before a single byte is
	// written. Refuse the whole hydration rather than adopt it document by
	// document — the cursor is not advanced, so it replays after adoption.
	if err := p.guardAdoption(ctx, notespaceRoot, IncomingFromManifest(manifest.Documents)); err != nil {
		return 0, err
	}

	// Reconcile: for each document in the manifest, check if we have a hash match
	for _, doc := range manifest.Documents {
		// A manifest path is server-supplied and this loop is where it first
		// becomes a TRACKED ROW. AdoptDocument accepts whatever path it is
		// handed, and a row whose path escapes the root is what later turns an
		// ordinary document_deleted into a delete outside the tree. Reject it
		// at the insert rather than relying on the delete guard alone.
		if err := requireUnderRoot(notespaceRoot, p.joinPath(notespaceRoot, doc.Path)); err != nil {
			p.log.Error("snapshot document path escapes the notespace root; skipped").
				Field("notespace", p.ws.Name).
				Field("path", doc.Path).
				Err(err).Log(ctx)
			continue
		}
		localDoc, _ := p.db.GetDocumentByPath(p.ws.Name, doc.Path)

		adopted := false
		if localDoc != nil && localDoc.ContentHash == doc.Hash {
			// Hash match: adopt the server UUID and version in place. The
			// hash-verified on-disk content becomes the merge base — version,
			// last_synced_hash, and base_content roll together (rolling the
			// version alone leaves a stale merge base → phantom conflicts).
			// If disk no longer matches the tracked hash, fall through and
			// re-fetch the server head like any divergent document.
			content, rerr := readFile(p.joinPath(notespaceRoot, doc.Path))
			if rerr == nil && hashContent(content) == doc.Hash {
				p.log.Debug("adopting hash-equal document").Field("path", doc.Path).Log(ctx)
				if err := p.db.AdoptDocument(p.ws.Name, doc.Path, doc.ID, doc.Version, doc.Hash, content); err != nil {
					p.log.Error("failed to adopt document").Field("path", doc.Path).Err(err).Log(ctx)
				}
				adopted = true
			}
		}
		if !adopted {
			// A recreated server assigns new document ids to re-pushed paths.
			// Rebind the existing path row before applying the snapshot head so
			// UNIQUE(notespace,path) cannot strand the pull loop permanently.
			if localDoc != nil && localDoc.DocumentID != doc.ID {
				if err := p.db.RemapDocument(localDoc.DocumentID, doc.ID); err != nil {
					p.log.Error("snapshot identity rebind failed").Field("path", doc.Path).Err(err).Log(ctx)
					continue
				}
				localDoc.DocumentID = doc.ID
			}
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
			if err := p.applyEvent(ctx, notespaceRoot, ev); err != nil {
				p.log.Error("snapshot apply failed").Field("path", doc.Path).Err(err).Log(ctx)
			}
		}
	}

	// Update the cursor to the manifest's snapshot cursor
	if err := p.db.UpdateNotespaceCursor(p.ws.Name, manifest.Cursor); err != nil {
		return 0, fmt.Errorf("failed to update cursor after snapshot: %w", err)
	}

	return manifest.Cursor, nil
}

// refuseMissingRoot is the per-batch root precondition (W3.2). It reports the
// refusal exactly once per episode — to the log, to the conflicts feed as
// durable evidence, and to OnConflict for the live SSE surface — because the
// pull loop retries on a timer and an unmaterialized root is a condition that
// persists for as long as the operator takes to fix it. A later successful
// check re-arms the report, so a recurrence is not swallowed.
func (p *PullPipeline) refuseMissingRoot(ctx context.Context, notespaceRoot string) error {
	err := RequireNotespaceRoot(notespaceRoot)
	if err == nil {
		p.missingRootReported = ""
		return nil
	}
	if p.missingRootReported == notespaceRoot {
		return err
	}
	p.missingRootReported = notespaceRoot

	p.log.Error("incoming apply refused: notespace root is missing").
		Field("notespace", p.ws.Name).
		Field("root", notespaceRoot).
		Err(err).Log(ctx)
	if _, werr := WriteNotespaceConflict(p.ws.Name, ConflictKindMissingRoot, err.Error()); werr != nil {
		p.log.Warn("failed to record missing-root evidence").
			Field("notespace", p.ws.Name).Err(werr).Log(ctx)
	}
	if p.OnConflict != nil {
		p.OnConflict(ConflictKindMissingRoot, p.ws.Name, ".", "", err.Error())
	}
	return err
}

// guardAdoption is the W3.5 pre-apply gate: it answers "would this batch write
// over notes this machine has never synced?" before any handler touches disk.
//
// EVERY batch is evaluated. A clean verdict settles nothing, because no batch
// is the tree: `PullEvents` returns a bounded window, so a shared notebook with
// more events than fit in one is split across batches, and a machine whose
// un-synced notes collide with a path in batch 2 would have had that batch
// applied unchecked by a flag set from batch 1. The same hole arrives more
// slowly on a hydrated notespace — a server document landing on a note the
// operator wrote locally in the meantime is the identical case, just later.
// Only the operator's ADOPTION settles the question, which is what the receipt
// below is.
//
// The cost of that is one stat per incoming path in the batch, plus a sync.db
// lookup and a read only where a local file actually sits on an incoming path
// (DetectAdoption's ordering). The one expensive leg — the server's inventory,
// for the subject evidence — is fetched only once a collision has already been
// found, because it is evidence for the operator's decision and never part of
// the verdict.
func (p *PullPipeline) guardAdoption(ctx context.Context, notespaceRoot string, incoming []IncomingDocument) error {
	if p.adoptionSettled || len(incoming) == 0 {
		return nil
	}
	// An adopted notespace is an ordinary synced notespace. Without this the
	// same untracked collision would be re-detected after every daemon restart
	// and the notespace would re-contest itself forever. The receipt is checked
	// against THIS root: one id can have two physical roots (D8) and an id
	// survives a move (W3.4), so an adoption made for another tree is not an
	// adoption for this one.
	if AdoptionRecorded(p.ws.Name, notespaceRoot) {
		p.adoptionSettled = true
		return nil
	}

	tracked := func(path string) (bool, error) {
		doc, err := p.db.GetDocumentByPath(p.ws.Name, path)
		if err != nil {
			return false, err
		}
		return doc != nil, nil
	}
	evidence, err := DetectAdoption(p.ws.Name, notespaceRoot, incoming, tracked, p.localSubject(notespaceRoot), "")
	if err != nil {
		// A sync.db that cannot be read leaves the gate with no verdict at
		// all. Withhold and retry on the existing timer: the batch is still
		// owed to this machine, and the one thing that must not happen is
		// letting it through because a lookup failed. Nothing is contested —
		// this is a daemon fault, not the operator's decision to make.
		p.log.Error("incoming apply withheld: the adoption gate could not read local sync state").
			Field("notespace", p.ws.Name).
			Field("root", notespaceRoot).
			Err(err).Log(ctx)
		return err
	}
	if !evidence.Contested() {
		return nil
	}
	// Contested: now the subject leg of the evidence is worth a round trip.
	evidence.ServerSubject = p.serverSubject(ctx)

	withheld := fmt.Errorf("notespace %s is contested: %d incoming path(s) would overwrite un-synced local notes; adopt it before it takes writes",
		p.ws.Name, evidence.Divergent)
	if p.adoptionReported {
		return withheld
	}
	p.adoptionReported = true

	detail := evidence.Detail()
	p.log.Error("incoming apply withheld: notespace is contested and not adopted yet").
		Field("notespace", p.ws.Name).
		Field("root", notespaceRoot).
		Field("colliding", len(evidence.Collisions)).
		Field("divergent", evidence.Divergent).
		Field("identical", evidence.Identical).
		Field("subject_match", evidence.SubjectMatch()).
		Log(ctx)
	if _, err := WriteNotespaceConflict(p.ws.Name, ConflictKindAdoption, detail); err != nil {
		p.log.Warn("failed to record adoption evidence").
			Field("notespace", p.ws.Name).Err(err).Log(ctx)
	}
	if p.OnConflict != nil {
		p.OnConflict(ConflictKindAdoption, p.ws.Name, ".", "", detail)
	}
	if p.OnContested != nil {
		p.OnContested(p.ws.Name, evidence)
	}
	return withheld
}

// localSubject is this root's stamp subject, or "" when the root carries no
// readable stamp. Absence is reported as unknown rather than as a mismatch:
// "no stamp" and "a different subject" are different facts and the operator's
// decision turns on which one it is.
func (p *PullPipeline) localSubject(notespaceRoot string) string {
	stamp, err := notespacepkg.LoadNotespace(notespaceRoot)
	if err != nil || stamp == nil {
		return ""
	}
	return stamp.Subject
}

// serverSubject is what the server records for this notespace. It is best
// effort by design: an inventory the daemon cannot fetch makes the subject leg
// of the evidence unknown, and unknown must never block the gate's real
// question, which is answered entirely from local state and the batch.
func (p *PullPipeline) serverSubject(ctx context.Context) string {
	if p.client == nil {
		return ""
	}
	inventory, err := p.client.Inventory(ctx)
	if err != nil || inventory == nil {
		return ""
	}
	for _, ns := range inventory.Notespaces {
		if ns.ID.String() == p.ws.Name {
			return ns.Subject
		}
	}
	return ""
}

// applyEvent applies a single event: creates, updates, moves, or deletes a document.
// Merge conflicts during update are recorded as conflict artifacts.
func (p *PullPipeline) applyEvent(ctx context.Context, notespaceRoot string, ev *syncproto.SyncEvent) error {
	// Defence in depth behind the per-batch check: applyEvent is also reached
	// from the snapshot path and from callers that hold a root resolved on an
	// earlier tick, and a root can vanish mid-batch.
	if err := RequireNotespaceRoot(notespaceRoot); err != nil {
		return err
	}
	// Containment for every event shape in one place. The per-call-site
	// *UnderRoot helpers below are the enforcement, but they were only ever
	// wired into the writes and moves; a single gate here means a new event
	// type cannot be added without one. Both fields are checked because a move
	// names two paths and either may escape.
	if err := p.requireEventUnderRoot(notespaceRoot, ev); err != nil {
		return err
	}
	if p.guardOwnRegistryNote(ctx, ev) {
		return nil
	}
	switch ev.Type {
	case syncproto.EventDocumentCreated:
		return p.applyCreate(ctx, notespaceRoot, ev)
	case syncproto.EventDocumentUpdated:
		return p.applyUpdate(ctx, notespaceRoot, ev)
	case syncproto.EventDocumentMoved:
		return p.applyMove(ctx, notespaceRoot, ev)
	case syncproto.EventDocumentDeleted:
		return p.applyDelete(ctx, notespaceRoot, ev)
	case syncproto.EventPrefixMoved:
		return p.applyPrefixMove(ctx, notespaceRoot, ev)
	case syncproto.EventPrefixDeleted:
		return p.applyPrefixDelete(ctx, notespaceRoot, ev)
	default:
		return fmt.Errorf("unknown event type: %s", ev.Type)
	}
}

// requireEventUnderRoot rejects an event whose Path or PrevPath resolves
// outside the notespace root, before any handler acts on it.
func (p *PullPipeline) requireEventUnderRoot(notespaceRoot string, ev *syncproto.SyncEvent) error {
	for _, path := range []string{ev.Path, ev.PrevPath} {
		if path == "" {
			continue
		}
		if err := requireUnderRoot(notespaceRoot, p.joinPath(notespaceRoot, path)); err != nil {
			return fmt.Errorf("refusing %s event: %w", ev.Type, err)
		}
	}
	return nil
}

// applyCreate writes a new document to the local filesystem.
func (p *PullPipeline) applyCreate(ctx context.Context, notespaceRoot string, ev *syncproto.SyncEvent) error {
	// Fetch content if blob-tier. A legitimately empty document (B10) also
	// arrives with no content — materialize its zero bytes directly instead
	// of chasing a blob that never existed.
	content := ev.Content
	if len(content) == 0 && ev.ContentHash != "" && ev.ContentHash != emptyContentHash {
		var err error
		content, err = p.client.FetchBlob(ctx, ev.ContentHash)
		if err != nil {
			return fmt.Errorf("failed to fetch blob: %w", err)
		}
	}

	// Write to disk, restoring the origin's file mtime when the event carries
	// one (zero = old server/client: keep the write time, as before).
	filePath := p.joinPath(notespaceRoot, ev.Path)
	if err := writeFileUnderRoot(notespaceRoot, filePath, content, ev.Mtime); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Record in sync DB
	doc := &Document{
		DocumentID:        ev.DocumentID,
		Notespace:         p.ws.Name,
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
func (p *PullPipeline) applyUpdate(ctx context.Context, notespaceRoot string, ev *syncproto.SyncEvent) error {
	// Fetch content if blob-tier (same empty-document carve-out as applyCreate).
	content := ev.Content
	if len(content) == 0 && ev.ContentHash != "" && ev.ContentHash != emptyContentHash {
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
	filePath := p.joinPath(notespaceRoot, ev.Path)
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
	if notespacepkg.IsIdentityStamp(ev.Path) && localHash != ev.ContentHash {
		detail := "identity stamp differs from the registered local identity; automatic merge is forbidden"
		if err := p.recordConflictArtifact(ctx, ev.Path, ev.DocumentID, ConflictKindRegistration, localContent); err != nil {
			return fmt.Errorf("failed to record identity conflict: %w", err)
		}
		if p.OnConflict != nil {
			p.OnConflict(ConflictKindRegistration, p.ws.Name, ev.Path, ev.DocumentID, detail)
		}
		return nil
	}
	if localHash == doc.LastSyncedHash || localHash == ev.ContentHash {
		// Fast-forward: disk becomes exactly the remote content, so the
		// origin's mtime is restored with it (zero mtime = keep write time).
		if err := writeFileUnderRoot(notespaceRoot, filePath, content, ev.Mtime); err != nil {
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
		if err := p.recordConflict(ctx, notespaceRoot, ev.Path, ev.DocumentID, localContent); err != nil {
			return fmt.Errorf("failed to record conflict: %w", err)
		}
		if p.OnConflict != nil {
			p.OnConflict(ConflictKindMerge, p.ws.Name, ev.Path, ev.DocumentID, "overlapping local and remote edits")
		}
		return nil
	}

	// Clean merge: both sides' edits land on disk. The remote head becomes
	// the merge base / last-synced state; ContentHash tracks the merged bytes
	// (disk truth — the local edit inside them is still unpushed).
	// Merged bytes are neither side's file verbatim, so no fidelity mtime
	// applies — the merge is a genuinely-new local modification (write time).
	mergedContent := reconstructDocument(merged, frontmatterKeys(localContent), mergedBody)
	if err := writeFileUnderRoot(notespaceRoot, filePath, mergedContent, time.Time{}); err != nil {
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
func (p *PullPipeline) applyMove(ctx context.Context, notespaceRoot string, ev *syncproto.SyncEvent) error {
	doc, err := p.db.GetDocumentByPath(p.ws.Name, ev.PrevPath)
	if err != nil {
		return fmt.Errorf("failed to look up document: %w", err)
	}
	if doc == nil {
		return fmt.Errorf("document not found at prev_path: %s", ev.PrevPath)
	}

	oldPath := p.joinPath(notespaceRoot, ev.PrevPath)
	newPath := p.joinPath(notespaceRoot, ev.Path)

	if err := moveFileUnderRoot(notespaceRoot, oldPath, newPath); err != nil {
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
func (p *PullPipeline) applyDelete(ctx context.Context, notespaceRoot string, ev *syncproto.SyncEvent) error {
	doc, err := p.db.GetDocumentByPath(p.ws.Name, ev.Path)
	if err != nil {
		return fmt.Errorf("failed to look up document: %w", err)
	}
	if doc == nil {
		// Already gone: idempotent
		return nil
	}

	// Read current local content
	filePath := p.joinPath(notespaceRoot, ev.Path)
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
			Notespace:   p.ws.Name,
			EventType:   syncproto.EventDocumentUpdated,
			Path:        ev.Path,
			ContentHash: hash,
			Payload:     string(localContent),
			Mtime:       statMtime(filePath),
		}
		return p.db.InsertOutboxEntry(outboxEv)
	}

	// No local edits: safe to delete
	filePath = p.joinPath(notespaceRoot, ev.Path)
	if err := deleteFileUnderRoot(notespaceRoot, filePath); err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return p.db.DeleteDocument(doc.DocumentID)
}

// applyPrefixMove moves a directory and updates all documents under it.
func (p *PullPipeline) applyPrefixMove(ctx context.Context, notespaceRoot string, ev *syncproto.SyncEvent) error {
	// Rename the directory on disk
	oldPath := p.joinPath(notespaceRoot, ev.PrevPath)
	newPath := p.joinPath(notespaceRoot, ev.Path)

	if err := moveFileUnderRoot(notespaceRoot, oldPath, newPath); err != nil {
		return fmt.Errorf("failed to move prefix: %w", err)
	}

	// Update all documents under this prefix in the database
	return p.db.MovePrefix(p.ws.Name, ev.PrevPath, ev.Path)
}

// applyPrefixDelete deletes a directory.
func (p *PullPipeline) applyPrefixDelete(ctx context.Context, notespaceRoot string, ev *syncproto.SyncEvent) error {
	path := p.joinPath(notespaceRoot, ev.Path)
	if err := deleteDirUnderRoot(notespaceRoot, path); err != nil {
		return fmt.Errorf("failed to delete prefix: %w", err)
	}

	// Delete all documents under this prefix in the database
	return p.db.DeletePrefix(p.ws.Name, ev.Path)
}

// guardOwnRegistryNote drops an inbound event that would overwrite THIS
// machine's own presence note, and reports whether it did.
//
// # Why the guard exists
//
// The registry is single-writer by construction: a machine writes only
// machines/<its own id>.md, which is what makes per-document OCC sufficient
// and conflicts impossible. An inbound event for our own path therefore
// cannot be a legitimate replication of our own write — the push pipeline
// never round-trips our own documents back through pull — so it is another
// party writing a document only this machine may write.
//
// # Why it is detection and not prevention
//
// It cannot be prevention. Under the interim trust model every token
// grove-syncd issues is the owner and can write any path (getUserPrefixes
// short-circuits for user_id 1; CreateToken cannot assign a user), so the
// server will accept that write and hand it to us. All this side can do is
// refuse to APPLY it and leave evidence: the artifact on disk plus a
// registry_foreign_write conflict on the SSE feed. Real enforcement is the
// device-principal phase, where the server rejects a push to machines/<d>.md
// unless the pushing token's device principal is <d>.
//
// The guard is scoped to role = "registry" subscriptions. A machines/ path in
// an ordinary notebook notespace is just a document, and dropping it there
// would be a silent data-loss bug rather than a safety property.
func (p *PullPipeline) guardOwnRegistryNote(ctx context.Context, ev *syncproto.SyncEvent) bool {
	if p.ws == nil || p.ws.Role != config.SyncRoleRegistry || p.OwnMachineID == "" {
		return false
	}
	own := registry.NotePath(p.OwnMachineID)
	// PrevPath covers a move whose DESTINATION is our note: renaming a foreign
	// document onto our path is the same write by another spelling.
	if ev.Path != own && ev.PrevPath != own {
		return false
	}

	detail := fmt.Sprintf(
		"inbound %s event for this machine's own registry note was dropped; the registry is single-writer",
		ev.Type)
	p.log.Warn("registry foreign write rejected").
		Field("notespace", p.ws.Name).
		Field("path", own).
		Field("type", string(ev.Type)).
		Field("document_id", ev.DocumentID).
		Log(ctx)

	// The rejected content IS the evidence, so it is what the artifact holds.
	if err := p.recordConflictArtifact(ctx, own, ev.DocumentID,
		ConflictKindRegistryForeignWrite, ev.Content); err != nil {
		p.log.Warn("failed to record registry conflict artifact").
			Field("path", own).Err(err).Log(ctx)
	}
	if p.OnRegistryForeignWrite != nil {
		p.OnRegistryForeignWrite(p.ws.Name, own, detail)
	}
	return true
}

// recordConflict writes a merge-conflict artifact to disk at
// ~/.local/state/grove/sync/conflicts/.
func (p *PullPipeline) recordConflict(ctx context.Context, notespaceRoot, path, docID string, localContent []byte) error {
	return p.recordConflictArtifact(ctx, path, docID, ConflictKindMerge, localContent)
}

// recordConflictArtifact writes one conflict artifact. The kind rides in the
// filename because the conflicts endpoint rebuilds its rows from these files
// and has nothing else to read (see conflict_artifact.go).
func (p *PullPipeline) recordConflictArtifact(ctx context.Context, relPath, docID, kind string, content []byte) error {
	// Create conflicts directory: ~/.local/state/grove/sync/conflicts/{notespace}/
	conflictDir := filepath.Join(paths.StateDir(), "sync", "conflicts", p.ws.Name)
	if err := os.MkdirAll(conflictDir, 0o700); err != nil {
		return fmt.Errorf("failed to create conflict directory: %w", err)
	}

	conflictFile := filepath.Join(conflictDir, filepath.FromSlash(conflictArtifactName(relPath, docID, kind)))
	if err := os.MkdirAll(filepath.Dir(conflictFile), 0o700); err != nil {
		return fmt.Errorf("failed to create conflict directory: %w", err)
	}
	if err := writeFile(conflictFile, content, time.Time{}); err != nil {
		return fmt.Errorf("failed to write conflict artifact: %w", err)
	}

	p.log.Info("conflict recorded").
		Field("notespace", p.ws.Name).
		Field("path", relPath).
		Field("kind", kind).
		Field("artifact", conflictFile).Log(ctx)
	// TODO: Emit store.UpdateSyncConflict SSE event
	return nil
}

func (p *PullPipeline) joinPath(root, path string) string {
	// Use filepath.Join for proper path handling across OS
	return filepath.Join(root, filepath.FromSlash(path))
}
