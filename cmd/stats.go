package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sync"
	"time"

	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/spf13/cobra"
)

// statsEndpointUnavailable is rendered for daemons whose binary predates
// GET /api/system/stats (the socket answers, but 404s the route).
const statsEndpointUnavailable = "stats endpoint unavailable (daemon predates /api/system/stats)"

// ---------------------------------------------------------------------------
// JSON document shapes. Stable snake_case contract, same rules as resources.
// ---------------------------------------------------------------------------

// statsDaemon is one enumerated groved with its /api/system/stats payload.
type statsDaemon struct {
	Scope   string `json:"scope"`
	PID     int    `json:"pid"`
	Running bool   `json:"running"`
	Socket  string `json:"socket"`
	// Error carries a per-daemon probe failure (socket dead, stale binary
	// without the endpoint, ...); Stats is nil in that case.
	Error string              `json:"error,omitempty"`
	Stats *models.SystemStats `json:"stats,omitempty"`

	age time.Duration // table only: pidfile age
}

// statsDoc is the whole `groved stats --json` document.
type statsDoc struct {
	Daemons []statsDaemon `json:"daemons"`
}

// ---------------------------------------------------------------------------
// Gathering
// ---------------------------------------------------------------------------

// statsProbeResult is one daemon's endpoint response (or failure).
type statsProbeResult struct {
	entry daemonEntry
	stats *models.SystemStats
	err   error
}

// probeStatsFn is the socket-probe seam; tests replace it with a fake.
var probeStatsFn = probeStats

