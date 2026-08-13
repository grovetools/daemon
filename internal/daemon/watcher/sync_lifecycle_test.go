package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/syncproto"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// lifecycleHarness is a SyncHandler wired to temp dirs and a fixture server
// that speaks the three verbs the reconcile pass uses: register, inventory and
// re-parent. Nothing here reads ambient config: GROVE_HOME and the XDG homes
// are redirected, and every root is a t.TempDir.
type lifecycleHarness struct {
	h            *SyncHandler
	notebookRoot string
	db           *syncdb.DB
	updates      <-chan store.Update

	mu            sync.Mutex
	registrations map[string]int
	// The fixture server's own records, so a test can ask what the server
	// believes rather than only what the daemon sent. serverNotespaces is
	// written by /sync/register (unparented, as the real server registers) and
	// moved by /sync/notespaces/reparent.
	serverNotespaces map[string]*fixtureNotespace
	serverNotebooks  map[string]fixtureNotebook
	reparents        []syncproto.NotespaceReparentRequest
	inventories      int
}

// fixtureNotespace is one notespace as the fixture server holds it: an id, the
// notebook it belongs to (empty = unparented) and the membership version that
// every membership write is CAS'd against.
type fixtureNotespace struct {
	name              string
	notebookID        string
	membershipVersion int64
}

type fixtureNotebook struct {
	name       string
	shareState string
}

func newLifecycleHarness(t *testing.T) *lifecycleHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	if err := os.MkdirAll(paths.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.MachineConfigPath(), []byte("[primaries]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	harness := &lifecycleHarness{
		registrations:    map[string]int{},
		serverNotespaces: map[string]*fixtureNotespace{},
		serverNotebooks:  map[string]fixtureNotebook{},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/register":
			var req syncproto.RegisterRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			harness.mu.Lock()
			id := req.ProposedNotespaceID.String()
			harness.registrations[id]++
			// A registration establishes identity and nothing else — the
			// notespace belongs to no notebook until something says so. That
			// is the exact shape of the gap probe 10 found, so the fixture
			// keeps it rather than helpfully parenting the row.
			if _, exists := harness.serverNotespaces[id]; !exists {
				harness.serverNotespaces[id] = &fixtureNotespace{name: req.NotespaceName.String()}
			}
			harness.mu.Unlock()
			_ = json.NewEncoder(w).Encode(syncproto.RegisterResponse{NotespaceID: req.ProposedNotespaceID})

		case "/sync/inventory":
			harness.mu.Lock()
			harness.inventories++
			out := syncproto.InventoryResponse{}
			for id, ns := range harness.serverNotespaces {
				out.Notespaces = append(out.Notespaces, syncproto.InventoryNotespace{
					ID:         syncproto.NotespaceID(id),
					Name:       syncproto.NotespaceName(ns.name),
					NotebookID: syncproto.NotebookID(ns.notebookID),
				})
			}
			for id, book := range harness.serverNotebooks {
				out.Notebooks = append(out.Notebooks, syncproto.InventoryNotebook{
					ID: syncproto.NotebookID(id), Name: book.name, ShareState: book.shareState,
				})
			}
			harness.mu.Unlock()
			_ = json.NewEncoder(w).Encode(out)

		case "/sync/notespaces/reparent":
			var req syncproto.NotespaceReparentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			harness.mu.Lock()
			defer harness.mu.Unlock()
			harness.reparents = append(harness.reparents, req)
			refuse := func(status int, wire *syncproto.ProtocolError) {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(syncproto.NotespaceReparentResponse{
					NotespaceID: req.NotespaceID, ToNotebookID: req.ToNotebookID, Error: wire,
				})
			}
			ns, held := harness.serverNotespaces[req.NotespaceID.String()]
			if !held {
				refuse(http.StatusNotFound, &syncproto.ProtocolError{
					Code: syncproto.ErrorUnregisteredNotespace, Message: "not registered",
				})
				return
			}
			book, known := harness.serverNotebooks[req.ToNotebookID.String()]
			if !known {
				refuse(http.StatusNotFound, &syncproto.ProtocolError{
					Code: syncproto.ErrorUnregisteredNotebook, Message: "destination notebook is not registered",
				})
				return
			}
			if book.shareState == syncproto.NotebookShareStateUnshared {
				refuse(http.StatusConflict, &syncproto.ProtocolError{
					Code: syncproto.ErrorNotebookUnshared, Message: "destination notebook is unshared",
				})
				return
			}
			// The two preconditions the real store applies, in its order: the
			// membership being moved out of, then the version it was decided
			// against. Both refusals report the version the server holds,
			// which is the only place a client can learn it.
			if ns.notebookID != req.FromNotebookID.String() {
				refuse(http.StatusPreconditionFailed, &syncproto.ProtocolError{
					Code:           syncproto.ErrorStaleResolution,
					Message:        "notespace belongs to another notebook",
					CurrentVersion: ns.membershipVersion,
				})
				return
			}
			if ns.membershipVersion != req.ExpectedVersion {
				refuse(http.StatusPreconditionFailed, &syncproto.ProtocolError{
					Code:           syncproto.ErrorStaleResolution,
					Message:        "membership has moved on",
					CurrentVersion: ns.membershipVersion,
				})
				return
			}
			ns.notebookID = req.ToNotebookID.String()
			ns.membershipVersion++
			_ = json.NewEncoder(w).Encode(syncproto.NotespaceReparentResponse{
				NotespaceID:      req.NotespaceID,
				FromNotebookID:   req.FromNotebookID,
				ToNotebookID:     req.ToNotebookID,
				Version:          ns.membershipVersion,
				HistoryPreserved: true,
			})

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	notebookRoot := filepath.Join(t.TempDir(), "notebooks", "default")
	if err := os.MkdirAll(filepath.Join(notebookRoot, workspace.NotespaceDirectory), 0o755); err != nil {
		t.Fatal(err)
	}

	st := store.New()
	h := NewSyncHandler(st, notebookConfig(notebookRoot), &config.SyncConfig{}, db, 50, 500)
	baseCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	h.baseCtx = baseCtx
	h.client = syncdb.NewClient(syncdb.ClientConfig{
		ServerURL: server.URL, Token: "fixture", DeviceID: "device", OriginID: "origin",
	})
	// Every reconcile in these tests is a fresh decision, never a rate-limited
	// repeat of the previous one.
	h.duplicateScanInterval = time.Nanosecond

	updates := st.Subscribe()
	t.Cleanup(func() { st.Unsubscribe(updates) })

	harness.h = h
	harness.db = db
	harness.notebookRoot = notebookRoot
	harness.updates = updates
	return harness
}

// notebookConfig records one notebook and makes it the default, so
// recordedNotebookRoot resolves any display name to this notebook's root.
func notebookConfig(root string) *config.Config {
	return &config.Config{
		Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{"default": {RootDir: root}},
			Rules:       &config.NotebookRules{Default: "default"},
		},
	}
}

