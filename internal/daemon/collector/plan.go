package collector

import (
	"context"
	"strings"
	"time"

	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/enrichment"
)

// planBackgroundInterval is how often to update non-focused workspaces.
const planBackgroundInterval = 2 * time.Minute

// PlanCollector updates plan statistics for all workspaces.
type PlanCollector struct {
	interval time.Duration
}

// NewPlanCollector creates a new PlanCollector with the specified interval.
// If interval is 0, defaults to 30 seconds.
func NewPlanCollector(interval time.Duration) *PlanCollector {
	if interval == 0 {
		interval = 30 * time.Second
	}
	return &PlanCollector{
		interval: interval,
	}
}

// Name returns the collector's name.
func (c *PlanCollector) Name() string { return "plan" }

// Run starts the plan stats collection loop.
func (c *PlanCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	logger := logging.NewLogger("collector.plan")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	var lastFullScan time.Time

	scan := func() {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 200*time.Millisecond {
				logger.WithField("duration", d).Warn("Slow plan scan detected")
			}
		}()

		planStats, err := enrichment.FetchPlanStatsMap()
		if err != nil {
			return
		}

		state := st.Get()
		focus := st.GetFocus()

		// Determine if this is a full scan or focused scan
		doFullScan := len(focus) == 0 || time.Since(lastFullScan) >= planBackgroundInterval
		if doFullScan {
			lastFullScan = time.Now()
		}

		// Build case-insensitive focus map
		focusLower := make(map[string]struct{}, len(focus))
		for p := range focus {
			focusLower[strings.ToLower(p)] = struct{}{}
		}

		// Clone existing workspaces and update plan stats
		newWorkspaces := make(map[string]*enrichment.EnrichedWorkspace)
		scanned := 0

		for k, v := range state.Workspaces {
			cpy := *v

			// Check if this workspace should be updated
			_, isFocused := focusLower[strings.ToLower(k)]
			if doFullScan || isFocused {
				if stats, ok := planStats[k]; ok {
					cpy.PlanStats = stats
				}
				scanned++
			}

			newWorkspaces[k] = &cpy
		}

		updates <- store.Update{
			Type:    store.UpdateWorkspaces,
			Source:  "plan",
			Scanned: scanned,
			Payload: newWorkspaces,
		}
	}

	// Wait for workspaces to be populated first
	time.Sleep(2 * time.Second)
	scan()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan()
		}
	}
}
