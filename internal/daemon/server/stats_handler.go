package server

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/procsample"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// processStart anchors uptime_ms when no RunningConfig was wired (early
// requests under --ready-at=bind, and test servers). The normal path uses
// runningConfig.StartedAt, which groved sets at boot.
var processStart = time.Now()

// statsSampleMaxAge caps how often /api/system/stats re-runs `ps`: requests
// arriving within this window share one cached sample, so a polling client
// (or several) costs at most one exec every 2s.
const statsSampleMaxAge = 2 * time.Second

// statsChildrenCap bounds self.children in the response (top by CPU).
const statsChildrenCap = 20

// procStatsCache serializes and caches process-table sampling for
// /api/system/stats. The handler never sleeps to widen a CPU window;
// instead the Sampler keeps the previous snapshot, so each refresh reports
// a true cputime delta over the time since the LAST refresh. Warm-up
// choice (documented per contract): the sampler is seeded in a background
// goroutine at server start (see Listen), so even the first request already
// has history and reports interval-true CPU%. If a request beats the seed,
// procsample falls back to ps's decaying pcpu for that one response —
// still meaningful, never blocking.
type procStatsCache struct {
	mu      sync.Mutex
	sampler *procsample.Sampler
	sample  *procsample.Sample
	// sampleFn is a test seam; nil means sampler.Sample.
	sampleFn func() (*procsample.Sample, error)
}

// get returns the cached sample when younger than maxAge, otherwise takes a
// fresh one. On sampling failure the previous sample (possibly nil) is
// returned alongside the error so callers can degrade instead of 500ing.
func (c *procStatsCache) get(maxAge time.Duration) (*procsample.Sample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sample != nil && time.Since(c.sample.At) < maxAge {
		return c.sample, nil
	}
	fn := c.sampleFn
	if fn == nil {
		if c.sampler == nil {
			c.sampler = procsample.NewSampler()
		}
		fn = c.sampler.Sample
	}
	s, err := fn()
	if err != nil {
		return c.sample, err
	}
	c.sample = s
	return s, nil
}

func statsRound1(v float64) float64 { return math.Round(v*10) / 10 }

// fillSelfStats populates self from the sample's rollup of pid. A pid absent
// from the sample leaves the zero values (Procs == 0, no Top).
func fillSelfStats(self *models.SelfStats, sample *procsample.Sample, pid int) {
	r := sample.Rollup(pid)
	if r.Procs == 0 {
		return
	}
	self.CPUPct = statsRound1(r.CPU)
	self.RSSKB = r.RSSKB
	self.Procs = r.Procs
	self.Top = &models.ProcStat{
		PID:    r.Top.PID,
		Comm:   r.Top.Comm,
		CPUPct: statsRound1(r.TopCPU),
		RSSKB:  r.Top.RSSKB,
	}
	self.Children = topChildren(sample, r.Pids, pid, statsChildrenCap)
}

