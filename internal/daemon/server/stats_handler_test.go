package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/procsample"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// TestHandleSystemStats exercises the real handler end-to-end (including one
// real `ps` exec via procsample) and pins the response contract: runtime
// block filled, self rooted at our own pid, reserved counters/warnings
// present as empty containers.
func TestHandleSystemStats(t *testing.T) {
	s := New(false)

	req := httptest.NewRequest(http.MethodGet, "/api/system/stats", nil)
	w := httptest.NewRecorder()
	s.handleSystemStats(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, w.Body.String())
	}

	// Reserved fields must be present in the raw JSON even though empty.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	for _, key := range []string{"sampled_at", "runtime", "self", "counters", "warnings", "budgets"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing key %q", key)
		}
	}
	// R3: counters/warnings/budgets are containers, never null — a client
	// must be able to range over them without a nil check.
	if string(raw["counters"]) == "null" {
		t.Error("counters serialized as null")
	}
	if string(raw["warnings"]) == "null" {
		t.Error("warnings serialized as null")
	}
	if string(raw["budgets"]) == "null" {
		t.Error("budgets serialized as null")
	}

	var stats models.SystemStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if stats.Runtime.Goroutines <= 0 {
		t.Errorf("goroutines = %d, want > 0", stats.Runtime.Goroutines)
	}
	if stats.Runtime.HeapAlloc == 0 || stats.Runtime.HeapSys == 0 {
		t.Errorf("heap stats zero: %+v", stats.Runtime)
	}
	if stats.Runtime.GoMemLimit == 0 {
		t.Errorf("gomemlimit = 0, want MaxInt64 (unlimited) or a real limit")
	}
	if stats.Runtime.UptimeMS < 0 {
		t.Errorf("uptime_ms = %d, want >= 0", stats.Runtime.UptimeMS)
	}
	if stats.Self.PID != os.Getpid() {
		t.Errorf("self.pid = %d, want %d", stats.Self.PID, os.Getpid())
	}
	// The test binary itself must appear in the sample: procs >= 1.
	if stats.Self.Procs < 1 {
		t.Errorf("self.procs = %d, want >= 1", stats.Self.Procs)
	}
	if stats.Self.Children == nil {
		t.Error("self.children is null, want []")
	}
}

func TestHandleSystemStatsMethodNotAllowed(t *testing.T) {
	s := New(false)
	req := httptest.NewRequest(http.MethodPost, "/api/system/stats", nil)
	w := httptest.NewRecorder()
	s.handleSystemStats(w, req)
	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Result().StatusCode)
	}
}

// TestProcStatsCacheReuse proves the 2s refresh-on-demand policy: requests
// inside the max-age window share one sample, a stale cache re-samples, and
// a sampling error degrades to the previous sample instead of dropping it.
func TestProcStatsCacheReuse(t *testing.T) {
	calls := 0
	c := &procStatsCache{sampleFn: func() (*procsample.Sample, error) {
		calls++
		return &procsample.Sample{At: time.Now()}, nil
	}}

	s1, err := c.get(statsSampleMaxAge)
	if err != nil || s1 == nil {
		t.Fatalf("first get: sample=%v err=%v", s1, err)
	}
	s2, err := c.get(statsSampleMaxAge)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (second get within max age must reuse)", calls)
	}
	if s1 != s2 {
		t.Error("second get returned a different sample than the cached one")
	}

	// maxAge 0 forces a refresh.
	if _, err := c.get(0); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 after forced refresh", calls)
	}

	// Sampling failure returns the previous sample alongside the error.
	c.sampleFn = func() (*procsample.Sample, error) {
		return nil, os.ErrDeadlineExceeded
	}
	stale, err := c.get(0)
	if err == nil {
		t.Fatal("want error from failing sampler")
	}
	if stale == nil {
		t.Error("want previous sample retained on sampling failure, got nil")
	}
}

