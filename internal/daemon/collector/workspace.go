package collector

import (
	"context"
	"strings"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// WorkspaceCollector discovers workspaces and maintains the base workspace list.
type WorkspaceCollector struct {
	interval     time.Duration
	scope        string // if non-empty, only nodes at or under this path are kept
	ulog         *logging.UnifiedLogger
	discoveryLog *logrus.Logger // Passed to workspace.GetProjects which requires *logrus.Logger
	refresh      chan chan struct{}
}

// NewWorkspaceCollector creates a new WorkspaceCollector with the specified interval
// and scope. If interval is 0, defaults to 5 minutes. If scope is non-empty, discovery
// results are filtered to nodes at or under that path — used by ecosystem-scoped
// daemons so each instance only watches its own workspaces.
func NewWorkspaceCollector(interval time.Duration, scope string) *WorkspaceCollector {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	discoveryLog := logrus.New()
	discoveryLog.SetLevel(logrus.WarnLevel)
	return &WorkspaceCollector{
		interval:     interval,
		scope:        scope,
		ulog:         logging.NewUnifiedLogger("groved.collector.workspace"),
		discoveryLog: discoveryLog,
		refresh:      make(chan chan struct{}),
	}
}

// Refresh triggers an immediate workspace scan and blocks until it completes.
func (c *WorkspaceCollector) Refresh(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case c.refresh <- reply:
		select {
		case <-reply:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Name returns the collector's name.
func (c *WorkspaceCollector) Name() string { return "workspace" }

// Run starts the workspace discovery loop.
func (c *WorkspaceCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	scan := func() {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 1*time.Second {
				c.ulog.Debug("Slow workspace discovery detected").Field("duration", d).Log(ctx)
			}
		}()

		// 1. Discover base nodes
		nodes, err := workspace.GetProjects(c.discoveryLog)
		if err != nil {
			return
		}

		// 1.5 Filter nodes by scope if the daemon is scope-restricted.
		// Keeps a scoped daemon from watching unrelated ecosystems.
		if c.scope != "" {
			scopePrefix := c.scope + "/"
			filtered := nodes[:0]
			for _, n := range nodes {
				if n.Path == c.scope || strings.HasPrefix(n.Path, scopePrefix) {
					filtered = append(filtered, n)
				}
			}
			nodes = filtered
		}

		// 2. Convert to EnrichedWorkspace (initially empty enrichment)
		// Preserve existing enrichment data if available in the store
		currentState := st.Get() // Read lock
		enrichedMap := make(map[string]*models.EnrichedWorkspace)

		for _, node := range nodes {
			ew := &models.EnrichedWorkspace{WorkspaceNode: node}

			// Preserve existing data if we have it
			if existing, ok := currentState.Workspaces[node.Path]; ok {
				ew.GitStatus = existing.GitStatus
				ew.NoteCounts = existing.NoteCounts
				ew.PlanStats = existing.PlanStats
				ew.ReleaseInfo = existing.ReleaseInfo
				ew.ActiveBinary = existing.ActiveBinary
				ew.CxStats = existing.CxStats
				ew.GitRemoteURL = existing.GitRemoteURL
			}
			enrichedMap[node.Path] = ew
		}

		updates <- store.Update{
			Type:    store.UpdateWorkspaces,
			Source:  "workspace",
			Payload: enrichedMap,
		}
	}

	// Initial scan
	scan()

	currentInterval := c.interval

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan()

			// Dynamically adjust interval based on active client focus.
			// When the TUI is open (focus set), scan more frequently to catch
			// worktree additions/removals quickly.
			focus := st.GetFocus()
			newInterval := c.interval
			if len(focus) > 0 {
				newInterval = 10 * time.Second
			}

			if newInterval != currentInterval {
				currentInterval = newInterval
				ticker.Reset(currentInterval)
			}
		case replyCh := <-c.refresh:
			scan()
			close(replyCh)
		}
	}
}
