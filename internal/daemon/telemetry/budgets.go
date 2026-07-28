package telemetry

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/procsample"
)

// Budgets implement doc 06's "resource classes and budgets for spawned
// tools". They are evaluated HERE, in the daemon, and shipped as verdicts on
// /api/system/stats so the TUI, `groved stats`, and an agent curling the
// socket cannot disagree about whether something is over budget.
//
// Every limit is overridable by environment variable (see budgetLimit) so a
// machine with a genuinely large fleet can raise a bar without a rebuild and
// without the daemon growing a config surface for it.
const (
	envBudgetPrefix = "GROVE_BUDGET_"

	// budgetHeapPct: percentage of GOMEMLIMIT at which the daemon is close
	// enough to the soft limit that GC pressure becomes the story. The 8.5 GB
	// groved incident in this plan showed ~710 MB RSS while holding 8.9 GB of
	// footprint — RSS never flagged it; heap-vs-limit does.
	defaultHeapPct = 80

	// budgetGoroutines: 480 log tailers parked in retry loops was this plan's
	// goroutine incident; a healthy daemon sits in the low hundreds.
	defaultGoroutines = 2000

	// budgetDaemonProcs: the hash-object storm ran 60-84 concurrent children.
	// A daemon with a handful of git helpers is normal; dozens is a storm.
	defaultDaemonProcs = 24

	// budgetAgentRSSKB: a single agent subtree (claude + node helpers) past
	// this is worth attributing before the machine starts swapping.
	defaultAgentRSSKB = 2 * 1024 * 1024 // 2 GiB

	// budgetEditors: doc 06's LRU cap on pinned per-file editors. Diff opens
	// spawn one pinned full nvim (+gopls) per file with no cap today.
	defaultEditors = 12

	// budgetOrphans: doc 06's acceptance criterion is literally zero — no
	// orphan surviving a pane close, a peek close, a reload, or a reaper pass.
	defaultOrphans = 0
)

