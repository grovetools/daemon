// SyncHandler implements DomainHandler for the notebook sync protocol's
// Phase 0 client-side state: it watches subscribed notebook notespaces,
// hash-gates content changes, and records them in sync.db (identity map +
// outbox) for the Phase 1 push loop.
//
// Standing rules baked in here:
//
//   - Dark by default: the handler is constructed unconditionally by the global
//     daemon but stays DORMANT while no sync config with notespace
//     subscriptions exists — no watches, no sync.db, no transport. sync.db is
//     opened lazily by ensureDB the first time a subscription appears, which is
//     what lets a first-ever `grove join` take effect on the next config reload
//     instead of requiring a daemon restart (contract §1 Q7, boot gate).
//   - Notebook-read-only, with exactly one carve-out: this handler never
//     writes to the USER's notes. All capture writes go to sync.db under
//     ~/.local/share/grove. The single exception is the machine presence note
//     (registry.go), which writes machines/<own-id>.md into the reserved
//     registry notespace — this machine's own document, in a notespace that
//     exists for nothing else, single-writer by construction. It is not the
//     user's note and the rule it looks like it breaks was never about it.
//   - Global daemon only: sync.db is owned by the global daemon, like
//     memory.db; scoped daemons proxy /api/sync/* to global.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/coderoot"
	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/models"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/syncproto"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/pathutil"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// Default debounce tuning: a change is flushed after quietMs of silence, but
// never later than maxWaitMs after the first event — rapid appends coalesce
// into one outbox row without starving under continuous writes.
const (
	defaultSyncQuietMs   = 2000
	defaultSyncMaxWaitMs = 15000

	// defaultEpochProbeInterval is how often a connected transport re-runs the
	// capabilities handshake purely to compare server epochs. It bounds how
	// long a recreated server can go unnoticed by a machine with nothing to
	// push; the anti-entropy pass is the hourly backstop, not the detector.
	defaultEpochProbeInterval = 1 * time.Minute
)

// syncWatch maps a watched directory to its sync notespace subscription.
type syncWatch struct {
	displayName string           // mutable config/stamp name; discovery only, never a DB/wire key
	notespace   string           // immutable stamp id; empty until registration succeeds
	root        string           // notespace root dir; wire paths are relative to this
	space       *syncdb.DocSpace // exclusion + routing policy for this subscription
}

// SyncHandler implements DomainHandler for sync change capture.
type SyncHandler struct {
	store *store.Store
	// cfg/locator are swapped wholesale by a config reload running on the
	// store dispatch goroutine, and read by the reconcile pass (routing,
	// containment) running on its own. They carry the same RWMutex treatment
	// syncCfg already has rather than being bare fields: since the reload
	// handler itself now kicks `go h.ensurePipelines()`, reload N's reconcile
	// is routinely still in flight — it registers over the network under
	// reconcileMu — when reload N+1 swaps the pointers. Read them through
	// configSnapshot/notebookLocator, never directly.
	cfgMu   sync.RWMutex
	cfg     *config.Config
	locator *workspace.NotebookLocator
	ulog    *logging.UnifiedLogger

	// db is written once, possibly long after construction (see ensureDB), and
	// read from the watcher, transport, and maintenance goroutines — atomic for
	// the same reason the server's late-wired deps are. Nil means dormant;
	// every reader goes through database() and nil-checks.
	db atomic.Pointer[syncdb.DB]
	// dbOpener/dbReady are installed by SetDeferredDB on the global daemon:
	// open sync.db on first need, then publish it to the HTTP server. Both nil
	// in tests and wherever the DB is supplied up front.
	dbOpener     func() (*syncdb.DB, error)
	dbReady      func(*syncdb.DB)
	dbOpenMu     sync.Mutex
	dbOpenFailed bool
	dbOpenError  string

	syncCfg   *config.SyncConfig
	syncCfgMu sync.RWMutex

	// Maps watched directory -> subscription info. routingErrors is the
	// doctor/status-visible fail-loud condition from the latest refresh.
	watchedPaths  map[string]*syncWatch
	routingErrors []string
	pathsMutex    sync.RWMutex

	quietMs   int
	maxWaitMs int
	timers    map[string]*time.Timer
	firstSeen map[string]time.Time
	timersMu  sync.Mutex

	// Transport state: a shared server client plus per-notespace pipeline
	// cancel funcs, spawned lazily once a notespace root is known (from the
	// discovery-driven watch set, or from config alone for pull = true
	// subscriptions — see ensurePipelines). aePasses
	// keeps each notespace's anti-entropy reconciler addressable so the
	// manual /api/sync/repush endpoint can kick an immediate pass (guarded by
	// pipelinesMu, same lifecycle as pipelines).
	client      *syncdb.Client
	clientMu    sync.RWMutex
	pipelines   map[string]*pipelineState
	aePasses    map[string]*syncdb.AntiEntropyPass
	pipelinesMu sync.Mutex
	baseCtx     context.Context
	maintenance atomic.Bool
	drainMu     sync.Mutex

	// Pipeline lifecycle (sync_lifecycle.go). reconcileMu serializes whole
	// reconcile passes — they are called from the transport tick, the unified
	// watcher's path refresh, and the adoption notification, and a teardown
	// interleaved with a start would leave orphaned transports. draining holds
	// pipelines that were cancelled but whose goroutines have not all returned
	// yet: no replacement pipeline for the same notespace id starts until the
	// old one is provably gone, which is what makes a re-root generation-safe.
	// configGeneration is bumped by every config reload and stamped onto each
	// pipeline, so a pass that started under stale config never installs.
	reconcileMu      sync.Mutex
	draining         map[string]*pipelineState
	configGeneration atomic.Uint64

	// parked/contested are the notespaces this daemon refuses to sync, keyed
	// by immutable id (sync_lifecycle.go): parked is the D8 duplicate-stamp
	// verdict recomputed each pass, contested is the W3.5 adoption seam.
	// Guarded by parkMu.
	parkMu    sync.Mutex
	parked    map[string]ParkedNotespace
	contested map[string]ContestedNotespace

	// duplicateScannedAt rate-limits the containing-notebook duplicate-stamp
	// sweep; zero duplicateScanInterval selects the production cadence.
	// duplicateSiblingsCache holds the last sweep's result so a rate-limited
	// pass replays the verdict instead of reporting "no duplicates" — see
	// duplicateSiblings. All three are touched only under reconcileMu.
	duplicateScannedAt     time.Time
	duplicateScanInterval  time.Duration
	duplicateSiblingsCache map[string][]string

	// ContainmentAutoRegister enables W3.2's "containment is consent"
	// inheritance (sync_containment.go). Dark by default and set by nothing in
	// the daemon: its recorded input, `[notebooks.<name>.sync] share = true`,
	// is the core half of Phase 3 and does not parse yet.
	ContainmentAutoRegister bool

	// Token-rejection state (sync_auth.go): the stale-token trap's detection,
	// reporting, and reconnect-backoff machinery. transportInterval and the
	// two authRetry bounds are test seams — zero values select the production
	// reconnect cadence and backoff window.
	authMu            sync.Mutex
	auth              syncAuthState
	transportInterval time.Duration
	authRetryBase     time.Duration
	authRetryMax      time.Duration

	// Server-epoch re-probe (probeServerEpoch): rate limiter for the periodic
	// handshake that catches a server recreated under a running daemon.
	// epochProbeInterval is a test seam; zero selects the production cadence.
	epochProbeMu       sync.Mutex
	epochProbedAt      time.Time
	epochProbeInterval time.Duration

	// Registry presence writer (registry.go). registryKick coalesces
	// structural-change triggers; registryInterval and registryNow are test
	// seams (zero values select the production ticker and the wall clock).
	// registryWarned is touched only from the writer goroutine.
	registryKick     chan struct{}
	registryInterval time.Duration
	registryNow      func() time.Time
	registryWarned   bool
}

