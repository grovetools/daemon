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
	mu sync.RWMutex
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
		logChan: make(chan logutil.TailedLine, 256),
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
	defer ws.mu.Unlock()

	ch := make(chan logutil.TailedLine, 100)
	ws.subscribers[ch] = opts

	replay := ws.replayLocked(opts)
	return replay, ch
}

// Unsubscribe removes a subscriber.
func (ws *WorkspaceStreamer) Unsubscribe(ch chan logutil.TailedLine) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	delete(ws.subscribers, ch)
	close(ch)
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

			ecoMap := ws.buildEcoMap()
			for ch, opts := range ws.subscribers {
				if matchesFilter(line, opts, ecoMap) {
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

	// Run immediately on start, then on tick.
	ws.syncTailers(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ws.syncTailers(ctx)
		}
	}
}

func (ws *WorkspaceStreamer) syncTailers(ctx context.Context) {
	workspaces := ws.store.GetWorkspaces()

	ws.mu.Lock()
	defer ws.mu.Unlock()

	// Build set of current workspace paths.
	currentPaths := make(map[string]bool, len(workspaces)+1)
	currentPaths["system"] = true

	for _, ews := range workspaces {
		if ews.WorkspaceNode == nil {
			continue
		}
		wsPath := ews.Path
		currentPaths[wsPath] = true

		if _, exists := ws.activeTailers[wsPath]; exists {
			continue
		}

		_, logsDir, err := logutil.FindLogFileForWorkspace(ews.WorkspaceNode)
		if err != nil && logsDir == "" {
			continue
		}

		tailCtx, cancel := context.WithCancel(ctx)
		ws.activeTailers[wsPath] = cancel

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

	// Cancel tailers for workspaces no longer in the store.
	for path, cancel := range ws.activeTailers {
		if !currentPaths[path] {
			cancel()
			delete(ws.activeTailers, path)
		}
	}
}

// replayLocked returns historical lines matching the filter. Must be called with ws.mu held.
func (ws *WorkspaceStreamer) replayLocked(opts models.LogStreamOptions) []logutil.TailedLine {
	ecoMap := ws.buildEcoMap()
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

// buildEcoMap returns a workspace-path -> RootEcosystemPath mapping from the store.
// Must be called with ws.mu held (or from within a locked section).
func (ws *WorkspaceStreamer) buildEcoMap() map[string]string {
	workspaces := ws.store.GetWorkspaces()
	m := make(map[string]string, len(workspaces))
	for _, ews := range workspaces {
		if ews.WorkspaceNode != nil {
			m[ews.Path] = ews.RootEcosystemPath
		}
	}
	return m
}

// levelSeverity maps log level strings to numeric severity for >= comparison.
var levelSeverity = map[string]int{
	"debug": 0,
	"info":  1,
	"warn":  2,
	"warning": 2,
	"error": 3,
	"fatal": 4,
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
