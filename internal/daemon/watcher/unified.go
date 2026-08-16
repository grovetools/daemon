package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// watchBackend is the slice of fsnotify the UnifiedWatcher depends on. The
// indirection exists so watch-registration recovery is testable: the real
// kqueue/inotify failure modes (an Add that half-installs a directory, a watch
// dropped underneath us) can be reproduced deterministically by a fake.
type watchBackend interface {
	Add(path string) error
	Remove(path string) error
	// WatchList returns the paths the backend itself believes are watched —
	// user-added paths only, never the per-file watches kqueue installs
	// internally underneath a watched directory.
	WatchList() []string
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

// fsnotifyBackend adapts *fsnotify.Watcher (channel fields) to watchBackend
// (channel accessors).
type fsnotifyBackend struct{ w *fsnotify.Watcher }

func (b fsnotifyBackend) Add(path string) error         { return b.w.Add(path) }
func (b fsnotifyBackend) Remove(path string) error      { return b.w.Remove(path) }
func (b fsnotifyBackend) WatchList() []string           { return b.w.WatchList() }
func (b fsnotifyBackend) Events() <-chan fsnotify.Event { return b.w.Events }
func (b fsnotifyBackend) Errors() <-chan error          { return b.w.Errors }
func (b fsnotifyBackend) Close() error                  { return b.w.Close() }

// WatchStats reports the outcome of the most recent watch-set reconciliation.
// `Failed` and `Recovered` are the health signals the old bookkeeping could not
// express: a registration the backend refused, and a watch the backend had lost
// while our own map still recorded it as healthy.
type WatchStats struct {
	Desired   int
	Watched   int
	Added     int
	Removed   int
	Recovered int
	Failed    int
	Refreshes int
}

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
	fsWatcher       watchBackend
	store           *store.Store
	handlers        []DomainHandler
	watchCounts     map[string]int
	batchInterval   time.Duration
	refreshInterval time.Duration
	ulog            *logging.UnifiedLogger
	stats           WatchStats
	failedPaths     map[string]bool
	mu              sync.Mutex
}

// NewUnifiedWatcher creates a new UnifiedWatcher with a single fsnotify instance.
func NewUnifiedWatcher(st *store.Store, batchInterval time.Duration) (*UnifiedWatcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return newUnifiedWatcherWithBackend(st, batchInterval, fsnotifyBackend{w: fw}), nil
}

// newUnifiedWatcherWithBackend builds a watcher over an arbitrary backend. Only
// tests substitute one; production always goes through NewUnifiedWatcher.
func newUnifiedWatcherWithBackend(st *store.Store, batchInterval time.Duration, backend watchBackend) *UnifiedWatcher {
	return &UnifiedWatcher{
		fsWatcher:       backend,
		store:           st,
		handlers:        make([]DomainHandler, 0),
		watchCounts:     make(map[string]int),
		failedPaths:     make(map[string]bool),
		batchInterval:   batchInterval,
		refreshInterval: 15 * time.Second,
		ulog:            logging.NewUnifiedLogger("groved.watcher.unified"),
	}
}

// WatchStats returns a copy of the last refresh's reconciliation counters.
func (w *UnifiedWatcher) WatchStats() WatchStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
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
	defer func() { _ = w.fsWatcher.Close() }()

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
	started := w.WatchStats()
	w.store.ApplyUpdate(store.Update{
		Type:   store.UpdateWatcherStatus,
		Source: "unified_watcher",
		Payload: map[string]interface{}{
			"event":    "started",
			"handlers": names,
			"paths":    started.Watched,
			"failed":   started.Failed,
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
		case event, ok := <-w.fsWatcher.Events():
			if !ok {
				return
			}
			eventBuffer = append(eventBuffer, event)
		case err, ok := <-w.fsWatcher.Errors():
			if !ok {
				return
			}
			w.ulog.Error("fsnotify watcher error").Err(err).Log(ctx)
		case <-batchTicker.C:
			if len(eventBuffer) > 0 {
				w.dispatch(ctx, eventBuffer)
				// A directory created inside a watched path (e.g. `flow plan
				// init` under a plans dir) is invisible to fsnotify until it is
				// added as its own watch. Recompute immediately instead of
				// waiting for the periodic refresh, so a config write that
				// follows the creation within the refresh interval still
				// delivers.
				if containsDirCreate(eventBuffer) {
					w.refreshWatches()
				}
				eventBuffer = nil
			}
		case <-refreshTicker.C:
			w.refreshWatches()
		case update := <-sub:
			// Refresh on workspace changes (new/removed) and on focus changes so
			// the git handler's watch set follows the user's nav scroll.
			if update.Type == store.UpdateWorkspaces || update.Type == store.UpdateFocus {
				w.refreshWatches()
			}
			// Broadcast store updates to all handlers
			w.mu.Lock()
			for _, h := range w.handlers {
				h.HandleStoreUpdate(update)
			}
			w.mu.Unlock()
			// A config reload changes which paths handlers want watched, but
			// only they can read the new config — so the refresh has to run
			// AFTER they have processed the update, not before it like the
			// workspace/focus edges above. Without this, a `grove join` or a
			// re-rooted notebook stayed uncovered until the 15s ticker, and a
			// reload could not repair a degraded watch set at all.
			if update.Type == store.UpdateConfigReload {
				w.refreshWatches()
			}
		}
	}
}

