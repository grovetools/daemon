package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// fakeBackend models the kqueue semantics that produced the observed watcher
// blindness, deterministically and without touching the filesystem:
//
//   - a user watch is only listed once Add has fully succeeded (kqueue's
//     addUserWatch runs after addWatch returns, so a failed Add leaves nothing
//     in WatchList);
//   - a failed Add can still leave the directory registered internally
//     ("partial"), and a retry against that partial state reports SUCCESS while
//     skipping the per-file watch installation — the directory-only state where
//     rename-style saves are seen and in-place writes never are. The fake marks
//     any such watch `degraded`;
//   - Remove tears the partial state down, so a retry after cleanup is clean.
type fakeBackend struct {
	mu       sync.Mutex
	watched  map[string]bool // backend truth == WatchList
	partial  map[string]bool // registered internally by a failed Add
	degraded map[string]bool // watched, but missing its per-file half
	failAdd  map[string]int  // remaining Add failures per path
	silent   map[string]bool // Add reports success without registering
	adds     []string
	removes  []string
	events   chan fsnotify.Event
	errs     chan error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		watched:  make(map[string]bool),
		partial:  make(map[string]bool),
		degraded: make(map[string]bool),
		failAdd:  make(map[string]int),
		silent:   make(map[string]bool),
		events:   make(chan fsnotify.Event),
		errs:     make(chan error),
	}
}

func (b *fakeBackend) Add(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adds = append(b.adds, path)

	if b.silent[path] {
		return nil // "success" that registers nothing
	}
	if b.failAdd[path] > 0 {
		b.failAdd[path]--
		b.partial[path] = true // directory half installed, then the failure
		return errors.New("simulated partial add failure")
	}
	if b.partial[path] {
		// The poisoned retry: already-watching short-circuit reports success
		// while the per-file watches stay missing.
		b.partial[path] = false
		b.watched[path] = true
		b.degraded[path] = true
		return nil
	}
	b.watched[path] = true
	return nil
}

func (b *fakeBackend) Remove(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removes = append(b.removes, path)
	if !b.watched[path] && !b.partial[path] {
		return errors.New("non-existent watch")
	}
	delete(b.watched, path)
	delete(b.partial, path)
	delete(b.degraded, path)
	return nil
}

func (b *fakeBackend) WatchList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := make([]string, 0, len(b.watched))
	for p := range b.watched {
		list = append(list, p)
	}
	return list
}

func (b *fakeBackend) Events() <-chan fsnotify.Event { return b.events }
func (b *fakeBackend) Errors() <-chan error          { return b.errs }
func (b *fakeBackend) Close() error                  { return nil }

// drop simulates the backend losing a watch underneath the watcher (an FD
// reclaimed, a directory replaced) without telling anyone.
func (b *fakeBackend) drop(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.watched, path)
}

func (b *fakeBackend) isWatched(path string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.watched[path]
}

func (b *fakeBackend) isDegraded(path string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.degraded[path]
}

func (b *fakeBackend) callLog() (adds, removes []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.adds...), append([]string(nil), b.removes...)
}

// stubHandler is a DomainHandler whose watch set and matcher are set by the
// test. It records every event batch it is handed.
type stubHandler struct {
	mu       sync.Mutex
	paths    []string
	events   []fsnotify.Event
	onUpdate func(store.Update, *stubHandler)
}

func (h *stubHandler) Name() string { return "stub" }

func (h *stubHandler) ComputeWatchPaths([]*models.EnrichedWorkspace) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

func (h *stubHandler) setPaths(paths ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = paths
}

func (h *stubHandler) MatchesEvent(fsnotify.Event) bool { return true }

func (h *stubHandler) HandleEvents(_ context.Context, events []fsnotify.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, events...)
	return nil
}

func (h *stubHandler) HandleStoreUpdate(update store.Update) {
	if h.onUpdate != nil {
		h.onUpdate(update, h)
	}
}

func (h *stubHandler) OnStart(context.Context) {}

func (h *stubHandler) captured() []fsnotify.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fsnotify.Event(nil), h.events...)
}

// sawWrite reports whether a non-Chmod, non-Create event arrived for path —
// the signature of an in-place write reaching the handler.
func (h *stubHandler) sawOp(path string, op fsnotify.Op) bool {
	for _, e := range h.captured() {
		if filepath.Clean(e.Name) == filepath.Clean(path) && e.Op&op != 0 {
			return true
		}
	}
	return false
}

