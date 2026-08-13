package sync

// Adoption with evidence (Phase 3, W3.5).
//
// The state this file exists for: a machine pulls a notebook onto a tree that
// ALREADY holds notes nobody ever synced. Same notebook name, same notespace
// name, same file names — and two different histories. Applying the incoming
// batch would overwrite local work that has never been anywhere else, and
// refusing it wholesale would mean a shared notebook could never be pulled onto
// a machine that had started taking notes before it joined.
//
// W3.5 splits the difference by CONTAINMENT rather than by heuristic:
//
//	Clean documents flow. An incoming document whose path holds no local
//	file, or holds a byte-identical one, is not contested by anything and is
//	applied as usual.
//
//	A contested notespace takes no writes until it is adopted. The moment
//	one incoming path collides with an un-synced local file whose content
//	DIFFERS, the whole notespace is withheld — not just that document. Per
//	W3.5, "clean documents flow regardless" is a statement about the
//	notebook, never a licence to write into a contested subtree.
//
// The verdict is never guessed. It is computed from two facts the operator can
// check, carried on the conflicts feed as the adoption case's evidence:
//
//	HASH OVERLAP — of the paths present on both sides, how many are already
//	byte-identical. High overlap says these two trees are the same notes that
//	drifted (adoption converges them); zero overlap says the names collided
//	and the contents have nothing to do with each other (adopting would bury
//	local work).
//
//	SUBJECT MATCH — whether the stamp on this root and the server's row for
//	the incoming notespace name the same subject. A match says both sides are
//	notes ABOUT the same thing; a mismatch says one name is doing duty for
//	two subjects, which is the case where adoption is almost certainly wrong.
//
// Adoption itself is an operator act (`grove sync adopt-notespace`), recorded as a
// receipt beside the conflict evidence so it survives a daemon restart: without
// it, the same untracked collision would be re-detected on the next pass and
// the notespace would re-contest itself forever. After adoption the notespace
// is an ordinary synced notespace and ordinary merge machinery governs it.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
)

// IncomingDocument is one document an incoming batch would write: the wire path
// and the content hash the server holds for it. Both the snapshot manifest and
// the event tail project into this, so the gate reads one shape.
type IncomingDocument struct {
	Path string
	Hash string
}

// IncomingFromManifest projects a snapshot manifest into the gate's input.
func IncomingFromManifest(docs []syncproto.DocumentSnapshot) []IncomingDocument {
	out := make([]IncomingDocument, 0, len(docs))
	for _, doc := range docs {
		out = append(out, IncomingDocument{Path: doc.Path, Hash: doc.Hash})
	}
	return out
}

// IncomingFromEvents projects an event batch into the gate's input. Only the
// events that WRITE content are carried: a delete or a prefix move cannot
// clobber an un-synced local file by content, and treating them as collisions
// would contest a notespace over a document that is going away.
func IncomingFromEvents(events []syncproto.SyncEvent) []IncomingDocument {
	out := make([]IncomingDocument, 0, len(events))
	for i := range events {
		switch events[i].Type {
		case syncproto.EventDocumentCreated, syncproto.EventDocumentUpdated, syncproto.EventDocumentMoved:
			out = append(out, IncomingDocument{Path: events[i].Path, Hash: events[i].ContentHash})
		}
	}
	return out
}

// AdoptionCollision is one incoming path that already exists locally as a file
// this machine has never synced.
type AdoptionCollision struct {
	Path         string `json:"path"`
	LocalHash    string `json:"local_hash"`
	IncomingHash string `json:"incoming_hash"`
	// Identical is the per-path half of the hash-overlap evidence.
	Identical bool `json:"identical"`
}

// AdoptionEvidence is the whole verdict for one notespace: what collides, how
// much of it already agrees, and whether the two sides are even about the same
// subject.
type AdoptionEvidence struct {
	NotespaceID string `json:"notespace_id"`
	Root        string `json:"root"`
	// Collisions are ordered by path so two runs render identically.
	Collisions []AdoptionCollision `json:"collisions,omitempty"`
	// Identical / Divergent partition Collisions; Clean counts incoming
	// documents that land on no local file at all.
	Identical int `json:"identical"`
	Divergent int `json:"divergent"`
	Clean     int `json:"clean"`
	// LocalSubject is this root's stamp subject. ServerSubject is the
	// server's row for the incoming notespace, or "" when the inventory could
	// not be read — reported as unknown rather than silently as a match.
	LocalSubject  string `json:"local_subject,omitempty"`
	ServerSubject string `json:"server_subject,omitempty"`
}

