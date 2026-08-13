package watcher

// Regression tests for the adversarial review of the W3.2/W3.3/W3.5/W3.6
// lifecycle commit. Every test here reproduces a probe that FAILED against the
// reviewed tree; each names the finding it pins so a future refactor that
// reintroduces the hole says so out loud.
//
// The shipped lifecycle suite could not see any of these, and the reason is
// worth keeping: newLifecycleHarness sets duplicateScanInterval to a
// nanosecond, so every reconcile is a fresh decision and the rate-limited path
// where F5 lives never runs; and it only ever marks contested BEFORE the first
// ensurePipelines, which is the one ordering F4 handles correctly. Tests that
// need those conditions set them up explicitly below.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// F1: LoadSyncConfig returns (nil, nil) when sync.toml is absent — the
// documented "sync is disabled" state, and the default on every machine that
// has never run `grove join`. The reload handler entered its branch on
// err == nil and then dereferenced the nil config to count subscriptions,
// panicking the store dispatch goroutine. Nothing on UnifiedWatcher's dispatch
// path recovers, so that took groved down on an ordinary config reload.
func TestConfigReloadWithoutSyncTomlDoesNotPanic(t *testing.T) {
	lh := newLifecycleHarness(t)

	// The harness home has a machine.toml and no sync.toml, which is exactly
	// the pre-`grove join` state.
	if _, err := os.Stat(filepath.Join(config.MachineConfigPath())); err != nil {
		t.Fatalf("harness precondition: %v", err)
	}
	if cfg, err := config.LoadSyncConfig(); err != nil || cfg != nil {
		t.Fatalf("LoadSyncConfig() = %v, %v; want (nil, nil) with sync.toml absent", cfg, err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a config reload with sync.toml absent panicked: %v", r)
		}
	}()
	lh.h.HandleStoreUpdate(store.Update{Type: store.UpdateConfigReload})

	// The handler is dormant, not broken: the subscription list is empty and
	// the generation still advanced, so a later reload that brings sync.toml
	// reconciles normally.
	if subs := lh.h.subscriptionsSnapshot(); len(subs) != 0 {
		t.Fatalf("subscriptions = %+v, want none", subs)
	}
	if lh.h.configGeneration.Load() == 0 {
		t.Fatal("the reload did not advance the config generation")
	}
}

// F4 (W3.5): MarkContested is documented as "loses its pull pipeline, so
// nothing incoming is written into the contested tree", and its intended
// caller is a live daemon that is ALREADY pulling when it discovers a
// collision. isContested was consulted once, inside startPipeline, and the
// health test compared only the root — so a running pipeline stayed "healthy"
// forever and kept writing into the contested tree.
func TestMarkContestedTearsDownPullOnARunningPipeline(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})

	lh.h.ensurePipelines()
	state := lh.pipeline(idA)
	if state == nil || !state.pull {
		t.Fatalf("precondition: want a running pull pipeline, got %+v", state)
	}

	// The collision is discovered mid-flight, after the pull loop is up.
	lh.h.MarkContested(idA, "adoption pending: colliding local notes")
	waitForLifecycle(t, "the contested notespace to lose its pull pipeline", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && !state.pull
	})

	// Push survives: local work still reaches the server while adoption waits.
	if state := lh.pipeline(idA); state == nil || state.root != root {
		t.Fatalf("contested notespace lost its push pipeline too: %+v", state)
	}

	// ClearContested is adoption: the next pass restores the pull loop.
	lh.h.ClearContested(idA)
	waitForLifecycle(t, "adoption to restore the pull pipeline", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && state.pull
	})
}

// F4, second symptom (W3.3): flipping pull = true -> false in sync.toml is a
// recorded-config change the transport must honour without a daemon restart.
// pipelineState.pull was written and never compared, so a subscription
// downgraded to push-only kept writing into the tree.
func TestPullDowngradeIsReconciledWithoutARestart(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})

	lh.h.ensurePipelines()
	if state := lh.pipeline(idA); state == nil || !state.pull {
		t.Fatalf("precondition: want a running pull pipeline, got %+v", state)
	}

	// The operator downgrades the subscription to push-only.
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: false})
	waitForLifecycle(t, "the downgraded subscription to stop pulling", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && !state.pull
	})

	// And the upgrade direction reconciles too.
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	waitForLifecycle(t, "the re-upgraded subscription to resume pulling", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && state.pull
	})
}

