package collector

import (
	"context"
	"strings"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/enrichment"
)

// planBackgroundInterval is how often to update non-focused workspaces.
const planBackgroundInterval = 10 * time.Minute

// PlanCollector updates plan statistics for all workspaces.
//
// On a scoped daemon, only workspaces inside scope (plus any focused
// workspaces) receive plan-stat refreshes during the periodic background
// scan. Focused workspaces are always refreshed regardless of scope so
// nav stays accurate for whatever the user points it at.
type PlanCollector struct {
	interval time.Duration
	scope    string
}

// NewPlanCollector creates a new PlanCollector with the specified interval
// and scope. If interval is 0, defaults to 5 minutes. An empty scope covers
// every workspace.
func NewPlanCollector(interval time.Duration, scope string) *PlanCollector {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	return &PlanCollector{
		interval: interval,
		scope:    scope,
	}
}

// Name returns the collector's name.
func (c *PlanCollector) Name() string { return "plan" }

// Run starts the plan stats collection loop.
func (c *PlanCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ulog := logging.NewUnifiedLogger("groved.collector.plan")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	var lastFullScan time.Time

	scan := func() {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 1*time.Second {
				ulog.Debug("Slow plan scan detected").Field("duration", d).Log(ctx)
			}
		}()

		state := st.Get()
		focus := st.GetFocus()

		// Determine if this is a full scan or focused scan
		doFullScan := len(focus) == 0 || time.Since(lastFullScan) >= planBackgroundInterval

		if len(focus) == 0 && !doFullScan {
			return // No focus, not time for background scan — skip entirely
		}

		if doFullScan {
			lastFullScan = time.Now()
		}

		planStats, err := enrichment.FetchPlanStatsMap()
		if err != nil {
			return
		}

		// Build case-insensitive focus map
		focusLower := make(map[string]struct{}, len(focus))
		for p := range focus {
			focusLower[strings.ToLower(p)] = struct{}{}
		}

		var deltas []*models.WorkspaceDelta

		for k, v := range state.Workspaces {
			_, isFocused := focusLower[strings.ToLower(k)]
			// Always scan focused workspaces. On full-scan ticks, restrict
			// the background sweep to scope so a scoped daemon doesn't pay
			// for plan enrichment on out-of-scope workspaces.
			if isFocused || (doFullScan && store.IsInScope(k, c.scope)) {
				if stats, ok := planStats[k]; ok {
					if !store.PlanStatsEqual(v.PlanStats, stats) {
						deltas = append(deltas, &models.WorkspaceDelta{
							Path:      k,
							PlanStats: stats,
						})
					}
				}
			}
		}

		if len(deltas) > 0 {
			updates <- store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "plan",
				Scanned: len(deltas),
				Payload: deltas,
			}
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