// NewSyncHandler creates a SyncHandler. Both syncCfg and db may be nil: the
// handler is then dormant, and stays dormant until a config reload brings
// subscriptions and (with SetDeferredDB installed) ensureDB opens sync.db.
// quietMs/maxWaitMs <= 0 select the defaults (2s quiet / 15s max latency).
func NewSyncHandler(st *store.Store, cfg *config.Config, syncCfg *config.SyncConfig, db *syncdb.DB, quietMs, maxWaitMs int) *SyncHandler {
	if quietMs <= 0 {
		quietMs = defaultSyncQuietMs
	}
	if maxWaitMs <= 0 {
		maxWaitMs = defaultSyncMaxWaitMs
	}

	h := &SyncHandler{
		store:        st,
		cfg:          cfg,
		locator:      workspace.NewNotebookLocator(cfg),
		ulog:         logging.NewUnifiedLogger("groved.watcher.sync"),
		syncCfg:      syncCfg,
		watchedPaths: make(map[string]*syncWatch),
		quietMs:      quietMs,
		maxWaitMs:    maxWaitMs,
		timers:       make(map[string]*time.Timer),
		firstSeen:    make(map[string]time.Time),
		pipelines:    make(map[string]*pipelineState),
		draining:     make(map[string]*pipelineState),
		aePasses:     make(map[string]*syncdb.AntiEntropyPass),
		parked:       make(map[string]ParkedNotespace),
		contested:    make(map[string]ContestedNotespace),
		registryKick: make(chan struct{}, 1),
	}
	if db != nil {
		h.db.Store(db)
	}
	return h
}

// SetDeferredDB installs the lazy sync.db opener. The global daemon constructs
// the SyncHandler unconditionally — before it knows whether sync is configured
// — and hands it these two hooks instead of an open database: open is called at
// most once, the first time a non-empty subscription list is observed, and
// ready then publishes the DB to the HTTP server. Without this the daemon had
// to decide at boot, so a first-ever `grove join` wrote a valid sync.toml that
// nothing picked up until the next restart.
func (h *SyncHandler) SetDeferredDB(open func() (*syncdb.DB, error), ready func(*syncdb.DB)) {
	h.dbOpenMu.Lock()
	defer h.dbOpenMu.Unlock()
	h.dbOpener = open
	h.dbReady = ready
}

// database returns the open sync database, or nil while dormant.
func (h *SyncHandler) database() *syncdb.DB { return h.db.Load() }

// SyncDBError is the status-only startup seam for a non-mutating legacy DB refusal.
func (h *SyncHandler) SyncDBError() string {
	h.dbOpenMu.Lock()
	defer h.dbOpenMu.Unlock()
	return h.dbOpenError
}

// hasSubscriptions reports whether the live config carries any subscription —
// the condition that wakes a dormant handler.
func (h *SyncHandler) hasSubscriptions() bool {
	h.syncCfgMu.RLock()
	defer h.syncCfgMu.RUnlock()
	return h.syncCfg != nil && len(h.syncCfg.Workspaces) > 0
}

// ensureDB returns the sync database, opening it on first need once the config
// carries at least one subscription. It returns nil while the handler is
// dormant (no subscriptions, no opener installed, or the open failed) — every
// caller nil-checks. Open failures are retried on the next call; only the log
// is rate-limited to one warning.
func (h *SyncHandler) ensureDB() *syncdb.DB {
	if db := h.database(); db != nil {
		return db
	}

	h.dbOpenMu.Lock()
	defer h.dbOpenMu.Unlock()
	if db := h.database(); db != nil {
		return db // another goroutine won the race
	}
	if h.dbOpener == nil || !h.hasSubscriptions() {
		return nil
	}

	db, err := h.dbOpener()
	if err != nil {
		h.dbOpenError = err.Error()
		if !h.dbOpenFailed {
			h.dbOpenFailed = true
			h.ulog.Warn("Failed to open sync database, sync stays dormant").Err(err).Emit()
		}
		return nil
	}
	h.dbOpenFailed = false
	h.dbOpenError = ""
	h.db.Store(db)
	if h.dbReady != nil {
		h.dbReady(db)
	}
	h.ulog.Info("Sync database opened").
		Field("origin_id", db.OriginID()).
		StructuredOnly().Emit()
	// origin_id only becomes knowable here, and the note records it so a wiped
	// sync.db reads as "same machine, new origin" rather than a new machine.
	h.kickRegistry()
	return db
}

func (h *SyncHandler) Name() string {
	return "sync"
}

// subscription returns the sync subscription for a notespace name, or nil.
func (h *SyncHandler) subscription(name string) *config.SyncWorkspace {
	h.syncCfgMu.RLock()
	defer h.syncCfgMu.RUnlock()
	if h.syncCfg == nil {
		return nil
	}
	for i := range h.syncCfg.Workspaces {
		if h.syncCfg.Workspaces[i].Name == name {
			return &h.syncCfg.Workspaces[i]
		}
	}
	return nil
}

// subscriptionsSnapshot returns a copy of the current subscription list, safe
// to iterate without holding syncCfgMu.
func (h *SyncHandler) subscriptionsSnapshot() []config.SyncWorkspace {
	h.syncCfgMu.RLock()
	defer h.syncCfgMu.RUnlock()
	if h.syncCfg == nil {
		return nil
	}
	return slices.Clone(h.syncCfg.Workspaces)
}

// SyncSubscriptions returns the configured sync server URL alongside a copy of
// the current subscription list — the same snapshot subscriptionsSnapshot
// takes, read under one lock so the URL and the notespaces can never straddle
// a hot reload. Wired into the HTTP server (SetSyncSubscriptions) so GET
// /api/sync/status can answer "where is this syncing, and in which direction"
// instead of only "how much is queued". Reads the live config rather than a
// value captured at wiring time, so an UpdateConfigReload is reflected without
// a daemon restart.
func (h *SyncHandler) SyncSubscriptions() (string, []config.SyncWorkspace) {
	h.syncCfgMu.RLock()
	defer h.syncCfgMu.RUnlock()
	if h.syncCfg == nil {
		return "", nil
	}
	return h.syncCfg.Server, slices.Clone(h.syncCfg.Workspaces)
}

// configSnapshot returns the recorded config a pass should route against. The
// pointer is immutable once published — a reload installs a NEW *config.Config
// rather than mutating the live one — so a pass that reads it once is
// internally consistent even if a reload lands mid-pass. The generation check
// in ensurePipelines, not this lock, is what stops a pass from ACTING on a
// snapshot that has since been superseded.
func (h *SyncHandler) configSnapshot() *config.Config {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.cfg
}

// notebookLocator returns the locator built from the current config snapshot.
func (h *SyncHandler) notebookLocator() *workspace.NotebookLocator {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.locator
}

