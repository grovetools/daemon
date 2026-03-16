// Package engine orchestrates background collectors for the daemon.
package engine

import (
	"context"
	"sync"

	"github.com/grovetools/daemon/internal/daemon/collector"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// Engine manages and runs all collectors.
type Engine struct {
	store      *store.Store
	collectors []collector.Collector
	logger     *logrus.Entry
}

// New creates a new Engine instance.
func New(st *store.Store, logger *logrus.Entry) *Engine {
	return &Engine{
		store:  st,
		logger: logger,
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
			e.logger.WithField("collector", col.Name()).Info("Starting collector")
			if err := col.Run(ctx, e.store, updates); err != nil {
				e.logger.WithField("collector", col.Name()).WithError(err).Error("Collector failed")
			}
		}(c)
	}

	wg.Wait()
}

// Store returns the engine's state store.
func (e *Engine) Store() *store.Store {
	return e.store
}
