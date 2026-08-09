package logstreamer

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/grovetools/core/pkg/logging/logutil"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// WorkspaceStreamer aggregates workspace log files into a single global ring
// buffer and broadcasts to SSE subscribers with per-client filtering.
type WorkspaceStreamer struct {
	store         *store.Store
	buffer        []logutil.TailedLine
	bufferIdx     int
	bufferFull    bool
	capacity      int
	subscribers   map[chan logutil.TailedLine]models.LogStreamOptions
	activeTailers map[string]context.CancelFunc // workspace path -> cancel
	logChan       chan logutil.TailedLine
	mu            sync.RWMutex

	// resync wakes watchWorkspaces between ticks. Subscribing is the one event
	// that must not wait out the 5s tick: a client opening a workspace-scoped
	// log stream would otherwise sit blank for up to five seconds while the
	// tailer it just created demand for did not exist yet. Buffered by 1 and
	// sent non-blocking — a coalesced wakeup is exactly as good as a queued one.
	resync chan struct{}

	// ecoMap is the workspace-path -> RootEcosystemPath projection used by
	// ecosystem-scoped filters. It is refreshed on the tailer sync tick and on
	// subscribe rather than per line: rebuilding it inside the aggregator meant
	// one store snapshot plus a several-hundred-entry map allocation for EVERY
	// tailed log line, under the write lock, whether or not any subscriber was
	// ecosystem-scoped. Workspace->ecosystem membership changes on the order of
	// minutes, so tick-level freshness is more than the filter needs.
	ecoMap map[string]string
}

// NewWorkspaceStreamer creates a new aggregated workspace log streamer.
func NewWorkspaceStreamer(st *store.Store, capacity int) *WorkspaceStreamer {
	if capacity <= 0 {
		capacity = 10000
	}
	return &WorkspaceStreamer{
		store:         st,
		buffer:        make([]logutil.TailedLine, capacity),
		capacity:      capacity,
		subscribers:   make(map[chan logutil.TailedLine]models.LogStreamOptions),
		activeTailers: make(map[string]context.CancelFunc),
		logChan:       make(chan logutil.TailedLine, 256),
		ecoMap:        make(map[string]string),
		resync:        make(chan struct{}, 1),
	}
}

// kickResync asks watchWorkspaces to recompute the demand set now instead of on
// the next tick. Non-blocking: the channel is a coalescing doorbell, not a queue.
func (ws *WorkspaceStreamer) kickResync() {
	select {
	case ws.resync <- struct{}{}:
	default:
	}
}

// Start launches the aggregator and workspace watcher goroutines.
func (ws *WorkspaceStreamer) Start(ctx context.Context) {
	go ws.runAggregator(ctx)
	go ws.watchWorkspaces(ctx)
}

// Stop cancels all active tailers and closes subscriber channels.
func (ws *WorkspaceStreamer) Stop() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	for path, cancel := range ws.activeTailers {
		cancel()
		delete(ws.activeTailers, path)
	}
	for ch := range ws.subscribers {
		close(ch)
		delete(ws.subscribers, ch)
	}
}

// Subscribe registers an SSE client. Returns the matching historical lines
// (limited to opts.Replay) and a channel for live updates.
func (ws *WorkspaceStreamer) Subscribe(opts models.LogStreamOptions) ([]logutil.TailedLine, chan logutil.TailedLine) {
	ws.mu.Lock()

	ch := make(chan logutil.TailedLine, 100)
	ws.subscribers[ch] = opts

	// A client subscribing right after a workspace appeared must not replay
	// against a stale ecosystem projection.
	ws.refreshEcoMapLocked()
	replay := ws.replayLocked(opts)
	ws.mu.Unlock()

	// This subscription may be the only thing demanding its workspace's tailer.
	// Kick the sync outside the lock — syncTailers takes ws.mu itself.
	ws.kickResync()
	return replay, ch
}

// Unsubscribe removes a subscriber.
func (ws *WorkspaceStreamer) Unsubscribe(ch chan logutil.TailedLine) {
	ws.mu.Lock()
	delete(ws.subscribers, ch)
	close(ch)
	ws.mu.Unlock()

	// Dropping the last reader of a workspace releases its tailer.
	ws.kickResync()
}

// runAggregator reads from the shared logChan, writes to the ring buffer,
// and broadcasts to matching subscribers.
func (ws *WorkspaceStreamer) runAggregator(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-ws.logChan:
			if !ok {
				return
			}
			ws.mu.Lock()
			ws.buffer[ws.bufferIdx] = line
			ws.bufferIdx++
			if ws.bufferIdx >= ws.capacity {
				ws.bufferIdx = 0
				ws.bufferFull = true
			}

			for ch, opts := range ws.subscribers {
				if matchesFilter(line, opts, ws.ecoMap) {
					select {
					case ch <- line:
					default:
					}
				}
			}
			ws.mu.Unlock()
		}
	}
}