// setConfig installs a reloaded config and the locator derived from it as one
// atomic swap, so no reader can observe a locator built from the other config.
func (h *SyncHandler) setConfig(cfg *config.Config) {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.cfg = cfg
	h.locator = workspace.NewNotebookLocator(cfg)
}

// recordedNotebookRoot returns the authoritative name+root pair. An exact
// compiled code-root binding is literal rung 0; a stamped notespace whose id is
// the recorded primary for its subject is rung 1; the recorded default is the
// only fallback. The compiled NotebookRoot is returned directly, never
// re-resolved through Definitions by notebook name.
func (h *SyncHandler) recordedNotebookRoot(name string) (string, string, error) {
	cfg := h.configSnapshot()
	if cfg == nil || name == "" {
		return "", "", fmt.Errorf("notespace %q has no recorded code-root/notebook binding", name)
	}
	if grove, ok := cfg.Groves[name]; ok {
		if grove.Notebook != "" || grove.NotebookRoot != "" {
			if grove.Notebook == "" || grove.NotebookRoot == "" {
				return "", "", fmt.Errorf("notespace %q has an incomplete recorded code-root/notebook binding", name)
			}
			return grove.Notebook, grove.NotebookRoot, nil
		}
	}
	// Identity rung, ahead of the default: a notes-plane subscription with no
	// compiled code-root binding is still locatable BY IDENTITY — its stamp id
	// has to be the recorded primary for its subject in machine.toml. Dropping
	// straight to notebooks.rules.default here is what sent a notespace bound
	// to a non-default notebook to <default>/notespaces/<name>, the wrong-root
	// class P2 exists to eliminate. Nothing is inferred: an unstamped tree (a
	// pull replica that does not exist yet) or a name that does not identify
	// exactly one recorded primary falls through to the default rung below,
	// byte for byte as before.
	if notebook, root, ok := h.stampedNotebookRoot(name); ok {
		return notebook, root, nil
	}
	if cfg.Notebooks == nil || cfg.Notebooks.Rules == nil || cfg.Notebooks.Rules.Default == "" {
		return "", "", fmt.Errorf("notespace %q has no recorded code-root/notebook binding or default notebook", name)
	}
	notebook := cfg.Notebooks.Rules.Default
	definition := cfg.Notebooks.Definitions[notebook]
	if definition == nil || definition.RootDir == "" {
		return "", "", fmt.Errorf("notespace %q routes to default notebook %q without a recorded root", name, notebook)
	}
	return notebook, notebookRootDir(definition), nil
}

// notebookRootDir resolves a recorded notebook definition's root to a path the
// daemon can watch, join and stat.
//
// core resolves these at config compile time, so on a machine with a recorded
// notebooks.toml this is already a no-op. It is here anyway because the
// daemon must not silently depend on that: a config shape that leaves the
// legacy `root_dir = '~/notebooks/<name>'` spelling in place (no notebooks.toml
// yet, a seeded satellite) would otherwise send every watch and every pull
// root to "<cwd>/~/notebooks/…" with nothing raising an error. Expansion is
// idempotent, so applying it twice costs nothing.
func notebookRootDir(definition *config.Notebook) string {
	if definition == nil {
		return ""
	}
	return coderoot.ExpandPath(definition.RootDir)
}

// stampedNotebookRoot answers which recorded notebook holds the stamped
// notespace a display name identifies, using core's recorded-primary resolver
// (stamp id + machine.toml [primaries]) rather than any name-to-directory
// guess — the same chain nb, grove.nvim and skills already route through.
//
// ok is false whenever that chain cannot answer EXACTLY: no readable
// machine.toml, no stamp, a name that is not a recorded primary, a name
// ambiguous across roots, a resolved root that is not
// <recorded notebook root>/notespaces/<name>, or a notebook root that
// notebooks.toml does not record. Every one of those leaves the decision to the
// caller's remaining rungs instead of inventing a root.
func (h *SyncHandler) stampedNotebookRoot(name string) (string, string, bool) {
	cfg := h.configSnapshot()
	if cfg == nil || cfg.Notebooks == nil || len(cfg.Notebooks.Definitions) == 0 {
		return "", "", false
	}
	machineCfg, err := config.LoadMachineConfig()
	if err != nil || machineCfg == nil {
		return "", "", false
	}
	resolution, err := workspace.ResolveNotespaceName(name, cfg, machineCfg)
	if err != nil || resolution.Root == "" {
		return "", "", false
	}
	// recordedNotebookRoot's contract is a NOTEBOOK root that nodeNotespaceRoot
	// re-joins with "notespaces/<name>", so only a resolution that round-trips
	// through that join can be reported here.
	if filepath.Base(resolution.Root) != name {
		return "", "", false
	}
	notespacesDir := filepath.Dir(resolution.Root)
	if filepath.Base(notespacesDir) != workspace.NotespaceDirectory {
		return "", "", false
	}
	notebookRoot := filepath.Dir(notespacesDir)
	// Both sides are normalized before comparison. resolution.Root is an
	// absolute path built by walking the notespace index; a definition's
	// RootDir is a RECORDED value, and comparing a recorded value to a
	// resolved one by raw string equality is exactly the mistake this rung
	// exists to correct one layer down. A declared "~/notebooks/canary-nb"
	// never equals "/Users/…/notebooks/canary-nb", so the rung would report
	// "no recorded notebook" and hand the decision back to the default — the
	// same wrong root, arrived at more expensively.
	for _, notebook := range slices.Sorted(maps.Keys(cfg.Notebooks.Definitions)) {
		if samePhysicalPath(notebookRootDir(cfg.Notebooks.Definitions[notebook]), notebookRoot) {
			return notebook, notebookRoot, true
		}
	}
	return "", "", false
}

// samePhysicalPath reports whether two paths name the same directory.
//
// Lexical comparison first, so an answer does not depend on the filesystem;
// symlink resolution only as a fallback, for the macOS /var -> /private/var
// aliasing that also defeats containment checks elsewhere in the daemon. A
// path that cannot be resolved (it does not exist yet — an unmaterialized pull
// replica) simply does not match, which is the conservative answer: the
// caller's remaining rungs decide rather than this one guessing.
func samePhysicalPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	canonicalA, errA := pathutil.CanonicalPath(a)
	canonicalB, errB := pathutil.CanonicalPath(b)
	return errA == nil && errB == nil && canonicalA == canonicalB
}

func (h *SyncHandler) syntheticNodeFor(name string) (*workspace.WorkspaceNode, error) {
	node := &workspace.WorkspaceNode{Name: name}
	notebook, _, err := h.recordedNotebookRoot(name)
	if err != nil {
		return node, err
	}
	node.NotebookName = notebook
	return node, nil
}

// nodeNotespaceRoot consumes a compiled root literally for discovered nodes as
// well as synthetic subscriptions. It has no existence requirement: pull must
// be able to materialize a replica into a tree that does not exist yet.
func (h *SyncHandler) nodeNotespaceRoot(node *workspace.WorkspaceNode) (string, error) {
	if node == nil {
		return "", fmt.Errorf("cannot route a nil notespace node")
	}
	_, root, err := h.recordedNotebookRoot(node.Name)
	if err != nil && node.Path != "" {
		binding := config.ResolveNotebook(config.NotebookQuery{
			Path:       node.Path,
			OwnerPaths: []string{node.ParentProjectPath, node.ParentEcosystemPath, node.RootEcosystemPath},
		}, h.configSnapshot())
		if binding.Notebook != "" && binding.NotebookRoot != "" {
			root, err = binding.NotebookRoot, nil
		}
	}
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("notespace %q has non-absolute recorded notebook root %q", node.Name, root)
	}
	return filepath.Join(root, "notespaces", node.Name), nil
}

