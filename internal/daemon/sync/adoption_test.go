package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

func writeLocal(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func untracked(string) (bool, error) { return false, nil }

// detect is DetectAdoption for the tests that assert on the verdict itself: a
// lookup error is its own regression case (see TestDetectAdoptionFailsClosed…)
// and must not be swallowed anywhere else.
func detect(t *testing.T, notespaceID, root string, incoming []IncomingDocument, tracked func(string) (bool, error), localSubject, serverSubject string) AdoptionEvidence {
	t.Helper()
	evidence, err := DetectAdoption(notespaceID, root, incoming, tracked, localSubject, serverSubject)
	if err != nil {
		t.Fatalf("DetectAdoption: %v", err)
	}
	return evidence
}

// W3.5's central case: incoming documents whose paths hold un-synced local
// notes with different content contest the notespace.
func TestDetectAdoptionContestsDivergentUnsyncedNotes(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "notes/a.md", "local only")
	writeLocal(t, root, "notes/same.md", "identical")

	incoming := []IncomingDocument{
		{Path: "notes/a.md", Hash: hashContent([]byte("server's version"))},
		{Path: "notes/same.md", Hash: hashContent([]byte("identical"))},
		{Path: "notes/new.md", Hash: hashContent([]byte("nothing local here"))},
	}
	evidence := detect(t, "01NS", root, incoming, untracked, "github.com/me/core", "github.com/me/core")

	if !evidence.Contested() {
		t.Fatal("a divergent un-synced collision did not contest the notespace")
	}
	if evidence.Divergent != 1 || evidence.Identical != 1 || evidence.Clean != 1 {
		t.Fatalf("divergent/identical/clean = %d/%d/%d, want 1/1/1", evidence.Divergent, evidence.Identical, evidence.Clean)
	}
	if evidence.SubjectMatch() != "match" {
		t.Fatalf("SubjectMatch = %q, want match", evidence.SubjectMatch())
	}
	detail := evidence.Detail()
	for _, want := range []string{"hash overlap: 1/2", "subject match", "notes/a.md", "grove sync adopt-notespace 01NS"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("evidence detail is missing %q:\n%s", want, detail)
		}
	}
}

// "Clean documents flow": nothing local to lose means nothing to decide.
func TestDetectAdoptionLeavesACleanTreeUncontested(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "notes/same.md", "identical")

	evidence := detect(t, "01NS", root, []IncomingDocument{
		{Path: "notes/same.md", Hash: hashContent([]byte("identical"))},
		{Path: "notes/new.md", Hash: hashContent([]byte("new"))},
	}, untracked, "", "")
	if evidence.Contested() {
		t.Fatalf("an identical/absent batch contested the notespace: %+v", evidence)
	}
	if evidence.SubjectMatch() != "unknown" {
		t.Fatalf("SubjectMatch with no subjects = %q, want unknown", evidence.SubjectMatch())
	}
}

// A path this machine already syncs is replication (and, on divergence, an
// ordinary merge conflict) — never an adoption case.
func TestDetectAdoptionIgnoresAlreadyTrackedPaths(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "notes/tracked.md", "local edit")

	tracked := func(path string) (bool, error) { return path == "notes/tracked.md", nil }
	evidence := detect(t, "01NS", root, []IncomingDocument{
		{Path: "notes/tracked.md", Hash: hashContent([]byte("server edit"))},
	}, tracked, "", "")
	if evidence.Contested() || len(evidence.Collisions) != 0 {
		t.Fatalf("a tracked path was treated as an adoption case: %+v", evidence)
	}
}

func TestSubjectMismatchIsReportedDistinctlyFromUnknown(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "a.md", "mine")
	evidence := detect(t, "01NS", root, []IncomingDocument{
		{Path: "a.md", Hash: hashContent([]byte("theirs"))},
	}, untracked, "github.com/me/core", "github.com/me/notes")
	if evidence.SubjectMatch() != "mismatch" {
		t.Fatalf("SubjectMatch = %q, want mismatch", evidence.SubjectMatch())
	}
	if !strings.Contains(evidence.Detail(), "subject MISMATCH") {
		t.Fatalf("mismatch was not called out in the evidence:\n%s", evidence.Detail())
	}
}

