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
//
// On a scoped daemon, the background full scan indexes only in-scope
// workspaces plus anything currently focused. The full-process note
// index still gets populated from every workspace node on disk — the
// scope only guards which counts are recomputed and emitted as deltas.
type NoteCollector struct {
	interval time.Duration
	scope    string
}

// NewNoteCollector creates a new NoteCollector with the specified interval
// and scope. If interval is 0, defaults to 5 minutes. An empty scope covers
// every workspace.
func NewNoteCollector(interval time.Duration, scope string) *NoteCollector {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	return &NoteCollector{
		interval: interval,
		scope:    scope,
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

		// Build case-insensitive focus map up front — we use it both to
		// decide which nodes to index and which deltas to emit.
		focusLower := make(map[string]struct{}, len(focus))
		for p := range focus {
			focusLower[strings.ToLower(p)] = struct{}{}
		}

		// inScope reports whether this workspace should participate in this
		// tick — focused workspaces always qualify, otherwise scope controls.
		inScope := func(path string) bool {
			if _, ok := focusLower[strings.ToLower(path)]; ok {
				return true
			}
			return store.IsInScope(path, c.scope)
		}

		var nodes []*workspace.WorkspaceNode
		for k, ws := range state.Workspaces {
			if ws.WorkspaceNode == nil {
				continue
			}
			if !inScope(k) {
				continue
			}
			nodes = append(nodes, ws.WorkspaceNode)
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

		var deltas []*models.WorkspaceDelta

		for k, v := range state.Workspaces {
			_, isFocused := focusLower[strings.ToLower(k)]
			if !(doFullScan || isFocused) {
				continue
			}
			if !inScope(k) {
				continue
			}
			if v.WorkspaceNode == nil {
				continue
			}
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