// F5: duplicateSiblings is rate-limited, and it returned nil on a rate-limited
// pass. resolveIdentities rebuilds `parked` wholesale from candidates +
// siblings, so on the ~5 of every 6 transport ticks that skip the sweep every
// sibling-derived verdict silently vanished — and reappeared as a NEW episode
// on the next sweep. Two consequences, both pinned here.
func TestDuplicateParkingSurvivesARateLimitedPass(t *testing.T) {
	lh := newLifecycleHarness(t)
	first := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": first})
	lh.h.ensurePipelines()

	// `cp -R alpha alpha-copy`: a sibling root carrying the same stamp that
	// nothing is subscribed to, so only the sweep can find it.
	copyRoot := lh.notespace(t, "alpha-copy", idA)
	lh.h.ensurePipelines()
	if parked := lh.h.ParkedNotespaces(); len(parked) != 1 || parked[0].Root != copyRoot {
		t.Fatalf("precondition: want the copy parked, got %+v", parked)
	}

	// Production cadence: the sweep is a minute apart, the transport ticks
	// every ten seconds. Every pass from here is a rate-limited one.
	lh.h.duplicateScanInterval = time.Hour
	drain(lh.updates)

	for pass := 0; pass < 3; pass++ {
		lh.h.ensurePipelines()

		// (a) the verdict holds, so recordParked sees the same episode and
		// emits no new evidence.
		parked := lh.h.ParkedNotespaces()
		if len(parked) != 1 || parked[0].Root != copyRoot {
			t.Fatalf("pass %d: rate-limited pass dropped the parking verdict: %+v", pass, parked)
		}
		// (b) the safety exclusion holds. isParkedRoot is what keeps a parked
		// duplicate out of the escrow apply (NotespaceRoots) and the
		// maintenance drain (BeginMaintenance); while it flickered, both gates
		// accepted the copy for most of every minute.
		if !lh.h.isParkedRoot(copyRoot) {
			t.Fatalf("pass %d: the parked copy became an escrow/maintenance target again", pass)
		}
	}

	select {
	case update := <-lh.updates:
		t.Fatalf("a rate-limited pass re-broadcast a settled parking decision: %+v", update)
	case <-time.After(100 * time.Millisecond):
	}

	matches, err := filepath.Glob(filepath.Join(stateConflictsGlob(idA)))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("duplicate-stamp evidence files = %v, want exactly one episode", matches)
	}

	// The verdict is still a verdict, not a latch: once the sweep runs again
	// and the copy is gone, parking clears.
	if err := os.RemoveAll(copyRoot); err != nil {
		t.Fatal(err)
	}
	lh.h.duplicateScanInterval = time.Nanosecond
	lh.h.ensurePipelines()
	if parked := lh.h.ParkedNotespaces(); len(parked) != 0 {
		t.Fatalf("parking survived the repair: %+v", parked)
	}
}

// F8: firstSeenRoot short-circuited on len(roots) == 1 BEFORE consulting the
// durable binding, and built its candidate set only from the DESIRED
// subscriptions — so a sibling the sweep found could never win, even when
// sync.db says it is the root this machine has been syncing all along. The
// result inverted D8: the historically-synced root was parked and a
// newly-subscribed copy took over its identity.
func TestFirstSeenHonoursABindingToAnUnsubscribedSibling(t *testing.T) {
	lh := newLifecycleHarness(t)
	subscribed := lh.notespace(t, "alpha", idA)
	sibling := lh.notespace(t, "alpha-copy", idA)

	// sync.db says this machine has been syncing the sibling all along.
	if err := lh.db.UpsertNotespaceBinding(syncdb.NotespaceBinding{
		ID: idA, Name: "alpha-copy", Root: sibling,
		Subject: "local:01ARZ3NDEKTSV4RRFFQ69G5FAW", Kind: "notes",
	}); err != nil {
		t.Fatal(err)
	}

	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": subscribed})
	lh.h.ensurePipelines()

	// The sweep detects but never promotes, so a bound-but-unsubscribed keeper
	// means nobody syncs this id: the subscribed copy does not inherit it.
	if state := lh.pipeline(idA); state != nil {
		t.Fatalf("a newly-subscribed copy took over an id bound to %q: %+v", sibling, state)
	}
	parked := lh.h.ParkedNotespaces()
	if len(parked) != 1 || parked[0].Root != subscribed {
		t.Fatalf("parked = %+v, want the subscribed copy %q parked", parked, subscribed)
	}
	if parked[0].Keeper != sibling {
		t.Fatalf("keeper = %q, want the durably bound root %q", parked[0].Keeper, sibling)
	}
	if !strings.Contains(parked[0].Detail, sibling) || !strings.Contains(parked[0].Detail, subscribed) {
		t.Fatalf("evidence must name both roots: %q", parked[0].Detail)
	}
	lh.h.pathsMutex.RLock()
	defer lh.h.pathsMutex.RUnlock()
	if lh.h.watchedPaths[subscribed].notespace != "" {
		t.Fatal("a parked root became a capture identity")
	}
}