// NotespaceRoots resolves explicitly selected subscribed notespaces to their
// configured laptop notebook roots. It is used by the user-authorized incoming
// apply boundary; unlike configuredPullRoots it includes push-only laptop
// subscriptions and never invents a wildcard/default selection.
func (h *SyncHandler) NotespaceRoots(ids []string) (map[string]string, error) {
	roots := make(map[string]string, len(ids))
	h.pathsMutex.RLock()
	for _, w := range h.watchedPaths {
		for _, id := range ids {
			if w.notespace == id {
				roots[id] = w.root
			}
		}
	}
	h.pathsMutex.RUnlock()
	// Push-only subscriptions may have no active filesystem watch. Resolve every
	// explicit subscription through recorded topology, then authorize by the
	// immutable stamp at that root rather than by its display name.
	missing := false
	for _, id := range ids {
		missing = missing || roots[id] == ""
	}
	if missing {
		for _, sub := range h.subscriptionsSnapshot() {
			node, err := h.syntheticNodeFor(sub.Name)
			if err != nil {
				return nil, err
			}
			root, err := h.nodeNotespaceRoot(node)
			if err != nil {
				return nil, err
			}
			stamp, err := notespacepkg.LoadNotespace(root)
			if err != nil || stamp == nil {
				continue
			}
			// A parked duplicate must not become an authorized apply target:
			// its whole point is that it does not sync, and the escrow path
			// writes into whatever root it is handed (W3.6 / D8).
			if h.isParkedRoot(root) {
				continue
			}
			for _, id := range ids {
				if stamp.ID == id {
					roots[id] = root
				}
			}
		}
	}
	for _, id := range ids {
		if roots[id] == "" {
			return nil, fmt.Errorf("notespace %q has no registered local root", id)
		}
	}
	return roots, nil
}

// configuredPullRoots derives notespace -> root for every pull = true
// subscription directly from sync.toml + notebook definitions, independent of
// code-notespace discovery. Pull targets are notebook notespaces whose paths
// are fully config-determined; requiring a .git under a grove path to
// materialize notes was accidental coupling (the empty-~/code satellite bug).
func (h *SyncHandler) configuredPullRoots() (map[string]string, error) {
	roots := make(map[string]string)
	for _, sub := range h.subscriptionsSnapshot() {
		if !sub.Pull || sub.Mode == config.SyncModeSearchOnly {
			continue
		}
		node, err := h.syntheticNodeFor(sub.Name)
		if err != nil {
			return nil, err
		}
		root, err := h.nodeNotespaceRoot(node)
		if err != nil {
			return nil, err
		}
		roots[sub.Name] = root
	}
	return roots, nil
}

// recordedContentDirs derives standard content paths from the already-routed
// notespace root, avoiding a notebook-name lookup through the locator.
func recordedContentDirs(root string) []workspace.ContentDirectory {
	return []workspace.ContentDirectory{
		{Path: root, Type: "notes"},
		{Path: filepath.Join(root, "plans"), Type: "plans"},
		{Path: filepath.Join(root, "chats"), Type: "chats"},
	}
}

// computeNotespaceWatches enumerates every Included directory of one subscribed
// notespace as its own watch entry, recursively (Phase 2, the S1 fix). fsnotify
// is non-recursive, so every directory in the doc space needs an individual
// watch; DocSpace.WalkTree prunes excluded subtrees (.git/, .artifacts/, …)
// during the walk at O(included dirs) cost. syncWatch.root stays the notespace
// root in every entry — lookupWatch/flush compute wire paths against it, and
// that must not change with the walk root. Extracted from ComputeWatchPaths so
// the S1 reproduction can test it without the locator machinery.
func computeNotespaceWatches(sub *config.SyncWorkspace, recordedRoot string, contentDirs []workspace.ContentDirectory) map[string]*syncWatch {
	watches := make(map[string]*syncWatch)
	if sub == nil {
		return watches
	}
	// Build the DocSpace once per subscription (per-notespace excludes + size
	// cap), shared across all of the notespace's watched dirs.
	space := syncdb.NewDocSpace(sub)

	// Root identity comes from the recorded route, never from whichever content
	// directory happens to stat first this tick.
	root := recordedRoot
	if root == "" {
		return watches
	}

	// Walk roots by mode: Full covers everything from the notespace root in one
	// walk (the plans/chats content dirs become redundant and are ignored);
	// PlansOnly walks only the plans/ content dirs, preserving today's filter.
	var walkRoots []string
	switch sub.Mode {
	case config.SyncModePlansOnly:
		for _, dir := range contentDirs {
			if dir.Type == "plans" {
				walkRoots = append(walkRoots, dir.Path)
			}
		}
	default: // SyncModeFull and "" — one walk from the root.
		walkRoots = []string{root}
	}

	for _, walkRoot := range walkRoots {
		if _, err := os.Stat(walkRoot); err != nil {
			continue
		}
		onDir := func(abs, _ string) error {
			watches[abs] = &syncWatch{displayName: sub.Name, root: root, space: space}
			return nil
		}
		if err := space.WalkTree(walkRoot, onDir, nil); err != nil {
			// Vanished/unreadable walk root: skip it this tick, mirroring the
			// old per-dir stat-failure continue.
			continue
		}
	}
	return watches
}

// AdoptedNotespace is the post-mint notification from the daemon adoption
// handler. It makes an already-subscribed adopted root visible to registration
// and pipeline creation immediately; the unified watcher's next normal refresh
// installs the corresponding fsnotify directories.
func (h *SyncHandler) AdoptedNotespace(root, displayName string) {
	// The post-mint hook is exactly where "containment is consent" applies: a
	// notespace that was just minted inside a shared notebook inherits that
	// notebook's subscription instead of needing its own (W3.2). With
	// containment dark, effectiveSubscription is the literal config lookup this
	// always was.
	sub := h.effectiveSubscription(displayName, root)
	if sub == nil || sub.Mode == config.SyncModeSearchOnly {
		return
	}
	watches := computeNotespaceWatches(sub, root, recordedContentDirs(root))
	h.pathsMutex.Lock()
	maps.Copy(h.watchedPaths, watches)
	h.pathsMutex.Unlock()
	h.ensurePipelines()
}

