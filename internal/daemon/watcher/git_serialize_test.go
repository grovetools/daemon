package watcher

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/gitlimits"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// fakeScanner stands in for scanAndEmit so scan serialization can be asserted
// against counts rather than log lines or wall-clock guesses. Every scan blocks
// on release until the test lets it finish, which is what makes "is a second
// scan running right now?" a decidable question.
type fakeScanner struct {
	mu       sync.Mutex
	started  int
	running  int
	peak     int
	entered  chan string   // one send per scan entry
	release  chan struct{} // closed or fed to let scans return
	onScan   func(*workspace.WorkspaceNode)
	onScanAt int // invoke onScan on this 1-based scan number (0 = never)
}

func newFakeScanner(buffer int) *fakeScanner {
	return &fakeScanner{
		entered: make(chan string, buffer),
		release: make(chan struct{}, buffer),
	}
}

func (f *fakeScanner) scan(node *workspace.WorkspaceNode, _ *scanState, _ uint64) {
	f.mu.Lock()
	f.started++
	n := f.started
	f.running++
	if f.running > f.peak {
		f.peak = f.running
	}
	hook := f.onScan
	fire := f.onScanAt == n
	f.mu.Unlock()

	f.entered <- node.Path
	if fire && hook != nil {
		hook(node)
	}
	<-f.release

	f.mu.Lock()
	f.running--
	f.mu.Unlock()
}

// counts returns (started, peak concurrent).
func (f *fakeScanner) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.peak
}

// waitEntered blocks until a scan enters, failing the test on timeout.
func (f *fakeScanner) waitEntered(t *testing.T, what string) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// expectNoEntry asserts no scan enters within d.
func (f *fakeScanner) expectNoEntry(t *testing.T, d time.Duration, what string) {
	t.Helper()
	select {
	case p := <-f.entered:
		t.Fatalf("unexpected scan of %s: %s", p, what)
	case <-time.After(d):
	}
}

func testNode(path string) *workspace.WorkspaceNode {
	return &workspace.WorkspaceNode{Name: path, Path: path, Kind: workspace.KindStandaloneProject}
}

// fakeHandler builds a GitHandler whose scans are the fake's, with no store
// dependency: these tests are about scheduling, not about what a scan computes.
func fakeHandler(t *testing.T, debounceMs int, f *fakeScanner) *GitHandler {
	t.Helper()
	h := NewGitHandler(store.New(), debounceMs)
	h.scanFn = f.scan
	return h
}

// TestGitHandlerScanCoalescesWhileInFlight is the core of the serialization
// fix: while a scan for one workspace is running, further scheduleScan calls
// must NOT start a second scan — they fold into exactly one trailing rerun.
// Before this, Timer.Stop's "already fired" return was discarded and every
// event during a scan armed another concurrent scan of the same repository.
func TestGitHandlerScanCoalescesWhileInFlight(t *testing.T) {
	f := newFakeScanner(64)
	h := fakeHandler(t, 5, f)
	node := testNode("/repo/one")

	before := telemetry.GitWatcherCoalesced.Value()

	// Get one scan in flight and hold it there.
	h.scheduleScan(node)
	f.waitEntered(t, "the first scan")

	// 50 concurrent requests land while it runs. All must coalesce.
	const requests = 50
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.scheduleScan(node)
		}()
	}
	wg.Wait()

	f.expectNoEntry(t, 100*time.Millisecond, "a second concurrent scan was started")
	if started, peak := f.counts(); started != 1 || peak != 1 {
		t.Fatalf("during one in-flight scan: started=%d peak=%d, want 1/1", started, peak)
	}

	// Releasing the in-flight scan must buy exactly ONE catch-up scan — not 50,
	// and not zero (the events it coalesced may describe state it never saw).
	f.release <- struct{}{}
	f.waitEntered(t, "the trailing rerun")
	f.expectNoEntry(t, 100*time.Millisecond, "the rerun fanned out into more than one scan")

	f.release <- struct{}{}
	// Let the loop observe the cleared rerun bit and exit.
	waitFor(t, "the scan loop to go idle", func() bool {
		h.scansMutex.Lock()
		defer h.scansMutex.Unlock()
		st := h.scans[node.Path]
		return st != nil && !st.inFlight && !st.rerun
	})

	if started, peak := f.counts(); started != 2 || peak != 1 {
		t.Fatalf("after coalescing %d requests: started=%d peak=%d, want 2/1", requests, started, peak)
	}
	if got := telemetry.GitWatcherCoalesced.Value() - before; got != requests {
		t.Fatalf("coalesced counter moved by %d, want %d", got, requests)
	}
}