// TestTopChildren pins ordering (CPU desc, RSS tiebreak), root exclusion,
// and the cap.
func TestTopChildren(t *testing.T) {
	procs := map[int]procsample.Proc{}
	cpu := map[int]float64{}
	pids := []int{100} // root
	procs[100] = procsample.Proc{PID: 100, Comm: "groved", RSSKB: 1000}
	cpu[100] = 50
	for i := 0; i < 25; i++ {
		pid := 200 + i
		procs[pid] = procsample.Proc{PID: pid, Comm: "child", RSSKB: int64(1000 - i)}
		cpu[pid] = float64(i) // hottest is the last one
		pids = append(pids, pid)
	}
	sample := &procsample.Sample{At: time.Now(), Procs: procs, CPU: cpu}

	rows := topChildren(sample, pids, 100, 20)
	if len(rows) != 20 {
		t.Fatalf("len = %d, want 20 (cap)", len(rows))
	}
	for _, r := range rows {
		if r.PID == 100 {
			t.Error("root pid leaked into children")
		}
	}
	if rows[0].PID != 224 {
		t.Errorf("hottest child = pid %d, want 224", rows[0].PID)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].CPUPct > rows[i-1].CPUPct {
			t.Fatalf("rows not sorted by CPU desc at %d", i)
		}
	}
}

// TestHandleSystemStatsFillsCountersAndBudgets pins R3's contribution to the
// endpoint: the counters map carries the telemetry registry plus the pulled
// gauges, and budgets are evaluated SERVER-side so every client reads one
// verdict.
func TestHandleSystemStatsFillsCountersAndBudgets(t *testing.T) {
	telemetry.RecordGitSweep("test-scope", 42, 30*time.Millisecond)

	s := New(false)
	req := httptest.NewRequest(http.MethodGet, "/api/system/stats", nil)
	w := httptest.NewRecorder()
	s.handleSystemStats(w, req)

	var stats models.SystemStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Recorded counters surface verbatim under their dotted names.
	if stats.Counters["git.sweep.count"] < 1 {
		t.Errorf("git.sweep.count = %v, want >= 1 (counters: %v)", stats.Counters["git.sweep.count"], stats.Counters)
	}
	if stats.Counters["git.sweep.workspaces"] < 42 {
		t.Errorf("git.sweep.workspaces = %v, want >= 42", stats.Counters["git.sweep.workspaces"])
	}
	// Pulled-at-request-time gauges are present even with no engine wired.
	if _, ok := stats.Counters["git.divergence_cache.hit_rate"]; !ok {
		t.Error("divergence cache counters missing")
	}

	// Budgets always include the runtime classes; the process-derived ones
	// need a sample, which this handler takes for real.
	if len(stats.Budgets) == 0 {
		t.Fatal("no budgets evaluated")
	}
	seen := map[string]bool{}
	for _, b := range stats.Budgets {
		seen[b.Name] = true
		if b.Unit == "" || b.Class == "" {
			t.Errorf("budget %+v missing class/unit", b)
		}
		if b.Exceeded != (b.Value > b.Limit) {
			t.Errorf("budget %+v: Exceeded disagrees with Value/Limit", b)
		}
	}
	if !seen["daemon.goroutines"] {
		t.Errorf("daemon.goroutines budget missing: %+v", stats.Budgets)
	}
}

// A daemon with no engine wired (the --ready-at=bind window, and every test
// server) must still answer with counters rather than 500ing on a nil store.
func TestHandleSystemStatsWithoutEngine(t *testing.T) {
	s := New(false)
	if s.engine != nil {
		t.Fatal("fixture assumed no engine")
	}
	counters := s.collectCounters()
	if len(counters) == 0 {
		t.Fatal("no counters without an engine")
	}
	if _, ok := counters["store.focused_workspaces"]; ok {
		t.Error("store counters reported without a store")
	}
	if pids := s.liveAgentPIDs(); pids != nil {
		t.Errorf("liveAgentPIDs = %v, want nil without an engine", pids)
	}
}

func TestLiveAgentPIDsFromState(t *testing.T) {
	ended := time.Now()
	got := liveAgentPIDsFromState(map[string]*models.Session{
		"live":    {ID: "live", PID: 100, PlanName: "perf-audit", JobTitle: "impl-r3"},
		"nopid":   {ID: "nopid", PID: 0},
		"ended":   {ID: "ended", PID: 200, EndedAt: &ended},
		"remote":  {ID: "remote", PID: 300, Origin: "satellite-1"},
		"minimal": {ID: "minimal", PID: 400},
		"nil":     nil,
	})
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 entries", got)
	}
	if got[100] != "perf-audit/impl-r3" {
		t.Errorf("label = %q", got[100])
	}
	// No title/plan falls back to the session id, never an empty label.
	if got[400] != "minimal" {
		t.Errorf("fallback label = %q", got[400])
	}
}