// ComputeWatchPaths returns every Included directory of subscribed notespaces
// (recursively) so the non-recursive fsnotify backend covers the whole tree.
func (h *SyncHandler) ComputeWatchPaths(notespaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*syncWatch)
	covered := make(map[string]bool) // subscription names covered by discovery
	var routingErrors []string

	for _, ew := range notespaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}
		sub := h.effectiveSubscription(node.Name, h.discoveredNotespaceRoot(node))
		if sub == nil {
			continue
		}
		covered[node.Name] = true
		if sub.Mode == config.SyncModeSearchOnly {
			// search-only keeps no local replica — nothing to watch.
			continue
		}

		root, err := h.nodeNotespaceRoot(node)
		if err != nil {
			routingErrors = append(routingErrors, err.Error())
			continue
		}
		dirs := recordedContentDirs(root)
		maps.Copy(newWatches, computeNotespaceWatches(sub, root, dirs))
	}

	// Subscriptions not covered by code discovery (pure notes satellite:
	// nothing under any grove path) are still config-locatable: resolve their
	// content dirs through a synthetic node so the notespace's own tree is
	// watched — local edits get captured and pushed just like on a machine
	// where discovery works. computeNotespaceWatches only registers stat-able
	// dirs, so this is a no-op until the tree exists (for a pull replica, until
	// the pull pipeline materializes it; the periodic refresh picks it up
	// afterwards).
	//
	// This loop deliberately does NOT filter on sub.Pull. Push-only
	// subscriptions were the F8 residual: configuredPullRoots covers the
	// pull side only, so a push-only notebook notespace that discovery never
	// yields (no .git under any grove path — a bare notebook) got no watches
	// here and, because ensurePipelines derives its roots from this watch set,
	// no push pipeline either. It went silently dark in both directions. The
	// nodeNotespaceRoot resolution below is the real gate: a subscription whose
	// root config cannot locate is still skipped. Pull remains strictly
	// opt-in — ensurePipelines still starts a pull loop only for sub.Pull, so
	// the legacy push-only invariant is untouched.
	for _, sub := range h.subscriptionsSnapshot() {
		if covered[sub.Name] || sub.Mode == config.SyncModeSearchOnly {
			continue
		}
		node, err := h.syntheticNodeFor(sub.Name)
		if err != nil {
			routingErrors = append(routingErrors, err.Error())
			continue
		}
		root, err := h.nodeNotespaceRoot(node)
		if err != nil {
			routingErrors = append(routingErrors, err.Error())
			continue
		}
		dirs := recordedContentDirs(root)
		maps.Copy(newWatches, computeNotespaceWatches(&sub, root, dirs))
	}

	// Containment is consent (W3.2): stamped notespaces inside a shared
	// notebook that neither an explicit subscription nor discovery covers.
	// Dark unless ContainmentAutoRegister — see sync_containment.go.
	for _, contained := range h.containedNotespaces(covered) {
		maps.Copy(newWatches, computeNotespaceWatches(&contained.sub, contained.root, recordedContentDirs(contained.root)))
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.routingErrors = slices.Clone(routingErrors)
	h.pathsMutex.Unlock()
	for _, message := range routingErrors {
		h.ulog.Error("sync routing configuration error").Field("error", message).Emit()
	}
	h.ensurePipelines()

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}
	return paths
}

// RoutingErrors exposes fail-loud routing diagnostics to daemon status/doctor
// adapters without coupling the watcher interface to sync-specific errors.
func (h *SyncHandler) RoutingErrors() []string {
	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()
	return slices.Clone(h.routingErrors)
}

// lookupWatch finds the subscription covering an absolute path (deepest
// watched directory wins) and the wire path relative to the notespace root.
func (h *SyncHandler) lookupWatch(absPath string) (*syncWatch, string) {
	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	var best *syncWatch
	var bestLen int
	for watched, w := range h.watchedPaths {
		if absPath == watched || strings.HasPrefix(absPath, watched+string(filepath.Separator)) {
			if len(watched) > bestLen {
				bestLen = len(watched)
				best = w
			}
		}
	}
	if best == nil {
		return nil, ""
	}
	rel, err := filepath.Rel(best.root, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, ""
	}
	return best, syncproto.NormalizePath(rel)
}

// MatchesEvent applies the default exclusion manifest (plus per-notespace
// extras) on top of subscription prefix matching.
func (h *SyncHandler) MatchesEvent(event fsnotify.Event) bool {
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}

	// Skip hidden files (but not .archive, which holds moved notes). The
	// exclusion manifest below handles the dot-directories that need
	// path-segment matching.
	baseName := filepath.Base(event.Name)
	if strings.HasPrefix(baseName, ".") && baseName != ".archive" {
		return false
	}

	watch, rel := h.lookupWatch(event.Name)
	if watch == nil {
		return false
	}
	return watch.space.Included(rel)
}

// HandleEvents debounces matched filesystem events per path. A newly-created
// directory is reconciled immediately: fsnotify is non-recursive, so files
// created inside it before the unified watcher installs the directory watch
// produce no individual events. Walking the completed subtree closes that
// registration gap; later writes are covered by the newly-installed watch.
func (h *SyncHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	for _, event := range events {
		if event.Op&fsnotify.Create != 0 {
			if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
				h.scheduleCreatedDirectory(event.Name)
				continue
			}
		}
		h.scheduleFlush(event.Name)
	}
	return nil
}

// scheduleCreatedDirectory captures files already present below a directory at
// the instant its non-recursive fsnotify watch is being installed. Paths are
// judged relative to the notespace root (not the new subtree) so per-notespace
// exclusion globs retain their normal semantics.
func (h *SyncHandler) scheduleCreatedDirectory(dir string) {
	watch, _ := h.lookupWatch(dir)
	if watch == nil {
		return
	}
	_ = filepath.WalkDir(dir, func(abs string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // transient create/delete races are covered by reconciliation
		}
		rel, relErr := filepath.Rel(watch.root, abs)
		if relErr != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		rel = syncproto.NormalizePath(rel)
		if entry.IsDir() {
			if abs != dir && !watch.space.Included(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if watch.space.Included(rel) {
			h.scheduleFlush(abs)
		}
		return nil
	})
}

// scheduleFlush arms (or re-arms) the per-path debounce timer: quietMs of
// silence flushes, but a path continuously written for maxWaitMs flushes
// anyway so the outbox never starves.
func (h *SyncHandler) scheduleFlush(absPath string) {
	// During destructive maintenance, capture changes immediately rather than
	// leaving a debounce timer that could make a final pending-state check lie.
	if h.maintenance.Load() {
		go h.flush(context.Background(), absPath)
		return
	}
	h.timersMu.Lock()
	defer h.timersMu.Unlock()

	if timer, exists := h.timers[absPath]; exists {
		timer.Stop()
	}

	first, seen := h.firstSeen[absPath]
	if !seen {
		first = time.Now()
		h.firstSeen[absPath] = first
	}

	delay := time.Duration(h.quietMs) * time.Millisecond
	if remaining := time.Duration(h.maxWaitMs)*time.Millisecond - time.Since(first); remaining < delay {
		delay = remaining
		if delay < 0 {
			delay = 0
		}
	}

	h.timers[absPath] = time.AfterFunc(delay, func() {
		h.timersMu.Lock()
		delete(h.timers, absPath)
		delete(h.firstSeen, absPath)
		h.timersMu.Unlock()
		h.flush(context.Background(), absPath)
	})
}

// flush records the current state of one path in sync.db: a created/updated
// outbox event when the content hash changed, a deleted event when the file
// is gone, and nothing at all when the hash is unchanged (hash-gating, the
// echo-suppression backstop).
func (h *SyncHandler) flush(ctx context.Context, absPath string) {
	watch, rel := h.lookupWatch(absPath)
	if watch == nil || watch.notespace == "" || !watch.space.Included(rel) {
		// A discovered watch is deliberately unroutable until its stamp has
		// registered. This prevents display names (or empty placeholders) from
		// ever becoming durable DB/wire keys and parks duplicate-id roots.
		return
	}
	// A watch exists, so a subscription exists — open sync.db if this is the
	// first capture since the handler woke up.
	db := h.ensureDB()
	if db == nil {
		return
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			h.recordDelete(ctx, watch.notespace, rel)
		}
		return
	}
	if fi.IsDir() {
		return
	}

	content, err := os.ReadFile(absPath) //nolint:gosec // G304: path from watched notebook tree
	if err != nil {
		h.ulog.Warn("Failed to read file for sync capture").Err(err).Field("path", absPath).Log(ctx)
		return
	}

	// Big-file policy: a per-notespace MaxFileSize skip is a quiet, user-
	// configured policy (not an error). The server-ceiling skip is the
	// surfaced one and happens at push time (DrainOutbox).
	if watch.space.Route(rel, int64(len(content))) == syncdb.RouteSkip {
		h.ulog.Debug("sync skip: file over notespace size cap").
			Field("notespace", watch.notespace).
			Field("path", rel).
			Field("size", len(content)).
			StructuredOnly().Log(ctx)
		return
	}

	// Record the change through the shared seeding helper: hash-gate → secret
	// quarantine (honoring the allow-list override) → UpsertDocument →
	// EnqueueOutbox. The anti-entropy reconcile's walkLocalTree calls the same
	// helper, so watch and reconcile can never disagree about the doc space or
	// the quarantine judgement.
	reason, err := syncdb.InsertAndEnqueue(db, watch.notespace, rel, content, fi.ModTime())
	if err != nil {
		h.ulog.Warn("Failed to record sync change").Err(err).Field("path", rel).Log(ctx)
		return
	}
	if reason != "" {
		// The watcher surfaces quarantine as a sync_conflict SSE update
		// (addendum #8: the watcher keeps its Store broadcast; the reconcile
		// only logs + counts).
		h.ulog.Warn("Sync quarantine: document matches secret heuristic, not queued").
			Field("notespace", watch.notespace).
			Field("path", rel).
			Field("heuristic", reason).
			Log(ctx)
		h.broadcastConflict(&store.SyncConflictPayload{
			Kind:        "secret_quarantine",
			NotespaceID: watch.notespace,
			Path:        rel,
			Detail:      reason,
		})
	}
}

