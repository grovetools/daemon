package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/syncproto"
)

// The identity stamp, on both sides of the seam it fell through.
//
// The regression these pin is the notebook lab's finding 7, found by probe 11:
// two machines holding one notespace hold two stamps for it, written by
// different verbs, so their bytes differ routinely while naming the same id.
// The adoption gate counted that difference as un-synced local notes and
// contested the notespace — no writes in, no writes out — over a file the
// operator never wrote. A notespace that machine B had just pulled therefore
// went dark on B, and nothing but `grove sync adopt-notespace` released it.
//
// The two halves are one rule: a stamp is identity, not notes. It does not
// weigh on the adoption verdict, and it never overwrites a local identity.

const localStamp = "id = \"01ARZ3NDEKTSV4RRFFQ69G5FAV\"\nname = \"widget\"\nsubject = \"github.com/me/widget\"\nkind = \"notes\"\n"

func TestAStampDoesNotContestTheNotespaceItIdentifies(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, notespacepkg.NotespaceStampName, localStamp)

	// The server's copy of the same stamp, spelled differently — the same id,
	// written by a different verb.
	incoming := []IncomingDocument{
		{Path: notespacepkg.NotespaceStampName, Hash: hashContent([]byte("name = \"widget\"\nid = \"01ARZ3NDEKTSV4RRFFQ69G5FAV\"\n"))},
		{Path: "inbox/decision.md", Hash: hashContent([]byte("a note nothing local collides with"))},
	}
	evidence := detect(t, "01NS", root, incoming, untracked, "github.com/me/widget", "github.com/me/widget")

	if evidence.Contested() {
		t.Fatalf("a freshly-pulled notespace was contested over its own stamp: %s", evidence.Detail())
	}
	if evidence.IdentityStamps != 1 {
		t.Fatalf("identity stamps counted = %d, want 1 — the evidence must account for it, not drop it", evidence.IdentityStamps)
	}
	if len(evidence.Collisions) != 0 {
		t.Fatalf("collisions = %+v, want none", evidence.Collisions)
	}
}

// The carve-out is exactly the stamp. A notespace whose real notes collide is
// contested as it always was, and the stamp beside them changes nothing.
func TestAStampDoesNotExcuseCollidingNotes(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, notespacepkg.NotespaceStampName, localStamp)
	writeLocal(t, root, "inbox/mine.md", "un-synced local work")

	incoming := []IncomingDocument{
		{Path: notespacepkg.NotespaceStampName, Hash: hashContent([]byte("a differently spelled stamp"))},
		{Path: "inbox/mine.md", Hash: hashContent([]byte("the server's version"))},
	}
	evidence := detect(t, "01NS", root, incoming, untracked, "github.com/me/widget", "github.com/me/widget")

	if !evidence.Contested() || evidence.Divergent != 1 {
		t.Fatalf("colliding notes stopped contesting the notespace: %+v", evidence)
	}
	for _, collision := range evidence.Collisions {
		if notespacepkg.IsIdentityStamp(collision.Path) {
			t.Fatalf("the stamp was weighed as a collision: %+v", collision)
		}
	}
	if !strings.Contains(evidence.Detail(), "identity stamp") {
		t.Fatalf("the evidence does not say what became of the stamp:\n%s", evidence.Detail())
	}
}

// The other half: with the stamp out of the verdict, the batch reaches the
// apply path — which must not let a remote file decide what this root IS. A
// CREATE is the shape a stamp arrives in, because the daemon has never
// recorded as a document the stamp `grove notebook pull` wrote.
func TestAnIncomingStampNeverOverwritesALocalIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	writeLocal(t, root, notespacepkg.NotespaceStampName, localStamp)
	pipeline := NewPullPipeline(&config.SyncWorkspace{Name: "01NS"}, nil, db, logging.NewUnifiedLogger("test.identity"))
	var conflicts []string
	pipeline.OnConflict = func(kind, _, _, _, _ string) { conflicts = append(conflicts, kind) }

	remote := []byte("id = \"01ARZ3NDEKTSV4RRFFQ69G5FAW\"\nname = \"someone-elses\"\n")
	err = pipeline.applyCreate(context.Background(), root, &syncproto.SyncEvent{
		Type:        syncproto.EventDocumentCreated,
		Path:        notespacepkg.NotespaceStampName,
		DocumentID:  "doc-1",
		ContentHash: hashContent(remote),
		Content:     remote,
		Version:     1,
	})
	if err != nil {
		t.Fatalf("applyCreate returned an error rather than recording a conflict: %v", err)
	}
	onDisk, readErr := os.ReadFile(filepath.Join(root, notespacepkg.NotespaceStampName))
	if readErr != nil || string(onDisk) != localStamp {
		t.Fatalf("the local identity was overwritten by the server's: %q, %v", onDisk, readErr)
	}
	if len(conflicts) != 1 || conflicts[0] != ConflictKindRegistration {
		t.Fatalf("conflict kinds = %v, want one %q", conflicts, ConflictKindRegistration)
	}
}

// A replica with no stamp yet is the one case that does write: that is how a
// pulled notespace materializes the identity it was sent, and refusing it
// would leave the root unidentifiable.
func TestAnIncomingStampMaterializesOnARootThatHasNone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	pipeline := NewPullPipeline(&config.SyncWorkspace{Name: "01NS"}, nil, db, logging.NewUnifiedLogger("test.identity"))
	var conflicts []string
	pipeline.OnConflict = func(kind, _, _, _, _ string) { conflicts = append(conflicts, kind) }

	remote := []byte(localStamp)
	if err := pipeline.applyCreate(context.Background(), root, &syncproto.SyncEvent{
		Type:        syncproto.EventDocumentCreated,
		Path:        notespacepkg.NotespaceStampName,
		DocumentID:  "doc-1",
		ContentHash: hashContent(remote),
		Content:     remote,
		Version:     1,
	}); err != nil {
		t.Fatalf("applyCreate: %v", err)
	}
	onDisk, readErr := os.ReadFile(filepath.Join(root, notespacepkg.NotespaceStampName))
	if readErr != nil || string(onDisk) != localStamp {
		t.Fatalf("the stamp was not materialized: %q, %v", onDisk, readErr)
	}
	if len(conflicts) != 0 {
		t.Fatalf("materializing a stamp onto a bare root reported %v", conflicts)
	}
}
