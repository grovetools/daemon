package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/workspace"
)

const idBeta = "01ARZ3NDEKTSV4RRFFQ69G5FA2"

// Containment ships dark: on a P2 machine a stamped sibling in a notebook that
// has a subscribed notespace inherits nothing, because the recorded input the
// rule needs (`[notebooks.<name>.sync] share = true`) does not exist yet.
func TestContainmentIsDarkByDefault(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.notespace(t, "alpha", idA)
	beta := lh.notespace(t, "beta", idBeta)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Mode: config.SyncModeFull})

	if sub := lh.h.effectiveSubscription("beta", beta); sub != nil {
		t.Fatalf("containment inherited %+v while dark", sub)
	}
	if found := lh.h.containedNotespaces(map[string]bool{"alpha": true}); found != nil {
		t.Fatalf("containment enumerated %+v while dark", found)
	}
}

func TestContainmentInheritsTheContainingNotebooksSubscription(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.notespace(t, "alpha", idA)
	beta := lh.notespace(t, "beta", idBeta)
	lh.h.ContainmentAutoRegister = true
	lh.subscribe(config.SyncWorkspace{
		Name: "alpha", Role: config.SyncRolePeer, Mode: config.SyncModeFull,
		Pull: true, Excludes: []string{"scratch/**"}, MaxFileSize: 4096,
	})

	sub := lh.h.effectiveSubscription("beta", beta)
	if sub == nil {
		t.Fatal("a stamped notespace inside a shared notebook inherited nothing")
	}
	if sub.Name != "beta" {
		t.Fatalf("inherited name = %q, want the contained notespace's own name", sub.Name)
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

	// An explicit subscription always wins over inheritance.
	lh.subscribe(
		config.SyncWorkspace{Name: "alpha", Pull: true},
		config.SyncWorkspace{Name: "beta", Mode: config.SyncModeSearchOnly},
	)
	if explicit := lh.h.effectiveSubscription("beta", beta); explicit == nil || explicit.Mode != config.SyncModeSearchOnly {
		t.Fatalf("explicit subscription = %+v, want the recorded one", explicit)
	}
}

func TestContainmentRequiresAStampAndANonRegistryTemplate(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.notespace(t, "alpha", idA)
	lh.h.ContainmentAutoRegister = true

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

	// The registry notespace is never a template for its notebook.
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRoleRegistry, Pull: true})
	if sub := lh.h.containmentSubscription(filepath.Join(lh.notebookRoot, workspace.NotespaceDirectory, "gamma")); sub != nil {
		t.Fatalf("the registry subscription was used as a share template: %+v", sub)
	}
}

// The end of the containment path: an enabled, contained notespace is watched
// and registered by the ordinary reconcile, with no sync.toml entry naming it.
func TestContainedNotespaceIsWatchedAndRegistered(t *testing.T) {
	lh := newLifecycleHarness(t)
	alpha := lh.notespace(t, "alpha", idA)
	beta := lh.notespace(t, "beta", idBeta)
	lh.h.ContainmentAutoRegister = true
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
