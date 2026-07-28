package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/procsample"
	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// JSON document shapes. These are a stable, snake_case contract consumed by
// agents and scripts — treat field renames/removals as breaking changes.
// ---------------------------------------------------------------------------

// resourceProc is one process row (top offender or --detail row).
type resourceProc struct {
	PID    int     `json:"pid"`
	Comm   string  `json:"comm"`
	CPUPct float64 `json:"cpu_pct"`
	RSSKB  int64   `json:"rss_kb"`
}

// resourceRollup aggregates one process subtree. RSSKB counts shared pages
// once per process, so it overstates the memory a subtree kill would reclaim.
type resourceRollup struct {
	RootPID     int            `json:"root_pid"`
	CPUPct      float64        `json:"cpu_pct"`
	RSSKB       int64          `json:"rss_kb"`
	Procs       int            `json:"procs"`
	Top         *resourceProc  `json:"top,omitempty"`
	ProcsDetail []resourceProc `json:"procs_detail,omitempty"` // --detail only
}

// resourcePTY is one PTY session subtree owned by a daemon's paired tuimuxd.
type resourcePTY struct {
	PtyID           string            `json:"pty_id"`
	Workspace       string            `json:"workspace"`
	Label           string            `json:"label"`
	Labels          map[string]string `json:"labels,omitempty"`
	RootPID         int               `json:"root_pid"`
	AttachedClients int               `json:"attached_clients"`
	CPUPct          float64           `json:"cpu_pct"`
	RSSKB           int64             `json:"rss_kb"`
	Procs           int               `json:"procs"`
	Top             *resourceProc     `json:"top,omitempty"`
	ProcsDetail     []resourceProc    `json:"procs_detail,omitempty"`

	// Table-only fields (not part of the JSON contract).
	rootComm string        // comm of the root process
	idle     time.Duration // time since last detach when unattached
}

// resourceAgent is one headless agent process (session with a PID but no PTY).
type resourceAgent struct {
	SessionID   string         `json:"session_id"`
	Workspace   string         `json:"workspace"`
	Label       string         `json:"label"`
	RootPID     int            `json:"root_pid"`
	CPUPct      float64        `json:"cpu_pct"`
	RSSKB       int64          `json:"rss_kb"`
	Procs       int            `json:"procs"`
	Top         *resourceProc  `json:"top,omitempty"`
	ProcsDetail []resourceProc `json:"procs_detail,omitempty"`

	rootComm string // table only
}

// resourceDaemon is one groved (running or stale) with its attributed subtrees.
type resourceDaemon struct {
	Scope   string `json:"scope"`
	PID     int    `json:"pid"`
	Running bool   `json:"running"`
	Socket  string `json:"socket"`
	// Error records a per-daemon probe failure (socket dead, stale binary
	// without the PTY endpoints, ...). The daemon's own rollup is still
	// reported; only the PTY/agent listings are missing.
	Error   string          `json:"error,omitempty"`
	Self    *resourceRollup `json:"self,omitempty"`
	Tuimuxd *resourceRollup `json:"tuimuxd,omitempty"`
	PTYs    []resourcePTY   `json:"ptys,omitempty"`
	Agents  []resourceAgent `json:"agents,omitempty"`

	age time.Duration // table only: pidfile age
}

// resourceHost is one registered interactive TUI host (StateDir()/hosts).
type resourceHost struct {
	PID         int            `json:"pid"`
	Program     string         `json:"program"`
	Scope       string         `json:"scope"`
	SocketPath  string         `json:"socket_path"`
	CPUPct      float64        `json:"cpu_pct"`
	RSSKB       int64          `json:"rss_kb"`
	Procs       int            `json:"procs"`
	Top         *resourceProc  `json:"top,omitempty"`
	ProcsDetail []resourceProc `json:"procs_detail,omitempty"`
}

// resourceOrphan is an interesting process living outside every tracked
// subtree.
type resourceOrphan struct {
	PID    int     `json:"pid"`
	Comm   string  `json:"comm"`
	CPUPct float64 `json:"cpu_pct"`
	RSSKB  int64   `json:"rss_kb"`
	Reason string  `json:"reason"`
}

