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
		routes = buildGitEventRoutes(ctx, st.GetWorkspaces())
		if len(routes) == 0 {
			return nil
		}
		paths := make([]string, 0, len(routes))
		for _, route := range routes {
			paths = append(paths, route.root)
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
		return nil
	}
	if err := rebuild(); err != nil {
		return err
	}

	// One cache for the life of the stream. It outlives topology rebuilds on
	// purpose: proofs are keyed by (repo, directory), which a workspace-set
	// change does not invalidate.
	deadCache := newDeadSubtreeCache(ctx)

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
					// A dropped/coalesced stream is a correctness signal: schedule every
					// repo once. Per-repo debounce still bounds the resulting burst.
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
				if !relevantGitEvent(event.Path) {
					continue
				}
				// Resolve once: routing, invalidation and suppression must all
				// see the same canonical path as the route roots they compare to.
				path := resolveEventPath(event.Path)
				route, nodes := routeGitEvent(path, routes)
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
