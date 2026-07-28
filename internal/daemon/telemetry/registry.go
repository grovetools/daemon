// Package telemetry is the daemon's in-process observability store: the
// counters, gauges and duration statistics that GET /api/system/stats exposes
// as its flat `counters` map, plus the health-warning ledger behind its
// `warnings` array.
//
// Design constraints (R3 of the inspector series):
//
//   - Recording must be cheap enough to sit on hot paths (every git sweep,
//     every filesystem event). Handles are resolved ONCE at package init and
//     each record is a single atomic add — never a map lookup under a lock.
//   - Reading must never block a recorder or an HTTP handler. Snapshot takes
//     a read lock only over the registry's name→handle maps, which mutate
//     only when a new handle is registered (effectively boot-time only).
//   - The wire shape is a flat map[string]float64 (pinned by models.SystemStats
//     in R2), so a Stat fans out to several dotted keys rather than nesting.
//
// Seam for job 50 (git-scan guardrails): doc 50 Layer 2 specifies the same
// per-workspace health surface ({path, condition, offender, since}) that
// Warnings implements here, and doc 50 Layer 1 will add real byte-budget skip
// accounting inside core/git.GetBlobHashes. When that lands it should feed
// this registry (RecordBlobHashBatch already carries a skipped count and a
// largest-offender slot) rather than introducing a second store.
package telemetry

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// rateInterval is how often Run recomputes per-minute rates. Rates are
// derived on a fixed cadence rather than at Snapshot time so a client polling
// every 2s and a client polling once an hour read the SAME number — a rate
// computed against the caller's poll interval would be meaningless.
const rateInterval = 15 * time.Second

// rateHalfLife makes the per-minute rate an exponentially weighted moving
// average: a burst decays out over roughly a minute instead of vanishing on
// the next tick, which is what makes a storm legible in a UI that samples
// every couple of seconds.
const rateHalfLife = 60 * time.Second

// Counter is a monotonically increasing count. Add is one atomic increment.
// When registered via RateCounter the registry also publishes a decayed
// per-minute rate under "<name>_per_min".
type Counter struct {
	v atomic.Int64

	// rate state, owned by the registry's rate ticker only.
	tracksRate bool
	prev       int64
	prevAt     time.Time
	perMin     atomic.Uint64 // float64 bits
}

