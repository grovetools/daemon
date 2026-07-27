package watcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// gitInitRepo creates a git repo with one commit at dir and returns the
// symlink-resolved absolute path (so it matches git's own --absolute-git-dir).
func gitInitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "initial")
	return dir
}

// seedWorkspace registers a single focused workspace in the store.
func seedWorkspace(t *testing.T, st *store.Store, path string) {
	t.Helper()
	node := &workspace.WorkspaceNode{Name: filepath.Base(path), Path: path, Kind: workspace.KindStandaloneProject}
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkspaces,
		Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{
			path: {WorkspaceNode: node},
		},
	})
	st.SetFocus("test", []string{path})
}

func TestGitHandlerComputeWatchPaths(t *testing.T) {
	repo := gitInitRepo(t)
	st := store.New()
	seedWorkspace(t, st, repo)

	h := NewGitHandler(st, 150)
	paths := h.ComputeWatchPaths(st.GetWorkspaces())

	want := map[string]bool{
		filepath.Join(repo, ".git", "HEAD"):            true,
		filepath.Join(repo, ".git", "index"):           true,
		filepath.Join(repo, ".git", "refs", "heads"):   true,
		filepath.Join(repo, ".git", "refs", "remotes"): false, // may not exist without a remote
	}
	got := make(map[string]bool)
	for _, p := range paths {
		got[p] = true
	}
	// HEAD, index, and refs/heads always exist after a commit.
	for p, required := range want {
		if required && !got[p] {
			t.Errorf("expected watch path %q to be present, got %v", p, paths)
		}
	}
}

func TestGitHandlerBroadCoverageKeepsFallbackWatchesFocused(t *testing.T) {
	repo := gitInitRepo(t)
	st := store.New()
	node := &workspace.WorkspaceNode{Name: filepath.Base(repo), Path: repo, Kind: workspace.KindStandaloneProject}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: map[string]*models.EnrichedWorkspace{repo: {WorkspaceNode: node}}})

	h := NewGitHandler(st, 150).SetBroadCoverage(true)
	if paths := h.ComputeWatchPaths(st.GetWorkspaces()); len(paths) != 0 {
		t.Fatalf("global handler created %d per-repo fallback watches for an unfocused repository", len(paths))
	}
}

func TestGitEventRoutingPrefersNestedRepository(t *testing.T) {
	outer := &workspace.WorkspaceNode{Path: "/code/ecosystem"}
	inner := &workspace.WorkspaceNode{Path: "/code/ecosystem/repo"}
	routes := []gitEventRoute{
		{root: inner.Path, nodes: []*workspace.WorkspaceNode{inner}},
		{root: outer.Path, nodes: []*workspace.WorkspaceNode{outer}},
	}
	got := routeGitEvent("/code/ecosystem/repo/pkg/file.go", routes)
	if len(got) != 1 || got[0] != inner {
		t.Fatalf("nested event routed to %+v, want inner repository", got)
	}
	if got := routeGitEvent("/code/ecosystem-other/file", routes); len(got) != 0 {
		t.Fatalf("path-boundary mismatch routed to %+v", got)
	}
}

func TestGitHandlerComputeWatchPathsSkipsUnfocused(t *testing.T) {
	repo := gitInitRepo(t)
	st := store.New()
	node := &workspace.WorkspaceNode{Name: filepath.Base(repo), Path: repo, Kind: workspace.KindStandaloneProject}
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateWorkspaces,
		Source:  "test",
		Payload: map[string]*models.EnrichedWorkspace{repo: {WorkspaceNode: node}},
	})
	// No focus set.

	h := NewGitHandler(st, 150)
	if paths := h.ComputeWatchPaths(st.GetWorkspaces()); len(paths) != 0 {
		t.Fatalf("expected no watch paths for unfocused workspace, got %v", paths)
	}
}

