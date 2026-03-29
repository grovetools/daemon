package collector

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// gitWorkers is the number of parallel git status workers.
// Uses half of CPU cores (min 2, max 8) to stay unobtrusive.
var gitWorkers = max(min(runtime.NumCPU()/2, 8), 2)

// backgroundScanInterval is how often to scan non-focused workspaces.
// Uses a long interval since CLI commands trigger /api/refresh on demand.
const backgroundScanInterval = 5 * time.Minute

// dynamicInterval returns a scan interval based on workspace count.
// Fewer workspaces = faster scanning since it's cheaper.
func dynamicInterval(count int, baseInterval time.Duration) time.Duration {
	switch {
	case count <= 5:
		return max(baseInterval/4, 1*time.Second) // 4x faster, min 1s
	case count <= 15:
		return max(baseInterval/2, 2*time.Second) // 2x faster, min 2s
	case count <= 30:
		return baseInterval // Normal speed
	default:
		return baseInterval * 2 // Slower for large sets
	}
}

// GitStatusCollector updates git status for all workspaces.
type GitStatusCollector struct {
	interval time.Duration
	refresh  chan chan struct{}
}

// NewGitStatusCollector creates a new GitStatusCollector with the specified interval.
// If interval is 0, defaults to 10 seconds.
func NewGitStatusCollector(interval time.Duration) *GitStatusCollector {
	if interval == 0 {
		interval = 10 * time.Second
	}
	return &GitStatusCollector{
		interval: interval,
		refresh:  make(chan chan struct{}),
	}
}

// Refresh triggers an immediate full git status scan and blocks until it completes.
func (c *GitStatusCollector) Refresh(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case c.refresh <- reply:
		select {
		case <-reply:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Name returns the collector's name.
func (c *GitStatusCollector) Name() string { return "git" }

// Run starts the git status collection loop.
func (c *GitStatusCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	logger := logging.NewLogger("collector.git")
	var lastFullScan time.Time
	var lastFocusCount int
	currentInterval := c.interval
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	// scanWorkspaces runs git status on the given workspaces and emits a delta update.
	scanWorkspaces := func(toScan []*models.EnrichedWorkspace) {
		if len(toScan) == 0 {
			return
		}

		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 1*time.Second {
				logger.WithField("duration", d).Debug("Slow git status scan detected")
			}
		}()

		var wg sync.WaitGroup
		sem := make(chan struct{}, gitWorkers)
		var mu sync.Mutex
		var deltas []*models.WorkspaceDelta

		for _, ws := range toScan {
			wg.Add(1)
			go func(ws *models.EnrichedWorkspace) {
				defer wg.Done()
				sem <- struct{}{}        // Acquire
				defer func() { <-sem }() // Release

				status, err := git.GetExtendedStatus(ws.Path)
				if err == nil && !store.GitStatusEqual(ws.GitStatus, status) {
					mu.Lock()
					deltas = append(deltas, &models.WorkspaceDelta{
						Path:      ws.Path,
						GitStatus: status,
					})
					mu.Unlock()
				}
			}(ws)
		}
		wg.Wait()

		if len(deltas) > 0 {
			updates <- store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "git",
				Scanned: len(toScan),
				Payload: deltas,
			}
		}
	}

	// scan determines which workspaces to scan this tick and scans them.
	scan := func() {
		state := st.Get()
		focus := st.GetFocus()

		var toScan []*models.EnrichedWorkspace

		if len(focus) == 0 {
			// No focus set (nav not running): only do periodic background scans
			if time.Since(lastFullScan) < backgroundScanInterval {
				return // Skip this tick
			}
			lastFullScan = time.Now()
			for _, ws := range state.Workspaces {
				toScan = append(toScan, ws)
			}
		} else if time.Since(lastFullScan) >= backgroundScanInterval {
			// Focus is set but it's time for a periodic full scan
			lastFullScan = time.Now()
			for _, ws := range state.Workspaces {
				toScan = append(toScan, ws)
			}
		} else {
			// Focused scan: only focused workspaces
			focusLower := make(map[string]struct{}, len(focus))
			for p := range focus {
				focusLower[strings.ToLower(p)] = struct{}{}
			}
			for _, ws := range state.Workspaces {
				if _, ok := focusLower[strings.ToLower(ws.Path)]; ok {
					toScan = append(toScan, ws)
				}
			}
		}

		scanWorkspaces(toScan)

		// Adjust interval dynamically based on focus count
		focusCount := len(focus)
		if focusCount == 0 {
			focusCount = len(state.Workspaces)
		}
		newInterval := dynamicInterval(focusCount, c.interval)
		if newInterval != currentInterval && focusCount != lastFocusCount {
			currentInterval = newInterval
			ticker.Reset(currentInterval)
			lastFocusCount = focusCount
		}
	}

	// fullScan forces a scan of all workspaces (used by Refresh).
	fullScan := func() {
		state := st.Get()
		var toScan []*models.EnrichedWorkspace
		for _, ws := range state.Workspaces {
			toScan = append(toScan, ws)
		}
		lastFullScan = time.Now()
		scanWorkspaces(toScan)
	}

	// Wait for workspaces to be populated first
	time.Sleep(1 * time.Second)
	fullScan()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan()
		case replyCh := <-c.refresh:
			fullScan()
			close(replyCh)
		}
	}
}

