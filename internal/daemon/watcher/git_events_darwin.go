//go:build darwin

package watcher

import (
	"context"
	"time"

	"github.com/fsnotify/fsevents"
	"github.com/grovetools/daemon/internal/daemon/store"
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
			for _, event := range batch {
				if event.Flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped) != 0 {
					// A dropped/coalesced stream is a correctness signal: schedule every
					// repo once. Per-repo debounce still bounds the resulting burst.
					for _, route := range routes {
						for _, node := range route.nodes {
							handler.scheduleScan(node)
						}
					}
					continue
				}
				if !relevantGitEvent(event.Path) {
					continue
				}
				for _, node := range routeGitEvent(event.Path, routes) {
					handler.scheduleScan(node)
				}
			}
		}
	}
}