// watchWorkspaces periodically syncs active tailers with the store's workspace list.
func (ws *WorkspaceStreamer) watchWorkspaces(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Run immediately on start, then on tick or on a subscribe/unsubscribe kick.
	ws.syncTailers(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ws.syncTailers(ctx)
		case <-ws.resync:
			ws.syncTailers(ctx)
		}
	}
}

// demandedPathsLocked returns the workspace paths that currently warrant a
// tailer. Must be called with ws.mu held and ecoMap fresh.
//
// Tailing used to be a property of a workspace EXISTING: every discovered
// workspace got a goroutine, an open directory, and a poll loop, forever. On a
// 649-workspace machine that was 650 tailers of which essentially none had a
// reader — 650 goroutines, ~44% of the daemon's total, against a 2000-goroutine
// budget that was being breached. Heap-wise they were cheap (~15MB), so this is
// a goroutine/descriptor fix, not the heap fix; see the plan summary.
//
// A workspace is demanded when either:
//
//   - some subscriber's filter can match it — the client is watching, so lines
//     must flow now; or
//   - it has a non-terminal job — work is happening there, so the ring should
//     be accumulating history for the client that subscribes a moment later.
//
// A subscriber with no scope (or "all") demands everything, which is the
// escape hatch: the aggregate `core logs` view is unchanged, it just no longer
// costs 650 goroutines when nobody has it open.
func (ws *WorkspaceStreamer) demandedPathsLocked(workspaces []*models.EnrichedWorkspace, jobs []*models.JobInfo) map[string]bool {
	demanded := make(map[string]bool, 16)

	// Subscriber-driven demand.
	wantAll := false
	for _, opts := range ws.subscribers {
		switch opts.Scope {
		case "system":
			// The system tailer is unconditional; no workspace demand.
		case "workspace":
			if opts.Workspace != "" {
				demanded[opts.Workspace] = true
			}
		case "ecosystem":
			clientEco := ws.ecoMap[opts.Workspace]
			if clientEco == "" {
				continue
			}
			for path, eco := range ws.ecoMap {
				if eco == clientEco {
					demanded[path] = true
				}
			}
		default:
			// "all" and the empty/unspecified scope both mean everything.
			wantAll = true
		}
	}

	if wantAll {
		for _, ews := range workspaces {
			if ews.WorkspaceNode != nil {
				demanded[ews.Path] = true
			}
		}
		return demanded
	}

	// Job-driven demand: keep history accruing where work is actually running.
	for _, job := range jobs {
		if job == nil || job.WorkDir == "" || isTerminalStatus(job.Status) {
			continue
		}
		demanded[store.NormalizePathKey(job.WorkDir)] = true
	}
	return demanded
}

func (ws *WorkspaceStreamer) syncTailers(ctx context.Context) {
	// Both store reads happen before ws.mu: the store has its own lock and
	// must never be acquired underneath this one.
	workspaces := ws.store.GetWorkspaces()
	jobs := ws.store.GetJobs()

	ws.mu.Lock()
	defer ws.mu.Unlock()

	// This tick is the streamer's one regular observation of the workspace
	// list, so it also refreshes the ecosystem projection the filters read.
	ws.refreshEcoMapLocked()

	demanded := ws.demandedPathsLocked(workspaces, jobs)

	// Only workspaces that both exist and are demanded get a tailer. The
	// "system" pseudo-workspace is always kept.
	keep := make(map[string]bool, len(demanded)+1)
	keep["system"] = true

	for _, ews := range workspaces {
		if ews.WorkspaceNode == nil {
			continue
		}
		wsPath := ews.Path
		if !demanded[wsPath] && !demanded[store.NormalizePathKey(wsPath)] {
			continue
		}
		keep[wsPath] = true

		if _, exists := ws.activeTailers[wsPath]; exists {
			continue
		}

		_, logsDir, err := logutil.FindLogFileForWorkspace(ews.WorkspaceNode)
		if err != nil && logsDir == "" {
			continue
		}

		tailCtx, cancel := context.WithCancel(ctx)
		ws.activeTailers[wsPath] = cancel

		// tailLines=100 means a tailer started on demand replays the last 100
		// lines into the ring as it opens. That is what keeps the subscriber
		// that just caused it from seeing an empty window: the backlog arrives
		// as live lines a moment after the (necessarily empty) replay.
		var wg sync.WaitGroup
		wg.Add(1)
		go logutil.TailDirectory(tailCtx, ews.Name, wsPath, logsDir, ws.logChan, &wg, true, 100)
		go func() {
			wg.Wait()
		}()

	}

	// Start system log tailer if not running.
	if _, exists := ws.activeTailers["system"]; !exists {
		systemDir := logutil.GetSystemLogsDir()
		tailCtx, cancel := context.WithCancel(ctx)
		ws.activeTailers["system"] = cancel

		var wg sync.WaitGroup
		wg.Add(1)
		go logutil.TailDirectory(tailCtx, "system", "", systemDir, ws.logChan, &wg, true, 100)
		go func() {
			wg.Wait()
		}()
	}

	// Cancel tailers that are gone from the store or no longer demanded.
	for path, cancel := range ws.activeTailers {
		if !keep[path] {
			cancel()
			delete(ws.activeTailers, path)
		}
	}
}

