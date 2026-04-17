package collector

import (
	"context"
	"strings"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/enrichment"
)

// noteBackgroundInterval is how often to update non-focused workspaces.
const noteBackgroundInterval = 10 * time.Minute

// NoteCollector updates note counts for all workspaces.
type NoteCollector struct {
	interval time.Duration
}

// NewNoteCollector creates a new NoteCollector with the specified interval.
// If interval is 0, defaults to 5 minutes.
func NewNoteCollector(interval time.Duration) *NoteCollector {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	return &NoteCollector{
		interval: interval,
	}
}

// Name returns the collector's name.
func (c *NoteCollector) Name() string { return "note" }

// Run starts the note counts collection loop.
func (c *NoteCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ulog := logging.NewUnifiedLogger("groved.collector.note")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	var lastFullScan time.Time

	scan := func() {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 1*time.Second {
				ulog.Debug("Slow note scan detected").Field("duration", d).Log(ctx)
			}
		}()

		state := st.Get()
		focus := st.GetFocus()

		// Determine if this is a full scan or focused scan
		doFullScan := len(focus) == 0 || time.Since(lastFullScan) >= noteBackgroundInterval

		if len(focus) == 0 && !doFullScan {
			return // No focus, not time for background scan — skip entirely
		}

		if doFullScan {
			lastFullScan = time.Now()
		}

		var nodes []*workspace.WorkspaceNode
		for _, ws := range state.Workspaces {
			if ws.WorkspaceNode != nil {
				nodes = append(nodes, ws.WorkspaceNode)
			}
		}

		cfg, _ := config.LoadDefault()
		locator := workspace.NewNotebookLocator(cfg)

		indexStart := time.Now()
		noteIndex := enrichment.IndexNotesInProcess(nodes, locator)
		// Derive counts from the index — single walk instead of two
		noteCounts := enrichment.DeriveCountsFromIndex(noteIndex)
		ulog.Info("Note index built").
			Field("entries", len(noteIndex)).
			Field("duration", time.Since(indexStart).Round(time.Millisecond)).
			Field("nodes", len(nodes)).
			Log(ctx)

		// Build case-insensitive focus map
		focusLower := make(map[string]struct{}, len(focus))
		for p := range focus {
			focusLower[strings.ToLower(p)] = struct{}{}
		}

		var deltas []*models.WorkspaceDelta

		for k, v := range state.Workspaces {
			_, isFocused := focusLower[strings.ToLower(k)]
			if doFullScan || isFocused {
				if v.WorkspaceNode != nil {
					newCounts, ok := noteCounts[v.Name]
					if !ok {
						newCounts = &models.NoteCounts{}
					}
					if !store.NoteCountsEqual(v.NoteCounts, newCounts) {
						deltas = append(deltas, &models.WorkspaceDelta{
							Path:       k,
							NoteCounts: newCounts,
						})
					}
				}
			}
		}

		if len(deltas) > 0 {
			updates <- store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "note",
				Scanned: len(deltas),
				Payload: deltas,
			}
		}

		updates <- store.Update{
			Type:    store.UpdateNoteIndex,
			Source:  "note",
			Payload: noteIndex,
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