// notespace mints a stamped notespace directory inside the harness notebook.
func (lh *lifecycleHarness) notespace(t *testing.T, name, id string) string {
	t.Helper()
	root := filepath.Join(lh.notebookRoot, workspace.NotespaceDirectory, name)
	if _, err := notespacepkg.InstallNotespace(root, notespacepkg.NotespaceStamp{
		ID: id, Name: name, Subject: "local:01ARZ3NDEKTSV4RRFFQ69G5FAW", Kind: "notes",
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

// subscribe replaces the live subscription list and bumps the config
// generation, exactly as a config reload does.
func (lh *lifecycleHarness) subscribe(subs ...config.SyncWorkspace) {
	lh.h.syncCfgMu.Lock()
	lh.h.syncCfg = &config.SyncConfig{Workspaces: subs}
	lh.h.syncCfgMu.Unlock()
	lh.h.bumpConfigGeneration()
}

// share records `[notebooks.<name>.sync] share` for the harness notebook, the
// way notebooks.toml does and compileCodeRootTable projects it, and reloads
// the config exactly as a config reload does.
func (lh *lifecycleHarness) share(shared bool) {
	cfg := notebookConfig(lh.notebookRoot)
	cfg.Notebooks.Definitions["default"].Shared = shared
	lh.h.setConfig(cfg)
	lh.h.bumpConfigGeneration()
}

// watch installs the watch entries a ComputeWatchPaths refresh would produce.
func (lh *lifecycleHarness) watch(entries map[string]string) {
	watches := make(map[string]*syncWatch, len(entries))
	for name, root := range entries {
		watches[root] = &syncWatch{displayName: name, root: root, space: syncdb.NewDocSpace(nil)}
	}
	lh.h.pathsMutex.Lock()
	lh.h.watchedPaths = watches
	lh.h.pathsMutex.Unlock()
}

func (lh *lifecycleHarness) pipeline(id string) *pipelineState {
	lh.h.pipelinesMu.Lock()
	defer lh.h.pipelinesMu.Unlock()
	return lh.h.pipelines[id]
}

func (lh *lifecycleHarness) registrationCount(id string) int {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	return lh.registrations[id]
}

// stampNotebook mints the harness notebook's .notebook.toml — the identity a
// notespace can be a member OF, written by `grove notebook share` in the real
// world and by nothing in the daemon.
func (lh *lifecycleHarness) stampNotebook(t *testing.T, id, name string) {
	t.Helper()
	if _, err := notespacepkg.InstallNotebook(lh.notebookRoot, notespacepkg.NotebookStamp{ID: id, Name: name}); err != nil {
		t.Fatal(err)
	}
}

// serverNotebook records a notebook on the fixture server, as `grove notebook
// share` registers one.
func (lh *lifecycleHarness) serverNotebook(id, name, shareState string) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	lh.serverNotebooks[id] = fixtureNotebook{name: name, shareState: shareState}
}

// serverMembership is the notebook the fixture server counts a notespace in,
// and the membership version it holds. A missing notespace reports "-" so a
// failure message tells "unparented" apart from "never registered".
func (lh *lifecycleHarness) serverMembership(id string) (string, int64) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	ns, ok := lh.serverNotespaces[id]
	if !ok {
		return "-", 0
	}
	return ns.notebookID, ns.membershipVersion
}

// placeOnServer puts a notespace on the fixture server directly, for the states
// a registration cannot produce: already a member, or moved before.
func (lh *lifecycleHarness) placeOnServer(id, name, notebookID string, version int64) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	lh.serverNotespaces[id] = &fixtureNotespace{name: name, notebookID: notebookID, membershipVersion: version}
}