// recordDelete enqueues a deleted event for a tracked document and drops it
// from the identity map. Untracked paths are ignored.
//
// The entry captures doc.LastSyncedVersion as its BaseVersion BEFORE the
// DeleteDocument below destroys the row (B7): the server's applyDelete OCC
// check rejects any base_version != head, so a delete pushed with the default
// 0 parks as a manufactured conflict forever. The row is still deleted
// immediately — keeping it alive until push-ack would collide with the
// sync_documents UNIQUE(notespace, path) constraint on delete-then-recreate.
func (h *SyncHandler) recordDelete(ctx context.Context, ws, rel string) {
	db := h.database()
	if db == nil {
		return
	}
	doc, err := db.GetDocumentByPath(ws, rel)
	if err != nil || doc == nil {
		return
	}
	if _, err := db.EnqueueOutbox(&syncdb.OutboxEntry{
		DocumentID:  doc.DocumentID,
		Notespace:   ws,
		EventType:   syncproto.EventDocumentDeleted,
		Path:        rel,
		BaseVersion: doc.LastSyncedVersion,
	}); err != nil {
		h.ulog.Warn("Failed to enqueue sync delete").Err(err).Field("path", rel).Log(ctx)
		return
	}
	if err := db.DeleteDocument(doc.DocumentID); err != nil {
		h.ulog.Warn("Failed to delete sync document").Err(err).Field("path", rel).Log(ctx)
	}
}

// HandleStoreUpdate consumes config reloads and nb's typed note events.
// Typed move events (PrevPath populated) are the rename-detection linchpin:
// they become first-class moved events instead of delete+create pairs.
func (h *SyncHandler) HandleStoreUpdate(update store.Update) {
	switch update.Type {
	case store.UpdateConfigReload:
		// The config/notebook delta W3.3 reacts to. Bumping BEFORE the reload
		// is deliberate: a reconcile that raced this update and is mid-pass now
		// observes a newer generation and abandons its remaining work instead
		// of installing pipelines against config that is already superseded.
		generation := h.bumpConfigGeneration()
		if newCfg, err := config.LoadDefault(); err == nil {
			h.setConfig(newCfg)
		}
		if syncCfg, err := config.LoadSyncConfig(); err == nil {
			h.syncCfgMu.Lock()
			h.syncCfg = syncCfg // nil drops all subscriptions → handler goes dormant
			h.syncCfgMu.Unlock()
			// A reload is very often the operator's ANSWER to a rejected
			// token — a new token, a new token_command, a different server.
			// Clear the backoff so the next tick tries it immediately instead
			// of sitting out the rest of a ten-minute window.
			h.clearAuthBackoff()
			// Wake immediately on the reload that first brings subscriptions
			// (a `grove join` on a machine that has never synced) rather than
			// waiting for the transport tick. Dormant on nil/empty config.
			h.ensureDB()
			// Count through the snapshot accessor, never off syncCfg
			// directly: LoadSyncConfig returns (nil, nil) when sync.toml is
			// absent — the documented "sync is disabled" state, and the
			// default on every machine that has never run `grove join` — so
			// this branch is entered with a nil config on the ordinary path.
			// A deref here panics the store dispatch goroutine, and nothing on
			// UnifiedWatcher's dispatch path recovers, so it takes groved down.
			h.ulog.Info("sync config reloaded").
				Field("generation", generation).
				Field("subscriptions", len(h.subscriptionsSnapshot())).
				StructuredOnly().Emit()
			// Reconcile now rather than at the next transport tick, so a
			// removed subscription stops its pipelines promptly (W3.3) and a
			// re-rooted notebook begins draining immediately. Off the store
			// dispatch goroutine because a reconcile registers over the
			// network; reconcileMu serializes it against the tick.
			go h.ensurePipelines()
		}
		// A config reload is the structural change the writer cannot observe
		// any other way: subscriptions and ecosystem declarations both live in
		// config, and both belong in this machine's presence note.
		h.kickRegistry()
	case store.UpdateNoteEvent:
		event, ok := update.Payload.(*models.NoteEvent)
		if !ok || event == nil {
			return
		}
		h.handleNoteEvent(context.Background(), event)
	}
}

