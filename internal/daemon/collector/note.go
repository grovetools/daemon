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
	logger := logging.NewLogger("collector.note")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	var lastFullScan time.Time

	scan := func() {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 1*time.Second {
				logger.WithField("duration", d).Debug("Slow note scan detected")
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
		noteCounts := enrichment.CountNotesInProcess(nodes, locator)

		indexStart := time.Now()
		noteIndex := enrichment.IndexNotesInProcess(nodes, locator)
		logger.WithFields(map[string]interface{}{
			"entries":  len(noteIndex),
			"duration": time.Since(indexStart).Round(time.Millisecond),
			"nodes":    len(nodes),
		}).Info("Note index built")

		// Build case-insensitive focus map
		focusLower := make(map[string]struct{}, len(focus))
		for p := range focus {
			focusLower[strings.ToLower(p)] = struct{}{}
		}

		// Clone existing workspaces and update note counts
		newWorkspaces := make(map[string]*models.EnrichedWorkspace)
		scanned := 0

		for k, v := range state.Workspaces {
			cpy := *v

			// Check if this workspace should be updated
			_, isFocused := focusLower[strings.ToLower(k)]
			if doFullScan || isFocused {
				if cpy.WorkspaceNode != nil {
					if counts, ok := noteCounts[cpy.Name]; ok {
						cpy.NoteCounts = counts
					} else {
						// Ensure counts reset to 0 if all notes are deleted
						cpy.NoteCounts = &models.NoteCounts{}
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
