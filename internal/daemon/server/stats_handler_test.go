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
	for _, key := range []string{"sampled_at", "runtime", "self", "counters", "warnings"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing key %q", key)
		}
	}
	if string(raw["counters"]) != "{}" {
		t.Errorf("counters = %s, want {}", raw["counters"])
	}
	if string(raw["warnings"]) != "[]" {
		t.Errorf("warnings = %s, want []", raw["warnings"])
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
