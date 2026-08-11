package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestFlowBurstInsertsAllReachSubscribers is the live-delta regression for
// ticket 20260723-tui-plans-large-portfolio-takes-over-110s-to-render: 15 plan
// directories created in a tight burst (mkdir, then config, then job file —
// the way the TUI pilot and `flow plan init` create them) must ALL surface as
// plan-index delta upserts to an attached SSE-equivalent subscriber within the
// 5-second acceptance bound. The burst interleaves scans with half-written
// plan dirs, which is exactly the window where the per-plansDir scan cache
// used to go permanently stale.
func TestFlowBurstInsertsAllReachSubscribers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

	notebookRoot := filepath.Join(root, "notebook")
	cfg := coldStartConfig(notebookRoot)

	workspaces := make(map[string]*models.EnrichedWorkspace)
	ws := coldStartWorkspace(t, root, "beta-repo")
	workspaces[ws.Path] = ws
	plansDir := filepath.Join(notebookRoot, "notespaces", "beta-repo", "plans")
	for i := 0; i < 24; i++ {
		writeIndexedPlan(t, filepath.Join(plansDir, fmt.Sprintf("beta-live-%02d", i)))
	}

	st := store.New()
	h := NewFlowHandler(st, cfg, 2000) // production debounce
	uw, err := NewUnifiedWatcher(st, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	uw.Register(h)
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uw.Start(ctx)
	for {
		update := <-ch
		if update.Type == store.UpdateWatcherStatus {
			if payload, ok := update.Payload.(map[string]interface{}); ok && payload["event"] == "started" {
				break
			}
		}
	}

	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "workspace", Payload: workspaces})

	// Wait for the initial populated snapshot (the daemon-live baseline).
	baseDeadline := time.Now().Add(10 * time.Second)
	for len(st.GetPlanIndexSnapshot().Plans) != 24 {
		if time.Now().After(baseDeadline) {
			t.Fatalf("initial snapshot never reached 24 plans: %d", len(st.GetPlanIndexSnapshot().Plans))
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Drain everything published so far; the burst assertions below only
	// consider deltas that arrive after the baseline.
	for {
		select {
		case <-ch:
			continue
		default:
		}
		break
	}

	// The burst: 15 plan dirs created back-to-back, pilot-identical shape.
	burstAt := time.Now()
	for i := 1; i <= 15; i++ {
		dir := filepath.Join(plansDir, fmt.Sprintf("burst-new-%02d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".grove-plan.yml"), []byte("status: active\nworktree: burst-new\nrepos:\n  - beta-repo\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "01.md"), []byte("---\nid: burst\nstatus: pending\ntype: file\n---\nburst\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Every burst plan must arrive as a delta upsert within the 5s bound, and
	// none may be spuriously removed afterward without a matching re-upsert.
	seen := make(map[string]bool)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(seen) < 15 {
		select {
		case update := <-ch:
			if update.Type != store.UpdatePlanIndexDelta {
				continue
			}
			delta := update.Payload.(*models.PlanIndexDelta)
			for _, up := range delta.Upserts {
				if up.PlansDir == plansDir && len(up.PlanName) > len("burst-new-") && up.PlanName[:len("burst-new-")] == "burst-new-" {
					seen[up.PlanName] = true
				}
			}
			for _, dir := range delta.Removed {
				name := filepath.Base(dir)
				if len(name) > len("burst-new-") && name[:len("burst-new-")] == "burst-new-" {
					delete(seen, name)
				}
			}
		case <-deadline.C:
			t.Fatalf("only %d/15 burst plans reached subscribers within 5s of insertion (elapsed %.2fs); snapshot has %d plans",
				len(seen), time.Since(burstAt).Seconds(), len(st.GetPlanIndexSnapshot().Plans))
		}
	}
	t.Logf("all 15 burst plans visible to subscribers %.0fms after insertion", time.Since(burstAt).Seconds()*1000)

	// The durable snapshot must agree with the deltas.
	if got := len(st.GetPlanIndexSnapshot().Plans); got != 39 {
		t.Fatalf("snapshot has %d plans after burst, want 39", got)
	}
}