// F8, the other half: the durable binding must still be consulted when it
// names a root that IS in the desired set, and a lone subscription with no
// duplicate anywhere must keep working. Dropping the short-circuit must not
// change either answer.
func TestFirstSeenKeepsTheLoneSubscribedRoot(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": root})
	lh.h.ensurePipelines()

	if state := lh.pipeline(idA); state == nil || state.root != root {
		t.Fatalf("pipeline = %+v, want a transport at %q", state, root)
	}
	if parked := lh.h.ParkedNotespaces(); len(parked) != 0 {
		t.Fatalf("a lone subscribed root was parked: %+v", parked)
	}
}

// F6: RunAntiEntropyLoop deliberately tolerates a failing pass ("continue
// polling on error"); the INITIAL pass did not, and W3.2 gave it a brand-new
// way to fail for a condition that is transient by construction. Losing that
// race by one moment left the notespace running push and pull with no
// reconciliation for the life of the process.
//
// The loop's liveness is observable through the kick channel: KickPending is
// non-destructive, so a kick that is consumed proves the loop was entered.
// Against the reviewed tree the goroutine had already exited and the kick sat
// pending forever.
func TestAntiEntropyLoopSurvivesAFailedInitialPass(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": root})
	lh.h.ensurePipelines()

	// The fixture server answers only /sync/register, so the initial pass
	// fails on its handshake — the same shape as a MissingRootError refusal
	// and the same branch in startPipeline.
	lh.h.pipelinesMu.Lock()
	pass := lh.h.aePasses[idA]
	lh.h.pipelinesMu.Unlock()
	if pass == nil {
		t.Fatal("no anti-entropy pass registered for the running pipeline")
	}

	pass.Kick()
	waitForLifecycle(t, "the anti-entropy loop to consume a kick", func() bool {
		return !pass.KickPending()
	})
}

// F9: draining entries were only ever reclaimed by startPipeline, and only
// when the SAME id became desired again. A subscription removed for good
// leaked its pipelineState and its cancel closure permanently — and
// resetTransport moves every pipeline into draining at once, so an auth-reset
// cycle on a shrinking config accumulated them.
func TestDrainedPipelineStateIsReclaimed(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": root})
	lh.h.ensurePipelines()
	if lh.pipeline(idA) == nil {
		t.Fatal("precondition: no pipeline started")
	}

	// Removed for good: this id never becomes desired again.
	lh.subscribe()
	lh.watch(nil)
	lh.h.ensurePipelines()

	waitForLifecycle(t, "the stopped pipeline's goroutines to exit", func() bool {
		lh.h.pipelinesMu.Lock()
		defer lh.h.pipelinesMu.Unlock()
		return lh.h.draining[idA].stopped()
	})

	lh.h.ensurePipelines()
	lh.h.pipelinesMu.Lock()
	defer lh.h.pipelinesMu.Unlock()
	if len(lh.h.draining) != 0 {
		t.Fatalf("fully-drained pipeline state retained forever: %+v", lh.h.draining)
	}
}

// F7: cfg and locator were bare fields, written by the config reload on the
// store dispatch goroutine and read by the reconcile pass (routing,
// containment) on its own. The race was latent before this commit; adding
// `go h.ensurePipelines()` inside the reload handler made reload N's reconcile
// routinely still in flight when reload N+1 swapped the pointers.
//
// Run under -race; without the guard this reports a data race on h.cfg, on
// h.locator, and on the Notebooks sub-struct.
func TestConfigReloadDoesNotRaceReconcile(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": root})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pass := 0; pass < 25; pass++ {
				lh.h.ensurePipelines()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for reload := 0; reload < 25; reload++ {
				// Same swap the reload handler performs, via the same setter.
				lh.h.setConfig(notebookConfig(lh.notebookRoot))
				lh.h.bumpConfigGeneration()
				_ = lh.h.configSnapshot()
				_ = lh.h.notebookLocator()
			}
		}(i)
	}
	wg.Wait()
}

// stateConflictsGlob is the on-disk evidence pattern for a notespace's
// duplicate-stamp artifacts.
func stateConflictsGlob(id string) string {
	return filepath.Join(paths.StateDir(), "sync", "conflicts", id, "*"+syncdb.ConflictKindDuplicateStamp+"*")
}