// TestGitHandlerEventEmitsDelta proves the core behavior: a filesystem event on
// a watched git-internal path produces a workspaces_delta SSE update sourced
// from git_watcher once the repo's status actually changes.
func TestGitHandlerEventEmitsDelta(t *testing.T) {
	repo := gitInitRepo(t)
	st := store.New()
	seedWorkspace(t, st, repo)

	// Prime the store's stored status to match the current repo so the first
	// commit produces a real (HEAD-changing) diff the handler will emit.
	initStatus, err := git.GetExtendedStatus(repo)
	if err != nil {
		t.Fatalf("initial status: %v", err)
	}
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateWorkspacesDelta,
		Source:  "test",
		Payload: []*models.WorkspaceDelta{{Path: repo, GitStatus: initStatus}},
	})

	h := NewGitHandler(st, 20) // short debounce for test speed
	_ = h.ComputeWatchPaths(st.GetWorkspaces())

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	// Stage a new file so the index changes and GetExtendedStatus reports a
	// real status delta (staged/dirty counts) that won't be suppressed by the
	// no-op GitStatusEqual gate. This also touches the watched index path.
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command("git", "add", "b.txt")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	// Drive an event on the watched index path, as the UnifiedWatcher would.
	indexPath := filepath.Join(repo, ".git", "index")
	if err := h.HandleEvents(context.Background(), []fsnotify.Event{{Name: indexPath, Op: fsnotify.Write}}); err != nil {
		t.Fatalf("HandleEvents: %v", err)
	}

	// Expect a workspaces_delta from git_watcher within the debounce window.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case update := <-sub:
			if update.Type != store.UpdateWorkspacesDelta {
				continue
			}
			if update.Source != "git_watcher" {
				continue
			}
			deltas, ok := update.Payload.([]*models.WorkspaceDelta)
			if !ok || len(deltas) != 1 || deltas[0].Path != repo || deltas[0].GitStatus == nil {
				t.Fatalf("unexpected delta payload: %+v", update.Payload)
			}
			return // success
		case <-deadline:
			t.Fatal("no git_watcher workspaces_delta broadcast after HEAD event")
		}
	}
}

// TestGitHandlerNewWorktreeImmediateScan proves Phase 3: a newly discovered,
// focused workspace gets an immediate scan via HandleStoreUpdate without waiting
// for the debounce timer.
func TestGitHandlerNewWorktreeImmediateScan(t *testing.T) {
	repo := gitInitRepo(t)
	st := store.New()

	h := NewGitHandler(st, 150)
	// Establish an empty known set.
	_ = h.ComputeWatchPaths(st.GetWorkspaces())

	// Focus the (not-yet-known) new workspace path before discovery arrives.
	st.SetFocus("test", []string{repo})

	// Seed the store with the new workspace so Get() can find it during scan.
	node := &workspace.WorkspaceNode{Name: filepath.Base(repo), Path: repo, Kind: workspace.KindStandaloneProject}
	wsMap := map[string]*models.EnrichedWorkspace{repo: {WorkspaceNode: node}}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: wsMap})

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	// Simulate the watcher dispatching the workspace discovery to the handler.
	h.HandleStoreUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: wsMap})

	deadline := time.After(2 * time.Second)
	for {
		select {
		case update := <-sub:
			if update.Type == store.UpdateWorkspacesDelta && update.Source == "git_watcher" {
				return // success: immediate scan emitted
			}
		case <-deadline:
			t.Fatal("no immediate git_watcher delta for new focused workspace")
		}
	}
}

// primeStore records the repo's CURRENT coarse status and per-file data in the
// store, exactly as a collector scan would, so a following watcher scan starts
// from a fully-backfilled snapshot.
func primeStore(t *testing.T, st *store.Store, repo string) (*git.ExtendedGitStatus, map[string]string) {
	t.Helper()
	status, err := git.GetExtendedStatus(repo)
	if err != nil {
		t.Fatalf("initial status: %v", err)
	}
	files, hashes := focusedFileData(repo)
	computed := true
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkspacesDelta,
		Source: "test",
		Payload: []*models.WorkspaceDelta{{
			Path:                 repo,
			GitStatus:            status,
			ChangedFiles:         files,
			BlobHashes:           hashes,
			ChangedFilesComputed: &computed,
		}},
	})
	return status, hashes
}

