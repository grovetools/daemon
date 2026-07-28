package telemetry

import (
	"fmt"
	"testing"
	"time"
)

func TestRaisePreservesSinceAndUpdatesOffender(t *testing.T) {
	l := NewWarningLedger()
	t0 := time.Unix(1_700_000_000, 0)

	l.raiseAt("/repo", CondLargeBlobHash, "a.bin (1G)", t0)
	l.raiseAt("/repo", CondLargeBlobHash, "b.bin (2G)", t0.Add(time.Minute))

	active := l.activeAt(t0.Add(time.Minute))
	if len(active) != 1 {
		t.Fatalf("got %d warnings, want 1: %+v", len(active), active)
	}
	if !active[0].Since.Equal(t0) {
		t.Errorf("Since = %v, want %v (re-raise must not reset the clock)", active[0].Since, t0)
	}
	if active[0].Offender != "b.bin (2G)" {
		t.Errorf("Offender = %q, want the newest", active[0].Offender)
	}
}

// Warnings are level-triggered: a rule that stops firing must age out without
// anyone calling Clear from a hot path.
func TestWarningsExpireAfterTTL(t *testing.T) {
	l := NewWarningLedger()
	t0 := time.Unix(1_700_000_000, 0)
	l.raiseAt("/repo", CondSlowGitSweep, "x", t0)

	if got := l.activeAt(t0.Add(warningTTL - time.Second)); len(got) != 1 {
		t.Fatalf("warning vanished early: %+v", got)
	}
	if got := l.activeAt(t0.Add(warningTTL + time.Second)); len(got) != 0 {
		t.Fatalf("warning outlived its TTL: %+v", got)
	}
}

func TestClearRemovesImmediately(t *testing.T) {
	l := NewWarningLedger()
	l.Raise("/repo", CondSlowGitScan, "x")
	l.Clear("/repo", CondSlowGitScan)
	if got := l.Active(); len(got) != 0 {
		t.Fatalf("Clear left %d warnings", len(got))
	}
}

func TestDistinctPathsAndConditionsAreDistinctWarnings(t *testing.T) {
	l := NewWarningLedger()
	l.Raise("/a", CondSlowGitSweep, "1")
	l.Raise("/b", CondSlowGitSweep, "2")
	l.Raise("/a", CondSlowGitScan, "3")
	if got := l.Active(); len(got) != 3 {
		t.Fatalf("got %d warnings, want 3", len(got))
	}
}

func TestLedgerIsBounded(t *testing.T) {
	l := NewWarningLedger()
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < warningCap*3; i++ {
		l.raiseAt(fmt.Sprintf("/repo/%d", i), CondSlowGitScan, "x", t0.Add(time.Duration(i)*time.Second))
	}
	if got := len(l.activeAt(t0.Add(time.Duration(warningCap*3) * time.Second))); got > warningCap {
		t.Fatalf("ledger grew to %d entries, cap is %d", got, warningCap)
	}
}

func TestActiveIsNeverNil(t *testing.T) {
	var l *WarningLedger
	if got := l.Active(); got == nil {
		t.Fatal("nil ledger returned nil slice")
	}
	if got := NewWarningLedger().Active(); got == nil {
		t.Fatal("empty ledger returned nil slice")
	}
}

func TestActiveOrdersNewestFirst(t *testing.T) {
	l := NewWarningLedger()
	t0 := time.Unix(1_700_000_000, 0)
	l.raiseAt("/old", CondSlowGitSweep, "", t0)
	l.raiseAt("/new", CondSlowGitScan, "", t0.Add(time.Minute))

	got := l.activeAt(t0.Add(2 * time.Minute))
	if len(got) != 2 || got[0].Path != "/new" {
		t.Fatalf("ordering wrong: %+v", got)
	}
}
