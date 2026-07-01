package collector

import (
	"context"
	"runtime"
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

// maxFocusedChangedFiles caps how many changed files a focused repo may have
// before the daemon skips computing per-file blob hashes for it. A huge change
// set (e.g. a checked-in node_modules) would make the batch hash-object too
// costly; above the cap we still cache the file list but leave BlobHashes nil so
// the consumer falls back to live hashing.
const maxFocusedChangedFiles = 500

// focusedFileData computes the per-file change list and (when the set is small
// enough) the per-file working-tree blob hashes for a focused workspace. It is
// best-effort: any git error yields nil for the affected field so the emitted
// delta still carries the coarse GitStatus. Above maxFocusedChangedFiles the
// file list is returned but blob hashes are skipped to bound cost.
func focusedFileData(repoPath string) ([]git.FileStatus, map[string]string) {
	files, err := git.GetChangedFiles(repoPath)
	if err != nil {
		return nil, nil
	}
	if len(files) > maxFocusedChangedFiles {
		return files, nil
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	hashes, err := git.GetBlobHashes(repoPath, paths)
	if err != nil {
		return files, nil
	}
	return files, hashes
}

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
	ulog := logging.NewUnifiedLogger("groved.collector.git")
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
				ulog.Debug("Slow git status scan detected").Field("duration", d).Log(ctx)
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
				if err != nil {
					return
				}
				coarseChanged := !store.GitStatusEqual(ws.GitStatus, status)
				focused := st.IsFocused(ws.Path)
				// Backfill: a focused repo whose coarse status is unchanged still
				// needs its per-file data computed the FIRST time it becomes
				// focused. The daemon boots / rescans with the focus set empty, so
				// ChangedFiles starts nil; without this, a stable focused repo
				// never gets per-file data (the coarse-changed gate alone never
				// fires) and the git-viewer cache-misses forever, falling back to
				// live git in the TUI. Emit when the coarse status changed OR a
				// focused repo is missing its per-file cache.
				needsFileBackfill := focused && ws.ChangedFiles == nil
				if !coarseChanged && !needsFileBackfill {
					return
				}
				delta := &models.WorkspaceDelta{
					Path:      ws.Path,
					GitStatus: status,
				}
				// Granular per-file data is computed ONLY for focused repos
				// (git-viewer panels / nav) — never on the 5-min background
				// sweep — to bound git cost. Best-effort: a fetch error just
				// leaves the file-level fields nil and the coarse status stands.
				if focused {
					delta.ChangedFiles, delta.BlobHashes = focusedFileData(ws.Path)
				}
				mu.Lock()
				deltas = append(deltas, delta)
				mu.Unlock()
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
			// Focused scan: only focused workspaces. Select via the same
			// st.IsFocused check the per-file ChangedFiles attachment uses below,
			// so a repo that is scanned as focused ALWAYS also gets its per-file
			// data — previously selection was case-insensitive while attachment
			// was case-sensitive, dropping ChangedFiles for case-mismatched paths.
			for _, ws := range state.Workspaces {
				if st.IsFocused(ws.Path) {
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
