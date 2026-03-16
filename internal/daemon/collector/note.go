package collector

import (
	"context"
	"strings"
	"time"

	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/enrichment"
)

// noteBackgroundInterval is how often to update non-focused workspaces.
const noteBackgroundInterval = 2 * time.Minute

// NoteCollector updates note counts for all workspaces.
type NoteCollector struct {
	interval time.Duration
}

// NewNoteCollector creates a new NoteCollector with the specified interval.
// If interval is 0, defaults to 60 seconds.
func NewNoteCollector(interval time.Duration) *NoteCollector {
	if interval == 0 {
		interval = 60 * time.Second
	}
	return &NoteCollector{
		interval: interval,
	}
}

// Name returns the collector's name.
func (c *NoteCollector) Name() string { return "note" }

// Run starts the note counts collection loop.
func (c *NoteCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	logger := logging.NewLogger("collector.note")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	var lastFullScan time.Time

	scan := func() {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 200*time.Millisecond {
				logger.WithField("duration", d).Warn("Slow note scan detected")
			}
		}()

		// FetchNoteCountsMap returns counts by workspace name, not path
		noteCounts, err := enrichment.FetchNoteCountsMap()
		if err != nil {
			return
		}

		state := st.Get()
		focus := st.GetFocus()

		// Determine if this is a full scan or focused scan
		doFullScan := len(focus) == 0 || time.Since(lastFullScan) >= noteBackgroundInterval
		if doFullScan {
			lastFullScan = time.Now()
		}

		// Build case-insensitive focus map
		focusLower := make(map[string]struct{}, len(focus))
		for p := range focus {
			focusLower[strings.ToLower(p)] = struct{}{}
		}

		// Clone existing workspaces and update note counts
		newWorkspaces := make(map[string]*enrichment.EnrichedWorkspace)
		scanned := 0

		for k, v := range state.Workspaces {
			cpy := *v

			// Check if this workspace should be updated
			_, isFocused := focusLower[strings.ToLower(k)]
			if doFullScan || isFocused {
				// Note counts are indexed by workspace name, not path
				if cpy.WorkspaceNode != nil {
					if counts, ok := noteCounts[cpy.Name]; ok {
						cpy.NoteCounts = counts
					}
				}
				scanned++
			}

			newWorkspaces[k] = &cpy
		}

		updates <- store.Update{
			Type:    store.UpdateWorkspaces,
			Source:  "note",
			Scanned: scanned,
			Payload: newWorkspaces,
		}
	}

	// Wait for workspaces to be populated first
	time.Sleep(3 * time.Second)
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