// replayLocked returns historical lines matching the filter. Must be called with ws.mu held.
func (ws *WorkspaceStreamer) replayLocked(opts models.LogStreamOptions) []logutil.TailedLine {
	ecoMap := ws.ecoMap
	maxReplay := opts.Replay
	if maxReplay <= 0 {
		maxReplay = 100
	}

	// Collect all buffered lines in order.
	var total int
	if ws.bufferFull {
		total = ws.capacity
	} else {
		total = ws.bufferIdx
	}

	var matched []logutil.TailedLine
	for i := 0; i < total; i++ {
		idx := i
		if ws.bufferFull {
			idx = (ws.bufferIdx + i) % ws.capacity
		}
		line := ws.buffer[idx]
		if matchesFilter(line, opts, ecoMap) {
			matched = append(matched, line)
		}
	}

	if len(matched) > maxReplay {
		matched = matched[len(matched)-maxReplay:]
	}
	return matched
}

// refreshEcoMapLocked rebuilds the cached workspace-path -> RootEcosystemPath
// mapping from the store. Must be called with ws.mu held for writing. The map
// is replaced rather than mutated so readers holding the previous value (the
// aggregator's broadcast loop) can never observe a half-built map.
func (ws *WorkspaceStreamer) refreshEcoMapLocked() {
	workspaces := ws.store.GetWorkspaces()
	m := make(map[string]string, len(workspaces))
	for _, ews := range workspaces {
		if ews.WorkspaceNode != nil {
			m[ews.Path] = ews.RootEcosystemPath
		}
	}
	ws.ecoMap = m
}

// levelSeverity maps log level strings to numeric severity for >= comparison.
var levelSeverity = map[string]int{
	"debug":   0,
	"info":    1,
	"warn":    2,
	"warning": 2,
	"error":   3,
	"fatal":   4,
}

// matchesFilter evaluates whether a log line matches the client's filter options.
func matchesFilter(line logutil.TailedLine, opts models.LogStreamOptions, ecoMap map[string]string) bool {
	// Scope filtering
	isSystem := line.WorkspacePath == "" || line.Workspace == "system"

	switch opts.Scope {
	case "workspace":
		if isSystem {
			if !opts.System {
				return false
			}
		} else if line.WorkspacePath != opts.Workspace {
			return false
		}
	case "ecosystem":
		if isSystem {
			if !opts.System {
				return false
			}
		} else {
			clientEco := ecoMap[opts.Workspace]
			lineEco := ecoMap[line.WorkspacePath]
			if clientEco == "" || lineEco != clientEco {
				return false
			}
		}
	case "system":
		if !isSystem {
			return false
		}
	case "all":
		if isSystem && !opts.System {
			return false
		}
	default:
		// No scope filter = show everything
		if isSystem && !opts.System {
			return false
		}
	}

	// Level filtering
	if opts.Level != "" {
		minSev, ok := levelSeverity[opts.Level]
		if ok {
			var parsed struct {
				Level string `json:"level"`
			}
			if err := json.Unmarshal([]byte(line.Line), &parsed); err == nil {
				lineSev, ok := levelSeverity[parsed.Level]
				if ok && lineSev < minSev {
					return false
				}
			}
		}
	}

	return true
}

// ActiveTailers reports how many workspace log tailers are currently running.
// It backs the "logstream.workspace_tailers" counter on /api/system/stats: a
// tailer count that climbs without bound (480 of them, parked in retry loops,
// in this plan's history) is invisible in CPU but obvious in goroutines, and
// this number is what attributes those goroutines to a cause.
func (ws *WorkspaceStreamer) ActiveTailers() int {
	if ws == nil {
		return 0
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.activeTailers)
}
