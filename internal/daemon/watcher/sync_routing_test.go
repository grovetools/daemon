package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/workspace"
)

// routingFixture is the shape the 2026-08-15 profile caught groved in: a
// machine that shares a notebook (so containment is live and every discovered
// workspace asks where it would live), a notebook holding a realistic number of
// stamped notespaces, and a discovery set two orders of magnitude larger than
// the subscription list.
type routingFixture struct {
	h        *SyncHandler
	notebook string
	enriched []*models.EnrichedWorkspace
}

func newRoutingFixture(tb testing.TB, notespaces, workspaces int) *routingFixture {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("GROVE_HOME", home)
	tb.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	tb.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	if err := os.MkdirAll(paths.ConfigDir(), 0o700); err != nil {
		tb.Fatal(err)
	}

	notebookRoot := filepath.Join(home, "notebooks", "nb")
	if err := os.MkdirAll(filepath.Join(notebookRoot, workspace.NotespaceDirectory), 0o755); err != nil {
		tb.Fatal(err)
	}
	machineTOML := "[primaries]\n"
	for i := range notespaces {
		name := fmt.Sprintf("notespace-%03d", i)
		id := fmt.Sprintf("01KZVCMCZ19M95YTJN3HC%05d", i)
		subject := fmt.Sprintf("local:01KZVCMCZ1GX41XHXZXH6%05d", i)
		root := filepath.Join(notebookRoot, workspace.NotespaceDirectory, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			tb.Fatal(err)
		}
		if _, err := notespacepkg.InstallNotespace(root, notespacepkg.NotespaceStamp{
			ID: id, Name: name, Subject: subject, Kind: "notes",
		}); err != nil {
			tb.Fatal(err)
		}
		machineTOML += fmt.Sprintf("%q = %q\n", subject, id)
	}
	if err := os.WriteFile(config.MachineConfigPath(), []byte(machineTOML), 0o600); err != nil {
		tb.Fatal(err)
	}

	cfg := &config.Config{
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{"nb": {RootDir: notebookRoot, Shared: true}},
			Rules:       &config.NotebookRules{Default: "nb"},
		},
	}
	fixture := &routingFixture{
		h:        NewSyncHandler(nil, cfg, &config.SyncConfig{}, nil, 50, 500),
		notebook: notebookRoot,
	}
	for i := range workspaces {
		fixture.enriched = append(fixture.enriched, &models.EnrichedWorkspace{
			WorkspaceNode: &workspace.WorkspaceNode{Name: fmt.Sprintf("repo-%04d", i)},
		})
	}
	return fixture
}

// TestRoutingSnapshotAnswersTheStampedRung pins the precomputed stamped table
// to the chain it replaced: a stamped notespace routes to the notebook holding
// it, and a name that is not a recorded primary falls through to the default
// rung rather than being invented a root.
func TestRoutingSnapshotAnswersTheStampedRung(t *testing.T) {
	fixture := newRoutingFixture(t, 3, 0)
	routing := fixture.h.newRouting()

	notebook, root, err := routing.recordedNotebookRoot("notespace-001")
	if err != nil || notebook != "nb" || root != fixture.notebook {
		t.Fatalf("stamped rung = %q, %q, %v; want nb at %s", notebook, root, err, fixture.notebook)
	}
	if _, ok := routing.stamped["notespace-001"]; !ok {
		t.Fatal("a recorded primary is absent from the precomputed stamped table")
	}
	// Unstamped: the stamped rung declines and the default rung answers. Both
	// happen to be the same notebook here, so the table membership is what
	// distinguishes them.
	if _, ok := routing.stamped["never-stamped"]; ok {
		t.Fatal("a name that is not a recorded primary entered the stamped table")
	}
	if _, root, err := routing.recordedNotebookRoot("never-stamped"); err != nil || root != fixture.notebook {
		t.Fatalf("default rung = %q, %v; want %s", root, err, fixture.notebook)
	}
}

// TestRoutingSnapshotSeesAMintedStamp is the invalidation contract the cache
// underneath must honour: a stamp minted after a pass has already routed is
// picked up by the next pass, without a daemon restart.
func TestRoutingSnapshotSeesAMintedStamp(t *testing.T) {
	fixture := newRoutingFixture(t, 1, 0)
	if _, ok := fixture.h.newRouting().stamped["late"]; ok {
		t.Fatal("a notespace that does not exist yet is already routable")
	}

	root := filepath.Join(fixture.notebook, workspace.NotespaceDirectory, "late")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const (
		lateID      = "01KZVCMCZ19M95YTJN3HC99999"
		lateSubject = "local:01KZVCMCZ1GX41XHXZXH699999"
	)
	if _, err := notespacepkg.InstallNotespace(root, notespacepkg.NotespaceStamp{
		ID: lateID, Name: "late", Subject: lateSubject, Kind: "notes",
	}); err != nil {
		t.Fatal(err)
	}
	machine, err := os.ReadFile(config.MachineConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.MachineConfigPath(),
		append(machine, []byte(fmt.Sprintf("%q = %q\n", lateSubject, lateID))...), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := fixture.h.newRouting().stamped["late"]; !ok {
		t.Fatal("a stamp minted after the last pass never became routable")
	}
}

// BenchmarkSyncComputeWatchPaths is the regression this whole change exists
// for. The unified watcher runs this on a 15s ticker, on every workspace/focus
// store update, and on every event batch containing a directory create; before
// the routing snapshot it re-read machine.toml and re-walked every notebook's
// notespaces/ directory once per discovered workspace.
func BenchmarkSyncComputeWatchPaths(b *testing.B) {
	fixture := newRoutingFixture(b, 76, 694)
	b.ResetTimer()
	for b.Loop() {
		fixture.h.ComputeWatchPaths(fixture.enriched)
	}
}