// dispatch routes batched events to matching handlers in parallel goroutines.
func (w *UnifiedWatcher) dispatch(ctx context.Context, events []fsnotify.Event) {
	w.mu.Lock()
	handlers := make([]DomainHandler, len(w.handlers))
	copy(handlers, w.handlers)
	w.mu.Unlock()

	// Ingest is counted once per batch; matches are counted per handler, so
	// matched/raw is a fan-out ratio (>1 is normal when several handlers care
	// about the same path) and both rates are published per minute. The pair
	// is what distinguishes "the filesystem is busy" from "our filters are
	// too broad" — the two causes of watcher-driven load look identical in
	// CPU alone.
	telemetry.RecordWatcherBatch(len(events))

	for _, h := range handlers {
		var matched []fsnotify.Event
		for _, e := range events {
			if h.MatchesEvent(e) {
				matched = append(matched, e)
			}
		}
		if len(matched) > 0 {
			telemetry.RecordWatcherMatched(len(matched))
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

// refreshWatches recomputes watch paths from all handlers and reconciles the
// shared fsnotify watcher against them, using reference counting to handle
// overlapping paths.
//
// The backend's own WatchList — not this watcher's watchCounts map — is the
// authority on what is installed. watchCounts records intent; only the backend
// knows what survived. A watch the backend lost or never finished installing
// used to stay recorded as healthy forever, which is how a daemon ended up in
// the observed degraded states: fully blind, or directory-only (rename-style
// saves captured, in-place writes never), for the life of the process.
func (w *UnifiedWatcher) refreshWatches() {
	ctx := context.Background()
	w.mu.Lock()
	defer w.mu.Unlock()

	workspaces := w.store.GetWorkspaces()

	desiredCounts := make(map[string]int)
	for _, h := range w.handlers {
		paths := h.ComputeWatchPaths(workspaces)
		for _, p := range paths {
			p = filepath.Clean(p)
			// Lstat first so a dangling symlink is distinguishable from an
			// ordinary missing path and never reaches fsnotify.Add. The fsnotify
			// version pinned by this module likewise skips dangling children
			// while installing a watch for their parent directory.
			if dangling, err := isDanglingSymlink(p); err == nil && dangling {
				w.ulog.Debug("Skipping watch for dangling symlink").Field("path", p).Log(ctx)
				continue
			}
			desiredCounts[p]++
		}
	}

	installed := w.backendWatches()

	// Remove watches no longer needed by any handler. The drop set is the union
	// of our bookkeeping and backend truth, so a stale backend registration is
	// torn down even when watchCounts never knew about it.
	drop := make(map[string]struct{})
	for p := range w.watchCounts {
		if desiredCounts[p] == 0 {
			drop[p] = struct{}{}
		}
	}
	for p := range installed {
		if desiredCounts[p] == 0 {
			drop[p] = struct{}{}
		}
	}
	removed := 0
	for p := range drop {
		if _, ok := installed[p]; ok {
			if err := w.fsWatcher.Remove(p); err != nil {
				w.ulog.Debug("Failed to remove watch").Err(err).Field("path", p).Log(ctx)
			}
		}
		delete(w.watchCounts, p)
		removed++
		w.ulog.Debug("Watch removed").Field("path", p).Log(ctx)
	}

	// Add missing watches. "Missing" is judged against the backend list, so a
	// dropped watch is re-added even though watchCounts still holds it.
	added, recovered := 0, 0
	newFailedPaths := make(map[string]bool)
	for p, count := range desiredCounts {
		if _, ok := installed[p]; ok {
			w.watchCounts[p] = count
			continue
		}
		if w.watchCounts[p] > 0 {
			// Recorded as watched, absent from the backend: a watch we lost.
			recovered++
			w.ulog.Warn("Recovering dropped watch").Field("path", p).Log(ctx)
		}
		// Skip paths that don't exist on disk
		if _, err := os.Stat(p); err != nil {
			w.ulog.Debug("Skipping watch for missing path").Err(err).Field("path", p).Log(ctx)
			delete(w.watchCounts, p)
			continue
		}
		if err := w.fsWatcher.Add(p); err != nil {
			// A failed Add is not necessarily a no-op. On macOS, kqueue installs
			// the directory watch and then opens one vnode watch per child file;
			// if that second half fails partway, Add returns an error with the
			// directory registration already in place and its NOTE_WRITE flag
			// set. The next Add then sees "already watching", skips
			// watchDirectoryFiles entirely, and reports success while the
			// per-file watches stay missing — a directory that reports renames
			// and never reports in-place writes, permanently. Tearing the
			// partial registration down makes the retry start from a clean
			// slate instead of inheriting the poisoned state.
			if rmErr := w.fsWatcher.Remove(p); rmErr != nil {
				w.ulog.Debug("Failed to clean up partial watch").Err(rmErr).Field("path", p).Log(ctx)
			}
			delete(w.watchCounts, p)
			newFailedPaths[p] = true
			entry := w.ulog.Debug("Failed to watch path")
			if !w.failedPaths[p] {
				entry = w.ulog.Warn("Failed to watch path")
			}
			entry.Err(err).Field("path", p).Log(ctx)
			continue
		}
		w.watchCounts[p] = count
		added++
		w.ulog.Debug("Watch added").Field("path", p).Log(ctx)
	}

	// Verify against backend truth before publishing the result: a path counts
	// as watched only once the backend lists it. An Add that reports success
	// without registering leaves nothing behind to notice otherwise.
	if added > 0 {
		verified := w.backendWatches()
		for p := range w.watchCounts {
			if _, ok := verified[p]; ok {
				continue
			}
			if rmErr := w.fsWatcher.Remove(p); rmErr != nil {
				w.ulog.Debug("Failed to clean up unverified watch").Err(rmErr).Field("path", p).Log(ctx)
			}
			delete(w.watchCounts, p)
			if added > 0 {
				added--
			}
			newFailedPaths[p] = true
			entry := w.ulog.Debug("Watch add reported success but backend does not list it")
			if !w.failedPaths[p] {
				entry = w.ulog.Warn("Watch add reported success but backend does not list it")
			}
			entry.Field("path", p).Log(ctx)
		}
	}

	for p := range w.failedPaths {
		if !newFailedPaths[p] && w.watchCounts[p] > 0 {
			w.ulog.Info("Watch path recovered").Field("path", p).Log(ctx)
		}
	}

	previousStats := w.stats
	w.stats = WatchStats{
		Desired:   len(desiredCounts),
		Watched:   len(w.watchCounts),
		Added:     added,
		Removed:   removed,
		Recovered: recovered,
		Failed:    len(newFailedPaths),
		Refreshes: w.stats.Refreshes + 1,
	}
	w.failedPaths = newFailedPaths

	// Watch-registration boundary: one aggregate line per refresh keeps the
	// evidence of fsnotify coverage in the live log without a per-path line
	// for every workspace rescan (which ran to thousands of lines a day).
	// `failed`/`recovered` are what make a degraded watch set visible in the
	// log at all — the symptom is silence everywhere else.
	if watchSetChanged(previousStats, w.stats) {
		w.ulog.Info("Watch set updated").
			Field("added", added).
			Field("removed", removed).
			Field("recovered", recovered).
			Field("failed", len(newFailedPaths)).
			Field("total", len(w.watchCounts)).
			Log(ctx)
	}
}

// isDanglingSymlink reports only symlinks whose target is absent. Lstat is
// intentional: Stat alone collapses this case into the ordinary missing-path
// branch and lets the broken link reach fsnotify on the next refresh.
func isDanglingSymlink(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}
	_, err = os.Stat(path)
	if err == nil {
		return false, nil
	}
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, err
}

// watchSetChanged is the aggregate-log delta gate. Added/removed/recovered are
// edge counters, while Desired/Watched/Failed are levels. Comparing the levels
// prevents a pinned failure count from producing an info line every refresh;
// the edge counters preserve real reconciliation work that leaves levels equal.
func watchSetChanged(previous, current WatchStats) bool {
	return current.Added > 0 || current.Removed > 0 || current.Recovered > 0 ||
		current.Desired != previous.Desired || current.Watched != previous.Watched ||
		current.Failed != previous.Failed
}

// backendWatches snapshots the backend's watch list as a set of cleaned paths.
// Callers must hold w.mu.
func (w *UnifiedWatcher) backendWatches() map[string]struct{} {
	list := w.fsWatcher.WatchList()
	set := make(map[string]struct{}, len(list))
	for _, p := range list {
		set[filepath.Clean(p)] = struct{}{}
	}
	return set
}

// containsDirCreate reports whether any batched event created a directory that
// still exists — the signal that the watch set may need to grow mid-interval.
func containsDirCreate(events []fsnotify.Event) bool {
	for _, event := range events {
		if event.Op&fsnotify.Create == 0 {
			continue
		}
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}
