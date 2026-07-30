//go:build darwin

package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// This exercises the actual recursive backend: a nested working-tree write (not
// a .git/index event) must reach an unfocused repository in the global owner.
func TestRecursiveGitEventPathsExcludeHomeRootAndExternalInputs(t *testing.T) {
	repo := gitInitRepo(t)
	node := &workspace.WorkspaceNode{Name: filepath.Base(repo), Path: repo, Kind: workspace.KindStandaloneProject}
	routes := buildGitEventRoutes(context.Background(), []*models.EnrichedWorkspace{{WorkspaceNode: node}})
	paths := recursiveGitEventPaths(routes)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if path == resolveEventPath(home) || path == string(filepath.Separator) {
			t.Fatalf("recursive FSEvents path set contains broad root %q: %v", path, paths)
		}
	}

	// Even after external inputs are learned, the actual recursive path builder
	// remains route-only. Those inputs are consumed by the exact polling observer.
	c := newDeadSubtreeCacheStopped()
	c.configFiles[resolveEventPath(filepath.Join(home, ".gitconfig"))] = true
	c.excludeFiles[resolveEventPath(filepath.Join(home, ".config", "git", "ignore"))] = true
	if paths := c.inputObservationPaths(); len(paths) == 0 {
		t.Fatal("exact-input observer did not receive external config inputs")
	}
	after := recursiveGitEventPaths(routes)
	if len(after) != len(paths) {
		t.Fatalf("external inputs changed recursive FSEvents roots: before=%v after=%v", paths, after)
	}
	for i := range paths {
		if after[i] != paths[i] {
			t.Fatalf("external inputs changed recursive FSEvents roots: before=%v after=%v", paths, after)
		}
	}
}

func TestExactInputObserverDetectsTargetAndAncestorTransitions(t *testing.T) {
	root := t.TempDir()
	target := resolveObservedInputPath(filepath.Join(root, "missing", "tree", "config"))
	observer, err := newExactInputObserver([]string{target}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()

	assertChanged := func(label string) {
		t.Helper()
		changed := observer.Poll()
		if len(changed) != 1 || changed[0] != target {
			t.Fatalf("%s: changed=%v, want [%s]", label, changed, target)
		}
	}
	if changed := observer.Poll(); len(changed) != 0 {
		t.Fatalf("stable missing target changed: %v", changed)
	}
	if err := os.Mkdir(filepath.Join(root, "missing"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertChanged("ancestor create")
	if err := os.Mkdir(filepath.Join(root, "missing", "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertChanged("nearest ancestor create")
	if err := os.WriteFile(target, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertChanged("target create")
	if err := os.WriteFile(target, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertChanged("target write")
	renamed := target + ".old"
	if err := os.Rename(target, renamed); err != nil {
		t.Fatal(err)
	}
	assertChanged("target rename away")
	if err := os.Rename(renamed, target); err != nil {
		t.Fatal(err)
	}
	assertChanged("target rename back")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	assertChanged("target remove")
}

func TestExactInputObserverDetectsSameSizeMtimePreservedRewrite(t *testing.T) {
	path := resolveObservedInputPath(filepath.Join(t.TempDir(), "gitconfig"))
	if err := os.WriteFile(path, []byte("aaaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observer, err := newExactInputObserver([]string{path}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Ensure the rewrite's change time cannot alias the construction snapshot,
	// then restore mtime so only Darwin's change time exposes the transition.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("bbbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, after) || info.Size() != after.Size() || info.ModTime() != after.ModTime() {
		t.Fatalf("test did not preserve identity/size/mtime: before=%v after=%v", info, after)
	}
	changed := observer.Poll()
	if len(changed) != 1 || changed[0] != path {
		t.Fatalf("same-size mtime-preserved rewrite changed=%v, want [%s]", changed, path)
	}
}

func TestExactInputObserverResourcesIgnoreUnrelatedSiblings(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "gitconfig")
	for i := 0; i < 500; i++ {
		name := filepath.Join(root, fmt.Sprintf("unrelated-%04d", i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	countFDs := func() int {
		entries, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Skipf("cannot inspect process descriptors: %v", err)
		}
		return len(entries)
	}
	before := countFDs()
	observer, err := newExactInputObserver([]string{target}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	after := countFDs()
	if delta := after - before; delta > 1 {
		t.Fatalf("one exact input with 500 unrelated siblings grew descriptors by %d (before=%d after=%d)", delta, before, after)
	}
	if got := len(observer.states); got != 1 {
		t.Fatalf("observer state grew with unrelated siblings: got %d entries, want 1", got)
	}
	if err := os.WriteFile(filepath.Join(root, "another-unrelated"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if changed := observer.Poll(); len(changed) != 0 {
		t.Fatalf("unrelated sibling churn changed exact inputs: %v", changed)
	}
}

func TestGlobalGitEventsObservesNestedWorkingTreeWrite(t *testing.T) {
	repo := gitInitRepo(t)
	st := store.New()
	seedWorkspace(t, st, repo)
	st.SetFocus("test", nil) // broad coverage, not focus, must own freshness
	clean, err := git.GetExtendedStatus(repo)
	if err != nil {
		t.Fatal(err)
	}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspacesDelta, Source: "test", Payload: []*models.WorkspaceDelta{{Path: repo, GitStatus: clean}}})

	h := NewGitHandler(st, 30).SetBroadCoverage(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunGlobalGitEvents(ctx, st, h)
	// Let CoreFoundation install the stream before producing the event.
	time.Sleep(300 * time.Millisecond)

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)
	nested := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "new.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	if delta := waitForWatcherDelta(sub, repo, 4*time.Second); delta == nil || delta.GitStatus == nil || !delta.GitStatus.IsDirty {
		t.Fatalf("recursive working-tree event did not produce dirty delta: %+v", delta)
	}
}
