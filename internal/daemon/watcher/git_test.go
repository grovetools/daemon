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
