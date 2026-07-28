package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/procsample"
)

// fakeSample builds a procsample.Sample from exported fields only. The
// internal parent/child index stays empty, so every Rollup is root-only —
// deterministic and sufficient for assembly/formatting tests (subtree math
// is procsample's own test surface). Orphan ancestry exclusion still works:
// it walks Procs PPIDs, which are exported.
func fakeSample(at time.Time, procs map[int]procsample.Proc, cpu map[int]float64) *procsample.Sample {
	return &procsample.Sample{At: at, Procs: procs, CPU: cpu}
}

func testSampleFixture() *procsample.Sample {
	at := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	procs := map[int]procsample.Proc{
		100: {PID: 100, PPID: 1, Comm: "groved", RSSKB: 700 * 1024},
		150: {PID: 150, PPID: 1, Comm: "tuimuxd", RSSKB: 90 * 1024},
		200: {PID: 200, PPID: 150, Comm: "nvim", RSSKB: 512 * 1024},
		210: {PID: 210, PPID: 150, Comm: "fish", RSSKB: 8 * 1024},
		300: {PID: 300, PPID: 100, Comm: "pi", RSSKB: 256 * 1024},
		400: {PID: 400, PPID: 1, Comm: "treemux", RSSKB: 120 * 1024},
		500: {PID: 500, PPID: 1, Comm: "gopls", RSSKB: 900 * 1024},
		501: {PID: 501, PPID: 1, Comm: "pip", RSSKB: 10 * 1024},
		502: {PID: 502, PPID: 1, Comm: "git", RSSKB: 40 * 1024},
		503: {PID: 503, PPID: 1, Comm: "github-runner", RSSKB: 30 * 1024},
	}
	cpu := map[int]float64{
		100: 41.02, 150: 2.0, 200: 12.34, 210: 0.1, 300: 88.8,
		400: 1.5, 500: 7.77, 501: 0.2, 502: 3.0, 503: 9.9,
	}
	return fakeSample(at, procs, cpu)
}

func testProbesFixture() []daemonProbeResult {
	return []daemonProbeResult{
		{
			entry: daemonEntry{Scope: "", PID: 100, Running: true, SockPath: "/state/groved.sock", Age: 3 * time.Hour},
			ptys: []daemon.PTYSessionInfo{
				{
					ID: "abcdef0123456789", Workspace: "core", Label: "impl", PID: 200, AttachedClients: 1,
					Labels: map[string]string{"job_id": "job-42"},
				},
				{
					ID: "fedcba9876543210", Workspace: "daemon", PID: 210, AttachedClients: 0,
					Labels:       map[string]string{"type": "shell"},
					LastDetached: time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC),
				},
			},
			agents: []*models.Session{
				{ID: "sess-headless-1", PID: 300, WorkingDirectory: "/x/core", JobTitle: "sweeper"},
				{ID: "sess-in-pty", PID: 200, PtyID: "abcdef0123456789"}, // PTY-hosted: excluded
				{ID: "sess-dead", PID: 9999},                             // not in sample: excluded
				{ID: "sess-no-pid", PID: 0},                              // no process: excluded
			},
		},
		{entry: daemonEntry{Scope: "old-plan", PID: 4242, Running: false, SockPath: "/state/groved-old-plan-deadbeef.sock"}},
	}
}

