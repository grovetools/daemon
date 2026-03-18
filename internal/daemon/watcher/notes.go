package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/enrichment"
	"github.com/sirupsen/logrus"
)

// noteTypes are the note categories to watch for filesystem changes.
var noteTypes = []string{"current", "issues", "inbox", "docs", "completed", "review", "in-progress"}

// NoteHandler implements DomainHandler for watching note directories.
// When note files change, it triggers an immediate note counts refresh
// rather than waiting for the NoteCollector's polling interval.
type NoteHandler struct {
	store   *store.Store
	cfg     *config.Config
	locator *workspace.NotebookLocator
	log     *logrus.Entry

	// Maps watched path -> workspace node
	watchedPaths map[string]*workspace.WorkspaceNode
	pathsMutex   sync.RWMutex

	// Debounce timer for note counts refresh
	refreshTimer *time.Timer
	refreshMu    sync.Mutex
	debounceMs   int
}

// NewNoteHandler creates a new NoteHandler instance.
func NewNoteHandler(st *store.Store, cfg *config.Config, debounceMs int) *NoteHandler {
	if debounceMs <= 0 {
		debounceMs = 3000
	}

	return &NoteHandler{
		store:        st,
		cfg:          cfg,
		locator:      workspace.NewNotebookLocator(cfg),
		log:          logging.NewLogger("groved.notes.watcher"),
		watchedPaths: make(map[string]*workspace.WorkspaceNode),
		debounceMs:   debounceMs,
	}
}

func (h *NoteHandler) Name() string {
	return "notes"
}

// ComputeWatchPaths returns note directories for all workspaces across all note types.
func (h *NoteHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*workspace.WorkspaceNode)

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		for _, noteType := range noteTypes {
			notesDir, err := h.locator.GetNotesDir(node, noteType)
			if err != nil || notesDir == "" {
				continue
			}
			// Only watch directories that actually exist on disk
			if _, err := os.Stat(notesDir); err != nil {
				continue
			}
			// Watch the note type directory (not recursive — notes are flat files)
			newWatches[notesDir] = node
		}
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.pathsMutex.Unlock()

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}
	return paths
}

func (h *NoteHandler) MatchesEvent(event fsnotify.Event) bool {
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}

	// Only care about markdown files
	if !strings.HasSuffix(event.Name, ".md") {
		return false
	}

	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watchedPath := range h.watchedPaths {
		if event.Name == watchedPath || filepath.HasPrefix(event.Name, watchedPath+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// HandleEvents triggers a debounced note counts refresh when note files change.
func (h *NoteHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	h.log.WithField("count", len(events)).Debug("Note file changes detected")
	h.triggerRefresh()
	return nil
}

func (h *NoteHandler) HandleStoreUpdate(update store.Update) {
	if update.Type == store.UpdateConfigReload {
		newCfg, err := config.LoadDefault()
		if err != nil {
			h.log.WithError(err).Error("Failed to reload config")
			return
		}
		h.cfg = newCfg
		h.locator = workspace.NewNotebookLocator(newCfg)
	}
}

func (h *NoteHandler) OnStart(ctx context.Context) {
	// No initial sync needed — the NoteCollector handles the first scan.
}

// triggerRefresh debounces the note counts re-scan. Because FetchNoteCountsMap
// shells out to `nb list`, we use a longer debounce (3s default) to avoid
// excessive process spawning.
func (h *NoteHandler) triggerRefresh() {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
	}

	h.refreshTimer = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		h.log.Debug("Refreshing note counts after file change")

		noteCounts, err := enrichment.FetchNoteCountsMap()
		if err != nil {
			h.log.WithError(err).Error("Failed to fetch note counts")
			return
		}

		state := h.store.Get()
		newWorkspaces := make(map[string]*models.EnrichedWorkspace)
		scanned := 0

		for k, v := range state.Workspaces {
			cpy := *v
			if cpy.WorkspaceNode != nil {
				if counts, ok := noteCounts[cpy.Name]; ok {
					cpy.NoteCounts = counts
					scanned++
				}
			}
			newWorkspaces[k] = &cpy
		}

		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdateWorkspaces,
			Source:  "note_watcher",
			Scanned: scanned,
			Payload: newWorkspaces,
		})
	})
}