// The evidence an operator meets first has to describe the gate they are
// actually behind. W3.5's veto is two-sided — pushDesired is pullDesired's twin
// — so the detail must not promise that local work is leaving the machine while
// the notespace is contested. It said exactly that while the gate was
// one-directional, and the sentence outlived the fix.
//
// This pins the negative as well as the positive: an operator deciding which
// copy wins may be about to reimage the machine holding the only copy of these
// notes, and "still pushes" is the sentence that would make that safe-looking.
func TestDetailDoesNotPromiseAContestedNotespaceStillPushes(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "notes/a.md", "local only, never synced")

	evidence := detect(t, "01NS", root, []IncomingDocument{
		{Path: "notes/a.md", Hash: hashContent([]byte("server's version"))},
	}, untracked, "github.com/me/core", "github.com/me/core")
	if !evidence.Contested() {
		t.Fatal("a divergent un-synced collision did not contest the notespace")
	}

	detail := evidence.Detail()
	if strings.Contains(detail, "local work still pushes") {
		t.Fatalf("the evidence still promises a contested notespace pushes:\n%s", detail)
	}
	for _, want := range []string{
		"No writes enter this notespace and none leave it",
		"local edits keep queuing",
		"adopting releases them",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("evidence detail is missing %q:\n%s", want, detail)
		}
	}
}

// Only content-writing events can clobber an un-synced note.
func TestIncomingFromEventsCarriesOnlyWrites(t *testing.T) {
	events := []syncproto.SyncEvent{
		{Type: syncproto.EventDocumentCreated, Path: "a.md", ContentHash: "h1"},
		{Type: syncproto.EventDocumentUpdated, Path: "b.md", ContentHash: "h2"},
		{Type: syncproto.EventDocumentMoved, Path: "c.md", ContentHash: "h3"},
		{Type: syncproto.EventDocumentDeleted, Path: "d.md"},
		{Type: syncproto.EventPrefixDeleted, Path: "e/"},
	}
	got := IncomingFromEvents(events)
	if len(got) != 3 {
		t.Fatalf("IncomingFromEvents = %+v, want the three writing events", got)
	}
}

// The gate is the enforcement of "no writes into a contested notespace until
// adopted": it refuses the batch, writes the evidence, and reports the verdict.
func TestGuardAdoptionWithholdsTheBatchAndReportsOnce(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	writeLocal(t, root, "notes/a.md", "un-synced local work")
	pipeline := NewPullPipeline(&config.SyncWorkspace{Name: "01NS"}, nil, db, logging.NewUnifiedLogger("test.adoption"))

	var contested []AdoptionEvidence
	var conflicts []string
	pipeline.OnContested = func(_ string, ev AdoptionEvidence) { contested = append(contested, ev) }
	pipeline.OnConflict = func(kind, _, _, _, _ string) { conflicts = append(conflicts, kind) }

	incoming := []IncomingDocument{{Path: "notes/a.md", Hash: hashContent([]byte("the server's version"))}}
	if err := pipeline.guardAdoption(context.Background(), root, incoming); err == nil {
		t.Fatal("the gate let a contested batch through")
	}
	if len(contested) != 1 || contested[0].Divergent != 1 {
		t.Fatalf("OnContested = %+v, want one verdict with one divergent path", contested)
	}
	if len(conflicts) != 1 || conflicts[0] != ConflictKindAdoption {
		t.Fatalf("conflict kinds = %v, want one %q", conflicts, ConflictKindAdoption)
	}
	// Restart-safe evidence: the artifact is on the conflicts feed.
	artifacts, err := filepath.Glob(filepath.Join(stateHome, "grove", "sync", "conflicts", "01NS", "*"+ConflictKindAdoption+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("adoption artifacts = %v, want exactly one", artifacts)
	}
	// Nothing was written into the contested tree.
	local, err := os.ReadFile(filepath.Join(root, "notes", "a.md"))
	if err != nil || string(local) != "un-synced local work" {
		t.Fatalf("the contested tree was written into: %q, %v", local, err)
	}

	// The pull loop retries on a timer and teardown is not instantaneous. A
	// retry must keep withholding without announcing the same contest again.
	if err := pipeline.guardAdoption(context.Background(), root, incoming); err == nil {
		t.Fatal("a retry let the contested batch through")
	}
	if len(contested) != 1 || len(conflicts) != 1 {
		t.Fatalf("a retry re-announced the contest: %d verdicts, %d conflicts", len(contested), len(conflicts))
	}
}

// Adoption is durable: the receipt stops the same untracked collision from
// re-contesting the notespace after a restart.
func TestGuardAdoptionRespectsAnAdoptionReceipt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	writeLocal(t, root, "notes/a.md", "un-synced local work")
	pipeline := NewPullPipeline(&config.SyncWorkspace{Name: "01NS"}, nil, db, logging.NewUnifiedLogger("test.adoption"))
	pipeline.OnContested = func(string, AdoptionEvidence) { t.Fatal("an adopted notespace contested itself again") }

	if _, err := RecordAdoption("01NS", root, "operator adopted"); err != nil {
		t.Fatal(err)
	}
	if !AdoptionRecorded("01NS", root) {
		t.Fatal("AdoptionRecorded did not see the receipt it had just written")
	}
	incoming := []IncomingDocument{{Path: "notes/a.md", Hash: hashContent([]byte("the server's version"))}}
	if err := pipeline.guardAdoption(context.Background(), root, incoming); err != nil {
		t.Fatalf("the gate withheld an adopted notespace: %v", err)
	}

	if err := ForgetAdoption("01NS"); err != nil {
		t.Fatal(err)
	}
	if AdoptionRecorded("01NS", root) {
		t.Fatal("ForgetAdoption left the receipt in place")
	}
}