// Contested reports whether any incoming write would land on un-synced local
// content that differs. It is the only question the gate asks; the rest of the
// evidence exists for the operator's decision, not for this one.
func (e AdoptionEvidence) Contested() bool { return e.Divergent > 0 }

// SubjectMatch is tri-state: match, mismatch, or unknown when either side has
// no subject to compare.
func (e AdoptionEvidence) SubjectMatch() string {
	if e.LocalSubject == "" || e.ServerSubject == "" {
		return "unknown"
	}
	if e.LocalSubject == e.ServerSubject {
		return "match"
	}
	return "mismatch"
}

// Detail is the operator-facing sentence written to the conflicts feed. It
// names both evidence legs and the verb that resolves the case, because the
// artifact is what an operator meets first and it has to be answerable on its
// own.
func (e AdoptionEvidence) Detail() string {
	var b strings.Builder
	fmt.Fprintf(&b, "notespace %s at %s is contested: %d of %d colliding path(s) hold un-synced local content that differs from the server's.\n",
		e.NotespaceID, e.Root, e.Divergent, len(e.Collisions))
	fmt.Fprintf(&b, "hash overlap: %d/%d colliding path(s) are already byte-identical; %d incoming document(s) collide with nothing.\n",
		e.Identical, len(e.Collisions), e.Clean)
	switch e.SubjectMatch() {
	case "match":
		fmt.Fprintf(&b, "subject match: both sides are notes about %s.\n", e.LocalSubject)
	case "mismatch":
		fmt.Fprintf(&b, "subject MISMATCH: this root is stamped %s, the server's notespace is %s — one name is doing duty for two subjects.\n",
			e.LocalSubject, e.ServerSubject)
	default:
		fmt.Fprintf(&b, "subject match: unknown (the server's inventory could not be read for this notespace).\n")
	}
	b.WriteString("No writes enter this notespace until it is adopted; local work still pushes.\n")
	for _, collision := range e.Collisions {
		state := "differs"
		if collision.Identical {
			state = "identical"
		}
		fmt.Fprintf(&b, "  %-9s %s (local %s, server %s)\n", state, collision.Path, shortHash(collision.LocalHash), shortHash(collision.IncomingHash))
	}
	fmt.Fprintf(&b, "Adopt it with `grove sync adopt-notespace %s --confirm` once the evidence above says these are the same notes.\n", e.NotespaceID)
	return b.String()
}

func shortHash(h string) string {
	switch {
	case h == "":
		// A move event carries no content hash. The write still lands on the
		// local file, so it still contests; saying so beats printing a blank.
		return "(no hash)"
	case len(h) <= 12:
		return h
	default:
		return h[:12]
	}
}

// DetectAdoption computes the verdict for one incoming batch.
//
// tracked reports whether a wire path is already a synced document of this
// notespace. It is the definition of "un-synced": a path this machine has a
// row for is one it has synced before, so an incoming write to it is ordinary
// replication (and, on divergence, an ordinary merge conflict) rather than an
// adoption case. Only paths with no row can be pre-existing local notes.
func DetectAdoption(notespaceID, root string, incoming []IncomingDocument, tracked func(path string) bool, localSubject, serverSubject string) AdoptionEvidence {
	evidence := AdoptionEvidence{
		NotespaceID:   notespaceID,
		Root:          root,
		LocalSubject:  localSubject,
		ServerSubject: serverSubject,
	}
	seen := make(map[string]bool, len(incoming))
	for _, doc := range incoming {
		if doc.Path == "" || seen[doc.Path] {
			continue
		}
		seen[doc.Path] = true

		local := filepath.Join(root, filepath.FromSlash(doc.Path))
		content, err := os.ReadFile(local)
		if err != nil {
			// Absent (the common case) or unreadable: nothing local is at
			// risk from this write.
			evidence.Clean++
			continue
		}
		if tracked != nil && tracked(doc.Path) {
			// Already a synced document here — replication, not adoption.
			continue
		}
		collision := AdoptionCollision{
			Path:         doc.Path,
			LocalHash:    hashContent(content),
			IncomingHash: doc.Hash,
		}
		collision.Identical = collision.LocalHash == collision.IncomingHash
		if collision.Identical {
			evidence.Identical++
		} else {
			evidence.Divergent++
		}
		evidence.Collisions = append(evidence.Collisions, collision)
	}
	sort.Slice(evidence.Collisions, func(i, j int) bool {
		return evidence.Collisions[i].Path < evidence.Collisions[j].Path
	})
	return evidence
}

