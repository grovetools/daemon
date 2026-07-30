//go:build darwin

package watcher

import (
	"context"
	"time"

	"github.com/fsnotify/fsevents"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

const gitEventTopologyDebounce = 250 * time.Millisecond

// runGlobalGitEvents uses one recursive FSEvents stream for every discovered
// repository/worktree and its external git dirs. FSEvents implements these as
// path filters on one stream, not one file descriptor per directory.
func runGlobalGitEvents(ctx context.Context, st *store.Store, handler *GitHandler) error {
	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	var stream *fsevents.EventStream
	var routes []gitEventRoute
	var events <-chan []fsevents.Event

	// One cache for the source lifetime. Topology rebuilds explicitly invalidate
	// its proofs, and discovered external git inputs expand this same bounded
	// recursive stream before those proofs are allowed to suppress.
	deadCache := newDeadSubtreeCache(ctx)
	stop := func() {
		if stream != nil {
			stream.Stop()
			stream = nil
			events = nil
		}
	}
	defer stop()

	rebuild := func() error {
		stop()
		// No proof may survive a period outside the watcher topology. This also
		// generation-invalidates probes that were in flight during the rebuild.
		deadCache.dropAll()
		routes = buildGitEventRoutes(ctx, st.GetWorkspaces())
		paths := make([]string, 0, len(routes))
		seen := make(map[string]bool)
		for _, route := range routes {
			if !seen[route.root] {
				paths = append(paths, route.root)
				seen[route.root] = true
			}
		}
		inputRoots := deadCache.watchRoots()
		for _, root := range inputRoots {
			if !seen[root] {
				paths = append(paths, root)
				seen[root] = true
			}
		}
		if len(paths) == 0 {
			deadCache.activateWatchRoots(nil)
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
		deadCache.activateWatchRoots(inputRoots)
		events = stream.Events
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

	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-sub:
			if update.Type == store.UpdateWorkspaces {
				if topologyTimer != nil {
					topologyTimer.Stop()
				}
				topologyTimer = time.NewTimer(gitEventTopologyDebounce)
				topologyC = topologyTimer.C
			}
		case <-deadCache.watchChanged:
			if topologyTimer != nil {
				topologyTimer.Stop()
			}
			topologyTimer = time.NewTimer(gitEventTopologyDebounce)
			topologyC = topologyTimer.C
		case <-topologyC:
			topologyC = nil
			topologyTimer = nil
			if err := rebuild(); err != nil {
				return err
			}
		case batch, ok := <-events:
			if !ok {
				return nil
			}
			// The recursive FSEvents stream is the daemon's highest-volume
			// event source; raw-vs-matched here is what tells a user whether
			// a busy machine or an over-broad watch set is driving git scans.
			telemetry.RecordWatcherBatch(len(batch))
			for _, event := range batch {
				if event.Flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped) != 0 {
					// A dropped/coalesced stream is a correctness signal: no proof may
					// survive potentially unseen index/ignore/config writes. Schedule
					// every repo once; per-repo debounce still bounds the burst.
					deadCache.dropAll()
					scheduled := 0
					for _, route := range routes {
						for _, node := range route.nodes {
							handler.scheduleScan(node)
							scheduled++
						}
					}
					telemetry.RecordWatcherDropped(scheduled)
					continue
				}
				// Resolve and route before filtering: only a route built from git's
				// actual gitdir/commondir identity may classify object or lock churn
				// as internal. Textual .git suffixes are valid working-tree names.
				path := resolveEventPath(event.Path)
				route, nodes := routeGitEvent(path, routes)
				// External ignore/config sources have no repository route. Match them
				// first, invalidate fleet-wide, and schedule each workspace once so
				// the effective status change is itself observable.
				if deadCache.ObserveGlobal(path) {
					scheduled := 0
					seenNodes := make(map[string]bool)
					for _, candidate := range routes {
						for _, node := range candidate.nodes {
							if node != nil && !seenNodes[node.Path] {
								seenNodes[node.Path] = true
								handler.scheduleScan(node)
								scheduled++
							}
						}
					}
					telemetry.RecordWatcherMatched(scheduled)
					continue
				}
				if !relevantGitEvent(route, path) {
					continue
				}
				telemetry.RecordWatcherMatched(len(nodes))
				// Invalidation runs FIRST and always falls through to a scan: an
				// ignore-rule or index write both voids cached proofs and is
				// itself news.
				deadCache.Observe(route, path)
				// !route.internal is the one place the .git split is enforced.
				// Without it a .gitignore containing `index` or `HEAD` could
				// suppress git-internal writes and grove would go blind to
				// commits and branch switches.
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