func newTestWatcher(t *testing.T, backend watchBackend, h DomainHandler) *UnifiedWatcher {
	t.Helper()
	uw := newUnifiedWatcherWithBackend(store.New(), 5*time.Millisecond, backend)
	uw.refreshInterval = time.Hour // no ticker rescue: refreshes must be deliberate
	uw.Register(h)
	return uw
}

// TestRefreshCleansUpPartialAddAndRetriesCleanly is the regression for the
// permanent directory-only watch state: a failed Add must be torn down, so the
// next refresh re-registers from scratch instead of hitting the backend's
// already-watching short-circuit and inheriting a half-installed watch.
func TestRefreshCleansUpPartialAddAndRetriesCleanly(t *testing.T) {
	dir := t.TempDir()
	backend := newFakeBackend()
	backend.failAdd[dir] = 1

	h := &stubHandler{}
	h.setPaths(dir)
	uw := newTestWatcher(t, backend, h)

	uw.refreshWatches()

	if stats := uw.WatchStats(); stats.Failed != 1 || stats.Added != 0 {
		t.Fatalf("after a failed Add: stats = %+v, want Failed=1 Added=0", stats)
	}
	uw.mu.Lock()
	_, recorded := uw.watchCounts[dir]
	uw.mu.Unlock()
	if recorded {
		t.Fatalf("watchCounts recorded %s as watched although Add failed", dir)
	}
	_, removes := backend.callLog()
	if len(removes) != 1 || removes[0] != dir {
		t.Fatalf("failed Add was not cleaned up: removes = %v", removes)
	}

	// Second refresh: the retry must produce a complete watch, not the
	// poisoned directory-only one.
	uw.refreshWatches()

	if !backend.isWatched(dir) {
		t.Fatalf("retry did not install a watch for %s", dir)
	}
	if backend.isDegraded(dir) {
		t.Fatalf("retry inherited the partial registration: %s is watched but missing its per-file half", dir)
	}
	if stats := uw.WatchStats(); stats.Added != 1 || stats.Failed != 0 || stats.Watched != 1 {
		t.Fatalf("after the clean retry: stats = %+v, want Added=1 Failed=0 Watched=1", stats)
	}
}

// TestPartialAddWithoutCleanupWouldDegrade pins the fake to the real kqueue
// behaviour it stands in for: retrying a partial Add *without* the intervening
// Remove is exactly what yields a degraded watch. Without this, the test above
// could pass against a fake that cannot express the bug.
func TestPartialAddWithoutCleanupWouldDegrade(t *testing.T) {
	backend := newFakeBackend()
	backend.failAdd["/some/dir"] = 1

	if err := backend.Add("/some/dir"); err == nil {
		t.Fatal("first Add should fail")
	}
	if err := backend.Add("/some/dir"); err != nil {
		t.Fatalf("retry against partial state should report success, got %v", err)
	}
	if !backend.isDegraded("/some/dir") {
		t.Fatal("retry without cleanup should have produced a degraded watch")
	}
}

// TestRefreshRecoversDroppedBackendWatch covers the state the old bookkeeping
// could not leave: watchCounts says healthy, the backend has nothing, and no
// amount of refreshing re-arms it.
func TestRefreshRecoversDroppedBackendWatch(t *testing.T) {
	dir := t.TempDir()
	backend := newFakeBackend()
	h := &stubHandler{}
	h.setPaths(dir)
	uw := newTestWatcher(t, backend, h)

	uw.refreshWatches()
	if !backend.isWatched(dir) {
		t.Fatalf("initial refresh did not watch %s", dir)
	}

	backend.drop(dir) // lost underneath us, silently

	uw.refreshWatches()

	if !backend.isWatched(dir) {
		t.Fatalf("refresh did not recover the dropped watch for %s", dir)
	}
	stats := uw.WatchStats()
	if stats.Recovered != 1 {
		t.Fatalf("stats = %+v, want Recovered=1", stats)
	}
	if stats.Watched != 1 {
		t.Fatalf("stats = %+v, want Watched=1", stats)
	}
}

