package collector

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// gitRepo creates a repo with one commit and returns its symlink-resolved path.
func gitRepo(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
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

// applyUpdates drains the collector's update channel into the store, the way
// the engine does.
func applyUpdates(ctx context.Context, st *store.Store, updates <-chan store.Update) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case u := <-updates:
				st.ApplyUpdate(u)
			}
		}
	}()
}

// The boot sweep's user-visible contract, end to end against real repos: the
// focused workspace is swept in the first batch and stops being pending
// immediately, everything else is marked pending up front and converges on the
// trickle, and the whole thing is narrated by sweep_* events.
func TestBootSweepPublishesTiersPendingAndEvents(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	hot := gitRepo(t, root, "hot")
	cold1 := gitRepo(t, root, "cold1")
	cold2 := gitRepo(t, root, "cold2")

	st := store.New()
	nodes := map[string]*models.EnrichedWorkspace{}
	for _, p := range []string{hot, cold1, cold2} {
		nodes[p] = &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{
			Name: filepath.Base(p), Path: p, Kind: workspace.KindStandaloneProject,
		}}
	}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: nodes})
	st.SetFocus("test", []string{hot})

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	c := NewGitStatusCollector(50*time.Millisecond, "")
	// Keep the shape (hot unpaced, cold trickled one at a time) but shrink the
	// trickle so the test measures ordering, not patience.
	c.pacing.coldBatch = 1
	c.pacing.duty[tierCold] = 0.5
	c.pacing.maxPause = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	updates := make(chan store.Update, 100)
	applyUpdates(ctx, st, updates)
	go func() { _ = c.Run(ctx, st, updates) }()

	// Assertions read the published DELTAS rather than the store's workspace
	// pointers: the store hands out live pointers that the applier goroutine
	// mutates in place, so reading them here would be a race in the test
	// harness rather than a fact about the sweep.
	var started, progress, completed int
	var hotClearedWhileOthersPending bool
	pending := map[string]bool{}
	scanned := map[string]bool{}
	deadline := time.After(25 * time.Second)
	for completed == 0 {
		select {
		case <-deadline:
			t.Fatalf("sweep did not complete (started=%d progress=%d)", started, progress)
		case u := <-sub:
			switch u.Type {
			case store.UpdateSweepStarted:
				started++
				p, ok := u.Payload.(*models.GitSweepProgress)
				if !ok || p.Total != 3 {
					t.Fatalf("sweep_started payload = %+v", u.Payload)
				}
				if p.Reason != "boot" {
					t.Errorf("first sweep reason = %q, want boot", p.Reason)
				}
				if p.TierTotals["hot"] != 1 {
					t.Errorf("plan = %v, want the focused workspace in the hot tier", p.TierTotals)
				}
			case store.UpdateSweepProgress:
				progress++
			case store.UpdateSweepCompleted:
				completed++
			case store.UpdateWorkspacesDelta:
				deltas, ok := u.Payload.([]*models.WorkspaceDelta)
				if !ok {
					t.Fatalf("workspaces_delta payload = %T", u.Payload)
				}
				for _, d := range deltas {
					if d.GitStatusPending != nil {
						pending[d.Path] = *d.GitStatusPending
					}
					if d.GitStatus != nil {
						scanned[d.Path] = true
					}
				}
				if scanned[hot] && !pending[hot] && (pending[cold1] || pending[cold2]) {
					hotClearedWhileOthersPending = true
				}
			}
		}
	}

	if started != 1 {
		t.Errorf("sweep_started count = %d, want exactly one boot sweep", started)
	}
	if progress < 2 {
		t.Errorf("progress events = %d, want one per batch (hot + trickle)", progress)
	}
	if !hotClearedWhileOthersPending {
		t.Error("the focused workspace's data did not land before the cold tail's")
	}
	for _, path := range []string{hot, cold1, cold2} {
		if !scanned[path] {
			t.Errorf("%s never got git status", path)
		}
		if pending[path] {
			t.Errorf("%s still marked pending after the sweep completed", path)
		}
	}
}

// A repo added after boot (`grove repo add`, an import, a new worktree) must
// not land at the back of a minutes-long trickle: once a first sweep has
// completed, "never swept" means "just appeared", which is demand.
func TestWorkspaceDiscoveredAfterBootIsSweptOnRefresh(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	existing := []string{gitRepo(t, root, "a"), gitRepo(t, root, "b")}

	st := store.New()
	nodes := map[string]*models.EnrichedWorkspace{}
	for _, p := range existing {
		nodes[p] = &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{
			Name: filepath.Base(p), Path: p, Kind: workspace.KindStandaloneProject,
		}}
	}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: nodes})

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	c := NewGitStatusCollector(time.Second, "")
	c.pacing.coldBatch = 1
	c.pacing.duty[tierCold] = 0.001
	c.pacing.maxPause = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	updates := make(chan store.Update, 100)
	applyUpdates(ctx, st, updates)
	go func() { _ = c.Run(ctx, st, updates) }()

	// Wait for the boot sweep to finish before introducing the new workspace.
	waitDone := time.After(45 * time.Second)
	for done := false; !done; {
		select {
		case <-waitDone:
			t.Fatal("boot sweep never completed")
		case u := <-sub:
			if u.Type == store.UpdateSweepCompleted {
				done = true
			}
		}
	}

	added := gitRepo(t, root, "added-later")
	nodes[added] = &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{
		Name: filepath.Base(added), Path: added, Kind: workspace.KindStandaloneProject,
	}}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: nodes})

	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// The assertion is structural rather than timed: the new workspace's data
	// must be published BEFORE the refresh sweep drops into a paced tier. If
	// the promotion were missing, it would be cold — swept some pauses later,
	// which is precisely the wait this rule exists to avoid.
	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("the newly discovered workspace was never swept")
		case u := <-sub:
			switch u.Type {
			case store.UpdateWorkspacesDelta:
				deltas, ok := u.Payload.([]*models.WorkspaceDelta)
				if !ok {
					continue
				}
				for _, d := range deltas {
					if d.Path == added && d.GitStatus != nil {
						return
					}
				}
			case store.UpdateSweepProgress:
				p, ok := u.Payload.(*models.GitSweepProgress)
				if ok && (p.Tier == tierCold.String() || p.Tier == tierWarm.String()) {
					t.Fatalf("sweep reached the %s tier before covering the newly discovered workspace", p.Tier)
				}
			}
		}
	}
}

// Refresh must not block for the cold tail, and it must not leave the caller
// hanging when there is nothing to sweep.
func TestRefreshReturnsWithoutWaitingForTheTail(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	repos := []string{gitRepo(t, root, "a"), gitRepo(t, root, "b"), gitRepo(t, root, "c")}

	st := store.New()
	nodes := map[string]*models.EnrichedWorkspace{}
	for _, p := range repos {
		nodes[p] = &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{
			Name: filepath.Base(p), Path: p, Kind: workspace.KindStandaloneProject,
		}}
	}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: nodes})

	c := NewGitStatusCollector(time.Second, "")
	c.pacing.coldBatch = 1
	c.pacing.duty[tierCold] = 0.001 // the tail is deliberately glacial here
	c.pacing.maxPause = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	updates := make(chan store.Update, 100)
	applyUpdates(ctx, st, updates)
	go func() { _ = c.Run(ctx, st, updates) }()

	start := time.Now()
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Three repos, one per batch, each inter-batch pause pinned at the 2s cap:
	// waiting out the tail takes ~4s. The demanded tiers are what Refresh
	// waits for, and they are done as soon as the first paced batch starts.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Refresh took %s — it waited for the paced tail", elapsed)
	}
}
