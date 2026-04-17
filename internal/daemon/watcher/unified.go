package watcher

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// DomainHandler represents a domain-specific event processor that plugs into the
// UnifiedWatcher infrastructure. Each handler (skills, flow, notes, memory) implements this
// interface to receive batched filesystem events and store updates.
type DomainHandler interface {
	Name() string
	ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string
	MatchesEvent(event fsnotify.Event) bool
	HandleEvents(ctx context.Context, events []fsnotify.Event) error
	HandleStoreUpdate(update store.Update)
	OnStart(ctx context.Context)
}

// UnifiedWatcher manages a single fsnotify.Watcher instance and dispatches batched
// events to registered DomainHandlers. It replaces multiple independent watchers with
// shared filesystem monitoring, reference-counted watch paths, and coordinated refresh.
type UnifiedWatcher struct {
	fsWatcher       *fsnotify.Watcher
	store           *store.Store
	handlers        []DomainHandler
	watchCounts     map[string]int
	batchInterval   time.Duration
	refreshInterval time.Duration
	ulog            *logging.UnifiedLogger
	mu              sync.Mutex
}

// NewUnifiedWatcher creates a new UnifiedWatcher with a single fsnotify instance.
func NewUnifiedWatcher(st *store.Store, batchInterval time.Duration) (*UnifiedWatcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &UnifiedWatcher{
		fsWatcher:       fw,
		store:           st,
		handlers:        make([]DomainHandler, 0),
		watchCounts:     make(map[string]int),
		batchInterval:   batchInterval,
		refreshInterval: 15 * time.Second,
		ulog:            logging.NewUnifiedLogger("groved.watcher.unified"),
	}, nil
}

// Register adds a DomainHandler to the unified watcher.
func (w *UnifiedWatcher) Register(h DomainHandler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers = append(w.handlers, h)

	w.store.ApplyUpdate(store.Update{
		Type:   store.UpdateWatcherStatus,
		Source: "unified_watcher",
		Payload: map[string]string{
			"event":   "handler_registered",
			"handler": h.Name(),
		},
	})
}

// Start begins the unified watch loop. It runs until ctx is canceled.
func (w *UnifiedWatcher) Start(ctx context.Context) {
	sub := w.store.Subscribe()
	defer w.store.Unsubscribe(sub)
	defer w.fsWatcher.Close()

	batchTicker := time.NewTicker(w.batchInterval)
	defer batchTicker.Stop()

	refreshTicker := time.NewTicker(w.refreshInterval)
	defer refreshTicker.Stop()

	var eventBuffer []fsnotify.Event

	// Initial watch path setup
	w.refreshWatches()

	// Broadcast watcher started
	names := make([]string, len(w.handlers))
	for i, h := range w.handlers {
		names[i] = h.Name()
	}
	w.store.ApplyUpdate(store.Update{
		Type:   store.UpdateWatcherStatus,
		Source: "unified_watcher",
		Payload: map[string]interface{}{
			"event":    "started",
			"handlers": names,
			"paths":    len(w.watchCounts),
		},
	})

	// Notify handlers to perform startup operations (e.g., initial skill sync)
	w.mu.Lock()
	for _, h := range w.handlers {
		h.OnStart(ctx)
	}
	w.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			eventBuffer = append(eventBuffer, event)
		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			w.ulog.Error("fsnotify watcher error").Err(err).Log(ctx)
		case <-batchTicker.C:
			if len(eventBuffer) > 0 {
				w.dispatch(ctx, eventBuffer)
				eventBuffer = nil
			}
		case <-refreshTicker.C:
			w.refreshWatches()
		case update := <-sub:
			if update.Type == store.UpdateWorkspaces {
				w.refreshWatches()
			}
			// Broadcast store updates to all handlers
			w.mu.Lock()
			for _, h := range w.handlers {
				h.HandleStoreUpdate(update)
			}
			w.mu.Unlock()
		}
	}
}

// dispatch routes batched events to matching handlers in parallel goroutines.
func (w *UnifiedWatcher) dispatch(ctx context.Context, events []fsnotify.Event) {
	w.mu.Lock()
	handlers := make([]DomainHandler, len(w.handlers))
	copy(handlers, w.handlers)
	w.mu.Unlock()

	for _, h := range handlers {
		var matched []fsnotify.Event
		for _, e := range events {
			if h.MatchesEvent(e) {
				matched = append(matched, e)
			}
		}
		if len(matched) > 0 {
			go func(handler DomainHandler, evts []fsnotify.Event) {
				if err := handler.HandleEvents(ctx, evts); err != nil {
					w.ulog.Error("Handler failed to process events").
						Err(err).
						Field("handler", handler.Name()).
						Log(ctx)
				}
			}(h, matched)
		}
	}
}

// refreshWatches recomputes watch paths from all handlers and updates the shared
// fsnotify watcher using reference counting to handle overlapping paths.
func (w *UnifiedWatcher) refreshWatches() {
	ctx := context.Background()
	w.mu.Lock()
	defer w.mu.Unlock()

	workspaces := w.store.GetWorkspaces()

	desiredCounts := make(map[string]int)
	for _, h := range w.handlers {
		paths := h.ComputeWatchPaths(workspaces)
		for _, p := range paths {
			desiredCounts[p]++
		}
	}

	// Remove watches no longer needed by any handler
	for p := range w.watchCounts {
		if desiredCounts[p] == 0 {
			if err := w.fsWatcher.Remove(p); err != nil {
				w.ulog.Debug("Failed to remove watch").Err(err).Field("path", p).Log(ctx)
			}
			delete(w.watchCounts, p)
		}
	}

	// Add new watches or update reference counts
	for p, count := range desiredCounts {
		if w.watchCounts[p] == 0 {
			// Skip paths that don't exist on disk
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if err := w.fsWatcher.Add(p); err != nil {
				w.ulog.Debug("Failed to watch path").Err(err).Field("path", p).Log(ctx)
			} else {
				w.watchCounts[p] = count
			}
		} else {
			w.watchCounts[p] = count
		}
	}
}
