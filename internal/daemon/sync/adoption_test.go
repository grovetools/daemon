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

func untracked(string) bool { return false }

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
	evidence := DetectAdoption("01NS", root, incoming, untracked, "github.com/me/core", "github.com/me/core")

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

	evidence := DetectAdoption("01NS", root, []IncomingDocument{
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

	tracked := func(path string) bool { return path == "notes/tracked.md" }
	evidence := DetectAdoption("01NS", root, []IncomingDocument{
		{Path: "notes/tracked.md", Hash: hashContent([]byte("server edit"))},
	}, tracked, "", "")
	if evidence.Contested() || len(evidence.Collisions) != 0 {
		t.Fatalf("a tracked path was treated as an adoption case: %+v", evidence)
	}
}

func TestSubjectMismatchIsReportedDistinctlyFromUnknown(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "a.md", "mine")
	evidence := DetectAdoption("01NS", root, []IncomingDocument{
		{Path: "a.md", Hash: hashContent([]byte("theirs"))},
	}, untracked, "github.com/me/core", "github.com/me/notes")
	if evidence.SubjectMatch() != "mismatch" {
		t.Fatalf("SubjectMatch = %q, want mismatch", evidence.SubjectMatch())
	}
	if !strings.Contains(evidence.Detail(), "subject MISMATCH") {
		t.Fatalf("mismatch was not called out in the evidence:\n%s", evidence.Detail())
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
	if !AdoptionRecorded("01NS") {
		t.Fatal("AdoptionRecorded did not see the receipt it had just written")
	}
	incoming := []IncomingDocument{{Path: "notes/a.md", Hash: hashContent([]byte("the server's version"))}}
	if err := pipeline.guardAdoption(context.Background(), root, incoming); err != nil {
		t.Fatalf("the gate withheld an adopted notespace: %v", err)
	}

	if err := ForgetAdoption("01NS"); err != nil {
		t.Fatal(err)
	}
	if AdoptionRecorded("01NS") {
		t.Fatal("ForgetAdoption left the receipt in place")
	}
}