// ContestedNotespace is one notespace withheld by the gate, and the evidence
// the operator decides from. It is the daemon's verdict rather than the
// pipeline's: the watcher holds the set, the HTTP layer serves it, and
// `grove sync adopt-notespace` renders it. It lives in this package because both of
// those import it and neither imports the other.
type ContestedNotespace struct {
	// NotespaceID is the immutable id taking no writes.
	NotespaceID string `json:"notespace_id"`
	// Root is the local tree the incoming batch would have written into.
	Root string `json:"root"`
	// Reason is the one-line summary; Detail is the full evidence body, the
	// same text the conflicts-feed artifact carries.
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
	// Colliding / Identical / Divergent are the hash-overlap evidence, and
	// SubjectMatch is match/mismatch/unknown. They are carried structured as
	// well as inside Detail so a UI does not have to parse prose.
	Colliding    int    `json:"colliding_paths"`
	Identical    int    `json:"identical_paths"`
	Divergent    int    `json:"divergent_paths"`
	SubjectMatch string `json:"subject_match,omitempty"`
}

// Contest projects a gate verdict into the daemon's contested entry.
func (e AdoptionEvidence) Contest(root string) ContestedNotespace {
	return ContestedNotespace{
		NotespaceID: e.NotespaceID,
		Root:        root,
		Reason: fmt.Sprintf("adoption pending: %d of %d colliding path(s) hold un-synced local notes that differ (subject %s)",
			e.Divergent, len(e.Collisions), e.SubjectMatch()),
		Detail:       e.Detail(),
		Colliding:    len(e.Collisions),
		Identical:    e.Identical,
		Divergent:    e.Divergent,
		SubjectMatch: e.SubjectMatch(),
	}
}

// ---- adoption receipts ---------------------------------------------------

// adoptionReceiptDir holds one file per adopted notespace. It sits beside the
// conflict artifacts for the same reason those are on disk: the daemon's
// in-memory contested set does not survive a restart, and a decision the
// operator already made must not be asked again.
func adoptionReceiptDir() string {
	return filepath.Join(paths.StateDir(), "sync", "adoptions")
}

// AdoptionReceiptPath is the receipt for one notespace id.
func AdoptionReceiptPath(notespaceID string) string {
	return filepath.Join(adoptionReceiptDir(), notespaceID+".toml")
}

// RecordAdoption writes the durable receipt for an operator's adoption. It is
// idempotent: adopting twice rewrites the same path with the later decision.
func RecordAdoption(notespaceID, root, detail string) (string, error) {
	if notespaceID == "" {
		return "", fmt.Errorf("adoption requires a notespace id")
	}
	if err := os.MkdirAll(adoptionReceiptDir(), 0o700); err != nil {
		return "", err
	}
	path := AdoptionReceiptPath(notespaceID)
	body := "# W3.5 adoption receipt: the operator decided this notespace and the\n" +
		"# server's are the same notes. Incoming writes are no longer withheld.\n" +
		"notespace_id = " + quoteTOML(notespaceID) + "\n" +
		"root = " + quoteTOML(root) + "\n" +
		"adopted_at = " + quoteTOML(time.Now().UTC().Format(time.RFC3339)) + "\n" +
		"evidence = " + quoteTOML(detail) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// AdoptionRecorded reports whether this notespace has already been adopted on
// this machine. An unreadable receipts directory reads as "not adopted": the
// conservative direction is to withhold writes, never to let them through.
func AdoptionRecorded(notespaceID string) bool {
	if notespaceID == "" {
		return false
	}
	_, err := os.Stat(AdoptionReceiptPath(notespaceID))
	return err == nil
}

// ForgetAdoption removes a receipt. It exists for tests and for an operator
// who wants the gate to re-evaluate; nothing in the daemon calls it.
func ForgetAdoption(notespaceID string) error {
	err := os.Remove(AdoptionReceiptPath(notespaceID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func quoteTOML(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	return `"` + replacer.Replace(value) + `"`
}
