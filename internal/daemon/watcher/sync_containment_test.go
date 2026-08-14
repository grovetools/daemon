package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/workspace"
)

const (
	idBeta  = "01ARZ3NDEKTSV4RRFFQ69G5FA2"
	idGamma = "01ARZ3NDEKTSV4RRFFQ69G5FA3"
)

// Consent is the recorded bit and nothing else. A notebook that has never been
// shared inherits nothing to its notespaces even when one of them is
// explicitly subscribed — the stand-in that read "some subscription resolves
// into this notebook" as consent would have turned one subscribed notespace
// into a decision about every sibling beside it.
func TestContainmentNeedsARecordedShare(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.notespace(t, "alpha", idA)
	beta := lh.notespace(t, "beta", idBeta)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Mode: config.SyncModeFull})

	if sub := lh.h.effectiveSubscription("beta", beta); sub != nil {
		t.Fatalf("an unshared notebook inherited %+v", sub)
	}
	if found := lh.h.containedNotespaces(map[string]bool{"alpha": true}); found != nil {
		t.Fatalf("an unshared notebook enumerated %+v", found)
	}
	if lh.h.anySharedNotebook() {
		t.Fatal("anySharedNotebook is true with nothing recorded as shared")
	}
}

// `share = true` alone — no [[workspaces]] entry naming anything — is what
// puts a contained notespace in scope, on bidirectional peer terms.
func TestRecordedShareAloneInheritsBidirectionalTerms(t *testing.T) {
	lh := newLifecycleHarness(t)
	beta := lh.notespace(t, "beta", idBeta)
	lh.share(true)

	sub := lh.h.effectiveSubscription("beta", beta)
	if sub == nil {
		t.Fatal("a stamped notespace inside a shared notebook inherited nothing")
	}
	if sub.Name != "beta" {
		t.Fatalf("inherited name = %q, want the contained notespace's own name", sub.Name)
	}
	if !sub.Pull || sub.Role != config.SyncRolePeer || sub.Mode != config.SyncModeFull {
		t.Fatalf("inherited terms = %+v, want bidirectional peer terms", sub)
	}
}

// When the notebook DOES have an explicit subscription, its recorded terms are
// what the siblings inherit: containment is a statement about the notebook, so
// an operator who wrote down mode/excludes/size for it governs the whole thing.
func TestContainmentInheritsTheContainingNotebooksSubscription(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.notespace(t, "alpha", idA)
	beta := lh.notespace(t, "beta", idBeta)
	lh.share(true)
	lh.subscribe(config.SyncWorkspace{
		Name: "alpha", Role: config.SyncRolePeer, Mode: config.SyncModeFull,
		Pull: true, Excludes: []string{"scratch/**"}, MaxFileSize: 4096,
	})

	sub := lh.h.effectiveSubscription("beta", beta)
	if sub == nil {
		t.Fatal("a stamped notespace inside a shared notebook inherited nothing")
	}
	if !sub.Pull || sub.Mode != config.SyncModeFull || sub.MaxFileSize != 4096 ||
		len(sub.Excludes) != 1 || sub.Excludes[0] != "scratch/**" {
		t.Fatalf("inherited terms = %+v, want the notebook's terms", sub)
	}
	// The inherited copy must not alias the template's slice.
	sub.Excludes[0] = "mutated"
	if again := lh.h.effectiveSubscription("beta", beta); again.Excludes[0] != "scratch/**" {
		t.Fatal("the inherited subscription aliased the template's excludes")
	}

	// An explicit subscription always wins over inheritance — the union of the
	// two mechanisms, resolved in favor of the recorded one.
	lh.subscribe(
		config.SyncWorkspace{Name: "alpha", Pull: true},
		config.SyncWorkspace{Name: "beta", Mode: config.SyncModeSearchOnly},
	)
	if explicit := lh.h.effectiveSubscription("beta", beta); explicit == nil || explicit.Mode != config.SyncModeSearchOnly {
		t.Fatalf("explicit subscription = %+v, want the recorded one", explicit)
	}
}

func TestContainmentRequiresAStampInsideTheSharedNotebook(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.notespace(t, "alpha", idA)
	lh.share(true)

	// An unstamped directory under notespaces/ is not a notespace.
	bare := filepath.Join(lh.notebookRoot, workspace.NotespaceDirectory, "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	if sub := lh.h.containmentSubscription(bare); sub != nil {
		t.Fatalf("an unstamped directory inherited %+v", sub)
	}

	// A directory that is not <notebook>/notespaces/<name> inherits nothing.
	outside := lh.notespace(t, "gamma", idBeta)
	elsewhere := filepath.Join(t.TempDir(), "gamma")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(outside, ".notespace.toml"), filepath.Join(elsewhere, ".notespace.toml")); err != nil {
		t.Fatal(err)
	}
	if sub := lh.h.containmentSubscription(elsewhere); sub != nil {
		t.Fatalf("a notespace outside any notebook inherited %+v", sub)
	}
}

