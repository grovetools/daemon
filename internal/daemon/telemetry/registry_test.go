package telemetry

import (
	"math"
	"testing"
	"time"
)

func TestCounterAddIgnoresNonPositive(t *testing.T) {
	r := New()
	c := r.Counter("a.b")
	c.Add(5)
	c.Add(0)
	c.Add(-3)
	c.Inc()
	if got := c.Value(); got != 6 {
		t.Fatalf("Value = %d, want 6", got)
	}
}

func TestCounterHandleIsStable(t *testing.T) {
	r := New()
	if r.Counter("x") != r.Counter("x") {
		t.Fatal("Counter returned different handles for the same name")
	}
	if r.Gauge("g") != r.Gauge("g") {
		t.Fatal("Gauge returned different handles for the same name")
	}
	if r.Stat("s") != r.Stat("s") {
		t.Fatal("Stat returned different handles for the same name")
	}
}

func TestStatFansOutToFourKeys(t *testing.T) {
	r := New()
	s := r.Stat("git.sweep")
	s.ObserveDuration(100 * time.Millisecond)
	s.ObserveDuration(300 * time.Millisecond)

	snap := r.Snapshot()
	if snap["git.sweep.count"] != 2 {
		t.Errorf("count = %v, want 2", snap["git.sweep.count"])
	}
	if snap["git.sweep.last_ms"] != 300 {
		t.Errorf("last_ms = %v, want 300", snap["git.sweep.last_ms"])
	}
	if snap["git.sweep.mean_ms"] != 200 {
		t.Errorf("mean_ms = %v, want 200", snap["git.sweep.mean_ms"])
	}
	if snap["git.sweep.max_ms"] != 300 {
		t.Errorf("max_ms = %v, want 300", snap["git.sweep.max_ms"])
	}
}

func TestSnapshotIncludesGaugesAndPulls(t *testing.T) {
	r := New()
	r.Gauge("focus").Set(12)
	calls := 0
	r.SetPull("tailers", func() float64 { calls++; return 480 })

	snap := r.Snapshot()
	if snap["focus"] != 12 {
		t.Errorf("gauge = %v", snap["focus"])
	}
	if snap["tailers"] != 480 {
		t.Errorf("pull = %v", snap["tailers"])
	}
	if calls != 1 {
		t.Errorf("pull called %d times, want 1", calls)
	}

	r.SetPull("tailers", nil)
	if _, ok := r.Snapshot()["tailers"]; ok {
		t.Error("nil pull did not unregister")
	}
}

// Rates must be a function of wall time, not of how often anyone polls: a
// client polling every 2s and one polling hourly have to read the same rate.
func TestRateCounterIsPollIndependent(t *testing.T) {
	r := New()
	c := r.RateCounter("events")

	base := time.Unix(1_700_000_000, 0)
	r.tickRates(base) // establishes the checkpoint

	// 600 events over 60s == 600/min instantaneous.
	c.Add(600)
	r.tickRates(base.Add(60 * time.Second))

	got := c.PerMin()
	if got <= 0 || got > 600 {
		t.Fatalf("PerMin = %v, want 0 < v <= 600", got)
	}

	// Snapshot publishes it under the _per_min suffix.
	snap := r.Snapshot()
	if _, ok := snap["events_per_min"]; !ok {
		t.Fatal("events_per_min missing from snapshot")
	}
	if snap["events"] != 600 {
		t.Errorf("raw total = %v, want 600", snap["events"])
	}

	// With no further events the rate decays toward zero, never negative.
	for i := 1; i <= 10; i++ {
		r.tickRates(base.Add(time.Duration(60+i*60) * time.Second))
	}
	if decayed := c.PerMin(); decayed < 0 || decayed >= got {
		t.Fatalf("rate did not decay: %v -> %v", got, decayed)
	}
}

func TestSnapshotNeverEmitsNaNOrInf(t *testing.T) {
	r := New()
	r.Gauge("bad").Set(math.NaN())
	r.SetPull("worse", func() float64 { return math.Inf(1) })
	snap := r.Snapshot()
	if snap["bad"] != 0 || snap["worse"] != 0 {
		t.Fatalf("non-finite values leaked: %v", snap)
	}
}

func TestNamesAreSorted(t *testing.T) {
	r := New()
	r.Counter("z")
	r.Counter("a")
	names := r.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "z" {
		t.Fatalf("Names = %v", names)
	}
}

func TestNilHandlesAreSafe(t *testing.T) {
	var c *Counter
	var g *Gauge
	var s *Stat
	c.Add(1)
	c.Inc()
	g.Set(1)
	s.Observe(1)
	if c.Value() != 0 || c.PerMin() != 0 || g.Value() != 0 {
		t.Fatal("nil handles returned non-zero")
	}
	if n, _, _, _ := s.Snapshot(); n != 0 {
		t.Fatal("nil stat returned samples")
	}
}
