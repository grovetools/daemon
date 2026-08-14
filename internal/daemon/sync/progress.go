package sync

import (
	stdsync "sync"
	"time"
)

// HydrationProgress is a snapshot of one notespace's tree-walk reconcile
// (walkLocalTree). The first pass on an empty sync.db is hydration; every pass
// after catches whatever the live watcher missed. The counters let the
// freshness benchmark read hydration wall-time and files/sec off
// /api/sync/status without a bespoke endpoint.
type HydrationProgress struct {
	Notespace string `json:"notespace"`
	// Root is the tree the walk actually enumerated. A notespace resolved to
	// the wrong root hydrates perfectly happily — plausible counts, no errors —
	// and the only way to see it was to correlate with the daemon's spawn log,
	// so the answer is reported alongside the counters it explains.
	Root        string    `json:"root"`
	Running     bool      `json:"running"`
	Scanned     int64     `json:"scanned"`
	Enqueued    int64     `json:"enqueued"`
	Quarantined int64     `json:"quarantined"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	FilesPerSec float64   `json:"files_per_sec"`
}

// The reconcile runs inside the watcher's per-notespace pipeline goroutines,
// but the HTTP status handler only holds the sync DB. A small package-level
// registry bridges the two: walkLocalTree writes progress here, and
// handleSyncStatus reads it via HydrationStatus.
var (
	progressMu   stdsync.Mutex
	progressByWS = map[string]*HydrationProgress{}
)

// setHydrationProgress records (a copy of) the latest progress for a notespace.
// Called at walk start, periodically during a long hydration, and at finish.
func setHydrationProgress(p HydrationProgress) {
	progressMu.Lock()
	defer progressMu.Unlock()
	cp := p
	progressByWS[p.Notespace] = &cp
}

// HydrationStatus returns a snapshot copy of the latest hydration progress for
// a notespace, or nil if the reconcile has never run for it. Exported for the
// sync status handler.
func HydrationStatus(notespace string) *HydrationProgress {
	progressMu.Lock()
	defer progressMu.Unlock()
	p, ok := progressByWS[notespace]
	if !ok {
		return nil
	}
	cp := *p
	return &cp
}