// TestGitHandlerRerunSurvivesLastMomentRequest guards the window the whole fix
// turns on: a request that arrives in the final moments of a scan, after the
// scan has done its git work but before it has given up the in-flight slot,
// must still produce a rerun. Dropping it is permanent staleness — the
// collector reconciler is hourly and scoped daemons never sweep at all.
func TestGitHandlerRerunSurvivesLastMomentRequest(t *testing.T) {
	f := newFakeScanner(8)
	h := fakeHandler(t, 5, f)
	node := testNode("/repo/lastmoment")

	// The first scan issues a request from inside its own body, i.e. at the
	// latest possible instant while it still holds the in-flight slot.
	f.onScanAt = 1
	f.onScan = func(n *workspace.WorkspaceNode) { h.scheduleScan(n) }

	h.scheduleScan(node)
	f.waitEntered(t, "the first scan")
	f.release <- struct{}{}

	f.waitEntered(t, "the rerun for the last-moment request")
	f.release <- struct{}{}

	f.expectNoEntry(t, 100*time.Millisecond, "the rerun should not itself re-trigger")
	if started, peak := f.counts(); started != 2 || peak != 1 {
		t.Fatalf("last-moment request: started=%d peak=%d, want 2/1", started, peak)
	}
}

// TestGitHandlerGlobalSemaphoreCapsConcurrency proves the fleet-wide bound: a
// burst of events across many repositories may not fan out to one git process
// per repository. The watcher pool mirrors the collector's, both sized from
// gitlimits.Workers.
func TestGitHandlerGlobalSemaphoreCapsConcurrency(t *testing.T) {
	workers := gitlimits.Workers
	total := workers * 4

	f := newFakeScanner(total)
	h := fakeHandler(t, 1, f)

	nodes := make([]*workspace.WorkspaceNode, total)
	for i := range nodes {
		nodes[i] = testNode("/repo/" + strconv.Itoa(i))
		h.beginScan(nodes[i])
	}

	// Exactly `workers` scans may be running; the rest queue on the semaphore.
	for range workers {
		f.waitEntered(t, "a scan to reach the semaphore")
	}
	f.expectNoEntry(t, 150*time.Millisecond, "concurrency exceeded the global watcher-scan bound")
	if started, peak := f.counts(); started != workers || peak != workers {
		t.Fatalf("under the semaphore: started=%d peak=%d, want %d/%d", started, peak, workers, workers)
	}

	// Every queued scan must still run — the bound defers, it never drops.
	for range total {
		f.release <- struct{}{}
	}
	waitFor(t, "all queued scans to run", func() bool {
		started, _ := f.counts()
		return started == total
	})
	if _, peak := f.counts(); peak > workers {
		t.Fatalf("peak concurrency %d exceeded the %d-worker bound", peak, workers)
	}
}

// TestGitHandlerStoreUpdatePrunesScanState covers the pre-existing leak: the
// per-path map was never deleted from, so it grew with every workspace ever
// seen. HandleStoreUpdate already rebuilds knownPaths from the live set; the
// scan state must shrink with it.
// TestGitHandlerFiredTimerCannotResurrectEvictedState deterministically parks
// a timer callback after it fires but before it claims scansMutex. Eviction
// must make that exact callback stale; it may neither scan nor recreate state.
func TestGitHandlerFiredTimerCannotResurrectEvictedState(t *testing.T) {
	f := newFakeScanner(1)
	h := fakeHandler(t, 1, f)
	node := testNode("/repo/fired-timer")

	callbackFired := make(chan struct{})
	releaseCallback := make(chan struct{})
	h.beforeTimerScan = func() {
		close(callbackFired)
		<-releaseCallback
	}
	h.scheduleScan(node)
	select {
	case <-callbackFired:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for timer callback to fire")
	}

	h.HandleStoreUpdate(store.Update{
		Type: store.UpdateWorkspaces, Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{},
	})
	close(releaseCallback)

	f.expectNoEntry(t, 100*time.Millisecond, "an evicted fired timer was allowed to scan")
	waitFor(t, "evicted fired-timer state to remain absent", func() bool {
		h.scansMutex.Lock()
		defer h.scansMutex.Unlock()
		_, ok := h.scans[node.Path]
		return !ok
	})
}

