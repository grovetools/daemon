package watcher

import (
	"context"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	navbindings "github.com/grovetools/nav/pkg/bindings"
)

// NavHandler implements DomainHandler for watching nav's live keymap state file
// (sessions.yml). When the file changes — whether written by nav itself or via
// the daemon's API write path — it reloads the bindings and applies a store
// update so the new state is broadcast to clients.
//
// The API write path also writes sessions.yml and fires this watcher, producing
// an expected double-broadcast. This handler intentionally does NOT track write
// origins to suppress the echo: the client side absorbs the duplicate
// idempotently.
type NavHandler struct {
	store *store.Store
	ulog  *logging.UnifiedLogger
}

// NewNavHandler creates a new NavHandler instance.
func NewNavHandler(st *store.Store) *NavHandler {
	return &NavHandler{
		store: st,
		ulog:  logging.NewUnifiedLogger("groved.watcher.nav"),
	}
}

func (h *NavHandler) Name() string {
	return "nav"
}

// ComputeWatchPaths returns the directory containing the nav sessions state file.
func (h *NavHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	return []string{filepath.Dir(navbindings.DefaultPath())}
}

func (h *NavHandler) MatchesEvent(event fsnotify.Event) bool {
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}
	return filepath.Base(event.Name) == "sessions.yml"
}

// HandleEvents reloads the nav bindings and applies a store update.
func (h *NavHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	file, err := navbindings.Load(navbindings.DefaultPath())
	if err != nil {
		// The file may be mid-write; skip and wait for the next event.
		h.ulog.Debug("Failed to load nav sessions file, skipping").Err(err).Log(ctx)
		return nil
	}

	h.store.ApplyUpdate(store.Update{
		Type:    store.UpdateNavBindings,
		Source:  "nav_watcher",
		Payload: file,
	})
	return nil
}

func (h *NavHandler) HandleStoreUpdate(update store.Update) {
	// No-op: NavHandler does not react to store updates.
}

func (h *NavHandler) OnStart(ctx context.Context) {
	// No initial sync needed — the first sessions.yml change triggers a load.
}
