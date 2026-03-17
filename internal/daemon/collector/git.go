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
const backgroundScanInterval = 60 * time.Second

// dynamicInterval returns a scan interval based on workspace count.
// Fewer workspaces = faster scanning since it's cheaper.
func dynamicInterval(count int, baseInterval time.Duration) time.Duration {
	switch {
	case count <= 5:
		return max(baseInterval/4, 250*time.Millisecond) // 4x faster, min 250ms
	case count <= 15:
		return max(baseInterval/2, 500*time.Millisecond) // 2x faster, min 500ms
	case count <= 30:
		return baseInterval // Normal speed
	default:
		return baseInterval * 2 // Slower for large sets
	}
}

// GitStatusCollector updates git status for all workspaces.
type GitStatusCollector struct {
	interval time.Duration
}

// NewGitStatusCollector creates a new GitStatusCollector with the specified interval.
// If interval is 0, defaults to 10 seconds.
func NewGitStatusCollector(interval time.Duration) *GitStatusCollector {
	if interval == 0 {
		interval = 10 * time.Second
	}
	return &GitStatusCollector{
		interval: interval,
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

	scan := func() {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 200*time.Millisecond {
				logger.WithField("duration", d).Debug("Slow git status scan detected")
			}
		}()

		state := st.Get()
		focus := st.GetFocus()

		// Clone existing state
		newWorkspaces := make(map[string]*models.EnrichedWorkspace)
		for k, v := range state.Workspaces {
			cpy := *v
			newWorkspaces[k] = &cpy
		}

		// Determine which workspaces to scan this tick
		var toScan []*models.EnrichedWorkspace
		doFullScan := len(focus) == 0 || time.Since(lastFullScan) >= backgroundScanInterval

		if doFullScan {
			// Full scan: all workspaces
			lastFullScan = time.Now()
			for _, ws := range newWorkspaces {
				toScan = append(toScan, ws)
			}
		} else {
			// Focused scan: only focused workspaces
			// Use case-insensitive comparison for macOS compatibility
			focusLower := make(map[string]struct{}, len(focus))
			for p := range focus {
				focusLower[strings.ToLower(p)] = struct{}{}
			}
			for _, ws := range newWorkspaces {
				if _, ok := focusLower[strings.ToLower(ws.Path)]; ok {
					toScan = append(toScan, ws)
				}
			}
		}

		if len(toScan) == 0 {
			return
		}

		// Parallel git status fetching using worker pool
		var wg sync.WaitGroup
		sem := make(chan struct{}, gitWorkers)
		var mu sync.Mutex
		changed := false

		for _, ws := range toScan {
			wg.Add(1)
			go func(ws *models.EnrichedWorkspace) {
				defer wg.Done()
				sem <- struct{}{}        // Acquire
				defer func() { <-sem }() // Release

				status, err := git.GetExtendedStatus(ws.Path)
				if err == nil {
					mu.Lock()
					ws.GitStatus = status
					changed = true
					mu.Unlock()
				}
			}(ws)
		}
		wg.Wait()

		if changed {
			updates <- store.Update{
				Type:    store.UpdateWorkspaces,
				Source:  "git",
				Scanned: len(toScan),
				Payload: newWorkspaces,
			}
		}

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

	// Wait for workspaces to be populated first
	time.Sleep(1 * time.Second)
	scan()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan()
		}
	}
}
