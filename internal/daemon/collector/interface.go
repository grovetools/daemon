// Package collector provides background workers that fetch and update daemon state.
package collector

import (
	"context"

	"github.com/grovetools/daemon/internal/daemon/store"
)

// Collector is a background worker that fetches data and emits updates.
type Collector interface {
	// Name returns the collector's name for logging.
	Name() string

	// Run starts the collector. It should block until context is canceled.
	// It emits updates via the updates channel.
	// It can read from the store (thread-safe) to get context (e.g. list of workspaces).
	Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error
}

// Refreshable is an optional interface that collectors can implement to support
// synchronous, on-demand refresh triggers (e.g. from the /api/refresh endpoint).
type Refreshable interface {
	Refresh(ctx context.Context) error
}