// forgetServerNotespace drops a notespace from the fixture server, as a server
// rebuilt or reset under a running daemon forgets one: the identity is gone
// until something registers it again.
func (lh *lifecycleHarness) forgetServerNotespace(id string) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	delete(lh.serverNotespaces, id)
}

func (lh *lifecycleHarness) attachRequests() []syncproto.NotespaceReparentRequest {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	return slices.Clone(lh.reparents)
}

func (lh *lifecycleHarness) inventoryCount() int {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	return lh.inventories
}

func waitForLifecycle(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const idA = "01ARZ3NDEKTSV4RRFFQ69G5FA1"

// Removing a subscription stops its transports without a daemon restart — the
// W3.3 property that did not exist before Phase 3.
func TestReconcileStopsPipelineWhenSubscriptionIsRemoved(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": root})

	lh.h.ensurePipelines()
	running := lh.pipeline(idA)
	if running == nil {
		t.Fatal("no pipeline started for the subscribed notespace")
	}
	if running.root != root {
		t.Fatalf("pipeline root = %q, want %q", running.root, root)
	}
	lh.h.pathsMutex.RLock()
	bound := lh.h.watchedPaths[root].notespace
	lh.h.pathsMutex.RUnlock()
	if bound != idA {
		t.Fatalf("watch identity = %q, want %q", bound, idA)
	}

	// The operator removes the subscription; the watch refresh that follows a
	// reload drops its watches too.
	lh.subscribe()
	lh.watch(nil)
	lh.h.ensurePipelines()

	if state := lh.pipeline(idA); state != nil {
		t.Fatalf("the removed subscription's pipeline is still running at %q", state.root)
	}
	waitForLifecycle(t, "the stopped pipeline's goroutines to exit", func() bool {
		lh.h.pipelinesMu.Lock()
		defer lh.h.pipelinesMu.Unlock()
		return lh.h.draining[idA].stopped()
	})
}

// A notebook re-rooted in config moves its transports, and never runs two of
// them against two roots: the replacement waits for the old one to drain.
func TestReconcileRerootsPipelineAfterTheOldOneDrains(t *testing.T) {
	lh := newLifecycleHarness(t)
	oldRoot := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": oldRoot})
	lh.h.ensurePipelines()
	if lh.pipeline(idA) == nil {
		t.Fatal("no pipeline started")
	}

	// The notebook root moves: same notespace id, new directory.
	newNotebook := filepath.Join(t.TempDir(), "notebooks", "moved")
	newRoot := filepath.Join(newNotebook, workspace.NotespaceDirectory, "alpha")
	if _, err := notespacepkg.InstallNotespace(newRoot, notespacepkg.NotespaceStamp{
		ID: idA, Name: "alpha", Subject: "local:01ARZ3NDEKTSV4RRFFQ69G5FAW", Kind: "notes",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(oldRoot); err != nil {
		t.Fatal(err)
	}
	lh.h.setConfig(notebookConfig(newNotebook))
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": newRoot})

	// The old transport is cancelled and the replacement comes up at the
	// recorded root — once the old one has drained, which may take more than
	// one pass.
	waitForLifecycle(t, "the pipeline to re-root", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && state.root == newRoot
	})
	if lh.registrationCount(idA) != 2 {
		t.Fatalf("registrations = %d, want one per pipeline start", lh.registrationCount(idA))
	}
	lh.h.pathsMutex.RLock()
	defer lh.h.pathsMutex.RUnlock()
	if lh.h.watchedPaths[newRoot].notespace != idA {
		t.Fatal("the re-rooted watch never picked up its identity")
	}
}

// The drain gate itself: while a cancelled pipeline's goroutines are still
// running, no replacement for that notespace id starts — not even a
// registration. Two transports for one id, writing into two roots, is the
// failure a re-root would otherwise invite.
func TestReplacementPipelineWaitsForTheOldOneToDrain(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": root})

	draining := make(chan struct{})
	lh.h.pipelinesMu.Lock()
	lh.h.draining[idA] = &pipelineState{cancel: func() {}, done: draining, root: filepath.Join(root, "..", "old")}
	lh.h.pipelinesMu.Unlock()

	lh.h.ensurePipelines()
	if state := lh.pipeline(idA); state != nil {
		t.Fatalf("a replacement started while the previous pipeline was draining: %+v", state)
	}
	if lh.registrationCount(idA) != 0 {
		t.Fatal("the replacement registered before the previous pipeline drained")
	}

	close(draining)
	lh.h.ensurePipelines()
	if state := lh.pipeline(idA); state == nil || state.root != root {
		t.Fatalf("pipeline after the drain completed = %+v, want root %q", state, root)
	}
}

// A config reload advances the generation, and every pipeline installed after
// it carries the new one — the stamp a stale pass checks itself against.
func TestConfigReloadStampsTheNewGenerationOntoPipelines(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": root})

	generation := lh.h.configGeneration.Load()
	lh.h.ensurePipelines()
	state := lh.pipeline(idA)
	if state == nil || state.generation != generation {
		t.Fatalf("pipeline generation = %+v, want %d", state, generation)
	}

	// A reconcile that observes a newer generation than the one it started
	// with abandons the rest of its pass rather than installing stale routing.
	lh.h.bumpConfigGeneration()
	if lh.h.configGeneration.Load() != generation+1 {
		t.Fatalf("generation = %d, want %d", lh.h.configGeneration.Load(), generation+1)
	}
}

// D8: two roots carrying one stamp id. The first-seen root keeps syncing, the
// copy is parked with evidence naming both paths, and the parked root never
// registers, never gets a transport, and never becomes a capture identity.
func TestDuplicateStampParksTheCopyAndKeepsTheFirstSeenRoot(t *testing.T) {
	lh := newLifecycleHarness(t)
	first := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha"})
	lh.watch(map[string]string{"alpha": first})
	lh.h.ensurePipelines()

	// `cp -R alpha alpha-copy` inside the same notebook: a second root with
	// the same stamp that nothing is subscribed to.
	copyRoot := lh.notespace(t, "alpha-copy", idA)
	lh.h.ensurePipelines()

	parked := lh.h.ParkedNotespaces()
	if len(parked) != 1 {
		t.Fatalf("parked = %+v, want exactly the copy", parked)
	}
	if parked[0].Root != copyRoot || parked[0].Keeper != first {
		t.Fatalf("parked verdict = %+v, want root=%q keeper=%q", parked[0], copyRoot, first)
	}
	if parked[0].Reason != syncdb.ConflictKindDuplicateStamp {
		t.Fatalf("parked reason = %q", parked[0].Reason)
	}
	if !strings.Contains(parked[0].Detail, first) || !strings.Contains(parked[0].Detail, copyRoot) {
		t.Fatalf("evidence must name both roots: %q", parked[0].Detail)
	}
	if state := lh.pipeline(idA); state == nil || state.root != first {
		t.Fatalf("the first-seen root lost its transport: %+v", state)
	}

	matches, err := filepath.Glob(filepath.Join(paths.StateDir(), "sync", "conflicts", idA, "*"+syncdb.ConflictKindDuplicateStamp+"*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("duplicate-stamp evidence files = %v err=%v", matches, err)
	}

	// Parking is idempotent: a second pass over the same duplicate writes no
	// new evidence and broadcasts nothing new.
	drain(lh.updates)
	lh.h.ensurePipelines()
	select {
	case update := <-lh.updates:
		t.Fatalf("re-broadcast a settled parking decision: %+v", update)
	case <-time.After(100 * time.Millisecond):
	}

	// Re-minting the copy clears the verdict without a restart.
	if err := os.RemoveAll(copyRoot); err != nil {
		t.Fatal(err)
	}
	lh.h.ensurePipelines()
	if parked := lh.h.ParkedNotespaces(); len(parked) != 0 {
		t.Fatalf("parking survived the repair: %+v", parked)
	}
}

// The first-seen answer survives a restart: it comes from the sync.db binding,
// not from whichever root sorts first this pass.
func TestFirstSeenRootComesFromTheDurableBinding(t *testing.T) {
	lh := newLifecycleHarness(t)
	// "zeta" sorts after "alpha", so a sort-order answer would pick alpha.
	alpha := lh.notespace(t, "alpha", idA)
	zeta := lh.notespace(t, "zeta", idA)
	if err := lh.db.UpsertNotespaceBinding(syncdb.NotespaceBinding{
		ID: idA, Name: "zeta", Root: zeta, Subject: "local:01ARZ3NDEKTSV4RRFFQ69G5FAW", Kind: "notes",
	}); err != nil {
		t.Fatal(err)
	}

	lh.subscribe(config.SyncWorkspace{Name: "alpha"}, config.SyncWorkspace{Name: "zeta"})
	lh.watch(map[string]string{"alpha": alpha, "zeta": zeta})
	lh.h.ensurePipelines()

	if state := lh.pipeline(idA); state == nil || state.root != zeta {
		t.Fatalf("pipeline = %+v, want the bound root %q", state, zeta)
	}
	parked := lh.h.ParkedNotespaces()
	if len(parked) != 1 || parked[0].Root != alpha {
		t.Fatalf("parked = %+v, want %q parked", parked, alpha)
	}
	lh.h.pathsMutex.RLock()
	defer lh.h.pathsMutex.RUnlock()
	if lh.h.watchedPaths[alpha].notespace != "" {
		t.Fatal("a parked root became a capture identity")
	}
}

// W3.5 seam: a contested notespace takes no incoming writes AND sends nothing
// out until it is adopted. It keeps its root binding, so local edits queue in
// the parked outbox and adoption releases them — see
// sync_contested_outbound_test.go for the two-machine proof.
func TestContestedNotespaceGetsNeitherPipeline(t *testing.T) {
	lh := newLifecycleHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})

	lh.h.MarkContested(idA, "adoption pending: colliding local notes")
	lh.h.ensurePipelines()

	state := lh.pipeline(idA)
	if state == nil {
		t.Fatal("a contested notespace lost its root binding; local edits would stop queuing")
	}
	if state.pull {
		t.Fatal("a contested notespace was given a pull pipeline")
	}
	if state.push {
		t.Fatal("a contested notespace was given a push pipeline; the other machine's copy is decided for it")
	}
	if reasons := lh.h.ContestedNotespaces(); len(reasons) != 1 || reasons[idA] == "" {
		t.Fatalf("contested = %+v", reasons)
	}
	lh.h.ClearContested(idA)
	if reasons := lh.h.ContestedNotespaces(); len(reasons) != 0 {
		t.Fatalf("adoption did not clear the contest: %+v", reasons)
	}
}

func drain(updates <-chan store.Update) {
	for {
		select {
		case <-updates:
		default:
			return
		}
	}
}