// TestRefreshDoesNotTrustUnverifiedAdd: an Add that reports success without
// registering must not be recorded as watched — otherwise the watcher believes
// it has coverage it does not have, forever.
func TestRefreshDoesNotTrustUnverifiedAdd(t *testing.T) {
	dir := t.TempDir()
	backend := newFakeBackend()
	backend.silent[dir] = true
	h := &stubHandler{}
	h.setPaths(dir)
	uw := newTestWatcher(t, backend, h)

	uw.refreshWatches()

	uw.mu.Lock()
	_, recorded := uw.watchCounts[dir]
	uw.mu.Unlock()
	if recorded {
		t.Fatalf("unverified Add for %s was recorded as watched", dir)
	}
	if stats := uw.WatchStats(); stats.Failed != 1 || stats.Added != 0 || stats.Watched != 0 {
		t.Fatalf("stats = %+v, want Failed=1 Added=0 Watched=0", stats)
	}
}

// TestRefreshRemovesStaleBackendWatch keeps the backend list a mirror of the
// desired set: a registration nobody wants any more is torn down even when this
// watcher's own map never knew about it.
func TestRefreshRemovesStaleBackendWatch(t *testing.T) {
	dir := t.TempDir()
	stale := t.TempDir()
	backend := newFakeBackend()
	if err := backend.Add(stale); err != nil {
		t.Fatal(err)
	}

	h := &stubHandler{}
	h.setPaths(dir)
	uw := newTestWatcher(t, backend, h)

	uw.refreshWatches()

	if backend.isWatched(stale) {
		t.Fatalf("stale backend watch %s survived the refresh", stale)
	}
	if !backend.isWatched(dir) {
		t.Fatalf("desired path %s was not watched", dir)
	}
}

// TestConfigReloadRefreshesWatchesImmediately: the handler learns its new paths
// while processing the reload, so the refresh must run after that — and without
// waiting for the periodic ticker (pinned to an hour here).
func TestConfigReloadRefreshesWatchesImmediately(t *testing.T) {
	dir := t.TempDir()
	backend := newFakeBackend()

	h := &stubHandler{}
	h.onUpdate = func(update store.Update, self *stubHandler) {
		if update.Type == store.UpdateConfigReload {
			self.setPaths(dir) // config arrives with the reload, as SyncHandler's does
		}
	}

	st := store.New()
	uw := newUnifiedWatcherWithBackend(st, 5*time.Millisecond, backend)
	uw.refreshInterval = time.Hour
	uw.Register(h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uw.Start(ctx)

	// Wait for the startup refresh to settle with an empty watch set.
	waitUntil(t, time.Second, func() bool { return uw.WatchStats().Refreshes >= 1 })
	if backend.isWatched(dir) {
		t.Fatalf("%s was watched before the config reload", dir)
	}

	st.BroadcastConfigReload("sync.toml")

	waitUntil(t, 5*time.Second, func() bool { return backend.isWatched(dir) })
}

// TestRealFsnotifyCapturesInPlaceAppend is the ticket's exact scenario against
// the real backend: a note that exists *before* the watcher starts (the
// cold-start ordering), then modified in place with O_APPEND rather than
// replaced by a rename. The handler-level tests that inject fsnotify.Event
// values cannot see this failure, because the defect is registration, not
// dispatch.
func TestRealFsnotifyCapturesInPlaceAppend(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "note.md")
	if err := os.WriteFile(note, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := store.New()
	uw, err := NewUnifiedWatcher(st, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	uw.refreshInterval = time.Hour // the initial registration must be enough
	h := &stubHandler{}
	h.setPaths(dir)
	uw.Register(h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uw.Start(ctx)

	waitUntil(t, 5*time.Second, func() bool {
		stats := uw.WatchStats()
		return stats.Refreshes >= 1 && stats.Watched == 1
	})

	f, err := os.OpenFile(note, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("appended in place\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 10*time.Second, func() bool { return h.sawOp(note, fsnotify.Write) })
}

// TestRealFsnotifyCapturesRenameInSave is the discriminator that runs beside
// the append case: directory-level-only coverage passes this one, so it must
// not be the only real-backend test.
func TestRealFsnotifyCapturesRenameInSave(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "note.md")
	if err := os.WriteFile(note, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := store.New()
	uw, err := NewUnifiedWatcher(st, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	uw.refreshInterval = time.Hour
	h := &stubHandler{}
	h.setPaths(dir)
	uw.Register(h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uw.Start(ctx)

	waitUntil(t, 5*time.Second, func() bool {
		stats := uw.WatchStats()
		return stats.Refreshes >= 1 && stats.Watched == 1
	})

	tmp := filepath.Join(dir, ".note.md.tmp")
	if err := os.WriteFile(tmp, []byte("seed\nsaved by rename\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, note); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		return h.sawOp(note, fsnotify.Create|fsnotify.Write|fsnotify.Rename)
	})
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
