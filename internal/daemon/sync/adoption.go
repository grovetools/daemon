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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
)

// ErrNotContested is the sentinel behind "this notespace is not contested".
// Adoption has exactly two failure shapes and an operator's script has to tell
// them apart: naming a notespace that is not withheld is the operator's
// mistake, and a receipt that could not be written is the daemon's failure.
// The HTTP layer maps this one to 409 and everything else to 500.
var ErrNotContested = errors.New("notespace is not contested")

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
	// Unreadable marks a local file that is present but could not be read
	// (mode 000, an EACCES on the parent). It is un-synced local content the
	// apply path would replace, so it collides — but its hash is unknown, so
	// it can never be identical and the operator is told which one it is.
	Unreadable bool `json:"unreadable,omitempty"`
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
	// Rejected counts incoming documents whose path does not resolve inside
	// the root. The apply paths refuse them, so they cannot write over
	// anything and must not contest; they are counted so the evidence does not
	// silently drop a document the server claims to hold.
	Rejected int `json:"rejected,omitempty"`
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
	if e.Rejected > 0 {
		fmt.Fprintf(&b, "%d incoming document(s) name a path outside this root and were ignored; the apply path refuses them too.\n", e.Rejected)
	}
	// Both directions, because the gate withholds both: pushDesired is
	// pullDesired's twin, so a contested notespace moves neither way and its
	// outbox is parked rather than drained. Saying "local work still pushes"
	// here — as this line did while the gate was one-directional — tells an
	// operator their un-synced notes are already off the machine when they are
	// not, which is the one thing they must not believe while deciding whether
	// this copy or the server's wins.
	b.WriteString("No writes enter this notespace and none leave it until it is adopted; local edits keep queuing, and adopting releases them.\n")
	for _, collision := range e.Collisions {
		state, local := "differs", shortHash(collision.LocalHash)
		switch {
		case collision.Unreadable:
			state, local = "unreadable", "(cannot read)"
		case collision.Identical:
			state = "identical"
		}
		fmt.Fprintf(&b, "  %-10s %s (local %s, server %s)\n", state, collision.Path, local, shortHash(collision.IncomingHash))
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
//
// tracked returns an ERROR rather than a bool alone, and that error aborts the
// whole verdict. A sync.db that cannot be read is not evidence that a path is
// untracked; answering "tracked" on a database error would clear every
// collision and wave the batch through, which is the one direction this gate
// must never fail in. The caller withholds and retries instead.
func DetectAdoption(notespaceID, root string, incoming []IncomingDocument, tracked func(path string) (bool, error), localSubject, serverSubject string) (AdoptionEvidence, error) {
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
		// The same containment rule both apply paths run on this input. A
		// server row of ../../x cannot be applied, so it must not be read,
		// hashed, or allowed to contest the notespace — that would hold a
		// tree hostage over a document that was going to be rejected anyway.
		if err := requireUnderRoot(root, local); err != nil {
			evidence.Rejected++
			continue
		}
		// Stat before read, and read only what the gate is actually about: an
		// untracked path with a local file on it. The gate now runs on every
		// batch (see PullPipeline.guardAdoption), so the ordinary case — an
		// incoming write to a path this machine already syncs — must not cost
		// a full read of the local file every time.
		if info, statErr := os.Lstat(local); statErr == nil && info.IsDir() {
			// A document write cannot replace a directory: the apply fails
			// loudly instead of overwriting anything.
			evidence.Clean++
			continue
		} else if statErr != nil && errors.Is(statErr, fs.ErrNotExist) {
			// Nothing local is at risk from this write.
			evidence.Clean++
			continue
		}
		if tracked != nil {
			isTracked, err := tracked(doc.Path)
			if err != nil {
				return AdoptionEvidence{}, fmt.Errorf("adoption gate could not read the sync state of %s: %w", doc.Path, err)
			}
			if isTracked {
				// Already a synced document here — replication, not adoption.
				continue
			}
		}
		content, readErr := os.ReadFile(local)
		if readErr != nil && errors.Is(readErr, fs.ErrNotExist) {
			// Raced away between the stat and the read.
			evidence.Clean++
			continue
		}
		collision := AdoptionCollision{
			Path:         doc.Path,
			IncomingHash: doc.Hash,
		}
		if readErr != nil {
			// Present but unreadable. applyCreate will replace it, so it is
			// exactly the un-synced local content this gate protects; its
			// hash is unknown, so it can never read as identical.
			collision.Unreadable = true
			evidence.Divergent++
		} else {
			collision.LocalHash = hashContent(content)
			collision.Identical = collision.LocalHash == collision.IncomingHash
			if collision.Identical {
				evidence.Identical++
			} else {
				evidence.Divergent++
			}
		}
		evidence.Collisions = append(evidence.Collisions, collision)
	}
	sort.Slice(evidence.Collisions, func(i, j int) bool {
		return evidence.Collisions[i].Path < evidence.Collisions[j].Path
	})
	return evidence, nil
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
//
// The ROOT is part of the decision, not decoration. The operator adopted the
// tree they were shown evidence about; under D8 one id can have two physical
// roots, and W3.4's `notespace move` carries an id into a different tree
// entirely. A receipt that only named the id would silently disable the gate
// for a root nobody ever looked at, so the root is recorded here and checked
// by AdoptionRecorded.
func RecordAdoption(notespaceID, root, detail string) (string, error) {
	if notespaceID == "" {
		return "", fmt.Errorf("adoption requires a notespace id")
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("adoption requires the root it was decided for; a receipt that names no root cannot be verified against the tree it would unblock")
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
// this machine FOR THIS ROOT. An unreadable, unparseable, or root-mismatched
// receipt reads as "not adopted": the conservative direction is to withhold
// writes, never to let them through.
//
// The root is compared physically (symlinks resolved), the same way
// requireUnderRoot compares a server-supplied path against the root, so the
// /var -> /private/var aliasing the rest of the daemon tolerates does not read
// as a different tree.
func AdoptionRecorded(notespaceID, root string) bool {
	if notespaceID == "" || strings.TrimSpace(root) == "" {
		return false
	}
	receipt, err := readAdoptionReceipt(AdoptionReceiptPath(notespaceID))
	if err != nil {
		return false
	}
	return samePhysicalPath(receipt.Root, root)
}

// adoptionReceipt is the decision on disk: which notespace, and which tree it
// was decided for.
type adoptionReceipt struct {
	NotespaceID string
	Root        string
}

// readAdoptionReceipt parses a receipt written by RecordAdoption. It reads the
// exact shape that function writes — `key = "value"` with quoteTOML's escapes —
// rather than reaching for a general parser, so the two halves cannot drift.
func readAdoptionReceipt(path string) (adoptionReceipt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return adoptionReceipt{}, err
	}
	var out adoptionReceipt
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(key), "#") {
			continue
		}
		unquoted, ok := unquoteTOML(strings.TrimSpace(value))
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "notespace_id":
			out.NotespaceID = unquoted
		case "root":
			out.Root = unquoted
		}
	}
	if out.Root == "" {
		return adoptionReceipt{}, fmt.Errorf("adoption receipt %s records no root", path)
	}
	return out, nil
}

// samePhysicalPath reports whether two recorded roots name the same directory.
// Neither side has to exist: a receipt outlives the tree it names, and an
// adoption for a root that is currently absent is still an adoption for that
// root, not for whatever else the id resolves to now.
func samePhysicalPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	resolve := func(p string) string {
		if resolved, err := resolveExisting(p); err == nil {
			return filepath.Clean(resolved)
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
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

// unquoteTOML reverses quoteTOML. ok is false for anything that is not one of
// its strings, so a hand-edited receipt is refused rather than half-read.
func unquoteTOML(value string) (string, bool) {
	if len(value) < 2 || !strings.HasPrefix(value, `"`) || !strings.HasSuffix(value, `"`) {
		return "", false
	}
	body := value[1 : len(value)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return "", false
		}
		switch body[i] {
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			return "", false
		}
	}
	return b.String(), true
}