// handleNoteEvent maps nb's typed move/rename/archive notifications onto
// document_moved outbox events.
func (h *SyncHandler) handleNoteEvent(ctx context.Context, event *models.NoteEvent) {
	switch event.Event {
	case models.NoteEventMoved, models.NoteEventRenamed, models.NoteEventArchived:
	default:
		return
	}
	if event.PrevPath == "" || event.Path == "" {
		return
	}

	prevWatch, prevRel := h.lookupWatch(event.PrevPath)
	newWatch, newRel := h.lookupWatch(event.Path)
	if prevWatch == nil || newWatch == nil {
		return
	}
	db := h.ensureDB()
	if db == nil {
		return
	}
	if !newWatch.space.Included(newRel) {
		// Moved out of sync scope — record as a delete of the old path.
		h.recordDelete(ctx, prevWatch.notespace, prevRel)
		return
	}

	doc, err := db.GetDocumentByPath(prevWatch.notespace, prevRel)
	if err != nil {
		h.ulog.Warn("Failed to query sync document for move").Err(err).Field("path", prevRel).Log(ctx)
		return
	}
	if doc == nil {
		// Old path untracked: the fsnotify create event on the new path
		// will capture it as document_created. Nothing to move.
		return
	}

	if err := db.MoveDocument(doc.DocumentID, newRel); err != nil {
		h.ulog.Warn("Failed to move sync document").Err(err).Field("path", newRel).Log(ctx)
		return
	}
	// Carry the moved file's mtime (fidelity metadata; zero when the stat
	// races the move) so replicas can restore it after their rename.
	var mtime time.Time
	if fi, err := os.Stat(event.Path); err == nil {
		mtime = fi.ModTime()
	}
	if _, err := db.EnqueueOutbox(&syncdb.OutboxEntry{
		DocumentID:  doc.DocumentID,
		Notespace:   newWatch.notespace,
		EventType:   syncproto.EventDocumentMoved,
		Path:        newRel,
		PrevPath:    prevRel,
		ContentHash: doc.ContentHash,
		Mtime:       mtime,
	}); err != nil {
		h.ulog.Warn("Failed to enqueue sync move").Err(err).Field("path", newRel).Log(ctx)
	}
}

func (h *SyncHandler) OnStart(ctx context.Context) {
	h.baseCtx = ctx
	go h.transportLoop(ctx)
	// The presence writer runs independently of the transport: a machine with
	// no reachable server should still keep its own note current locally, so
	// that whatever it was doing while offline replicates the moment the
	// server comes back.
	h.startRegistryWriter(ctx)
}

