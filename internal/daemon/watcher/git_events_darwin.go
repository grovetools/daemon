//go:build darwin

package watcher

import (
	"context"
	"sort"
	"time"

	"github.com/fsnotify/fsevents"
	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

const gitEventTopologyDebounce = 250 * time.Millisecond

// recursiveGitEventPaths is deliberately route-only. External config/exclude
// inputs belong to the separate non-recursive exact-input observer and must
// never widen this high-volume FSEvents stream to $HOME or /.
func recursiveGitEventPaths(routes []gitEventRoute) []string {
	seen := make(map[string]bool)
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.root != "" && !seen[route.root] {
			seen[route.root] = true
			paths = append(paths, route.root)
		}
	}
	sort.Strings(paths)
	return paths
}

func scheduleGitFleet(handler *GitHandler, routes []gitEventRoute) int {
	scheduled := 0
	seenNodes := make(map[string]bool)
	for _, route := range routes {
		for _, node := range route.nodes {
			if node != nil && !seenNodes[node.Path] {
				seenNodes[node.Path] = true
				handler.scheduleScan(node)
				scheduled++
			}
		}
	}
	return scheduled
}

// runGlobalGitEvents uses one recursive FSEvents stream for repositories and
// git dirs, plus a separate non-recursive fsnotify observer for exact external
// config/exclude inputs. The latter may watch $HOME as a directory, but sibling
// churn is exact-filtered and can neither enter FSEvents nor schedule the fleet.
func runGlobalGitEvents(ctx context.Context, st *store.Store, handler *GitHandler) error {
	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	var stream *fsevents.EventStream
	var routes []gitEventRoute
	var events <-chan []fsevents.Event
	var inputWatcher *fsnotify.Watcher
	var inputEvents <-chan fsnotify.Event
	var inputErrors <-chan error

	deadCache := newDeadSubtreeCache(ctx)
	stop := func() {
		if stream != nil {
			stream.Stop()
			stream = nil
			events = nil
		}
		if inputWatcher != nil {
			_ = inputWatcher.Close()
			inputWatcher = nil
			inputEvents = nil
			inputErrors = nil
		}
	}
	defer stop()

	rebuild := func() error {
		stop()
		// No proof may survive a period outside either watcher topology. This also
		// generation-invalidates probes that were in flight during the rebuild.
		deadCache.dropAll()
		routes = buildGitEventRoutes(ctx, st.GetWorkspaces())
		paths := recursiveGitEventPaths(routes)
		if len(paths) == 0 {
			deadCache.activateInputWatchDirs(nil)
			return nil
		}
		stream = &fsevents.EventStream{
			Paths:   paths,
			Latency: time.Duration(handler.debounceMs) * time.Millisecond,
			Flags:   fsevents.FileEvents | fsevents.WatchRoot | fsevents.NoDefer,
		}
		if err := stream.Start(); err != nil {
			stream = nil
			return err
		}
		events = stream.Events

		// Exact inputs use non-recursive directory watches. Partial registration
		// is safe: only successfully active directories authorize new proofs.
		activeDirs := make([]string, 0)
		if watcher, err := fsnotify.NewWatcher(); err == nil {
			inputWatcher = watcher
			for _, dir := range deadCache.inputWatchDirs() {
				if err := watcher.Add(dir); err == nil {
					activeDirs = append(activeDirs, dir)
				}
			}
			inputEvents = watcher.Events
			inputErrors = watcher.Errors
		}
		deadCache.activateInputWatchDirs(activeDirs)
		return nil
	}
	if err := rebuild(); err != nil {
		return err
	}

	var topologyTimer *time.Timer
	var topologyC <-chan time.Time
	defer func() {
		if topologyTimer != nil {
			topologyTimer.Stop()
		}
	}()
	requestRebuild := func() {
		if topologyTimer != nil {
			topologyTimer.Stop()
		}
		topologyTimer = time.NewTimer(gitEventTopologyDebounce)
		topologyC = topologyTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-sub:
			if update.Type == store.UpdateWorkspaces {
				requestRebuild()
			}
		case <-deadCache.watchChanged:
			requestRebuild()
		case <-topologyC:
			topologyC = nil
			topologyTimer = nil
			if err := rebuild(); err != nil {
				return err
			}
		case event, ok := <-inputEvents:
			if !ok {
				inputEvents = nil
				continue
			}
			if event.Op&fsnotify.Chmod != 0 {
				continue
			}
			path := resolveEventPath(event.Name)
			if deadCache.ObserveGlobal(path) {
				telemetry.RecordWatcherMatched(scheduleGitFleet(handler, routes))
			}
		case _, ok := <-inputErrors:
			if !ok {
				inputErrors = nil
				continue
			}
			// An exact-input observer error means a config mutation may have been
			// missed. Fail open and rescan once; rebuilding re-establishes coverage.
			deadCache.dropAll()
			telemetry.RecordWatcherDropped(scheduleGitFleet(handler, routes))
			requestRebuild()
		case batch, ok := <-events:
			if !ok {
				return nil
			}
			telemetry.RecordWatcherBatch(len(batch))
			for _, event := range batch {
				if event.Flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped) != 0 {
					deadCache.dropAll()
					telemetry.RecordWatcherDropped(scheduleGitFleet(handler, routes))
					continue
				}
				path := resolveEventPath(event.Path)
				route, nodes := routeGitEvent(path, routes)
				if !relevantGitEvent(route, path) {
					continue
				}
				telemetry.RecordWatcherMatched(len(nodes))
				deadCache.Observe(route, path)
				if route != nil && !route.internal && deadCache.Suppress(route.root, path) {
					telemetry.RecordWatcherSuppressed()
					continue
				}
				for _, node := range nodes {
					handler.scheduleScan(node)
				}
			}
		}
	}
}
