package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/workspace"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// A notebook root MOVE is not a duplicate stamp, and the P3 gate caught the
// daemon saying it was.
//
// The shape of the bug: the durable binding named the old root, the sibling
// sweep's cached verdict still named it too, and a rate-limited pass replayed
// that cache verbatim. firstSeenRoot then voted for a path that no longer
// existed — cached string against cached string, no disk in the loop — so the
// relocated notespace lost to its own former self, stopped syncing, and left a
// duplicate_stamp artifact on the conflicts feed advising the operator to
// "re-mint one of the two" when only one of them was there to re-mint.
//
// Everything here is set to the cadence that produced it: the sweep an hour
// away, so the reconcile after the config edit is a replayed pass.
func TestARootMoveRebindsRatherThanContestingTheVacatedPath(t *testing.T) {
	lh := newLifecycleHarness(t)
	vacated := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": vacated})
	lh.h.ensurePipelines()
	if state := lh.pipeline(idA); state == nil || state.root != vacated {
		t.Fatalf("precondition: want a pipeline at %q, got %+v", vacated, state)
	}
	// What a machine that has been syncing this notespace holds: a durable
	// binding to the root it has been syncing, and a swept sibling list that
	// names that same root.
	if err := lh.db.UpsertNotespaceBinding(syncdb.NotespaceBinding{
		ID: idA, Name: "alpha", Root: vacated,
		Subject: "local:01ARZ3NDEKTSV4RRFFQ69G5FAW", Kind: "notes",
	}); err != nil {
		t.Fatal(err)
	}
	if cached := lh.h.duplicateSiblingsCache[idA]; len(cached) != 1 || cached[0] != vacated {
		t.Fatalf("precondition: want the sweep to have cached %q, got %v", vacated, cached)
	}
	lh.h.duplicateScanInterval = time.Hour

	// The operator moves the notebook and edits notebooks.toml to match.
	relocated := filepath.Join(filepath.Dir(lh.notebookRoot), "relocated")
	if err := os.Rename(lh.notebookRoot, relocated); err != nil {
		t.Fatal(err)
	}
	lh.notebookRoot = relocated
	lh.h.setConfig(notebookConfig(relocated))
	lh.h.bumpConfigGeneration()
	live := filepath.Join(relocated, workspace.NotespaceDirectory, "alpha")
	lh.watch(map[string]string{"alpha": live})
	lh.h.ensurePipelines()

	if parked := lh.h.ParkedNotespaces(); len(parked) != 0 {
		t.Fatalf("the relocated root was parked against the path it just left: %+v", parked)
	}
	waitForLifecycle(t, "the pipeline to follow the moved root", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && state.root == live
	})
	matches, err := filepath.Glob(stateConflictsGlob(idA))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("a root move left duplicate-stamp evidence naming a path that no longer exists: %v", matches)
	}
}

// The pruning is by STAMP, not by existence, so the other way a cached sibling
// stops claiming an id — `grove doctor --fix --remint` rewriting the copy's
// stamp — clears in the same pass instead of replaying as a duplicate for the
// rest of the scan interval.
func TestARemintedCopyStopsBeingReplayedAsADuplicate(t *testing.T) {
	lh := newLifecycleHarness(t)
	keeper := lh.notespace(t, "alpha", idA)
	copyRoot := lh.notespace(t, "alpha-copy", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": keeper})
	lh.h.ensurePipelines()
	if parked := lh.h.ParkedNotespaces(); len(parked) != 1 || parked[0].Root != copyRoot {
		t.Fatalf("precondition: want the copy parked, got %+v", parked)
	}
	lh.h.duplicateScanInterval = time.Hour

	// The repair: same directory, new identity. The sweep will not run again
	// for an hour, so only the replay path can see this.
	if err := os.Remove(filepath.Join(copyRoot, ".notespace.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := notespacepkg.InstallNotespace(copyRoot, notespacepkg.NotespaceStamp{
		ID: idBeta, Name: "alpha-copy", Subject: "local:01ARZ3NDEKTSV4RRFFQ69G5FAW", Kind: "notes",
	}); err != nil {
		t.Fatal(err)
	}
	lh.h.ensurePipelines()

	if parked := lh.h.ParkedNotespaces(); len(parked) != 0 {
		t.Fatalf("a re-minted copy was replayed as a duplicate: %+v", parked)
	}
	if state := lh.pipeline(idA); state == nil || state.root != keeper {
		t.Fatalf("the keeper stopped syncing after the repair: %+v", state)
	}
}
