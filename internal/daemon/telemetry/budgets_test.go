package telemetry

import (
	"math"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/procsample"
)

// fixtureSample builds a small grove-shaped process tree:
//
//	1 launchd
//	├── 100 groved            (the daemon)
//	│   └── 101 git           (a normal helper child)
//	├── 200 tuimuxd
//	│   ├── 201 nvim          (pinned editor, owned)
//	│   └── 202 gopls         (owned)
//	├── 300 claude            (headless agent)
//	│   └── 301 node
//	└── 900 nvim              (ORPHAN: no tuimuxd ancestor)
//	    └── 901 gopls         (ORPHAN)
func fixtureSample() *procsample.Sample {
	procs := map[int]procsample.Proc{
		1:   {PID: 1, PPID: 0, Comm: "launchd"},
		100: {PID: 100, PPID: 1, Comm: "groved", RSSKB: 700_000},
		101: {PID: 101, PPID: 100, Comm: "git", RSSKB: 20_000},
		200: {PID: 200, PPID: 1, Comm: "tuimuxd", RSSKB: 400_000},
		201: {PID: 201, PPID: 200, Comm: "nvim", RSSKB: 180_000},
		202: {PID: 202, PPID: 201, Comm: "gopls", RSSKB: 640_000},
		300: {PID: 300, PPID: 1, Comm: "claude", RSSKB: 900_000},
		301: {PID: 301, PPID: 300, Comm: "node", RSSKB: 400_000},
		900: {PID: 900, PPID: 1, Comm: "nvim", RSSKB: 210_000},
		901: {PID: 901, PPID: 900, Comm: "gopls", RSSKB: 500_000},
	}
	return procsample.NewSample(procs, time.Unix(1_700_000_000, 0))
}

func TestEvaluateBudgetsCoversEveryDocSixClass(t *testing.T) {
	got := EvaluateBudgets(BudgetInputs{
		HeapAlloc:  1 << 30,
		GoMemLimit: 2 << 30,
		Goroutines: 412,
		DaemonPID:  100,
		Sample:     fixtureSample(),
		AgentPIDs:  map[int]string{300: "perf-audit/impl-r3"},
	})

	byName := map[string]float64{}
	classes := map[string]bool{}
	for _, b := range got {
		byName[b.Name] = b.Value
		classes[b.Class] = true
	}
	for _, class := range []string{"daemon", "agent", "editor", "pty"} {
		if !classes[class] {
			t.Errorf("no budget for doc 06 resource class %q: %+v", class, got)
		}
	}

	// heap 1G of 2G == 50%.
	if v := byName["daemon.heap_vs_gomemlimit"]; math.Abs(v-50) > 0.01 {
		t.Errorf("heap pct = %v, want 50", v)
	}
	// groved + git.
	if v := byName["daemon.subtree_procs"]; v != 2 {
		t.Errorf("daemon.subtree_procs = %v, want 2", v)
	}
	// claude + node RSS.
	if v := byName["agent.subtree_rss"]; v != 1_300_000 {
		t.Errorf("agent.subtree_rss = %v, want 1300000", v)
	}
	// nvim + gopls under tuimuxd.
	if v := byName["editor.pinned"]; v != 2 {
		t.Errorf("editor.pinned = %v, want 2", v)
	}
	// nvim + gopls with no tuimuxd ancestor.
	if v := byName["pty.orphans"]; v != 2 {
		t.Errorf("pty.orphans = %v, want 2", v)
	}
}

func TestOrphanBudgetIsExceededAtOne(t *testing.T) {
	got := EvaluateBudgets(BudgetInputs{DaemonPID: 100, Sample: fixtureSample()})
	for _, b := range got {
		if b.Name != "pty.orphans" {
			continue
		}
		if !b.Exceeded {
			t.Fatalf("2 orphans did not exceed the zero-orphan budget: %+v", b)
		}
		if b.Offender == "" {
			t.Error("exceeded orphan budget named no offender")
		}
		return
	}
	t.Fatal("pty.orphans budget missing")
}

func TestAgentOffenderIsTheLargestSubtree(t *testing.T) {
	procs := map[int]procsample.Proc{
		1:   {PID: 1, PPID: 0, Comm: "launchd"},
		300: {PID: 300, PPID: 1, Comm: "claude", RSSKB: 100},
		400: {PID: 400, PPID: 1, Comm: "claude", RSSKB: 9000},
	}
	got := EvaluateBudgets(BudgetInputs{
		DaemonPID: 1,
		Sample:    procsample.NewSample(procs, time.Unix(1, 0)),
		AgentPIDs: map[int]string{300: "small", 400: "big"},
	})
	for _, b := range got {
		if b.Name == "agent.subtree_rss" {
			if b.Offender != "big" {
				t.Fatalf("offender = %q, want \"big\"", b.Offender)
			}
			return
		}
	}
	t.Fatal("agent.subtree_rss budget missing")
}

// A missing process sample must not be reported as a healthy zero.
func TestNilSampleSkipsProcessBudgets(t *testing.T) {
	got := EvaluateBudgets(BudgetInputs{HeapAlloc: 1, GoMemLimit: 2, Goroutines: 1})
	for _, b := range got {
		if b.Class == "pty" || b.Class == "editor" || b.Class == "agent" {
			t.Fatalf("process-derived budget %q reported without a sample", b.Name)
		}
	}
	if len(got) == 0 {
		t.Fatal("runtime budgets should still be evaluated")
	}
}

// No GOMEMLIMIT means the heap budget is unevaluatable, not "0% of 0".
func TestUnsetGoMemLimitOmitsHeapBudget(t *testing.T) {
	got := EvaluateBudgets(BudgetInputs{HeapAlloc: 1 << 30, GoMemLimit: math.MaxInt64})
	for _, b := range got {
		if b.Name == "daemon.heap_vs_gomemlimit" {
			t.Fatal("heap budget evaluated against an unset GOMEMLIMIT")
		}
	}
}

func TestBudgetLimitEnvOverride(t *testing.T) {
	t.Setenv("GROVE_BUDGET_PTY_ORPHANS", "5")
	got := EvaluateBudgets(BudgetInputs{DaemonPID: 100, Sample: fixtureSample()})
	for _, b := range got {
		if b.Name == "pty.orphans" {
			if b.Limit != 5 || b.Exceeded {
				t.Fatalf("env override ignored: %+v", b)
			}
			return
		}
	}
	t.Fatal("pty.orphans budget missing")
}

func TestBudgetLimitIgnoresMalformedEnv(t *testing.T) {
	t.Setenv("GROVE_BUDGET_DAEMON_GOROUTINES", "not-a-number")
	if got := budgetLimit("daemon.goroutines", defaultGoroutines); got != defaultGoroutines {
		t.Fatalf("limit = %v, want the default %v", got, float64(defaultGoroutines))
	}
}

func TestRaiseBudgetWarningsMirrorsOnlyBreaches(t *testing.T) {
	l := NewWarningLedger()
	budgets := EvaluateBudgets(BudgetInputs{DaemonPID: 100, Sample: fixtureSample()})
	RaiseBudgetWarnings(l, budgets)

	active := l.Active()
	if len(active) == 0 {
		t.Fatal("no warning raised for the exceeded orphan budget")
	}
	for _, w := range active {
		if w.Condition == "" || w.Offender == "" {
			t.Errorf("incomplete warning: %+v", w)
		}
	}
	// Every in-budget row must NOT appear.
	if len(active) >= len(budgets) {
		t.Errorf("raised %d warnings for %d budgets — in-budget rows leaked", len(active), len(budgets))
	}
}