// F1 (review finding, W3.5): the gate is not a one-shot. `PullEvents` returns a
// bounded window, so a collision can arrive in a later batch than the first —
// and before this, a clean first batch set adoptionSettled and every batch
// after it was applied unchecked.
func TestGuardAdoptionEvaluatesEveryBatchNotJustTheFirst(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	// The un-synced local note the operator has never pushed. Nothing in the
	// FIRST batch touches it.
	writeLocal(t, root, "notes/mine.md", "un-synced local work")
	pipeline := NewPullPipeline(&config.SyncWorkspace{Name: "01NS"}, nil, db, logging.NewUnifiedLogger("test.adoption"))
	var contested []AdoptionEvidence
	pipeline.OnContested = func(_ string, ev AdoptionEvidence) { contested = append(contested, ev) }

	first := []IncomingDocument{{Path: "notes/theirs.md", Hash: hashContent([]byte("nothing local here"))}}
	if err := pipeline.guardAdoption(context.Background(), root, first); err != nil {
		t.Fatalf("a clean batch was withheld: %v", err)
	}
	if pipeline.adoptionSettled {
		t.Fatal("a clean batch settled the adoption question; no batch is the tree")
	}

	// Batch 2 lands on the un-synced note. It must be withheld.
	second := []IncomingDocument{{Path: "notes/mine.md", Hash: hashContent([]byte("the server's version"))}}
	if err := pipeline.guardAdoption(context.Background(), root, second); err == nil {
		t.Fatal("a collision in the second batch was applied: the gate settled on batch 1's evidence")
	}
	if len(contested) != 1 || contested[0].Divergent != 1 {
		t.Fatalf("OnContested = %+v, want one verdict for the second batch", contested)
	}
	local, err := os.ReadFile(filepath.Join(root, "notes", "mine.md"))
	if err != nil || string(local) != "un-synced local work" {
		t.Fatalf("the contested note was written into: %q, %v", local, err)
	}
}

// F3: an unreadable sync.db is not evidence that a path is untracked. Before
// this, the lookup error read as "already synced", which cleared every
// collision, applied the batch, and set adoptionSettled for good.
func TestGuardAdoptionWithholdsWhenLocalSyncStateIsUnreadable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeLocal(t, root, "notes/a.md", "un-synced local work")
	pipeline := NewPullPipeline(&config.SyncWorkspace{Name: "01NS"}, nil, db, logging.NewUnifiedLogger("test.adoption"))
	pipeline.OnContested = func(string, AdoptionEvidence) {
		t.Fatal("a database error contested the notespace; it is a daemon fault, not the operator's decision")
	}
	// Every lookup now fails.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	incoming := []IncomingDocument{{Path: "notes/a.md", Hash: hashContent([]byte("the server's version"))}}
	if err := pipeline.guardAdoption(context.Background(), root, incoming); err == nil {
		t.Fatal("the gate let a batch through while it could not read local sync state")
	}
	if pipeline.adoptionSettled {
		t.Fatal("a failed lookup settled the gate for the life of the pipeline")
	}
	local, err := os.ReadFile(filepath.Join(root, "notes", "a.md"))
	if err != nil || string(local) != "un-synced local work" {
		t.Fatalf("the tree was written into: %q, %v", local, err)
	}
}

