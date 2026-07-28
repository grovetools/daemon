package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func testStatsFixture() *models.SystemStats {
	return &models.SystemStats{
		SampledAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		Runtime: models.RuntimeStats{
			Goroutines:     412,
			HeapAlloc:      128 << 20, // 128M
			HeapSys:        256 << 20,
			NumGC:          184,
			GCPauseTotalMS: 1200.5,
			GoMemLimit:     2 << 30, // 2G
			UptimeMS:       (3*time.Hour + 12*time.Minute).Milliseconds(),
		},
		Self: models.SelfStats{
			PID:    27462,
			CPUPct: 41.0,
			RSSKB:  710 * 1024,
			Procs:  63,
			Top:    &models.ProcStat{PID: 999, Comm: "git", CPUPct: 12.5, RSSKB: 40 * 1024},
			Children: []models.ProcStat{
				{PID: 999, Comm: "git", CPUPct: 12.5, RSSKB: 40 * 1024},
				{PID: 1000, Comm: "gopls", CPUPct: 7.7, RSSKB: 900 * 1024},
				{PID: 1001, Comm: "pi", CPUPct: 3.0, RSSKB: 256 * 1024},
				{PID: 1002, Comm: "fish", CPUPct: 1.0, RSSKB: 8 * 1024},
				{PID: 1003, Comm: "nvim", CPUPct: 0.9, RSSKB: 512 * 1024},
				{PID: 1004, Comm: "git", CPUPct: 0.1, RSSKB: 4 * 1024},
			},
		},
		Counters: map[string]float64{},
		Warnings: []models.HealthWarning{},
	}
}

func testStatsProbes() []statsProbeResult {
	return []statsProbeResult{
		{
			entry: daemonEntry{Scope: "", PID: 27462, Running: true, SockPath: "/state/groved.sock", Age: 3 * time.Hour},
			stats: testStatsFixture(),
		},
		{
			// Old daemon: socket up, endpoint 404s.
			entry: daemonEntry{Scope: "grovetools", PID: 300, Running: true, SockPath: "/state/groved-grovetools-abc.sock", Age: time.Hour},
			err:   fmt.Errorf("wrapped: %w", errors.New("daemon endpoint not found (stale groved binary?)")),
		},
		{
			// Stale pidfile, daemon down.
			entry: daemonEntry{Scope: "old", PID: 999, Running: false, SockPath: "/state/groved-old-def.sock"},
		},
	}
}

// TestStatsEndpointUnavailableMessage proves the real 404 path end-to-end:
// probeStats against a socket that serves no /api/system/stats route yields
// core's endpoint-not-found sentinel, and assembleStatsDoc maps it to the
// human-facing statsEndpointUnavailable message.
func TestStatsEndpointUnavailableMessage(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "stats404")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "s") // short path: macOS caps sun_path

	ul, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go srv.Serve(ul)
	t.Cleanup(func() { srv.Close(); ul.Close() })

	entry := daemonEntry{Scope: "old", PID: 42, Running: true, SockPath: sockPath}
	_, probeErr := probeStats(context.Background(), entry)
	if probeErr == nil {
		t.Fatal("want error from stale daemon")
	}

	doc := assembleStatsDoc([]statsProbeResult{{entry: entry, err: probeErr}})
	if doc.Daemons[0].Error != statsEndpointUnavailable {
		t.Fatalf("error = %q, want %q", doc.Daemons[0].Error, statsEndpointUnavailable)
	}
}

// TestAssembleStatsDoc pins the JSON document shape and per-daemon error
// handling.
func TestAssembleStatsDoc(t *testing.T) {
	doc := assembleStatsDoc(testStatsProbes())

	if len(doc.Daemons) != 3 {
		t.Fatalf("daemons = %d, want 3", len(doc.Daemons))
	}
	d0 := doc.Daemons[0]
	if d0.Stats == nil || d0.Stats.Runtime.Goroutines != 412 {
		t.Errorf("running daemon stats missing: %+v", d0)
	}
	d1 := doc.Daemons[1]
	if d1.Stats != nil {
		t.Errorf("errored daemon should have no stats: %+v", d1)
	}
	if d1.Error == "" {
		t.Error("errored daemon should carry an error string")
	}
	d2 := doc.Daemons[2]
	if d2.Running {
		t.Error("stale daemon should be running=false")
	}

	// JSON contract: snake_case fields, stats nested intact.
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"daemons"`, `"scope"`, `"pid"`, `"running"`, `"socket"`, `"error"`,
		`"stats"`, `"sampled_at"`, `"runtime"`, `"goroutines"`, `"heap_alloc"`,
		`"heap_sys"`, `"num_gc"`, `"gc_pause_total_ms"`, `"gomemlimit"`,
		`"uptime_ms"`, `"self"`, `"cpu_pct"`, `"rss_kb"`, `"procs"`, `"top"`,
		`"children"`, `"counters"`, `"warnings"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("JSON missing %s", want)
		}
	}
}

// TestRenderStatsTable pins the human-facing rendering: runtime line with
// heap/GOMEMLIMIT percentage, self line, children line, the
// endpoint-unavailable message for old daemons, and the stale section.
func TestRenderStatsTable(t *testing.T) {
	probes := testStatsProbes()
	// Make the old-daemon row deterministic: assembleStatsDoc renders
	// unrecognized errors verbatim; the 404 mapping itself is asserted in
	// TestStatsEndpointUnavailableMessage below.
	doc := assembleStatsDoc(probes)
	doc.Daemons[1].Error = statsEndpointUnavailable

	var buf bytes.Buffer
	renderStatsTable(&buf, doc)
	out := buf.String()

	for _, want := range []string{
		"pid 27462",
		"heap 128.0M / 2.00G (6.2%)",
		"goroutines 412",
		"gc 184 runs / 1.20s pause",
		"uptime 3h12m",
		"cpu 41.0%",
		"rss 710.0M",
		"procs 63",
		"top git(999) 12.5% 40.0M",
		"git(999) 12.5% 40.0M · gopls(1000) 7.7% 900.0M",
		"(+1 more)",
		statsEndpointUnavailable,
		"STALE DAEMONS",
		"last pid 999",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n---\n%s", want, out)
		}
	}
}

// TestFmtHeapLimitUnlimited pins the no-GOMEMLIMIT rendering (the runtime
// reports MaxInt64 when no limit is set).
func TestFmtHeapLimitUnlimited(t *testing.T) {
	got := fmtHeapLimit(128<<20, math.MaxInt64)
	if got != "heap 128.0M (no GOMEMLIMIT)" {
		t.Errorf("got %q", got)
	}
}

// TestFilterStatsScope mirrors the resources scope filter semantics.
func TestFilterStatsScope(t *testing.T) {
	doc := assembleStatsDoc(testStatsProbes())
	filterStatsScope(doc, "grovetools")
	if len(doc.Daemons) != 1 || doc.Daemons[0].Scope != "grovetools" {
		t.Fatalf("scope filter failed: %+v", doc.Daemons)
	}

	doc = assembleStatsDoc(testStatsProbes())
	filterStatsScope(doc, "global")
	if len(doc.Daemons) != 1 || doc.Daemons[0].Scope != "" {
		t.Fatalf("global alias failed: %+v", doc.Daemons)
	}
}