// resourceDoc is the whole `groved resources --json` document.
type resourceDoc struct {
	SampledAt  time.Time        `json:"sampled_at"`
	IntervalMS int64            `json:"interval_ms"`
	Daemons    []resourceDaemon `json:"daemons,omitempty"`
	Hosts      []resourceHost   `json:"hosts,omitempty"`
	Orphans    []resourceOrphan `json:"orphans"`
}

// ---------------------------------------------------------------------------
// Gathering
// ---------------------------------------------------------------------------

// daemonProbeResult is what we learned from one enumerated daemon before
// sampling: its pidfile entry plus, for running daemons, the PTY and session
// listings from its socket. err carries a per-daemon failure without aborting
// the fleet view.
type daemonProbeResult struct {
	entry  daemonEntry
	ptys   []daemon.PTYSessionInfo
	agents []*models.Session
	err    error
}

// probeDaemonFn is the socket-probe seam; tests replace it with a fake.
var probeDaemonFn = probeDaemon

// probeDaemon queries one running daemon's socket for PTY sessions and
// (headless-agent) sessions. Partial results are returned alongside the error.
func probeDaemon(ctx context.Context, e daemonEntry) ([]daemon.PTYSessionInfo, []*models.Session, error) {
	client, err := daemon.NewRemoteClient(e.SockPath)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ptys, perr := client.ListPTYs(ctx)
	sessions, serr := client.GetSessions(ctx)
	return ptys, sessions, errors.Join(perr, serr)
}

// gatherDaemonProbes probes every running daemon in parallel (each is bounded
// by probeDaemon's own timeout, so a hung daemon can't stall the fleet view).
func gatherDaemonProbes(ctx context.Context, entries []daemonEntry) []daemonProbeResult {
	results := make([]daemonProbeResult, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		results[i].entry = e
		if !e.Running {
			continue
		}
		wg.Add(1)
		go func(i int, e daemonEntry) {
			defer wg.Done()
			results[i].ptys, results[i].agents, results[i].err = probeDaemonFn(ctx, e)
		}(i, e)
	}
	wg.Wait()
	return results
}

// uiHostEntry mirrors the hosts-registry JSON written by core's
// RegisterUIHost (StateDir()/hosts/host-<pid>.json). Read directly here; a
// core accessor arrives with R3.
type uiHostEntry struct {
	PID        int    `json:"pid"`
	Program    string `json:"program"`
	Scope      string `json:"scope"`
	SocketPath string `json:"socket_path"`
}

// readUIHosts loads every parseable host registration under dir. Liveness is
// NOT checked here — assembly skips hosts absent from the process sample,
// which prunes stale registrations for free.
func readUIHosts(dir string) []uiHostEntry {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil
	}
	var hosts []uiHostEntry
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var h uiHostEntry
		if json.Unmarshal(data, &h) != nil || h.PID <= 0 {
			continue
		}
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].PID < hosts[j].PID })
	return hosts
}

// ---------------------------------------------------------------------------
// Assembly
// ---------------------------------------------------------------------------

// orphanInterestMatch is the CLI's stricter interest filter over
// procsample.Orphans output: short comms ("pi", "git") must match exactly —
// substring matching flags pip, pinentry, github-runner, ... — while the
// longer DefaultInterest patterns keep substring semantics.
func orphanInterestMatch(comm string) bool {
	c := strings.ToLower(comm)
	if c == "pi" || c == "git" {
		return true
	}
	for _, pat := range []string{"nvim", "gopls", "claude", "hash-object"} {
		if strings.Contains(c, pat) {
			return true
		}
	}
	return false
}

// rollupFor converts a procsample rollup into the JSON shape. A root missing
// from the sample yields Procs == 0 and no Top.
func rollupFor(sample *procsample.Sample, root int, detail bool) *resourceRollup {
	r := sample.Rollup(root)
	out := &resourceRollup{RootPID: root, CPUPct: round1(r.CPU), RSSKB: r.RSSKB, Procs: r.Procs}
	if r.Procs > 0 {
		out.Top = &resourceProc{PID: r.Top.PID, Comm: r.Top.Comm, CPUPct: round1(r.TopCPU), RSSKB: r.Top.RSSKB}
	}
	if detail {
		out.ProcsDetail = procDetail(sample, r.Pids)
	}
	return out
}