// F4: the receipt records the ROOT the operator decided about. One id can have
// two physical roots (D8) and an id survives `notespace move` (W3.4), so an
// adoption made for another tree must not disable the gate for this one.
func TestAdoptionReceiptBindsTheRootItWasDecidedFor(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	adopted, other := t.TempDir(), t.TempDir()
	if _, err := RecordAdoption("01NS", adopted, "operator adopted the copy at "+adopted); err != nil {
		t.Fatal(err)
	}
	if !AdoptionRecorded("01NS", adopted) {
		t.Fatal("the receipt does not answer for the root it was written for")
	}
	if AdoptionRecorded("01NS", other) {
		t.Fatal("a receipt for one root answered for a different root")
	}

	writeLocal(t, other, "notes/a.md", "un-synced local work in the OTHER root")
	pipeline := NewPullPipeline(&config.SyncWorkspace{Name: "01NS"}, nil, db, logging.NewUnifiedLogger("test.adoption"))
	incoming := []IncomingDocument{{Path: "notes/a.md", Hash: hashContent([]byte("the server's version"))}}
	if err := pipeline.guardAdoption(context.Background(), other, incoming); err == nil {
		t.Fatal("an adoption recorded for one root unblocked writes into another")
	}

	// A receipt with no root cannot be verified against anything, so it is
	// never written in the first place.
	if _, err := RecordAdoption("01NS", "", "no root"); err == nil {
		t.Fatal("a rootless adoption receipt was accepted")
	}
}

// F5: a present-but-unreadable local file is un-synced local content that the
// apply path would replace — a collision, not a clean write. Counting it clean
// also inflated one of the two numbers the operator decides from.
func TestDetectAdoptionContestsAnUnreadableLocalFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files")
	}
	root := t.TempDir()
	writeLocal(t, root, "notes/locked.md", "un-synced local work")
	locked := filepath.Join(root, "notes", "locked.md")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	evidence := detect(t, "01NS", root, []IncomingDocument{
		{Path: "notes/locked.md", Hash: hashContent([]byte("the server's version"))},
	}, untracked, "", "")
	if !evidence.Contested() {
		t.Fatalf("an unreadable local file read as clean: %+v", evidence)
	}
	if evidence.Clean != 0 || evidence.Divergent != 1 || len(evidence.Collisions) != 1 {
		t.Fatalf("evidence = %+v, want one divergent collision and nothing clean", evidence)
	}
	if !evidence.Collisions[0].Unreadable {
		t.Fatalf("the collision does not say WHY it has no local hash: %+v", evidence.Collisions[0])
	}
	if !strings.Contains(evidence.Detail(), "unreadable") {
		t.Fatalf("the operator's evidence does not name the unreadable file:\n%s", evidence.Detail())
	}
}

// F6: server-supplied paths get the same containment check both apply paths
// run. A traversing path cannot be applied, so it must not be read, hashed, or
// allowed to hold the notespace hostage.
func TestDetectAdoptionSkipsPathsThatEscapeTheRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ns")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// An un-synced local file OUTSIDE the notespace, which the traversing
	// document would otherwise be compared against.
	writeLocal(t, parent, "outside.md", "not this notespace's business")

	evidence := detect(t, "01NS", root, []IncomingDocument{
		{Path: "../outside.md", Hash: hashContent([]byte("the server's version"))},
	}, untracked, "", "")
	if evidence.Contested() {
		t.Fatalf("a path outside the root contested the notespace: %+v", evidence)
	}
	if evidence.Rejected != 1 || len(evidence.Collisions) != 0 || evidence.Clean != 0 {
		t.Fatalf("evidence = %+v, want the escaping document rejected and nothing else", evidence)
	}
	if !strings.Contains(evidence.Detail(), "outside this root") {
		t.Fatalf("the evidence does not report the rejected document:\n%s", evidence.Detail())
	}
}

// F7: adoption retires the artifact its own case wrote. `grove sync conflicts`
// is artifact-backed, so a case with an explicit positive resolution must not
// keep being reported after the operator resolved it.
func TestRetireNotespaceConflictClearsTheAdoptionCase(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	path, err := WriteNotespaceConflict("01NS", ConflictKindAdoption, "evidence")
	if err != nil {
		t.Fatal(err)
	}
	if err := RetireNotespaceConflict("01NS", ConflictKindAdoption); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the adoption artifact survived its resolution: %v", err)
	}
	// Idempotent: a case resolved twice is not an error.
	if err := RetireNotespaceConflict("01NS", ConflictKindAdoption); err != nil {
		t.Fatalf("retiring an already-retired case failed: %v", err)
	}
	// And it never touches another kind's evidence.
	other, err := WriteNotespaceConflict("01NS", ConflictKindMissingRoot, "still true")
	if err != nil {
		t.Fatal(err)
	}
	if err := RetireNotespaceConflict("01NS", ConflictKindAdoption); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("retiring the adoption case removed the missing-root evidence: %v", err)
	}
}
