package watcher

import (
	"os/exec"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// Every git delta must carry landing state, and a landing-only move must not be
// suppressed. Pushing a branch updates refs/remotes/origin/<branch> and nothing
// GitStatusEqual compares — so a consumer keying its landing verdict on the
// coarse status alone would keep rendering "not pushed" forever.
func TestGitWatcherEmitsLandingStateOnRemoteRefMove(t *testing.T) {
	repo := gitInitRepo(t)
	st := store.New()
	seedWorkspace(t, st, repo)
	// primeStore records the current status AND landing state, so only a genuine
	// change can emit below.
	primeStore(t, st, repo)

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	// Publish the branch: a new origin ref, an untouched working tree.
	origin := t.TempDir()
	gitRunIn(t, origin, "init", "--bare")
	gitRunIn(t, repo, "remote", "add", "origin", origin)
	gitRunIn(t, repo, "push", "origin", "HEAD")
	gitRunIn(t, repo, "fetch", "origin")

	node := &workspace.WorkspaceNode{Path: repo}
	h := NewGitHandler(st, 1)
	h.scanAndEmit(node)

	delta := waitForWatcherDelta(sub, repo, 2*time.Second)
	if delta == nil {
		t.Fatal("pushing the branch emitted no delta: landing state moved with the coarse status standing still")
	}
	if delta.GitLanding == nil || !delta.GitLanding.Computed {
		t.Fatalf("delta carried no computed landing state: %+v", delta.GitLanding)
	}
	if !delta.GitLanding.HasRemote {
		t.Fatalf("landing state = %+v, want the pushed branch's origin ref", delta.GitLanding)
	}
	if got, ok := st.Get().Workspaces[repo]; !ok || got.GitLanding == nil || !got.GitLanding.HasRemote {
		t.Fatal("the store did not fold the landing state into the workspace snapshot")
	}
}

func gitRunIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
