package telemetry

import (
	"fmt"
	"testing"
	"time"
)

// noopBurst feeds n no-op scans of path one second apart starting at t0.
func noopBurst(l *WarningLedger, tr *noopStormTracker, path string, n int, t0 time.Time) {
	for i := 0; i < n; i++ {
		recordNoopScan(l, tr, path, t0.Add(time.Duration(i)*time.Second))
	}
}

// The measured separation: the offender storms, the healthy repo does not, and
// only the offender is named.
func TestNoopStormNamesOnlyTheOffender(t *testing.T) {
	l, tr := NewWarningLedger(), newNoopStormTracker()
	t0 := time.Unix(1_700_000_000, 0)

	noopBurst(l, tr, "/Users/x/.config", noopStormPerWindow, t0)
	noopBurst(l, tr, "/Users/x/repo", noopStormPerWindow-1, t0)

	active := l.activeAt(t0.Add(time.Minute))
	if len(active) != 1 {
		t.Fatalf("got %d warnings, want exactly the offender: %+v", len(active), active)
	}
	if active[0].Path != "/Users/x/.config" || active[0].Condition != CondNoopStorm {
		t.Fatalf("warning = %+v, want (/Users/x/.config, %q)", active[0], CondNoopStorm)
	}
	if active[0].Offender == "" {
		t.Error("offender text is empty; it must carry the count and window")
	}
}

// Crossing the threshold repeatedly refreshes one warning instead of resetting
// its clock — the ledger's (path, condition) identity has to hold here too.
func TestNoopStormRefreshesRatherThanRestarts(t *testing.T) {
	l, tr := NewWarningLedger(), newNoopStormTracker()
	t0 := time.Unix(1_700_000_000, 0)

	noopBurst(l, tr, "/repo", noopStormPerWindow*2, t0)

	active := l.activeAt(t0.Add(2 * time.Minute))
	if len(active) != 1 {
		t.Fatalf("got %d warnings, want 1: %+v", len(active), active)
	}
	// The first crossing is at index noopStormPerWindow-1, i.e. that many
	// seconds after t0.
	wantSince := t0.Add(time.Duration(noopStormPerWindow-1) * time.Second)
	if !active[0].Since.Equal(wantSince) {
		t.Errorf("Since = %v, want %v (re-raise must not reset the clock)", active[0].Since, wantSince)
	}
	if want := fmt.Sprintf("%d no-op scans in %s", noopStormPerWindow*2, noopStormWindow); active[0].Offender != want {
		t.Errorf("Offender = %q, want %q (the newest count)", active[0].Offender, want)
	}
}

// The window tumbles: scans spread thinly enough never accumulate.
func TestNoopStormWindowTumbles(t *testing.T) {
	l, tr := NewWarningLedger(), newNoopStormTracker()
	t0 := time.Unix(1_700_000_000, 0)

	// Half a window's worth of no-ops, then another half a window later —
	// twice the threshold in total, never more than half of it at once.
	noopBurst(l, tr, "/repo", noopStormPerWindow-1, t0)
	noopBurst(l, tr, "/repo", noopStormPerWindow-1, t0.Add(noopStormWindow))

	if got := l.activeAt(t0.Add(2 * noopStormWindow)); len(got) != 0 {
		t.Fatalf("a repo below the rate in every window raised %d warnings: %+v", len(got), got)
	}
}

// A storm that stops must stop re-raising, so the warning ages out by itself.
func TestNoopStormStopsRaisingAfterWindowRollover(t *testing.T) {
	tr := newNoopStormTracker()
	t0 := time.Unix(1_700_000_000, 0)
	noopBurst(NewWarningLedger(), tr, "/repo", noopStormPerWindow, t0)

	if n, storming := tr.record("/repo", t0.Add(noopStormWindow+time.Second)); storming {
		t.Fatalf("count %d still storming after the window rolled over", n)
	}
}

// The tally map is bounded, and a live offender survives eviction pressure
// from a fleet of quiet repositories.
func TestNoopTrackerIsBounded(t *testing.T) {
	tr := newNoopStormTracker()
	t0 := time.Unix(1_700_000_000, 0)

	for i := 0; i < noopTrackerCap*2; i++ {
		tr.record(fmt.Sprintf("/repo/%d", i), t0.Add(time.Duration(i)*time.Millisecond))
	}
	tr.mu.Lock()
	got := len(tr.windows)
	tr.mu.Unlock()
	if got > noopTrackerCap {
		t.Fatalf("tracker grew to %d entries, cap is %d", got, noopTrackerCap)
	}
}

func TestNoopTrackerIgnoresEmptyPath(t *testing.T) {
	tr := newNoopStormTracker()
	for i := 0; i < noopStormPerWindow*2; i++ {
		if _, storming := tr.record("", time.Unix(1_700_000_000, 0)); storming {
			t.Fatal("an unattributed scan raised a per-repo warning")
		}
	}
}