// procDetail renders per-process rows for a pid set, hottest first.
func procDetail(sample *procsample.Sample, pids []int) []resourceProc {
	rows := make([]resourceProc, 0, len(pids))
	for _, pid := range pids {
		p := sample.Procs[pid]
		rows = append(rows, resourceProc{PID: pid, Comm: p.Comm, CPUPct: round1(sample.CPU[pid]), RSSKB: p.RSSKB})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CPUPct != rows[j].CPUPct {
			return rows[i].CPUPct > rows[j].CPUPct
		}
		return rows[i].RSSKB > rows[j].RSSKB
	})
	return rows
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// ptyLabel picks the display label for a PTY session: explicit label first,
// then the flow job id, then the session type.
func ptyLabel(p daemon.PTYSessionInfo) string {
	if p.Label != "" {
		return p.Label
	}
	if v := p.Labels["job_id"]; v != "" {
		return v
	}
	return p.Labels["type"]
}

// agentLabel picks the display label for a headless agent session.
func agentLabel(s *models.Session) string {
	if s.JobTitle != "" {
		return s.JobTitle
	}
	return s.Type
}

// assembleResourceDoc joins one process sample with the daemon probes and
// host registrations into the output document. Pure — all inputs are values —
// so tests can drive it with fakes.
func assembleResourceDoc(sample *procsample.Sample, intervalMS int64, probes []daemonProbeResult, hosts []uiHostEntry, detail bool, now time.Time) *resourceDoc {
	doc := &resourceDoc{
		SampledAt:  sample.At,
		IntervalMS: intervalMS,
		Orphans:    []resourceOrphan{},
	}

	var tracked []int
	track := func(pid int) {
		if pid > 0 {
			tracked = append(tracked, pid)
		}
	}

	for _, pr := range probes {
		e := pr.entry
		d := resourceDaemon{
			Scope:   e.Scope,
			PID:     e.PID,
			Running: e.Running,
			Socket:  e.SockPath,
			age:     e.Age,
		}
		if pr.err != nil {
			d.Error = trimStatusError(pr.err.Error())
		}
		if e.Running {
			d.Self = rollupFor(sample, e.PID, detail)
			track(e.PID)

			// tuimuxd is the parent of any PTY root — free from the sample.
			tuimuxdPID := 0
			for _, pty := range pr.ptys {
				if p, ok := sample.Procs[pty.PID]; ok && p.PPID > 1 && p.PPID != e.PID {
					tuimuxdPID = p.PPID
					break
				}
			}
			if tuimuxdPID > 0 {
				d.Tuimuxd = rollupFor(sample, tuimuxdPID, detail)
				track(tuimuxdPID)
			}

			for _, pty := range pr.ptys {
				r := rollupFor(sample, pty.PID, detail)
				row := resourcePTY{
					PtyID:           pty.ID,
					Workspace:       pty.Workspace,
					Label:           ptyLabel(pty),
					Labels:          pty.Labels,
					RootPID:         pty.PID,
					AttachedClients: pty.AttachedClients,
					CPUPct:          r.CPUPct,
					RSSKB:           r.RSSKB,
					Procs:           r.Procs,
					Top:             r.Top,
					ProcsDetail:     r.ProcsDetail,
					rootComm:        sample.Procs[pty.PID].Comm,
				}
				if pty.AttachedClients == 0 && !pty.LastDetached.IsZero() {
					row.idle = now.Sub(pty.LastDetached)
				}
				d.PTYs = append(d.PTYs, row)
				track(pty.PID)
			}
			sort.SliceStable(d.PTYs, func(i, j int) bool { return d.PTYs[i].CPUPct > d.PTYs[j].CPUPct })

			for _, s := range pr.agents {
				if s == nil || s.PID <= 0 || s.PtyID != "" {
					continue
				}
				if _, alive := sample.Procs[s.PID]; !alive {
					continue // dead PID the collector hasn't swept yet
				}
				r := rollupFor(sample, s.PID, detail)
				workspaceName := s.Repo
				if s.WorkingDirectory != "" {
					workspaceName = filepath.Base(s.WorkingDirectory)
				}
				d.Agents = append(d.Agents, resourceAgent{
					SessionID:   s.ID,
					Workspace:   workspaceName,
					Label:       agentLabel(s),
					RootPID:     s.PID,
					CPUPct:      r.CPUPct,
					RSSKB:       r.RSSKB,
					Procs:       r.Procs,
					Top:         r.Top,
					ProcsDetail: r.ProcsDetail,
					rootComm:    sample.Procs[s.PID].Comm,
				})
				track(s.PID)
			}
			sort.SliceStable(d.Agents, func(i, j int) bool { return d.Agents[i].CPUPct > d.Agents[j].CPUPct })
		}
		doc.Daemons = append(doc.Daemons, d)
	}

	for _, h := range hosts {
		if _, alive := sample.Procs[h.PID]; !alive {
			continue // stale registration
		}
		r := rollupFor(sample, h.PID, detail)
		doc.Hosts = append(doc.Hosts, resourceHost{
			PID:         h.PID,
			Program:     h.Program,
			Scope:       h.Scope,
			SocketPath:  h.SocketPath,
			CPUPct:      r.CPUPct,
			RSSKB:       r.RSSKB,
			Procs:       r.Procs,
			Top:         r.Top,
			ProcsDetail: r.ProcsDetail,
		})
		track(h.PID)
	}

	// Orphans: procsample handles subtree/ancestry exclusion; the interest
	// filter is re-applied here with exact matching for the short comms
	// (DefaultInterest is substring-based and noisy for "pi"/"git").
	for _, p := range sample.Orphans(tracked, procsample.DefaultInterest) {
		if !orphanInterestMatch(p.Comm) {
			continue
		}
		doc.Orphans = append(doc.Orphans, resourceOrphan{
			PID:    p.PID,
			Comm:   p.Comm,
			CPUPct: round1(sample.CPU[p.PID]),
			RSSKB:  p.RSSKB,
			Reason: "unaccounted",
		})
	}
	sort.SliceStable(doc.Orphans, func(i, j int) bool { return doc.Orphans[i].CPUPct > doc.Orphans[j].CPUPct })

	return doc
}