func TestAssembleResourceDoc(t *testing.T) {
	sample := testSampleFixture()
	now := sample.At.Add(time.Second)
	hosts := []uiHostEntry{
		{PID: 400, Program: "treemux", Scope: "", SocketPath: "/state/groved.sock"},
		{PID: 9998, Program: "treemux", Scope: "/x/stale"}, // dead: skipped
	}

	doc := assembleResourceDoc(sample, 1002, testProbesFixture(), hosts, false, now)

	if doc.IntervalMS != 1002 {
		t.Errorf("interval_ms = %d, want 1002", doc.IntervalMS)
	}
	if len(doc.Daemons) != 2 {
		t.Fatalf("daemons = %d, want 2", len(doc.Daemons))
	}

	d := doc.Daemons[0]
	if !d.Running || d.PID != 100 {
		t.Fatalf("first daemon should be running pid 100, got %+v", d)
	}
	if d.Self == nil || d.Self.RootPID != 100 || d.Self.Procs != 1 {
		t.Errorf("self rollup wrong: %+v", d.Self)
	}
	if d.Self.CPUPct != 41.0 {
		t.Errorf("self cpu = %v, want rounded 41.0", d.Self.CPUPct)
	}
	if d.Tuimuxd == nil || d.Tuimuxd.RootPID != 150 {
		t.Errorf("tuimuxd should be detected from PTY root PPID, got %+v", d.Tuimuxd)
	}

	if len(d.PTYs) != 2 {
		t.Fatalf("ptys = %d, want 2", len(d.PTYs))
	}
	// Sorted CPU desc: nvim (12.3) before fish (0.1).
	if d.PTYs[0].RootPID != 200 || d.PTYs[1].RootPID != 210 {
		t.Errorf("ptys not sorted by CPU desc: %d then %d", d.PTYs[0].RootPID, d.PTYs[1].RootPID)
	}
	p := d.PTYs[0]
	if p.Label != "impl" || p.Workspace != "core" || p.CPUPct != 12.3 || p.rootComm != "nvim" {
		t.Errorf("pty row wrong: %+v", p)
	}
	if d.PTYs[1].Label != "shell" {
		t.Errorf("label fallback to Labels[type] failed: %q", d.PTYs[1].Label)
	}
	if got := d.PTYs[1].idle; got != time.Hour+time.Second {
		t.Errorf("idle = %v, want 1h1s (now - last_detached)", got)
	}

	if len(d.Agents) != 1 {
		t.Fatalf("agents = %d, want 1 (pty-hosted, dead and pid-less excluded)", len(d.Agents))
	}
	a := d.Agents[0]
	if a.SessionID != "sess-headless-1" || a.RootPID != 300 || a.Workspace != "core" || a.Label != "sweeper" {
		t.Errorf("agent row wrong: %+v", a)
	}

	if stale := doc.Daemons[1]; stale.Running || stale.Scope != "old-plan" || stale.Self != nil {
		t.Errorf("stale daemon should be running:false with no rollup: %+v", stale)
	}

	if len(doc.Hosts) != 1 || doc.Hosts[0].PID != 400 {
		t.Fatalf("hosts should keep only live registrations: %+v", doc.Hosts)
	}

	// Orphans: gopls (substring), git (exact) qualify; pip and github-runner
	// must be dropped by exact matching on the short comms; tracked pids
	// (groved, tuimuxd, ptys, agent, host) must be excluded.
	got := map[int]bool{}
	for _, o := range doc.Orphans {
		got[o.PID] = true
		if o.Reason != "unaccounted" {
			t.Errorf("orphan %d reason = %q", o.PID, o.Reason)
		}
	}
	if !got[500] || !got[502] {
		t.Errorf("expected orphans 500 (gopls) and 502 (git), got %v", got)
	}
	if got[501] || got[503] {
		t.Errorf("pip/github-runner must not match exact-only interest: %v", got)
	}
	if got[100] || got[150] || got[200] || got[300] || got[400] {
		t.Errorf("tracked pids leaked into orphans: %v", got)
	}
	// Orphans sorted CPU desc: gopls 7.8 before git 3.0.
	if len(doc.Orphans) == 2 && doc.Orphans[0].PID != 500 {
		t.Errorf("orphans not sorted CPU desc: first is %d", doc.Orphans[0].PID)
	}
}

