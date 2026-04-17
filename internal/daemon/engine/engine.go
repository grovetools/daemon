// Package engine orchestrates background collectors for the daemon.
package engine

import (
	"context"
	"sync"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/daemon/internal/daemon/collector"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// Engine manages and runs all collectors.
type Engine struct {
	store      *store.Store
	collectors []collector.Collector
	ulog       *logging.UnifiedLogger
}

// New creates a new Engine instance.
func New(st *store.Store) *Engine {
	return &Engine{
		store: st,
		ulog:  logging.NewUnifiedLogger("groved.engine"),
	}
}

// Register adds a collector to the engine.
func (e *Engine) Register(c collector.Collector) {
	e.collectors = append(e.collectors, c)
}

// Start runs all collectors and blocks until context is canceled.
func (e *Engine) Start(ctx context.Context) {
	updates := make(chan store.Update, 100)
	var wg sync.WaitGroup

	// 1. Start Update Consumer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case u := <-updates:
				e.store.ApplyUpdate(u)
			}
		}
	}()

	// 2. Start Collectors
	for _, c := range e.collectors {
		wg.Add(1)
		go func(col collector.Collector) {
			defer wg.Done()
			e.ulog.Info("Starting collector").Field("collector", col.Name()).Log(ctx)
			if err := col.Run(ctx, e.store, updates); err != nil {
				e.ulog.Error("Collector failed").
					Err(err).
					Field("collector", col.Name()).
					Log(ctx)
			}
		}(c)
	}

	wg.Wait()
}

// Refresh triggers an immediate, out-of-band collection cycle for all refreshable collectors.
func (e *Engine) Refresh(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range e.collectors {
		if r, ok := c.(collector.Refreshable); ok {
			wg.Add(1)
			go func(refreshable collector.Refreshable) {
				defer wg.Done()
				_ = refreshable.Refresh(ctx)
			}(r)
		}
	}
	wg.Wait()
}

// Store returns the engine's state store.
func (e *Engine) Store() *store.Store {
	return e.store
}