// filterDocScope keeps only the daemon whose scope label matches.
// "unscoped"/"global" select the unscoped daemon. Hosts and orphans stay:
// they are fleet-level, not owned by any one daemon.
func filterDocScope(doc *resourceDoc, scope string) {
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

func fmtRSS(kb int64) string {
	switch {
	case kb >= 1024*1024:
		return fmt.Sprintf("%.2fG", float64(kb)/(1024*1024))
	case kb >= 1024:
		return fmt.Sprintf("%.1fM", float64(kb)/1024)
	default:
		return fmt.Sprintf("%dK", kb)
	}
}

// shortDur renders a duration compactly for table cells: 42s, 17m, 3h12m, 2d5h.
func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		days := d / (24 * time.Hour)
		hours := (d % (24 * time.Hour)) / time.Hour
		return fmt.Sprintf("%dd%dh", days, hours)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", d/time.Hour, (d%time.Hour)/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}

func fmtTopOffender(p *resourceProc) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%s(%d) %.1f%% %s", p.Comm, p.PID, p.CPUPct, fmtRSS(p.RSSKB))
}

func truncCell(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func short8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// resourceRow is one table row (a PTY subtree or a headless agent).
type resourceRow struct {
	id, workspace, label, comm string
	cpu                        float64
	rss                        int64
	procs                      int
	top                        string
	clIdle                     string
	detail                     []resourceProc
}

func ptyRow(p resourcePTY) resourceRow {
	clIdle := fmt.Sprintf("%d", p.AttachedClients)
	if p.AttachedClients == 0 && p.idle > 0 {
		clIdle = "idle " + shortDur(p.idle)
	}
	return resourceRow{
		id: short8(p.PtyID), workspace: p.Workspace, label: p.Label, comm: p.rootComm,
		cpu: p.CPUPct, rss: p.RSSKB, procs: p.Procs, top: fmtTopOffender(p.Top),
		clIdle: clIdle, detail: p.ProcsDetail,
	}
}

func agentRow(a resourceAgent) resourceRow {
	return resourceRow{
		id: short8(a.SessionID), workspace: a.Workspace, label: a.Label, comm: a.rootComm,
		cpu: a.CPUPct, rss: a.RSSKB, procs: a.Procs, top: fmtTopOffender(a.Top),
		clIdle: "agent", detail: a.ProcsDetail,
	}
}

func fmtRollupLine(name string, r *resourceRollup) string {
	if r == nil || r.Procs == 0 {
		return fmt.Sprintf("  %s: (not in sample)", name)
	}
	return fmt.Sprintf("  %s: pid %d  cpu %.1f%%  rss %s  procs %d  top %s",
		name, r.RootPID, r.CPUPct, fmtRSS(r.RSSKB), r.Procs, fmtTopOffender(r.Top))
}

func writeDetailRows(w io.Writer, rows []resourceProc, indent string) {
	for _, p := range rows {
		fmt.Fprintf(w, "%s%-8d %-24s %6.1f  %8s\n", indent, p.PID, truncCell(p.Comm, 24), p.CPUPct, fmtRSS(p.RSSKB))
	}
}

// renderResourceTable renders the human-facing fleet view.
func renderResourceTable(w io.Writer, doc *resourceDoc, detail bool) {
	fmt.Fprintf(w, "sampled %s  interval %dms\n", doc.SampledAt.Format("15:04:05"), doc.IntervalMS)

	var stale []resourceDaemon
	for _, d := range doc.Daemons {
		if !d.Running {
			stale = append(stale, d)
			continue
		}
		fmt.Fprintf(w, "\n%s  pid %d  up %s  %s\n",
			displayScope(d.Scope), d.PID, shortDur(d.age), filepath.Base(d.Socket))
		if d.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", d.Error)
		}
		fmt.Fprintln(w, fmtRollupLine("self", d.Self))
		if d.Tuimuxd != nil {
			fmt.Fprintln(w, fmtRollupLine("tuimuxd", d.Tuimuxd)+"  (includes PTY subtrees)")
		}
		if detail && d.Self != nil {
			writeDetailRows(w, d.Self.ProcsDetail, "      ")
		}

		rows := make([]resourceRow, 0, len(d.PTYs)+len(d.Agents))
		for _, p := range d.PTYs {
			rows = append(rows, ptyRow(p))
		}
		for _, a := range d.Agents {
			rows = append(rows, agentRow(a))
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].cpu > rows[j].cpu })

		if len(rows) > 0 {
			fmt.Fprintf(w, "  %-8s  %-18s  %-22s  %-12s  %6s  %8s  %5s  %-28s  %s\n",
				"PTY", "WORKSPACE", "LABEL", "COMM", "CPU%", "RSS", "PROCS", "TOP OFFENDER", "CL/IDLE")
			for _, r := range rows {
				fmt.Fprintf(w, "  %-8s  %-18s  %-22s  %-12s  %6.1f  %8s  %5d  %-28s  %s\n",
					r.id, truncCell(r.workspace, 18), truncCell(r.label, 22), truncCell(r.comm, 12),
					r.cpu, fmtRSS(r.rss), r.procs, truncCell(r.top, 28), r.clIdle)
				if detail {
					writeDetailRows(w, r.detail, "      ")
				}
			}
		}
	}

	if len(doc.Hosts) > 0 {
		fmt.Fprintf(w, "\nTUI HOSTS\n")
		fmt.Fprintf(w, "  %-8s  %-10s  %-32s  %6s  %8s  %5s  %s\n",
			"PID", "PROGRAM", "SCOPE", "CPU%", "RSS", "PROCS", "TOP OFFENDER")
		for _, h := range doc.Hosts {
			scope := h.Scope
			if scope == "" {
				scope = "(global)"
			}
			fmt.Fprintf(w, "  %-8d  %-10s  %-32s  %6.1f  %8s  %5d  %s\n",
				h.PID, truncCell(h.Program, 10), truncCell(scope, 32), h.CPUPct, fmtRSS(h.RSSKB), h.Procs, fmtTopOffender(h.Top))
			if detail {
				writeDetailRows(w, h.ProcsDetail, "      ")
			}
		}
	}

	renderOrphansSection(w, doc.Orphans)

	if len(stale) > 0 {
		fmt.Fprintf(w, "\nSTALE DAEMONS\n")
		for _, d := range stale {
			fmt.Fprintf(w, "  %-32s  last pid %d  %s\n", displayScope(d.Scope), d.PID, filepath.Base(d.Socket))
		}
	}
}