// transportLoop connects to the sync server (retrying quietly — sync stays
// passive and the outbox accumulates until the server is reachable), then
// keeps per-notespace pipelines in step with discovered notespaces.
//
// "Quietly" holds for a server that is merely down. It does NOT hold for a
// server that rejects our token: that failure never fixes itself, so it is
// reported once, loudly, with its remediation, and reconnects back off instead
// of hammering a handshake every tick (sync_auth.go).
func (h *SyncHandler) transportLoop(ctx context.Context) {
	interval := h.transportInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Failed dials repeat every tick for as long as the server is down, so
	// log only on state changes: the first failure, a changed error, and the
	// eventual connect (with how many dials it took).
	var lastDialErr string
	dialFailures := 0

	for {
		// Open sync.db if a config reload has brought subscriptions since the
		// last tick. Nil = still dormant: no client, no pipelines, nothing to
		// do until a subscription appears. This tick is what makes a
		// first-ever join take effect without a daemon restart.
		db := h.ensureDB()
		if db == nil {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}

		// A live pipeline was rejected: the cached client holds a dead token
		// and nothing downstream of it can recover while it survives. Drop it
		// and its pipelines so this loop rebuilds them below against a token
		// resolved fresh from config.
		if h.takeTransportReset() {
			h.resetTransport(ctx)
		}

		h.clientMu.RLock()
		ready := h.client != nil
		h.clientMu.RUnlock()

		if !ready {
			h.syncCfgMu.RLock()
			cfg := h.syncCfg
			h.syncCfgMu.RUnlock()
			if cfg != nil && h.authConnectDue(time.Now()) {
				// DeviceID is this machine's durable identity (ULID in
				// state), distinct from OriginID (per-sync.db install id that
				// dies with the DB). It rides CapabilitiesRequest/PushRequest;
				// the server is free to keep discarding it — rendezvous stays
				// dumb — but the wire now carries "which machine" so a wiped
				// sync.db is diagnosable as same machine, new origin.
				client, err := syncdb.NewClientFromConfig(ctx, cfg, machine.ID(), db.OriginID(), "", h.ulog)
				switch {
				case syncdb.IsAuthError(err):
					// The stale-token trap: a recreated server rejects every
					// token minted against its predecessor. Retrying cannot
					// fix it, so say so once, with the fix, and back off. This
					// is not a reachability failure — the server answered — so
					// it deliberately does not touch the dial-failure counter.
					h.noteAuthFailure(ctx, authSourceHandshake, err)
				case err != nil:
					dialFailures++
					if err.Error() != lastDialErr {
						lastDialErr = err.Error()
						h.ulog.Debug("sync server not reachable yet (suppressing repeats until the error changes)").Err(err).StructuredOnly().Log(ctx)
					}
				default:
					h.noteAuthSuccess(ctx)
					// A live pipeline that meets a rejected token must be able
					// to reach the transport owner; the handshake above ran
					// before any hook could exist, so install it now.
					client.SetAuthFailureHook(func(authErr error) {
						h.noteAuthFailure(ctx, authSourcePipeline, authErr)
					})
					// Epoch guard, BEFORE the client goes live: a recreated
					// server (fresh, empty DB) advertises a new epoch in the
					// handshake NewClientFromConfig just performed; comparing
					// it against the persisted last-seen epoch voids the local
					// synced state so the pipelines started below re-push the
					// full document set instead of enqueueing UPDATEs the
					// empty server rejects. (Mid-run recreates are caught by
					// the same check re-run in each anti-entropy pass.)
					if _, err := syncdb.CheckServerEpoch(ctx, db, client.ServerEpoch(), h.ulog); err != nil {
						h.ulog.Warn("sync server epoch check failed").Err(err).Log(ctx)
					}
					h.clientMu.Lock()
					h.client = client
					h.clientMu.Unlock()
					// The handshake just above IS this connection's first epoch
					// probe; start the re-probe clock here so the next tick
					// does not immediately repeat it.
					h.epochProbeMu.Lock()
					h.epochProbedAt = time.Now()
					h.epochProbeMu.Unlock()
					h.ulog.Info("sync server connected").Field("server", cfg.Server).Field("failed_dials", dialFailures).StructuredOnly().Log(ctx)
					lastDialErr = ""
					dialFailures = 0
				}
			}
		} else {
			h.probeServerEpoch(ctx, db)
			h.ensurePipelines()
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probeServerEpoch re-runs the capabilities handshake on a slow cadence and
// feeds its epoch to CheckServerEpoch, so a server recreated UNDER a running
// daemon is noticed in about a minute rather than at the next hourly
// anti-entropy tick.
//
// Why this is needed at all: the connect-time check only fires when a client
// is built, and transportLoop builds one exactly once. Nothing else on a quiet
// push-only machine talks to the server — no local edits means no pushes — so
// a wiped-and-restarted server was invisible until anti-entropy came round,
// and until then the machine's whole document set existed nowhere but locally.
// The per-document "unknown document" self-heal in the push pipeline only
// covers documents something happened to push.
//
// The cost is one small JSON round trip per minute per daemon; a reconnect
// storm is not: the probe reuses the live client and never rebuilds it.
func (h *SyncHandler) probeServerEpoch(ctx context.Context, db *syncdb.DB) {
	interval := h.epochProbeInterval
	if interval <= 0 {
		interval = defaultEpochProbeInterval
	}

	h.epochProbeMu.Lock()
	if time.Since(h.epochProbedAt) < interval {
		h.epochProbeMu.Unlock()
		return
	}
	h.epochProbedAt = time.Now()
	h.epochProbeMu.Unlock()

	h.clientMu.RLock()
	client := h.client
	h.clientMu.RUnlock()
	if client == nil {
		return
	}

	caps, err := client.Capabilities(ctx, "")
	if err != nil {
		// An auth failure here is the mid-run token rejection; the client's
		// hook has already reported it. Anything else is a transport blip the
		// next probe retries.
		return
	}
	reset, err := syncdb.CheckServerEpoch(ctx, db, caps.ServerEpoch, h.ulog)
	if err != nil {
		h.ulog.Warn("sync server epoch probe failed").Err(err).Log(ctx)
		return
	}
	if reset {
		// Every notespace was just voided with an empty outbox. Sweep them all
		// now — that sweep IS the re-push.
		h.KickAntiEntropy("")
	}
}

func registrationIntent(stamp *notespacepkg.NotespaceStamp) (string, error) {
	machineCfg, err := config.LoadMachineConfig()
	if err != nil {
		return "", err
	}
	if machineCfg != nil {
		if primaryID := machineCfg.Primaries[stamp.Subject]; primaryID != "" && primaryID != stamp.ID {
			return syncproto.RegistrationIntentCreateSibling, nil
		}
	}
	return syncproto.RegistrationIntentProposePrimary, nil
}

func (h *SyncHandler) registerRoot(ctx context.Context, client *syncdb.Client, stamp *notespacepkg.NotespaceStamp, root string) error {
	intent, err := registrationIntent(stamp)
	if err != nil {
		return fmt.Errorf("load primary registration intent: %w", err)
	}
	sum := sha256.Sum256([]byte(stamp.ID + "\x00" + stamp.Name + "\x00" + stamp.Subject + "\x00" + stamp.Kind + "\x00" + intent))
	resp, err := client.Register(ctx, syncproto.RegisterRequest{
		RequestIdentity: syncproto.RequestIdentity{IdempotencyKey: "daemon-" + hex.EncodeToString(sum[:])},
		Intent:          intent, Subject: stamp.Subject,
		NotespaceName: syncproto.NotespaceName(stamp.Name), Kind: stamp.Kind,
		ProposedNotespaceID: syncproto.NotespaceID(stamp.ID),
	})
	if err != nil {
		detail := fmt.Sprintf("registration failed for %s at %s: %v", stamp.ID, root, err)
		_, _ = syncdb.WriteRegistrationConflict(stamp.ID, detail)
		h.broadcastConflict(&store.SyncConflictPayload{Kind: syncdb.ConflictKindRegistration, NotespaceID: stamp.ID, NotespaceName: stamp.Name, Path: ".notespace.toml", Detail: detail})
		return err
	}
	if resp.NotespaceID.String() != stamp.ID {
		return fmt.Errorf("server registered %s as unexpected id %s", stamp.ID, resp.NotespaceID)
	}
	if db := h.database(); db != nil {
		if err := db.UpsertNotespaceBinding(syncdb.NotespaceBinding{ID: stamp.ID, Name: stamp.Name, Root: root, Subject: stamp.Subject, Kind: stamp.Kind}); err != nil {
			return err
		}
	}
	return nil
}

// BeginMaintenance flushes debounce, reconcile, and push state synchronously.
// New filesystem events are captured immediately until EndMaintenance. A
// non-zero pending/parked/diverged result is dirty, never a successful drain.
func (h *SyncHandler) BeginMaintenance(ctx context.Context) error {
	h.drainMu.Lock()
	defer h.drainMu.Unlock()
	h.maintenance.Store(true)

	h.timersMu.Lock()
	paths := make([]string, 0, len(h.timers))
	for path, timer := range h.timers {
		timer.Stop()
		paths = append(paths, path)
	}
	h.timers = make(map[string]*time.Timer)
	h.firstSeen = make(map[string]time.Time)
	h.timersMu.Unlock()
	for _, path := range paths {
		h.flush(ctx, path)
	}

	h.clientMu.RLock()
	client := h.client
	h.clientMu.RUnlock()
	if client == nil {
		return fmt.Errorf("sync server disconnected")
	}
	db := h.database()
	if db == nil {
		return fmt.Errorf("sync is not configured")
	}
	roots := make(map[string]string)
	h.pathsMutex.RLock()
	for _, w := range h.watchedPaths {
		if w.notespace != "" {
			roots[w.notespace] = w.root
		}
	}
	h.pathsMutex.RUnlock()
	pullRoots, err := h.configuredPullRoots()
	if err != nil {
		return err
	}
	for _, root := range pullRoots {
		stamp, loadErr := notespacepkg.LoadNotespace(root)
		if loadErr != nil {
			return loadErr
		}
		// Parked duplicates are excluded here for the same reason they have no
		// pipeline: a maintenance drain must not reconcile or push a root the
		// daemon has decided is not the one that syncs.
		if stamp != nil && !h.isParkedRoot(root) {
			roots[stamp.ID] = root
		}
	}

	h.pipelinesMu.Lock()
	passes := make(map[string]*syncdb.AntiEntropyPass, len(h.aePasses))
	for n, p := range h.aePasses {
		passes[n] = p
	}
	h.pipelinesMu.Unlock()
	for name, root := range roots {
		if ae := passes[name]; ae != nil {
			if err := ae.Run(ctx); err != nil {
				return fmt.Errorf("reconcile %s: %w", name, err)
			}
		}
		push := syncdb.NewPushPipeline(db, client, name, h.ulog, syncdb.PushConfig{})
		for {
			n, err := push.DrainOutbox(ctx, root)
			if err != nil {
				return fmt.Errorf("flush outbox %s: %w", name, err)
			}
			if n == 0 {
				break
			}
		}
	}
	return nil
}

func (h *SyncHandler) EndMaintenance() { h.maintenance.Store(false) }

// KickAntiEntropy triggers an immediate anti-entropy pass for the named
// notespace, or for every running notespace when notespace is empty. Wired
// into the HTTP server (SetSyncKick) so POST /api/sync/repush can convert its
// state reset into re-pushes without waiting for the hourly tick. A notespace
// whose transport has not started yet is silently skipped — its initial pass
// runs at pipeline start anyway.
func (h *SyncHandler) KickAntiEntropy(notespace string) {
	h.pipelinesMu.Lock()
	defer h.pipelinesMu.Unlock()
	for name, ae := range h.aePasses {
		if notespace == "" || name == notespace {
			ae.Kick()
		}
	}
}

// runWithRecovery wraps a sync pipeline goroutine with panic recovery.
// If the goroutine panics, it logs the panic and exits gracefully rather than
// crashing the daemon. This ensures server restarts or protocol edge cases
// don't kill the entire sync handler.
func (h *SyncHandler) runWithRecovery(ctx context.Context, notespace, pipelineType string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			h.ulog.Error("sync pipeline panic (recovered)").
				Field("notespace", notespace).
				Field("pipeline", pipelineType).
				Field("panic", fmt.Sprint(r)).
				Log(ctx)
		}
	}()
	if err := fn(); err != nil {
		// Normal error exit (context cancelled, etc)
		h.ulog.Debug("sync pipeline stopped").
			Field("notespace", notespace).
			Field("pipeline", pipelineType).
			Err(err).Log(ctx)
	}
}

// broadcastConflict publishes an UpdateSyncConflict store update so SSE
// subscribers can surface quarantine/conflict notifications.
func (h *SyncHandler) broadcastConflict(payload *store.SyncConflictPayload) {
	if h.store == nil {
		return
	}
	h.store.ApplyUpdate(store.Update{
		Type:    store.UpdateSyncConflict,
		Source:  "sync",
		Payload: payload,
	})
}
