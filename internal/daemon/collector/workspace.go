package collector

import (
	"context"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// WorkspaceCollector discovers workspaces and maintains the base workspace list.
type WorkspaceCollector struct {
	interval time.Duration
	logger   *logrus.Logger
	refresh  chan chan struct{}
}

// NewWorkspaceCollector creates a new WorkspaceCollector with the specified interval.
// If interval is 0, defaults to 5 minutes.
func NewWorkspaceCollector(interval time.Duration) *WorkspaceCollector {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	return &WorkspaceCollector{
		interval: interval,
		logger:   logger,
		refresh:  make(chan chan struct{}),
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
			if d := time.Since(start); d > 500*time.Millisecond {
				c.logger.WithField("duration", d).Debug("Slow workspace discovery detected")
			}
		}()

		// 1. Discover base nodes
		nodes, err := workspace.GetProjects(c.logger)
		if err != nil {
			return
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

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan()
		case replyCh := <-c.refresh:
			scan()
			close(replyCh)
		}
	}
}
