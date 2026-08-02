package watcher

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// TestDeferredDBBootGate is the boot-gate fix made executable (contract §1 Q7).
//
// The daemon used to decide at boot whether to build a SyncHandler at all, so a
// first-ever `grove join` wrote a valid sync.toml that nothing acted on until
// the daemon was restarted. The handler is now constructed unconditionally and
// stays dormant — no sync.db, no transport — until a config reload brings
// subscriptions, at which point ensureDB opens the database in place and
// publishes it to the HTTP server.
//
// The assertions are the two halves of that contract: nothing happens while
// dormant, and exactly one open happens on the transition.
func TestDeferredDBBootGate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync.db")

	opens := 0
	var opened *syncdb.DB
	var published *syncdb.DB

	// Dormant construction: no config, no database — exactly what the global
	// daemon now does before it knows whether sync is configured.
	h := NewSyncHandler(nil, nil, nil, nil, 50, 500)
	h.SetDeferredDB(
		func() (*syncdb.DB, error) {
			opens++
			db, err := syncdb.Open(dbPath)
			if err != nil {
				return nil, err
			}
			opened = db
			t.Cleanup(func() { _ = db.Close() })
			return db, nil
		},
		func(db *syncdb.DB) { published = db },
	)

	if got := h.database(); got != nil {
		t.Fatalf("dormant handler has a database: %v", got)
	}
	if got := h.ensureDB(); got != nil {
		t.Fatalf("ensureDB opened a database with no subscriptions: %v", got)
	}
	if opens != 0 {
		t.Fatalf("opener called %d times while dormant, want 0", opens)
	}
	if paths := h.ComputeWatchPaths(nil); len(paths) != 0 {
		t.Fatalf("dormant handler produced watches: %v", paths)
	}
	// Capture paths must be inert, not panicking, while dormant.
	h.flush(context.Background(), filepath.Join(t.TempDir(), "note.md"))

	// The config reload a `grove join` triggers: subscriptions appear.
	h.syncCfgMu.Lock()
	h.syncCfg = &config.SyncConfig{
		Server:     "http://localhost:8788",
		Workspaces: []config.SyncWorkspace{{Name: "grovetools"}},
	}
	h.syncCfgMu.Unlock()

	db := h.ensureDB()
	if db == nil {
		t.Fatal("ensureDB returned nil after subscriptions appeared")
	}
	if opens != 1 {
		t.Fatalf("opener called %d times, want 1", opens)
	}
	if db != opened {
		t.Fatalf("ensureDB returned %v, want the opened database %v", db, opened)
	}
	if published != db {
		t.Fatalf("ready callback got %v, want %v — the HTTP server would still report sync as unconfigured", published, db)
	}
	if h.database() != db {
		t.Fatalf("database() = %v after open, want %v", h.database(), db)
	}

	// Idempotent: later calls reuse the open database rather than reopening.
	if again := h.ensureDB(); again != db || opens != 1 {
		t.Fatalf("second ensureDB reopened: got %v (opens=%d)", again, opens)
	}
}

// TestDeferredDBOpenFailureStaysDormant: an open failure must leave the handler
// dormant and retryable rather than latching a broken state or panicking a
// caller that assumed a database.
func TestDeferredDBOpenFailureStaysDormant(t *testing.T) {
	attempts := 0
	h := NewSyncHandler(nil, nil, &config.SyncConfig{
		Workspaces: []config.SyncWorkspace{{Name: "grovetools"}},
	}, nil, 50, 500)

	dbPath := filepath.Join(t.TempDir(), "sync.db")
	h.SetDeferredDB(
		func() (*syncdb.DB, error) {
			attempts++
			if attempts < 3 {
				return nil, fmt.Errorf("simulated open failure %d", attempts)
			}
			db, err := syncdb.Open(dbPath)
			if err != nil {
				return nil, err
			}
			t.Cleanup(func() { _ = db.Close() })
			return db, nil
		},
		nil, // no ready hook: SetDeferredDB must tolerate it
	)

	for i := 1; i <= 2; i++ {
		if db := h.ensureDB(); db != nil {
			t.Fatalf("attempt %d: ensureDB returned a database despite the open failing", i)
		}
		if h.database() != nil {
			t.Fatalf("attempt %d: a failed open latched a database", i)
		}
	}
	if db := h.ensureDB(); db == nil {
		t.Fatal("ensureDB never retried after transient failures")
	}
	if attempts != 3 {
		t.Fatalf("open attempts = %d, want 3 (retry on every call until it succeeds)", attempts)
	}
}
