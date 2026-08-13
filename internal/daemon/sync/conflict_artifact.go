package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/grovetools/core/pkg/paths"
)

// Conflict kinds. These are the values that reach a user, both as
// store.SyncConflictPayload.Kind on the SSE stream and — since this file — as
// the Kind field of GET /api/sync/conflicts.
//
// Why they have to be encoded in the FILENAME: the conflicts endpoint is
// artifact-backed. It scans StateDir/sync/conflicts/<notespace>/ and rebuilds
// each row from the file it finds, so anything the broadcast payload carried
// but the file did not was lost the moment the daemon restarted (or, in fact,
// the moment the SSE subscriber looked away). The kind is the one field that
// distinguishes "your two edits overlapped" from "someone else wrote your
// machine's registry note", so it has to survive in the artifact.
const (
	// ConflictKindMerge is the historical kind: a 3-way merge that could not
	// be resolved. It is IMPLICIT in the legacy filename shape
	// "<path>.<document_id>.conflict.md" and is never written into a name, so
	// every artifact that predates this file keeps parsing exactly as before.
	ConflictKindMerge = "merge"

	// ConflictKindRegistryForeignWrite marks an inbound event for THIS
	// machine's own registry note. The registry is single-writer by design, so
	// such an event can only mean another party wrote a document only this
	// machine may write. It is dropped, and the artifact is the evidence.
	ConflictKindRegistryForeignWrite = "registry_foreign_write"

	// ConflictKindRegistration records duplicate physical ids and server-side
	// registration conflicts. Identity stamps are never document-merged.
	ConflictKindRegistration = "registration"

	// ConflictKindMissingRoot records an incoming apply refused because the
	// notespace's recorded local root does not exist (W3.2). It is evidence,
	// not a merge: nothing was written, and nothing will be until the operator
	// materializes the root the config records.
	ConflictKindMissingRoot = "missing_root"

	// ConflictKindDuplicateStamp records the D8 runtime rule: two roots carry
	// the same notespace stamp id, the first-seen one keeps syncing, and the
	// later one is parked. The artifact names both roots so `grove doctor
	// --fix` can re-mint whichever copy the operator designates.
	ConflictKindDuplicateStamp = "duplicate_stamp"

	// ConflictKindAdoption records the W3.5 adoption case: an incoming batch
	// would have written over un-synced local notes, so the notespace is
	// contested and takes no writes until the operator adopts it. The artifact
	// carries the evidence — hash overlap and subject match — because the
	// decision is made from it.
	ConflictKindAdoption = "adoption"
)

// namedConflictKinds are the kinds that appear as a filename segment. Merge is
// deliberately absent: it is the unnamed default, which is what keeps the
// legacy name shape unambiguous. A document id is a UUID and can therefore
// never collide with one of these words.
var namedConflictKinds = map[string]bool{
	ConflictKindRegistryForeignWrite: true,
	ConflictKindRegistration:         true,
	ConflictKindMissingRoot:          true,
	ConflictKindDuplicateStamp:       true,
	ConflictKindAdoption:             true,
}

const conflictArtifactSuffix = ".conflict.md"

// WriteRegistrationConflict persists restart-safe evidence under the immutable
// notespace id. It is intentionally separate from document merge machinery.
func WriteRegistrationConflict(notespaceID, detail string) (string, error) {
	return WriteNotespaceConflict(notespaceID, ConflictKindRegistration, detail)
}

// WriteNotespaceConflict persists restart-safe, notespace-level (as opposed to
// document-level) evidence: a refusal or a parking decision, keyed by the
// immutable notespace id and carrying its reason as the whole artifact body.
// The kind rides in the filename because the conflicts endpoint rebuilds its
// rows from these files and has nothing else to read.
func WriteNotespaceConflict(notespaceID, kind, detail string) (string, error) {
	if !namedConflictKinds[kind] {
		return "", fmt.Errorf("conflict kind %q is not a notespace-level kind", kind)
	}
	dir := filepath.Join(paths.StateDir(), "sync", "conflicts", notespaceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := conflictArtifactName(".notespace.toml", notespaceID, kind)
	path := filepath.Join(dir, name)
	body := "# " + strings.ReplaceAll(kind, "_", " ") + " conflict\n\n" + detail + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// conflictArtifactName builds the artifact filename for one conflict.
//
//	merge:                 <path>.<document_id>.conflict.md
//	any other kind:        <path>.<document_id>.<kind>.conflict.md
//
// The document id stays in its historical position so handleSyncConflicts's
// path/id split is unchanged for every existing artifact on disk.
func conflictArtifactName(relPath, docID, kind string) string {
	if kind == "" || kind == ConflictKindMerge || !namedConflictKinds[kind] {
		return fmt.Sprintf("%s.%s%s", relPath, docID, conflictArtifactSuffix)
	}
	return fmt.Sprintf("%s.%s.%s%s", relPath, docID, kind, conflictArtifactSuffix)
}

// ParseConflictArtifactName reverses conflictArtifactName for a
// notespace-relative artifact path. ok is false when the name is not an
// artifact at all, or when it carries no parseable document id — in which case
// the caller should skip the file rather than guess at its provenance.
//
// Exported because the daemon's HTTP layer (handleSyncConflicts) is the
// reader, and having two copies of this split is exactly how the kind would
// come to mean different things at the two ends.
func ParseConflictArtifactName(rel string) (origPath, docID, kind string, ok bool) {
	if !strings.HasSuffix(rel, conflictArtifactSuffix) {
		return "", "", "", false
	}
	stem := strings.TrimSuffix(rel, conflictArtifactSuffix)

	kind = ConflictKindMerge
	if idx := strings.LastIndex(stem, "."); idx >= 0 && namedConflictKinds[stem[idx+1:]] {
		kind = stem[idx+1:]
		stem = stem[:idx]
	}

	idx := strings.LastIndex(stem, ".")
	if idx < 0 {
		return "", "", "", false
	}
	return stem[:idx], stem[idx+1:], kind, true
}