func renderOrphansSection(w io.Writer, orphans []resourceOrphan) {
	fmt.Fprintf(w, "\nORPHANS\n")
	if len(orphans) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	fmt.Fprintf(w, "  %-8s  %-24s  %6s  %8s  %s\n", "PID", "COMM", "CPU%", "RSS", "REASON")
	for _, o := range orphans {
		fmt.Fprintf(w, "  %-8d  %-24s  %6.1f  %8s  %s\n",
			o.PID, truncCell(o.Comm, 24), o.CPUPct, fmtRSS(o.RSSKB), o.Reason)
	}
}

// ---------------------------------------------------------------------------
// Command
// ---------------------------------------------------------------------------

func newGrovedResourcesCmd() *cobra.Command {
	var (
		jsonOut     bool
		orphansOnly bool
		detail      bool
		scope       string
		all         bool
	)
	cmd := &cobra.Command{
		Use:   "resources",
		Short: "Show per-daemon process-tree resource usage across the fleet",
		Long: `Sample the system process table twice (~1s apart, so CPU% is a true
interval measurement) and attribute every process subtree to its owner:
each groved daemon, its paired tuimuxd, every PTY session, every headless
agent, and every registered TUI host. Interesting processes that belong to
no tracked subtree are listed as orphans.

Per-daemon probe failures are reported inline; the command exits 0 even
when daemons are down (they appear as running:false / stale entries).

RSS sums count shared pages once per process, so subtree totals overstate
the memory a kill would actually reclaim.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sampler := procsample.NewSampler()
			if _, err := sampler.Sample(); err != nil {
				return fmt.Errorf("sample processes: %w", err)
			}
			t0 := time.Now()

			entries, err := enumerateDaemons()
			if err != nil {
				return fmt.Errorf("enumerate daemons: %w", err)
			}
			// Probe every daemon even under --scope: orphan classification
			// needs the full set of tracked subtrees or foreign PTYs would
			// show up as false orphans. --scope filters display only.
			probes := gatherDaemonProbes(cmd.Context(), entries)
			hosts := readUIHosts(filepath.Join(paths.StateDir(), "hosts"))

			// The probes soak up part of the interval; top up to ~1s so the
			// CPU% window stays meaningful.
			if wait := time.Second - time.Since(t0); wait > 0 {
				time.Sleep(wait)
			}
			sample, err := sampler.Sample()
			if err != nil {
				return fmt.Errorf("sample processes: %w", err)
			}
			intervalMS := time.Since(t0).Milliseconds()

			doc := assembleResourceDoc(sample, intervalMS, probes, hosts, detail, time.Now())
			if scope != "" {
				filterDocScope(doc, scope)
				if len(doc.Daemons) == 0 {
					return fmt.Errorf("no daemon matched scope %q (try `groved status`)", scope)
				}
			}
			if orphansOnly {
				doc.Daemons = nil
				doc.Hosts = nil
			}

			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(doc)
			}
			if orphansOnly {
				fmt.Fprintf(out, "sampled %s  interval %dms\n", doc.SampledAt.Format("15:04:05"), doc.IntervalMS)
				renderOrphansSection(out, doc.Orphans)
				return nil
			}
			renderResourceTable(out, doc, detail)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit one JSON document (stable snake_case fields)")
	cmd.Flags().BoolVar(&orphansOnly, "orphans", false, "Show only the orphans section")
	cmd.Flags().BoolVar(&detail, "detail", false, "Include per-process rows under each subtree")
	cmd.Flags().StringVar(&scope, "scope", "", "Show a single daemon by scope label (\"unscoped\" for the global daemon)")
	cmd.Flags().BoolVar(&all, "all", true, "Show every daemon (default)")
	cmd.MarkFlagsMutuallyExclusive("scope", "all")
	return cmd
}
