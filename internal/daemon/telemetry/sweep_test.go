package telemetry

import (
	"strings"
	"testing"
	"time"
)

// hasWarning reports whether the default ledger currently holds condition.
func hasWarning(condition string) (string, bool) {
	for _, w := range Default().Warnings().Active() {
		if w.Condition == condition {
			return w.Offender, true
		}
	}
	return "", false
}

func clearWarning(condition string) {
	for _, w := range Default().Warnings().Active() {
		if w.Condition == condition {
			Default().Warnings().Clear(w.Path, w.Condition)
		}
	}
}

// The alarm must not fire just because the sweep is long. Since the sweep
// became paced, a multi-minute wall time is the design; only the hot tier's
// latency and git's own cost are symptoms.
func TestSlowSweepWarningIgnoresIntentionalSlowness(t *testing.T) {
	clearWarning(CondSlowGitSweep)
	clearWarning(CondSlowGitTrickle)

	// A four-minute sweep of 681 workspaces whose hot tier landed in 1.2s and
	// whose trickle cost 570ms of git per workspace — the shipped happy path,
	// with the trickle figure taken from the CONTENDED 08-10 boot (48.4s × 8
	// workers / 681 ws), the worst healthy number on record. If that fires an
	// alarm, the alarm is measuring the wrong thing.
	RecordGitSweep("", 681, 30*time.Second, 4*time.Minute)
	RecordGitSweepHot("", 8, 1200*time.Millisecond)
	RecordGitSweepTrickle("", 600, 342*time.Second, 4*time.Minute)

	if offender, raised := hasWarning(CondSlowGitSweep); raised {
		t.Errorf("paced sweep raised the slow-sweep warning (%q)", offender)
	}
	if offender, raised := hasWarning(CondSlowGitTrickle); raised {
		t.Errorf("paced sweep raised the trickle warning (%q)", offender)
	}
}

// It must still catch what it always caught: the part users wait on going
// slow, and git itself going slow underneath the trickle.
func TestSlowSweepWarningsStillCatchRegressions(t *testing.T) {
	clearWarning(CondSlowGitSweep)
	RecordGitSweepHot("", 10, 6*time.Second)
	offender, raised := hasWarning(CondSlowGitSweep)
	if !raised {
		t.Fatal("a six-second hot tier did not raise the slow-sweep warning")
	}
	if !strings.Contains(offender, "hot tier") {
		t.Errorf("offender = %q, want it to name the hot tier", offender)
	}

	clearWarning(CondSlowGitTrickle)
	// 100 workspaces costing 1.4s of git each — the storm signature.
	RecordGitSweepTrickle("", 100, 140*time.Second, 10*time.Minute)
	if _, raised := hasWarning(CondSlowGitTrickle); !raised {
		t.Fatal("1.4s of git per workspace did not raise the trickle warning")
	}

	clearWarning(CondSlowGitSweep)
	clearWarning(CondSlowGitTrickle)
}

// Progress gauges are what a poller (groved stats) reads; the event stream is
// what a progress bar reads. Both must agree.
func TestSweepProgressGauges(t *testing.T) {
	RecordGitSweepProgress(int(1), 4, 8, 40, 200)
	snap := Default().Snapshot()
	for key, want := range map[string]float64{
		"git.sweep.tier":       1,
		"git.sweep.tier_done":  4,
		"git.sweep.tier_total": 8,
		"git.sweep.done":       40,
		"git.sweep.total":      200,
		"git.sweep.progress":   20,
	} {
		if snap[key] != want {
			t.Errorf("%s = %v, want %v", key, snap[key], want)
		}
	}

	RecordGitSweepIdle()
	if got := Default().Snapshot()["git.sweep.tier"]; got != 0 {
		t.Errorf("git.sweep.tier = %v after idle, want 0 — a stale tier reads as a sweep that never ends", got)
	}
}

// The two duration measurements are deliberately different quantities.
func TestRecordGitSweepSeparatesWorkFromWall(t *testing.T) {
	RecordGitSweep("", 100, 5*time.Second, 90*time.Second)
	snap := Default().Snapshot()
	if snap["git.sweep.last_ms"] != 5000 {
		t.Errorf("git.sweep.last_ms = %v, want the 5s of WORK", snap["git.sweep.last_ms"])
	}
	if snap["git.sweep.wall_ms"] != 90000 {
		t.Errorf("git.sweep.wall_ms = %v, want the 90s of wall time", snap["git.sweep.wall_ms"])
	}
	if snap["git.sweep.workspaces_last"] != 100 {
		t.Errorf("git.sweep.workspaces_last = %v, want 100", snap["git.sweep.workspaces_last"])
	}
}

func TestRecordGitSweepPendingPublishesTheHonestyGauge(t *testing.T) {
	RecordGitSweepPending(613)
	if got := Default().Snapshot()["git.sweep.pending"]; got != 613 {
		t.Errorf("git.sweep.pending = %v, want 613", got)
	}
	RecordGitSweepPending(0)
	if got := Default().Snapshot()["git.sweep.pending"]; got != 0 {
		t.Errorf("git.sweep.pending = %v, want 0 once the fleet is swept", got)
	}
}