// budgetLimit reads GROVE_BUDGET_<NAME> (uppercased, dots→underscores),
// falling back to def. A malformed value is ignored rather than fatal: a
// typo in an env var must never stop the daemon reporting its health.
func budgetLimit(name string, def float64) float64 {
	key := envBudgetPrefix + strings.ToUpper(strings.NewReplacer(".", "_").Replace(name))
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// BudgetInputs is everything EvaluateBudgets needs. It is passed explicitly
// (rather than read from globals) so the evaluator is a pure function of a
// single process-table sample plus the runtime numbers the caller already
// collected — no second `ps`, no store lock held across evaluation.
type BudgetInputs struct {
	HeapAlloc  uint64
	GoMemLimit int64
	Goroutines int
	// DaemonPID roots the daemon's own subtree (its git children).
	DaemonPID int
	// Sample is the shared process-table snapshot; nil disables every
	// process-derived budget rather than reporting zeros as "healthy".
	Sample *procsample.Sample
	// AgentPIDs maps a live headless agent's pid to a display label
	// (job id / plan). Used for the agent-subtree RSS budget's offender.
	AgentPIDs map[int]string
}

// editor comms are the doc 06 helper-process-heavy class: the editor itself
// plus the LSP server its config autostarts, which is the process that
// actually escapes the PTY's process group.
var editorComms = []string{"nvim", "gopls"}

// muxComms identify the PTY-owning daemon. Anything in editorComms that is
// NOT under one of these is, by doc 06's definition, an orphan: its owning
// PTY is gone but it kept running in its own process group.
var muxComms = []string{"tuimuxd", "tuimux"}

// EvaluateBudgets returns every budget, exceeded or not. Never returns nil.
func EvaluateBudgets(in BudgetInputs) []models.Budget {
	out := make([]models.Budget, 0, 6)

	// --- daemon class -----------------------------------------------------
	if in.GoMemLimit > 0 && in.GoMemLimit != math.MaxInt64 {
		pct := float64(in.HeapAlloc) / float64(in.GoMemLimit) * 100
		out = append(out, budget("daemon.heap_vs_gomemlimit", "daemon", round2(pct),
			budgetLimit("daemon.heap_vs_gomemlimit", defaultHeapPct), "pct",
			fmt.Sprintf("heap %s of %s", humanBytes(int64(in.HeapAlloc)), humanBytes(in.GoMemLimit))))
	}
	out = append(out, budget("daemon.goroutines", "daemon", float64(in.Goroutines),
		budgetLimit("daemon.goroutines", defaultGoroutines), "count", ""))

	if in.Sample == nil {
		return out
	}

	// --- daemon subprocess storm -----------------------------------------
	self := in.Sample.Rollup(in.DaemonPID)
	offender := ""
	if self.Procs > 0 && self.Top.PID != 0 {
		offender = fmt.Sprintf("%s(%d)", self.Top.Comm, self.Top.PID)
	}
	out = append(out, budget("daemon.subtree_procs", "daemon", float64(self.Procs),
		budgetLimit("daemon.subtree_procs", defaultDaemonProcs), "count", offender))

	// --- agent class ------------------------------------------------------
	var worstRSS int64
	worstLabel := ""
	for pid, label := range in.AgentPIDs {
		r := in.Sample.Rollup(pid)
		if r.Procs == 0 {
			continue
		}
		if r.RSSKB > worstRSS {
			worstRSS, worstLabel = r.RSSKB, label
		}
	}
	out = append(out, budget("agent.subtree_rss", "agent", float64(worstRSS),
		budgetLimit("agent.subtree_rss", defaultAgentRSSKB), "kb", worstLabel))

	// --- editor / pty classes --------------------------------------------
	muxOwned := map[int]bool{}
	for pid, p := range in.Sample.Procs {
		if !commMatches(p.Comm, muxComms) {
			continue
		}
		for _, d := range in.Sample.Subtree(pid) {
			muxOwned[d] = true
		}
	}
	daemonOwned := map[int]bool{}
	for _, d := range self.Pids {
		daemonOwned[d] = true
	}

	pinned, orphans := 0, 0
	var orphanNames []string
	for pid, p := range in.Sample.Procs {
		if !commMatches(p.Comm, editorComms) {
			continue
		}
		switch {
		case muxOwned[pid]:
			pinned++
		case daemonOwned[pid]:
			// Spawned by us on purpose (e.g. a --remote-wait client): owned,
			// not orphaned.
		default:
			orphans++
			if len(orphanNames) < 3 {
				orphanNames = append(orphanNames, fmt.Sprintf("%s(%d)", p.Comm, pid))
			}
		}
	}
	out = append(out, budget("editor.pinned", "editor", float64(pinned),
		budgetLimit("editor.pinned", defaultEditors), "count", ""))
	out = append(out, budget("pty.orphans", "pty", float64(orphans),
		budgetLimit("pty.orphans", defaultOrphans), "count", strings.Join(orphanNames, " ")))

	return out
}

// RaiseBudgetWarnings mirrors every exceeded budget into the warning ledger,
// so the single strip at the top of the inspector (and `groved stats`) shows
// one list of "things that are wrong" rather than making the user cross-read
// two tables.
func RaiseBudgetWarnings(l *WarningLedger, budgets []models.Budget) {
	for _, b := range budgets {
		cond := fmt.Sprintf(CondBudgetExceededFn, b.Name)
		if !b.Exceeded {
			l.Clear(b.Class, cond)
			continue
		}
		detail := fmt.Sprintf("%s %s of %s", trimFloat(b.Value), b.Unit, trimFloat(b.Limit))
		if b.Offender != "" {
			detail += " — " + b.Offender
		}
		l.Raise(b.Class, cond, detail)
	}
}

func budget(name, class string, value, limit float64, unit, offender string) models.Budget {
	return models.Budget{
		Name:     name,
		Class:    class,
		Value:    value,
		Limit:    limit,
		Unit:     unit,
		Exceeded: value > limit,
		Offender: offender,
	}
}

// commMatches reports whether comm contains any of the wanted names
// (case-insensitive substring, matching procsample's interest semantics —
// "nvim" must match "nvim", ".nvim-wrapped" and "nvim --headless").
func commMatches(comm string, wanted []string) bool {
	lc := strings.ToLower(comm)
	for _, w := range wanted {
		if strings.Contains(lc, w) {
			return true
		}
	}
	return false
}

// trimFloat renders a budget number without trailing ".00" noise.
func trimFloat(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}