// Add increments the counter by n. Non-positive values are ignored so a
// counter can never run backwards and corrupt the rate EWMA.
func (c *Counter) Add(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.v.Add(n)
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Value returns the current total.
func (c *Counter) Value() int64 {
	if c == nil {
		return 0
	}
	return c.v.Load()
}

// PerMin returns the decayed per-minute rate (0 until the first rate tick).
func (c *Counter) PerMin() float64 {
	if c == nil {
		return 0
	}
	return math.Float64frombits(c.perMin.Load())
}

// Gauge is a point-in-time value set by its owner.
type Gauge struct{ bits atomic.Uint64 }

// Set replaces the gauge's value.
func (g *Gauge) Set(v float64) {
	if g == nil {
		return
	}
	g.bits.Store(math.Float64bits(v))
}

// Value returns the gauge's current value.
func (g *Gauge) Value() float64 {
	if g == nil {
		return 0
	}
	return math.Float64frombits(g.bits.Load())
}

// Stat accumulates observations of a duration-like quantity and publishes
// count / last / mean / max. It is the shape the design doc asks for
// ("last/mean git sweep duration"): a bare counter cannot answer "is a sweep
// slow right now?" and a full histogram is far more than the inspector needs.
type Stat struct {
	mu    sync.Mutex
	count int64
	sum   float64
	last  float64
	max   float64
}

// Observe records one sample (milliseconds, by convention for durations).
func (s *Stat) Observe(v float64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.count++
	s.sum += v
	s.last = v
	if v > s.max {
		s.max = v
	}
	s.mu.Unlock()
}

// ObserveDuration records a duration in milliseconds.
func (s *Stat) ObserveDuration(d time.Duration) {
	s.Observe(float64(d.Microseconds()) / 1000)
}

// Snapshot returns count, last, mean and max.
func (s *Stat) Snapshot() (count int64, last, mean, mx float64) {
	if s == nil {
		return 0, 0, 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count > 0 {
		mean = s.sum / float64(s.count)
	}
	return s.count, s.last, mean, s.max
}

// Registry owns a named set of counters, gauges, stats and pull-gauges.
//
// Handles are stable for the process lifetime: registering the same name twice
// returns the same handle, so `var x = telemetry.Default().Counter("a.b")`
// package vars in several files cannot silently shadow one another.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*Counter
	gauges   map[string]*Gauge
	stats    map[string]*Stat
	pulls    map[string]func() float64

	// rateNames is the subset of counters publishing "<name>_per_min".
	rateNames []string

	warnings *WarningLedger
}

// New creates an empty registry (tests; production uses Default).
func New() *Registry {
	return &Registry{
		counters: map[string]*Counter{},
		gauges:   map[string]*Gauge{},
		stats:    map[string]*Stat{},
		pulls:    map[string]func() float64{},
		warnings: NewWarningLedger(),
	}
}

var (
	defaultOnce sync.Once
	defaultReg  *Registry
)

// Default is the process-wide registry. A singleton is deliberate: the
// alternative — threading a *Registry through every collector, watcher and
// store constructor in the daemon — would touch dozens of signatures to
// deliver one process-global observability sink, and the daemon already
// treats logging the same way (logging.NewUnifiedLogger).
func Default() *Registry {
	defaultOnce.Do(func() { defaultReg = New() })
	return defaultReg
}

// Counter returns (registering if needed) the counter named name.
func (r *Registry) Counter(name string) *Counter {
	r.mu.RLock()
	c, ok := r.counters[name]
	r.mu.RUnlock()
	if ok {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c = &Counter{}
	r.counters[name] = c
	return c
}

// RateCounter is Counter plus publication of "<name>_per_min".
func (r *Registry) RateCounter(name string) *Counter {
	c := r.Counter(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !c.tracksRate {
		c.tracksRate = true
		r.rateNames = append(r.rateNames, name)
	}
	return c
}

// Gauge returns (registering if needed) the gauge named name.
func (r *Registry) Gauge(name string) *Gauge {
	r.mu.RLock()
	g, ok := r.gauges[name]
	r.mu.RUnlock()
	if ok {
		return g
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g = &Gauge{}
	r.gauges[name] = g
	return g
}

// Stat returns (registering if needed) the stat named name.
func (r *Registry) Stat(name string) *Stat {
	r.mu.RLock()
	s, ok := r.stats[name]
	r.mu.RUnlock()
	if ok {
		return s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.stats[name]; ok {
		return s
	}
	s = &Stat{}
	r.stats[name] = s
	return s
}

// SetPull registers a pull-gauge: fn is called at Snapshot time. Use it for
// values that already live somewhere authoritative (the store's focus set
// size, a manager's live tailer count) so the number can never drift from its
// source through a missed Set. fn must be cheap and non-blocking; a nil fn
// unregisters the name.
func (r *Registry) SetPull(name string, fn func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fn == nil {
		delete(r.pulls, name)
		return
	}
	r.pulls[name] = fn
}

// Warnings returns the registry's health-warning ledger.
func (r *Registry) Warnings() *WarningLedger { return r.warnings }

// Snapshot renders the whole registry as the flat name→value map that
// /api/system/stats publishes. Never returns nil.
func (r *Registry) Snapshot() map[string]float64 {
	out := make(map[string]float64, 64)

	r.mu.RLock()
	for name, c := range r.counters {
		out[name] = float64(c.v.Load())
		if c.tracksRate {
			out[name+"_per_min"] = round2(c.PerMin())
		}
	}
	for name, g := range r.gauges {
		out[name] = round2(g.Value())
	}
	for name, s := range r.stats {
		count, last, mean, mx := s.Snapshot()
		out[name+".count"] = float64(count)
		out[name+".last_ms"] = round2(last)
		out[name+".mean_ms"] = round2(mean)
		out[name+".max_ms"] = round2(mx)
	}
	pulls := make(map[string]func() float64, len(r.pulls))
	for name, fn := range r.pulls {
		pulls[name] = fn
	}
	r.mu.RUnlock()

	// Pull-gauges run OUTSIDE the registry lock: a provider that takes the
	// store lock must never be able to deadlock against a concurrent
	// registration, and a slow provider must not stall recorders.
	for name, fn := range pulls {
		out[name] = round2(fn())
	}
	return out
}

// Names returns every key Snapshot would emit, sorted. Used by tests and by
// the CLI's stable ordering.
func (r *Registry) Names() []string {
	snap := r.Snapshot()
	names := make([]string, 0, len(snap))
	for n := range snap {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Run recomputes per-minute rates until ctx is done. Cheap: one goroutine,
// one tick every rateInterval, O(rate counters) work per tick.
func (r *Registry) Run(done <-chan struct{}) {
	ticker := time.NewTicker(rateInterval)
	defer ticker.Stop()
	r.tickRates(time.Now())
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			r.tickRates(now)
		}
	}
}

// tickRates advances every rate counter's EWMA. Exported to tests via a
// direct call with a synthetic clock.
func (r *Registry) tickRates(now time.Time) {
	r.mu.RLock()
	names := append([]string(nil), r.rateNames...)
	counters := make([]*Counter, 0, len(names))
	for _, n := range names {
		counters = append(counters, r.counters[n])
	}
	r.mu.RUnlock()

	for _, c := range counters {
		if c == nil {
			continue
		}
		total := c.v.Load()
		if c.prevAt.IsZero() {
			c.prev, c.prevAt = total, now
			continue
		}
		elapsed := now.Sub(c.prevAt)
		if elapsed <= 0 {
			continue
		}
		instant := float64(total-c.prev) / elapsed.Minutes()
		c.prev, c.prevAt = total, now

		// EWMA with a half-life of rateHalfLife over the elapsed window.
		alpha := 1 - math.Exp2(-elapsed.Seconds()/rateHalfLife.Seconds())
		cur := math.Float64frombits(c.perMin.Load())
		c.perMin.Store(math.Float64bits(cur + alpha*(instant-cur)))
	}
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}
