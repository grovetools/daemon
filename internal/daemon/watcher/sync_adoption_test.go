package watcher

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/grovetools/core/config"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// The gate's verdict is this daemon's verdict: a contested notespace loses its
// pull loop, keeps its push loop, and carries its evidence structured.
func TestPullGateVerdictContestsTheNotespaceWithEvidence(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})
	lh.h.ensurePipelines()
	if state := lh.pipeline(idA); state == nil || !state.pull {
		t.Fatalf("pipeline = %+v, want a pulling pipeline before the contest", state)
	}

	// What the pull pipeline's pre-apply gate hands back when an incoming
	// batch would have written over un-synced local notes.
	evidence := syncdb.AdoptionEvidence{
		NotespaceID:   idA,
		Root:          root,
		Collisions:    []syncdb.AdoptionCollision{{Path: "notes/a.md", Identical: false}, {Path: "notes/b.md", Identical: true}},
		Identical:     1,
		Divergent:     1,
		Clean:         3,
		LocalSubject:  "github.com/me/core",
		ServerSubject: "github.com/me/core",
	}
	lh.h.markContestedFromPull(root, evidence)
	// The pulling pipeline is torn down and replaced by a push-only one; the
	// replacement waits for the old one to drain, so this takes more than the
	// one pass.
	waitForLifecycle(t, "the contested notespace to be left with a push-only pipeline", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && !state.pull
	})
	if state := lh.pipeline(idA); state == nil {
		t.Fatal("a contested notespace lost its push pipeline too; local work must still reach the server")
	} else if state.pull {
		t.Fatal("a contested notespace kept its pull pipeline; W3.5 admits no writes before adoption")
	}
	details := lh.h.ContestedDetails()
	if len(details) != 1 {
		t.Fatalf("ContestedDetails = %+v, want one entry", details)
	}
	entry := details[0]
	if entry.NotespaceID != idA || entry.Root != root {
		t.Fatalf("contested entry = %+v, want %s at %s", entry, idA, root)
	}
	if entry.Divergent != 1 || entry.Identical != 1 || entry.Colliding != 2 || entry.SubjectMatch != "match" {
		t.Fatalf("evidence = %+v, want 1 divergent / 1 identical / 2 colliding / subject match", entry)
	}
	if !strings.Contains(entry.Reason, "adoption pending") || !strings.Contains(entry.Detail, "hash overlap") {
		t.Fatalf("contested entry does not carry its evidence: %+v", entry)
	}
}

// Adoption writes the receipt BEFORE clearing, so a resumed pull loop cannot
// re-contest a decision the operator already made.
func TestAdoptContestedRecordsAReceiptAndRestoresPull(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})
	lh.h.markContestedFromPull(root, syncdb.AdoptionEvidence{NotespaceID: idA, Root: root, Divergent: 1, Collisions: []syncdb.AdoptionCollision{{Path: "notes/a.md"}}})
	lh.h.ensurePipelines()
	if state := lh.pipeline(idA); state == nil || state.pull {
		t.Fatalf("pipeline = %+v, want a contested (push-only) pipeline", state)
	}

	adopted, receipt, err := lh.h.AdoptContested(idA)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.NotespaceID != idA {
		t.Fatalf("adopted = %+v, want %s", adopted, idA)
	}
	if _, statErr := os.Stat(receipt); statErr != nil {
		t.Fatalf("adoption receipt %s is not on disk: %v", receipt, statErr)
	}
	if !syncdb.AdoptionRecorded(idA, root) {
		t.Fatal("the adoption receipt is not where the pull gate looks for it")
	}
	if len(lh.h.ContestedDetails()) != 0 {
		t.Fatalf("adoption left the verdict in place: %+v", lh.h.ContestedDetails())
	}

	// The next reconcile restores the pull loop — the drain from the contested
	// pipeline has to complete first, so the pass may take more than one turn.
	waitForLifecycle(t, "the adopted notespace to get its pull pipeline back", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && state.pull
	})
}

// Adoption names what it adopts. A daemon that picked the single contested
// notespace would be making the operator's decision for them.
func TestAdoptContestedRefusesAnUncontestedNotespace(t *testing.T) {
	lh := newLifecycleHarness(t)
	if _, _, err := lh.h.AdoptContested(idA); err == nil {
		t.Fatal("adopting a notespace that is not contested was accepted")
	}
	if _, _, err := lh.h.AdoptContested(""); err == nil {
		t.Fatal("adoption accepted an empty notespace id")
	}
}

// F7: the conflicts feed is artifact-backed, so the case the gate wrote has to
// be retired by the act that resolves it. An operator told "incoming writes
// resume" must not still see the notespace listed as contested.
func TestAdoptContestedRetiresTheConflictArtifact(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})

	artifact, err := syncdb.WriteNotespaceConflict(idA, syncdb.ConflictKindAdoption, "the gate's evidence")
	if err != nil {
		t.Fatal(err)
	}
	lh.h.markContestedFromPull(root, syncdb.AdoptionEvidence{
		NotespaceID: idA, Root: root, Divergent: 1,
		Collisions: []syncdb.AdoptionCollision{{Path: "notes/a.md"}},
	})

	if _, _, err := lh.h.AdoptContested(idA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("`grove sync conflicts` still reports the resolved adoption case: %v", err)
	}
}

// F8: the two ways adoption fails are different failures, and a script has to
// tell them apart. "Not contested" carries the sentinel; nothing else does.
func TestAdoptContestedMarksAnUncontestedNotespaceWithTheSentinel(t *testing.T) {
	lh := newLifecycleHarness(t)
	_, _, err := lh.h.AdoptContested(idA)
	if err == nil {
		t.Fatal("adopting a notespace that is not contested was accepted")
	}
	if !errors.Is(err, syncdb.ErrNotContested) {
		t.Fatalf("error %v does not carry ErrNotContested; the HTTP layer cannot tell it from a broken daemon", err)
	}
}
