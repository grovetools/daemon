package collector

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestSweepAtScale is the on-demand rig behind this design's acceptance
// numbers: it builds N throwaway git repos, boots a real collector over them
// and prints the sweep's actual shape. It is skipped unless asked for, because
// it takes minutes by construction — the cold tail's slowness is the feature.
//
//	GROVE_SWEEP_SCALE=300 go test ./internal/daemon/collector -run TestSweepAtScale -v -timeout 15m
//
// Measured 2026-08-13 at N=300 with 8 focused (a desktop treemux's focus set):
// hot tier complete in 244ms, cold tail 2m45s wall for 17.0s of work — a 10.2%
// duty cycle against the 10% target — published over 39 streaming delta
// batches instead of one 300-workspace frame at the end.
func TestSweepAtScale(t *testing.T) {
	nStr := os.Getenv("GROVE_SWEEP_SCALE")
	if nStr == "" {
		t.Skip("set GROVE_SWEEP_SCALE=N to run")
	}
	n, _ := strconv.Atoi(nStr)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	build := time.Now()
	paths := make([]string, 0, n)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dir := filepath.Join(root, fmt.Sprintf("repo%03d", i))
			_ = os.MkdirAll(dir, 0o755)
			for _, args := range [][]string{
				{"init", "-q", "-b", "main"},
				{"config", "user.email", "t@e.com"},
				{"config", "user.name", "T"},
				{"commit", "-q", "--allow-empty", "-m", "init"},
			} {
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("git %v: %v %s", args, err, out)
					return
				}
			}
			mu.Lock()
			paths = append(paths, dir)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	t.Logf("built %d repos in %s", len(paths), time.Since(build).Round(time.Millisecond))

	st := store.New()
	nodes := map[string]*models.EnrichedWorkspace{}
	for _, p := range paths {
		nodes[p] = &models.EnrichedWorkspace{WorkspaceNode: &workspace.WorkspaceNode{
			Name: filepath.Base(p), Path: p, Kind: workspace.KindStandaloneProject,
		}}
	}
	st.ApplyUpdate(store.Update{Type: store.UpdateWorkspaces, Source: "test", Payload: nodes})
	// 8 focused, as the live daemon reports for a desktop treemux.
	st.SetFocus("treemux_git", paths[:8])

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	c := NewGitStatusCollector(10*time.Second, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	updates := make(chan store.Update, 200)
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
	go func() { _ = c.Run(ctx, st, updates) }()

	var deltaBatches int
	start := time.Now()
	for {
		u := <-sub
		switch u.Type {
		case store.UpdateSweepStarted:
			p := u.Payload.(*models.GitSweepProgress)
			start = time.Now()
			t.Logf("sweep_started reason=%s total=%d plan=%v", p.Reason, p.Total, p.TierTotals)
		case store.UpdateWorkspacesDelta:
			deltaBatches++
		case store.UpdateSweepProgress:
			p := u.Payload.(*models.GitSweepProgress)
			if p.Tier == "hot" || p.Done%64 == 0 {
				t.Logf("  t=%7s tier=%-6s tier=%d/%-4d done=%d/%d work=%dms",
					time.Since(start).Round(time.Millisecond), p.Tier,
					p.TierDone, p.TierTotal, p.Done, p.Total, p.WorkMS)
			}
		case store.UpdateSweepCompleted:
			p := u.Payload.(*models.GitSweepProgress)
			t.Logf("sweep_completed done=%d/%d elapsed=%s work=%s duty=%.1f%% delta_publishes=%d",
				p.Done, p.Total,
				(time.Duration(p.ElapsedMS) * time.Millisecond).Round(time.Millisecond),
				(time.Duration(p.WorkMS) * time.Millisecond).Round(time.Millisecond),
				100*float64(p.WorkMS)/float64(p.ElapsedMS), deltaBatches)
			return
		}
	}
}