func TestAssembleResourceDocDetailAndErrors(t *testing.T) {
	sample := testSampleFixture()
	probes := testProbesFixture()
	probes[0].err = errFake("dial unix: connection refused")

	doc := assembleResourceDoc(sample, 1000, probes, nil, true, sample.At)

	d := doc.Daemons[0]
	if d.Error == "" || !strings.Contains(d.Error, "connection refused") {
		t.Errorf("probe error not surfaced: %q", d.Error)
	}
	// Rollups still present despite the probe error.
	if d.Self == nil || d.Self.RootPID != 100 {
		t.Errorf("self rollup missing on errored daemon: %+v", d.Self)
	}
	if len(d.Self.ProcsDetail) != 1 || d.Self.ProcsDetail[0].PID != 100 {
		t.Errorf("--detail should populate procs_detail: %+v", d.Self.ProcsDetail)
	}
	if len(d.PTYs) == 0 || d.PTYs[0].ProcsDetail == nil {
		t.Errorf("--detail should populate pty procs_detail")
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestFilterDocScope(t *testing.T) {
	mk := func() *resourceDoc {
		return &resourceDoc{Daemons: []resourceDaemon{
			{Scope: "", PID: 1, Running: true},
			{Scope: "plan-a", PID: 2, Running: true},
		}}
	}

	doc := mk()
	filterDocScope(doc, "plan-a")
	if len(doc.Daemons) != 1 || doc.Daemons[0].Scope != "plan-a" {
		t.Errorf("scope filter failed: %+v", doc.Daemons)
	}

	for _, alias := range []string{"unscoped", "global"} {
		doc = mk()
		filterDocScope(doc, alias)
		if len(doc.Daemons) != 1 || doc.Daemons[0].Scope != "" {
			t.Errorf("%q should select the unscoped daemon: %+v", alias, doc.Daemons)
		}
	}

	doc = mk()
	filterDocScope(doc, "nope")
	if len(doc.Daemons) != 0 {
		t.Errorf("unknown scope should filter everything: %+v", doc.Daemons)
	}
}

func TestOrphanInterestMatch(t *testing.T) {
	cases := map[string]bool{
		"pi":            true,
		"git":           true,
		"pip":           false,
		"pinentry":      false,
		"github-runner": false,
		"nvim":          true,
		"gopls":         true,
		"claude":        true,
		"claude-code":   true,
		"hash-object":   true,
		"Git":           true, // case-insensitive
		"fish":          false,
	}
	for comm, want := range cases {
		if got := orphanInterestMatch(comm); got != want {
			t.Errorf("orphanInterestMatch(%q) = %v, want %v", comm, got, want)
		}
	}
}

func TestRenderResourceTable(t *testing.T) {
	sample := testSampleFixture()
	hosts := []uiHostEntry{{PID: 400, Program: "treemux", Scope: "", SocketPath: "/state/groved.sock"}}
	doc := assembleResourceDoc(sample, 1002, testProbesFixture(), hosts, false, sample.At.Add(time.Second))

	var b strings.Builder
	renderResourceTable(&b, doc, false)
	out := b.String()

	for _, want := range []string{
		"sampled 12:00:00  interval 1002ms",
		"(unscoped)  pid 100  up 3h00m  groved.sock",
		"self: pid 100  cpu 41.0%",
		"tuimuxd: pid 150",
		"TOP OFFENDER",
		"abcdef01",   // pty id truncated to 8
		"sess-hea",   // agent session id truncated to 8
		"idle 1h00m", // unattached pty idle age
		"TUI HOSTS",
		"treemux",
		"ORPHANS",
		"gopls",
		"unaccounted",
		"STALE DAEMONS",
		"old-plan",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "pip") {
		t.Errorf("pip must not appear as an orphan:\n%s", out)
	}
	// Row sort: the hot agent (88.8) must precede the nvim pty (12.3).
	if ai, pi := strings.Index(out, "sess-hea"), strings.Index(out, "abcdef01"); ai > pi {
		t.Errorf("rows not sorted CPU desc (agent at %d, pty at %d)\n%s", ai, pi, out)
	}
}

func TestRenderOrphansOnlyEmpty(t *testing.T) {
	var b strings.Builder
	renderOrphansSection(&b, nil)
	if !strings.Contains(b.String(), "(none)") {
		t.Errorf("empty orphans should render (none): %q", b.String())
	}
}

func TestFmtHelpers(t *testing.T) {
	if got := fmtRSS(512); got != "512K" {
		t.Errorf("fmtRSS(512) = %q", got)
	}
	if got := fmtRSS(512 * 1024); got != "512.0M" {
		t.Errorf("fmtRSS(512M) = %q", got)
	}
	if got := fmtRSS(3 * 1024 * 1024); got != "3.00G" {
		t.Errorf("fmtRSS(3G) = %q", got)
	}
	if got := shortDur(42 * time.Second); got != "42s" {
		t.Errorf("shortDur(42s) = %q", got)
	}
	if got := shortDur(17 * time.Minute); got != "17m" {
		t.Errorf("shortDur(17m) = %q", got)
	}
	if got := shortDur(3*time.Hour + 12*time.Minute); got != "3h12m" {
		t.Errorf("shortDur(3h12m) = %q", got)
	}
	if got := shortDur(50 * time.Hour); got != "2d2h" {
		t.Errorf("shortDur(50h) = %q", got)
	}
	if got := fmtTopOffender(nil); got != "-" {
		t.Errorf("fmtTopOffender(nil) = %q", got)
	}
}

func TestReadUIHosts(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("host-400.json", `{"pid":400,"program":"treemux","scope":"","socket_path":"/s/groved.sock"}`)
	write("host-bad.json", `{not json`)
	write("host-0.json", `{"pid":0,"program":"x"}`)

	hosts := readUIHosts(dir)
	if len(hosts) != 1 || hosts[0].PID != 400 || hosts[0].Program != "treemux" {
		t.Errorf("readUIHosts = %+v, want single pid-400 entry", hosts)
	}
	if got := readUIHosts(dir + "/missing"); got != nil {
		t.Errorf("missing dir should yield nil, got %+v", got)
	}
}