// waitForWatcherDelta returns the first git_watcher workspaces_delta for repo,
// or nil if none arrives before the deadline.
func waitForWatcherDelta(sub chan store.Update, repo string, within time.Duration) *models.WorkspaceDelta {
	deadline := time.After(within)
	for {
		select {
		case update := <-sub:
			if update.Type != store.UpdateWorkspacesDelta || update.Source != "git_watcher" {
				continue
			}
			deltas, ok := update.Payload.([]*models.WorkspaceDelta)
			if !ok {
				continue
			}
			for _, d := range deltas {
				if d.Path == repo {
					return d
				}
			}
		case <-deadline:
			return nil
		}
	}
}

// TestGitHandlerEmitsDeltaForContentOnlyChange is the regression guard for the
// per-file suppression fix (291d3fb): a content-only edit — here rewriting an
// UNTRACKED file, whose numstat is always 0 and whose presence keeps every
// coarse count identical — must still recompute per-file data and emit a delta
// from the watcher path. Gating the changed-file/blob pass on GitStatusEqual
// makes such edits invisible, leaving git-viewer's review state keyed on stale
// blob hashes (an edited file keeps rendering as reviewed).
func TestGitHandlerEmitsDeltaForContentOnlyChange(t *testing.T) {
	repo := gitInitRepo(t)
	untracked := filepath.Join(repo, "u.txt")
	if err := os.WriteFile(untracked, []byte("before"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	st := store.New()
	seedWorkspace(t, st, repo)
	before, beforeHashes := primeStore(t, st, repo)
	if beforeHashes["u.txt"] == "" {
		t.Fatalf("expected a primed blob hash for the untracked file, got %v", beforeHashes)
	}

	// Same length, same file count, same (zero) numstat: nothing GitStatusEqual
	// looks at moves, only the bytes on disk.
	if err := os.WriteFile(untracked, []byte("after!"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	after, err := git.GetExtendedStatus(repo)
	if err != nil {
		t.Fatalf("status after edit: %v", err)
	}
	if !store.GitStatusEqual(before, after) {
		t.Fatalf("test precondition broken: coarse status moved (%+v vs %+v)", before.StatusInfo, after.StatusInfo)
	}

	h := NewGitHandler(st, 20)
	_ = h.ComputeWatchPaths(st.GetWorkspaces())

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	indexPath := filepath.Join(repo, ".git", "index")
	if err := h.HandleEvents(context.Background(), []fsnotify.Event{{Name: indexPath, Op: fsnotify.Write}}); err != nil {
		t.Fatalf("HandleEvents: %v", err)
	}

	delta := waitForWatcherDelta(sub, repo, 2*time.Second)
	if delta == nil {
		t.Fatal("no git_watcher delta for a content-only change (coarse-status gate suppressed it)")
	}
	if delta.ChangedFilesComputed == nil || !*delta.ChangedFilesComputed {
		t.Fatalf("expected per-file data to be computed, got %+v", delta.ChangedFilesComputed)
	}
	found := false
	for _, f := range delta.ChangedFiles {
		if f.Path == "u.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected u.txt in ChangedFiles, got %+v", delta.ChangedFiles)
	}
	if got := delta.BlobHashes["u.txt"]; got == "" || got == beforeHashes["u.txt"] {
		t.Fatalf("expected a fresh blob hash for u.txt, got %q (was %q)", got, beforeHashes["u.txt"])
	}
}

// TestGitHandlerSuppressesTrueNoOp proves the suppression that remains is the
// safe one: an event that moved neither the coarse status nor the per-file
// snapshot emits nothing, so noisy fs churn still can't storm subscribers.
func TestGitHandlerSuppressesTrueNoOp(t *testing.T) {
	repo := gitInitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "u.txt"), []byte("stable"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	st := store.New()
	seedWorkspace(t, st, repo)
	primeStore(t, st, repo)

	h := NewGitHandler(st, 20)
	_ = h.ComputeWatchPaths(st.GetWorkspaces())

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	indexPath := filepath.Join(repo, ".git", "index")
	if err := h.HandleEvents(context.Background(), []fsnotify.Event{{Name: indexPath, Op: fsnotify.Write}}); err != nil {
		t.Fatalf("HandleEvents: %v", err)
	}

	if delta := waitForWatcherDelta(sub, repo, 500*time.Millisecond); delta != nil {
		t.Fatalf("expected no delta for an unchanged repo, got %+v", delta)
	}
}