// The registry entry `grove join` writes is never a share template: it would
// hand an ordinary notespace the registry's own-note guard and pull posture.
// Since it is usually the ONLY subscription a machine has, this is also what
// makes the default terms the ordinary answer rather than a rare one.
func TestTheRegistrySubscriptionIsNeverAShareTemplate(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.notespace(t, "registry", idA)
	gamma := lh.notespace(t, "gamma", idGamma)
	lh.share(true)
	lh.subscribe(config.SyncWorkspace{
		Name: "registry", Role: config.SyncRoleRegistry, Pull: true, Excludes: []string{"registry-only/**"},
	})

	sub := lh.h.containmentSubscription(gamma)
	if sub == nil {
		t.Fatal("a shared notebook whose only subscription is the registry inherited nothing")
	}
	if sub.Role == config.SyncRoleRegistry || len(sub.Excludes) != 0 {
		t.Fatalf("the registry subscription was used as a share template: %+v", sub)
	}
}

// The end of the containment path: an enabled, contained notespace is watched
// and registered by the ordinary reconcile, with no sync.toml entry naming it.
func TestContainedNotespaceIsWatchedAndRegistered(t *testing.T) {
	lh := newLifecycleHarness(t)
	alpha := lh.notespace(t, "alpha", idA)
	beta := lh.notespace(t, "beta", idBeta)
	lh.share(true)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Mode: config.SyncModeFull})

	paths := lh.h.ComputeWatchPaths(nil)
	var sawAlpha, sawBeta bool
	for _, p := range paths {
		sawAlpha = sawAlpha || p == alpha
		sawBeta = sawBeta || p == beta
	}
	if !sawAlpha || !sawBeta {
		t.Fatalf("watch paths = %v, want both the subscribed and the contained notespace", paths)
	}

	lh.h.ensurePipelines()
	if lh.registrationCount(idBeta) != 1 {
		t.Fatalf("the contained notespace registered %d times, want 1", lh.registrationCount(idBeta))
	}
	if state := lh.pipeline(idBeta); state == nil || state.root != beta {
		t.Fatalf("contained pipeline = %+v, want root %q", state, beta)
	}
}

// The headline promise of W3.2, as the daemon has to keep it: a notespace
// created INSIDE an already-shared notebook, after the daemon is running and
// with nothing recorded about it anywhere, is registered and transported by
// the next reconcile. Nothing is edited between the two passes but the disk.
func TestANotespaceCreatedInsideASharedNotebookAutoRegisters(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.share(true)
	lh.subscribe()

	if paths := lh.h.ComputeWatchPaths(nil); len(paths) != 0 {
		t.Fatalf("watch paths = %v before anything exists, want none", paths)
	}
	lh.h.ensurePipelines()
	if state := lh.pipeline(idGamma); state != nil {
		t.Fatalf("a pipeline ran for a notespace that does not exist yet: %+v", state)
	}

	// The operator makes a notespace. No verb, no config edit.
	gamma := lh.notespace(t, "gamma", idGamma)

	if paths := lh.h.ComputeWatchPaths(nil); !contains(paths, gamma) {
		t.Fatalf("watch paths = %v after the notespace was created, want %q among them", paths, gamma)
	}
	lh.h.ensurePipelines()
	if lh.registrationCount(idGamma) != 1 {
		t.Fatalf("the new notespace registered %d times, want 1", lh.registrationCount(idGamma))
	}
	state := lh.pipeline(idGamma)
	if state == nil || state.root != gamma {
		t.Fatalf("auto-registered pipeline = %+v, want root %q", state, gamma)
	}
	if !state.pull {
		t.Fatal("the auto-registered pipeline runs push-only; `share = true` is recorded by pull as well as by share")
	}

	// And unsharing it takes the transport away again, without a restart.
	lh.share(false)
	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()
	waitForLifecycle(t, "the unshared notespace's pipeline to stop", func() bool {
		return lh.pipeline(idGamma) == nil
	})
}

// A recorded share alone must wake the dormant handler. `grove notebook
// share` writes notebooks.toml and nothing into sync.toml, so a machine whose
// only sync intent is containment has zero [[workspaces]] entries — and a
// dormancy gate counting only those left exactly that machine asleep:
// registered on the server, share printed, pipelines never spawned, sync.db
// never opened (found live on the first real share, 2026-08-14). An absent
// sync.toml must stay dormant regardless: no server relationship means there
// is nothing to wake for.
func TestARecordedShareAloneWakesTheDormantHandler(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.notespace(t, "beta", idBeta)

	// Server recorded, nothing subscribed, nothing shared: dormant.
	lh.subscribe()
	if lh.h.hasSubscriptions() {
		t.Fatal("handler awake with no subscriptions and nothing shared")
	}

	// The share is the wake condition.
	lh.share(true)
	if !lh.h.hasSubscriptions() {
		t.Fatal("a recorded share alone did not wake the dormant handler")
	}

	// Withdrawing the share puts it back to sleep.
	lh.share(false)
	if lh.h.hasSubscriptions() {
		t.Fatal("handler stayed awake after the share was withdrawn")
	}

	// No sync.toml at all: dormant even with a share recorded.
	lh.share(true)
	lh.h.syncCfgMu.Lock()
	lh.h.syncCfg = nil
	lh.h.syncCfgMu.Unlock()
	if lh.h.hasSubscriptions() {
		t.Fatal("handler awake with no sync.toml; a share cannot sync without a server relationship")
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
