//go:build darwin

package watcher

import (
	"context"
	"sort"
	"time"

	"github.com/fsnotify/fsevents"
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
// git dirs, plus a bounded polling observer for exact external config/exclude
// inputs. Polling avoids Darwin kqueue's per-sibling descriptors when an input
// lives under a broad directory such as $HOME or /.
func runGlobalGitEvents(ctx context.Context, st *store.Store, handler *GitHandler) error {
	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	var stream *fsevents.EventStream
	var routes []gitEventRoute
	var events <-chan []fsevents.Event
	var inputObserver *exactInputObserver
	var inputEvents <-chan time.Time

	deadCache := newDeadSubtreeCache(ctx)
	stop := func() {
		if stream != nil {
			stream.Stop()
			stream = nil
			events = nil
		}
		if inputObserver != nil {
			inputObserver.Close()
			inputObserver = nil
			inputEvents = nil
		}
	}
	defer stop()

	rebuild := func() error {
		stop()
		// Disable activation before replacing either watcher topology. This also
		// generation-invalidates probes that were in flight during the rebuild.
		deadCache.activateInputSnapshot(nil)
		routes = buildGitEventRoutes(ctx, st.GetWorkspaces())
		paths := recursiveGitEventPaths(routes)
		if len(paths) == 0 {
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

		// Exact inputs are bounded by deadSubtreeMaxObservedFiles. If observer
		// construction ever rejects the topology, no path is activated and
		// dead-subtree proofs safely remain disabled. Publishing the exact snapshot
		// and invalidating proofs under one cache lock closes both in-flight and
		// same-parent learned-input races.
		activePaths := []string(nil)
		if observer, err := newExactInputObserver(deadCache.inputObservationPaths(), gitInputPollInterval); err == nil {
			inputObserver = observer
			inputEvents = observer.Events()
			activePaths = observer.ActivePaths()
		}
		deadCache.activateInputSnapshot(activePaths)
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
		case <-inputEvents:
			changed := inputObserver.Poll()
			if len(changed) == 0 {
				continue
			}
			for _, path := range changed {
				deadCache.ObserveGlobal(path)
			}
			// Polling reports a conservative exact-target transition. Invalidate the
			// fleet once and rebuild so missing-target ancestry and config topology
			// are both refreshed before any proof can become active again.
			deadCache.dropAll()
			telemetry.RecordWatcherMatched(scheduleGitFleet(handler, routes))
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
