package telemetry

import (
	"fmt"
	"sync"
	"time"
)

// Per-repository attribution for the watcher's no-op scans.
//
// git.watcher_scan.noop already proves that ~99% of event-driven scans change
// nothing; what it cannot say is WHICH repository is burning the forks. The
// registry is a flat map[string]float64 with get-or-create registration, no
// label dimension and no eviction, and Snapshot() copies every key into every
// /api/system/stats response — minting one counter per repository would
// permanently inflate both the map and the wire payload for 479+ workspaces.
// The WarningLedger is the only bounded per-repository surface in this package
// (keyed (path, condition), 64-entry cap, 5-minute TTL), so that is where the
// attribution lands: not a new metric, one named offender when a single repo's
// no-op rate separates it from the fleet.
//
// The signal was already recoverable at debug level —
//
//	grep "git watcher: scan no-op" ~/.local/state/grove/logs/system-$(date +%F).log \
//	  | jq -r .path | sort | uniq -c | sort -rn | head
//
// — and that grep is what sized the threshold below. This makes the same fact
// available without turning on debug logging.

const (
	// noopStormWindow is deliberately warningTTL: the tally window and the
	// warning's lifetime are the same 5 minutes, so "60 no-op scans in 5m"
	// stays true for exactly as long as the warning it raised is shown.
	noopStormWindow = warningTTL

	// noopStormPerWindow is the no-op count in one window that names a
	// repository as an offender. Measured on the live global daemon
	// (2026-07-30, 479 workspaces, per-repo no-ops bucketed by 5 minutes):
	// ~/.config ran 98–657 per bucket while the worst healthy repo peaked at
	// 41 and the median repo sat at 0–2. 60 is ~1.5x above the healthy peak
	// and ~1.6x below the offender's quietest bucket, so neither a boot burst
	// (~20 scans per repo, spread over minutes) nor a busy branch-switching
	// repo trips it.
	noopStormPerWindow = 60

	// noopTrackerCap bounds the tally map. It is sized ABOVE the observed
	// fleet (479 workspaces) on purpose: with a cap below the repository count
	// the healthy majority would evict the one offender we are trying to name.
	// Worst case is ~1024 * (path string + 40B) ≈ 100 KB, and entries whose
	// window has lapsed are dropped before any live entry is.
	noopTrackerCap = 1024
)

// noopStormTracker counts per-repository no-op scans inside a tumbling
// noopStormWindow. It is NOT a metric: nothing serializes it, it never leaves
// this package, and its only output is the boolean that raises a warning.
type noopStormTracker struct {
	mu      sync.Mutex
	windows map[string]*noopWindow
}

type noopWindow struct {
	count int
	start time.Time // when the current tumbling window opened
	last  time.Time // last no-op seen, for eviction
}

func newNoopStormTracker() *noopStormTracker {
	return &noopStormTracker{windows: map[string]*noopWindow{}}
}

// record counts one no-op scan of path and returns the running count for the
// current window plus whether it has reached noopStormPerWindow. Once the
// threshold is crossed every subsequent no-op in the window reports true, so
// the caller re-raises and the warning stays fresh (health warnings are
// level-triggered — see warningTTL). When the window rolls over the count
// restarts from zero, so a repository that stops storming stops re-raising and
// its warning ages out on its own.
func (t *noopStormTracker) record(path string, now time.Time) (int, bool) {
	if t == nil || path == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.windows[path]
	switch {
	case !ok:
		t.evictLocked(now)
		w = &noopWindow{start: now}
		t.windows[path] = w
	case now.Sub(w.start) >= noopStormWindow:
		w.count, w.start = 0, now
	}
	w.count++
	w.last = now
	return w.count, w.count >= noopStormPerWindow
}

// evictLocked makes room when the tally map is full, dropping windows that
// have already lapsed before the least recently seen live one.
func (t *noopStormTracker) evictLocked(now time.Time) {
	if len(t.windows) < noopTrackerCap {
		return
	}
	for path, w := range t.windows {
		if now.Sub(w.last) > noopStormWindow {
			delete(t.windows, path)
		}
	}
	for len(t.windows) >= noopTrackerCap {
		var oldestPath string
		var oldest time.Time
		first := true
		for path, w := range t.windows {
			if first || w.last.Before(oldest) {
				oldestPath, oldest, first = path, w.last, false
			}
		}
		delete(t.windows, oldestPath)
	}
}

// recordNoopScan tallies one no-op scan of path and raises the storm warning
// against it once the repository crosses the threshold. Split out from
// RecordGitWatcherScan so it can be exercised against a fresh ledger and
// tracker with an injected clock.
func recordNoopScan(l *WarningLedger, t *noopStormTracker, path string, now time.Time) {
	n, storming := t.record(path, now)
	if !storming {
		return
	}
	l.raiseAt(path, CondNoopStorm,
		fmt.Sprintf("%d no-op scans in %s", n, noopStormWindow), now)
}