// probeStats queries one running daemon's GET /api/system/stats.
func probeStats(ctx context.Context, e daemonEntry) (*models.SystemStats, error) {
	client, err := daemon.NewRemoteClient(e.SockPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return client.GetSystemStats(ctx)
}

// gatherStatsProbes probes every running daemon in parallel (each bounded by
// probeStats' own timeout, so one hung daemon can't stall the fleet view).
func gatherStatsProbes(ctx context.Context, entries []daemonEntry) []statsProbeResult {
	results := make([]statsProbeResult, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		results[i].entry = e
		if !e.Running {
			continue
		}
		wg.Add(1)
		go func(i int, e daemonEntry) {
			defer wg.Done()
			results[i].stats, results[i].err = probeStatsFn(ctx, e)
		}(i, e)
	}
	wg.Wait()
	return results
}

// assembleStatsDoc converts probe results into the output document. Pure so
// tests can drive it with fakes. Old daemons (404 on the endpoint) get the
// statsEndpointUnavailable error string and the fleet view continues.
func assembleStatsDoc(probes []statsProbeResult) *statsDoc {
	doc := &statsDoc{Daemons: []statsDaemon{}}
	for _, pr := range probes {
		d := statsDaemon{
			Scope:   pr.entry.Scope,
			PID:     pr.entry.PID,
			Running: pr.entry.Running,
			Socket:  pr.entry.SockPath,
			Stats:   pr.stats,
			age:     pr.entry.Age,
		}
		if pr.err != nil {
			if daemon.IsEndpointNotFound(pr.err) {
				d.Error = statsEndpointUnavailable
			} else {
				d.Error = trimStatusError(pr.err.Error())
			}
		}
		doc.Daemons = append(doc.Daemons, d)
	}
	return doc
}

// filterStatsScope keeps only the daemon whose scope label matches;
// "unscoped"/"global" select the unscoped daemon.
func filterStatsScope(doc *statsDoc, scope string) {
	if scope == "unscoped" || scope == "global" {
		scope = ""
	}
	kept := doc.Daemons[:0]
	for _, d := range doc.Daemons {
		if d.Scope == scope {
			kept = append(kept, d)
		}
	}
	doc.Daemons = kept
}

// ---------------------------------------------------------------------------
// Table rendering
// ---------------------------------------------------------------------------

// fmtHeapBytes renders a byte count with the same units as fmtRSS.
func fmtHeapBytes(b uint64) string {
	return fmtRSS(int64(b / 1024))
}

// fmtHeapLimit renders "heap X / LIMIT (P%)"; a gomemlimit of MaxInt64
// means no limit was set.
func fmtHeapLimit(heapAlloc uint64, limit int64) string {
	if limit <= 0 || limit == math.MaxInt64 {
		return fmt.Sprintf("heap %s (no GOMEMLIMIT)", fmtHeapBytes(heapAlloc))
	}
	pct := float64(heapAlloc) / float64(limit) * 100
	return fmt.Sprintf("heap %s / %s (%.1f%%)", fmtHeapBytes(heapAlloc), fmtRSS(limit/1024), pct)
}

// fmtPause renders a cumulative GC pause total: sub-second in ms, else seconds.
func fmtPause(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.2fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

// statsChildrenShown caps the children shown in the table; --json carries the
// full server-side list (up to 20).
const statsChildrenShown = 5

func fmtChildren(children []models.ProcStat) string {
	if len(children) == 0 {
		return "(none)"
	}
	s := ""
	n := len(children)
	shown := n
	if shown > statsChildrenShown {
		shown = statsChildrenShown
	}
	for i := 0; i < shown; i++ {
		c := children[i]
		if i > 0 {
			s += " · "
		}
		s += fmt.Sprintf("%s(%d) %.1f%% %s", c.Comm, c.PID, c.CPUPct, fmtRSS(c.RSSKB))
	}
	if n > shown {
		s += fmt.Sprintf("  (+%d more)", n-shown)
	}
	return s
}

// renderStatsTable renders the human-facing per-daemon stats view.
func renderStatsTable(w io.Writer, doc *statsDoc) {
	var stale []statsDaemon
	for _, d := range doc.Daemons {
		if !d.Running {
			stale = append(stale, d)
			continue
		}
		fmt.Fprintf(w, "%s  pid %d  up %s  %s\n",
			displayScope(d.Scope), d.PID, shortDur(d.age), filepath.Base(d.Socket))
		if d.Error != "" {
			fmt.Fprintf(w, "  %s\n\n", d.Error)
			continue
		}
		if d.Stats == nil {
			fmt.Fprintf(w, "  (no stats)\n\n")
			continue
		}
		rt := d.Stats.Runtime
		fmt.Fprintf(w, "  runtime: %s  goroutines %d  gc %d runs / %s pause  uptime %s\n",
			fmtHeapLimit(rt.HeapAlloc, rt.GoMemLimit), rt.Goroutines, rt.NumGC,
			fmtPause(rt.GCPauseTotalMS), shortDur(time.Duration(rt.UptimeMS)*time.Millisecond))
		self := d.Stats.Self
		top := "-"
		if self.Top != nil {
			top = fmt.Sprintf("%s(%d) %.1f%% %s", self.Top.Comm, self.Top.PID, self.Top.CPUPct, fmtRSS(self.Top.RSSKB))
		}
		fmt.Fprintf(w, "  self:    cpu %.1f%%  rss %s  procs %d  top %s\n",
			self.CPUPct, fmtRSS(self.RSSKB), self.Procs, top)
		fmt.Fprintf(w, "  children: %s\n\n", fmtChildren(self.Children))
	}

	if len(stale) > 0 {
		fmt.Fprintf(w, "STALE DAEMONS\n")
		for _, d := range stale {
			fmt.Fprintf(w, "  %-32s  last pid %d  %s\n", displayScope(d.Scope), d.PID, filepath.Base(d.Socket))
		}
	}
}

// ---------------------------------------------------------------------------
// Command
// ---------------------------------------------------------------------------

func newGrovedStatsCmd() *cobra.Command {
	var (
		jsonOut bool
		scope   string
		all     bool
	)
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show per-daemon runtime and process-tree stats (GET /api/system/stats)",
		Long: `Query every running groved's /api/system/stats endpoint and render its Go
runtime state (heap vs GOMEMLIMIT, goroutines, GC activity, uptime) plus the
two-sample process-tree rollup of the daemon's own pid (CPU%, RSS, hottest
children).

Daemons whose binary predates the endpoint are reported as
"` + statsEndpointUnavailable + `" and the fleet view continues. The command
exits 0 even when daemons are down; they appear as running:false / stale
entries.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := enumerateDaemons()
			if err != nil {
				return fmt.Errorf("enumerate daemons: %w", err)
			}
			probes := gatherStatsProbes(cmd.Context(), entries)
			doc := assembleStatsDoc(probes)
			if scope != "" {
				filterStatsScope(doc, scope)
				if len(doc.Daemons) == 0 {
					return fmt.Errorf("no daemon matched scope %q (try `groved status`)", scope)
				}
			}

			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(doc)
			}
			renderStatsTable(out, doc)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit one JSON document (stable snake_case fields)")
	cmd.Flags().StringVar(&scope, "scope", "", "Show a single daemon by scope label (\"unscoped\" for the global daemon)")
	cmd.Flags().BoolVar(&all, "all", true, "Show every daemon (default)")
	cmd.MarkFlagsMutuallyExclusive("scope", "all")
	return cmd
}