// topChildren renders the subtree's descendant processes (root excluded) as
// per-process rows, hottest first (CPU desc, ties by RSS), capped at max.
func topChildren(sample *procsample.Sample, pids []int, root, max int) []models.ProcStat {
	rows := make([]models.ProcStat, 0, len(pids))
	for _, pid := range pids {
		if pid == root {
			continue
		}
		p := sample.Procs[pid]
		rows = append(rows, models.ProcStat{
			PID:    pid,
			Comm:   p.Comm,
			CPUPct: statsRound1(sample.CPU[pid]),
			RSSKB:  p.RSSKB,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CPUPct != rows[j].CPUPct {
			return rows[i].CPUPct > rows[j].CPUPct
		}
		return rows[i].RSSKB > rows[j].RSSKB
	})
	if len(rows) > max {
		rows = rows[:max]
	}
	return rows
}

// collectCounters merges the telemetry registry's recorded counters with the
// gauges that are better read straight from their owner at request time
// (store sizes, live tailer counts, core's package-global git caches). Those
// are pulled rather than pushed on purpose: a Set() on every focus change or
// every tailer teardown is one more thing that can be forgotten in a future
// refactor, and the number would then lie forever. Reading len() at snapshot
// time cannot drift from the truth.
func (s *Server) collectCounters() map[string]float64 {
	counters := telemetry.Default().Snapshot()

	if s.engine != nil {
		if st := s.engine.Store(); st != nil {
			counters["store.focused_workspaces"] = float64(len(st.GetFocus()))
			counters["store.workspaces"] = float64(len(st.GetWorkspaces()))
			counters["store.sessions_live"] = float64(len(liveAgentPIDsFromState(st.Get().Sessions)))
		}
	}
	if s.workspaceStreamer != nil {
		counters["logstream.workspace_tailers"] = float64(s.workspaceStreamer.ActiveTailers())
	}
	if s.logStreamer != nil {
		counters["logstream.job_tailers"] = float64(s.logStreamer.ActiveStreams())
	}

	// The ahead/behind cache lives as a package global in core/git (it has no
	// injectable handle and sits on GetExtendedStatus' hot path), so its
	// counters are exported the same way and read here.
	hits, misses, wasted := git.DivergenceCacheStats()
	counters["git.divergence_cache.hits"] = float64(hits)
	counters["git.divergence_cache.misses"] = float64(misses)
	counters["git.divergence_cache.wasted_forks"] = float64(wasted)
	if total := hits + misses; total > 0 {
		counters["git.divergence_cache.hit_rate"] = statsRound1(float64(hits) / float64(total) * 100)
	} else {
		counters["git.divergence_cache.hit_rate"] = 0
	}

	return counters
}

// liveAgentPIDs maps every live headless agent's pid to a display label, for
// the agent-subtree RSS budget.
func (s *Server) liveAgentPIDs() map[int]string {
	if s.engine == nil {
		return nil
	}
	st := s.engine.Store()
	if st == nil {
		return nil
	}
	return liveAgentPIDsFromState(st.Get().Sessions)
}

// liveAgentPIDsFromState is the pure half of liveAgentPIDs. A session counts
// when it has a real pid, has not ended, and is local (Origin != "" means it
// belongs to a satellite, whose pids are meaningless in our process table).
func liveAgentPIDsFromState(sessions map[string]*models.Session) map[int]string {
	out := make(map[int]string, len(sessions))
	for _, sess := range sessions {
		if sess == nil || sess.PID <= 0 || sess.EndedAt != nil || sess.Origin != "" {
			continue
		}
		label := sess.JobTitle
		if label == "" {
			label = sess.ID
		}
		if sess.PlanName != "" {
			label = sess.PlanName + "/" + label
		}
		out[sess.PID] = label
	}
	return out
}

// handleSystemStats serves GET /api/system/stats: the daemon's Go runtime
// state plus a process-tree rollup of its own pid (models.SystemStats).
// counters and warnings are reserved for R3 and always present-but-empty.
func (s *Server) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	startedAt := processStart
	if s.runningConfig != nil && !s.runningConfig.StartedAt.IsZero() {
		startedAt = s.runningConfig.StartedAt
	}

	stats := models.SystemStats{
		SampledAt: time.Now(),
		Runtime: models.RuntimeStats{
			Goroutines:     runtime.NumGoroutine(),
			HeapAlloc:      ms.HeapAlloc,
			HeapSys:        ms.HeapSys,
			NumGC:          ms.NumGC,
			GCPauseTotalMS: float64(ms.PauseTotalNs) / 1e6,
			GoMemLimit:     debug.SetMemoryLimit(-1),
			UptimeMS:       time.Since(startedAt).Milliseconds(),
		},
		Self:     models.SelfStats{PID: os.Getpid(), Children: []models.ProcStat{}},
		Counters: map[string]float64{},
		Warnings: []models.HealthWarning{},
		Budgets:  []models.Budget{},
	}

	sample, err := s.statsCache.get(statsSampleMaxAge)
	if sample != nil {
		stats.SampledAt = sample.At
		fillSelfStats(&stats.Self, sample, stats.Self.PID)
	}
	if err != nil {
		// Degrade: runtime block is still useful without the process table.
		s.ulog.Warn("system stats: process sample failed").
			Field("error", err.Error()).
			Log(r.Context())
	}

	// R3: counters, budgets, warnings. Order matters — budgets are evaluated
	// before warnings are read so an exceeded budget appears in the same
	// response that reports it, rather than one poll later.
	stats.Counters = s.collectCounters()
	stats.Budgets = telemetry.EvaluateBudgets(telemetry.BudgetInputs{
		HeapAlloc:  stats.Runtime.HeapAlloc,
		GoMemLimit: stats.Runtime.GoMemLimit,
		Goroutines: stats.Runtime.Goroutines,
		DaemonPID:  stats.Self.PID,
		Sample:     sample,
		AgentPIDs:  s.liveAgentPIDs(),
	})
	warnings := telemetry.Default().Warnings()
	telemetry.RaiseBudgetWarnings(warnings, stats.Budgets)
	telemetry.CheckWatcherStorm()
	stats.Warnings = warnings.Active()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}