// TestGitHandlerRemoveReaddRetainsAuthorityAndRejectsStalePublish covers the
// eviction ABA: an in-flight old generation remains the sole scan authority,
// cannot publish after re-add, and hands off to exactly one current catch-up.
func TestGitHandlerRemoveReaddRetainsAuthorityAndRejectsStalePublish(t *testing.T) {
	h := NewGitHandler(store.New(), 1).SetBroadCoverage(true)
	oldNode := testNode("/repo/aba")
	newNode := testNode("/repo/aba")

	entered := make(chan uint64, 2)
	release := make(chan struct{}, 2)
	published := make(chan uint64, 2)
	rejected := make(chan uint64, 2)
	h.scanFn = func(node *workspace.WorkspaceNode, st *scanState, generation uint64) {
		entered <- generation
		<-release
		if h.publishIfCurrent(node.Path, st, generation, func() { published <- generation }) {
			return
		}
		rejected <- generation
	}

	// Make the path known, then start and hold generation zero.
	h.HandleStoreUpdate(store.Update{
		Type: store.UpdateWorkspaces, Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{oldNode.Path: {WorkspaceNode: oldNode}},
	})
	h.beginScan(oldNode)
	oldGeneration := <-entered

	// Remove and immediately re-add while the old scan is still running.
	h.HandleStoreUpdate(store.Update{
		Type: store.UpdateWorkspaces, Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{},
	})
	h.HandleStoreUpdate(store.Update{
		Type: store.UpdateWorkspaces, Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{newNode.Path: {WorkspaceNode: newNode}},
	})

	select {
	case generation := <-entered:
		t.Fatalf("generation %d started concurrently with old generation %d", generation, oldGeneration)
	case <-time.After(100 * time.Millisecond):
	}

	// The old generation loses its publication race; only after it returns may
	// the current generation's one catch-up enter and publish.
	release <- struct{}{}
	select {
	case got := <-rejected:
		if got != oldGeneration {
			t.Fatalf("rejected generation = %d, want old generation %d", got, oldGeneration)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for stale generation rejection")
	}
	var currentGeneration uint64
	select {
	case currentGeneration = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for current-generation catch-up")
	}
	if currentGeneration == oldGeneration {
		t.Fatalf("catch-up reused stale generation %d", currentGeneration)
	}
	release <- struct{}{}
	select {
	case got := <-published:
		if got != currentGeneration {
			t.Fatalf("published generation = %d, want current %d", got, currentGeneration)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for current-generation publication")
	}
}

func TestGitHandlerStoreUpdatePrunesScanState(t *testing.T) {
	f := newFakeScanner(8)
	// A long debounce keeps the scheduled scans pending so the test is asserting
	// on state teardown, not racing timers.
	h := fakeHandler(t, 60_000, f)

	gone1, gone2, stays := testNode("/repo/gone1"), testNode("/repo/gone2"), testNode("/repo/stays")
	for _, n := range []*workspace.WorkspaceNode{gone1, gone2, stays} {
		h.scheduleScan(n)
	}
	h.scansMutex.Lock()
	n := len(h.scans)
	h.scansMutex.Unlock()
	if n != 3 {
		t.Fatalf("expected 3 tracked paths, got %d", n)
	}

	// The live workspace set now holds only one of them.
	h.HandleStoreUpdate(store.Update{
		Type:    store.UpdateWorkspaces,
		Source:  "test",
		Payload: map[string]*models.EnrichedWorkspace{stays.Path: {WorkspaceNode: stays}},
	})

	h.scansMutex.Lock()
	remaining := make([]string, 0, len(h.scans))
	for p := range h.scans {
		remaining = append(remaining, p)
	}
	h.scansMutex.Unlock()
	if len(remaining) != 1 || remaining[0] != stays.Path {
		t.Fatalf("scan state after prune = %v, want only %q", remaining, stays.Path)
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
