//go:build darwin

package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// This exercises the actual recursive backend: a nested working-tree write (not
// a .git/index event) must reach an unfocused repository in the global owner.
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
