package collector

import (
	"testing"
	"time"

	"github.com/grovetools/core/git"
)

func TestDynamicIntervalHasFocusedFloor(t *testing.T) {
	base := 10 * time.Second
	for _, count := range []int{0, 1, 5, 6, 15} {
		if got := dynamicInterval(count, base); got < focusedScanFloor {
			t.Fatalf("count %d interval = %s, below floor %s", count, got, focusedScanFloor)
		}
	}
	if got := dynamicInterval(5, base); got != 5*time.Second {
		t.Fatalf("small focus interval = %s, want 5s", got)
	}
	if got := dynamicInterval(20, base); got != base {
		t.Fatalf("medium focus interval = %s, want %s", got, base)
	}
}

func TestFocusedFileDataDecisionUsesStatusFingerprint(t *testing.T) {
	status := &git.ExtendedGitStatus{}
	if shouldComputeFocusedFileData(false, false, nil, status) {
		t.Fatal("unfocused repo requested per-file data")
	}
	if !shouldComputeFocusedFileData(true, false, status, status) {
		t.Fatal("first focused snapshot must backfill per-file data")
	}
	if shouldComputeFocusedFileData(true, true, status, status) {
		t.Fatal("unchanged status fingerprint recomputed per-file data")
	}
	changed := &git.ExtendedGitStatus{LinesAdded: 1}
	if !shouldComputeFocusedFileData(true, true, status, changed) {
		t.Fatal("changed status fingerprint did not recompute per-file data")
	}
}

func TestPathRefreshCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	last := map[string]time.Time{}
	if !pathRefreshDue(last, "/repo", now) {
		t.Fatal("first refresh should be due")
	}
	last["/repo"] = now
	if pathRefreshDue(last, "/repo", now.Add(pathRefreshCooldown-time.Nanosecond)) {
		t.Fatal("refresh inside cooldown should be suppressed")
	}
	if !pathRefreshDue(last, "/repo", now.Add(pathRefreshCooldown)) {
		t.Fatal("refresh at cooldown boundary should be due")
	}
}
