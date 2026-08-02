// SyncHandler implements DomainHandler for the notebook sync protocol's
// Phase 0 client-side state: it watches subscribed notebook workspaces,
// hash-gates content changes, and records them in sync.db (identity map +
// outbox) for the Phase 1 push loop.
//
// Standing rules baked in here:
//
//   - Dark by default: the handler is constructed unconditionally by the global
//     daemon but stays DORMANT while no sync config with workspace
//     subscriptions exists — no watches, no sync.db, no transport. sync.db is
//     opened lazily by ensureDB the first time a subscription appears, which is
//     what lets a first-ever `grove join` take effect on the next config reload
//     instead of requiring a daemon restart (contract §1 Q7, boot gate).
//   - Notebook-read-only: this handler never writes to the notebook tree.
//     All writes go to sync.db under ~/.local/share/grove.
//   - Global daemon only: sync.db is owned by the global daemon, like
//     memory.db; scoped daemons proxy /api/sync/* to global.
package watcher

import (
	"context"
	"fmt"
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
	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/syncproto"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// Default debounce tuning: a change is flushed after quietMs of silence, but
// never later than maxWaitMs after the first event — rapid appends coalesce
// into one outbox row without starving under continuous writes.
const (
	defaultSyncQuietMs   = 2000
	defaultSyncMaxWaitMs = 15000
)

// syncWatch maps a watched directory to its sync workspace subscription.
type syncWatch struct {
	workspace string           // sync workspace name
	root      string           // workspace root dir; wire paths are relative to this
	space     *syncdb.DocSpace // exclusion + routing policy for this subscription
}

// SyncHandler implements DomainHandler for sync change capture.
type SyncHandler struct {
	store   *store.Store
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

	syncCfg   *config.SyncConfig
	syncCfgMu sync.RWMutex

	// Maps watched directory -> subscription info.
	watchedPaths map[string]*syncWatch
	pathsMutex   sync.RWMutex

	quietMs   int
	maxWaitMs int
	timers    map[string]*time.Timer
	firstSeen map[string]time.Time
	timersMu  sync.Mutex

	// Transport state: a shared server client plus per-workspace pipeline
	// cancel funcs, spawned lazily once a workspace root is known (from the
	// discovery-driven watch set, or from config alone for pull = true
	// subscriptions — see ensurePipelines). aePasses
	// keeps each workspace's anti-entropy reconciler addressable so the
	// manual /api/sync/repush endpoint can kick an immediate pass (guarded by
	// pipelinesMu, same lifecycle as pipelines).
	client      *syncdb.Client
	clientMu    sync.RWMutex
	pipelines   map[string]context.CancelFunc
	aePasses    map[string]*syncdb.AntiEntropyPass
	pipelinesMu sync.Mutex
	baseCtx     context.Context
	maintenance atomic.Bool
	drainMu     sync.Mutex
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
		pipelines:    make(map[string]context.CancelFunc),
		aePasses:     make(map[string]*syncdb.AntiEntropyPass),
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
		if !h.dbOpenFailed {
			h.dbOpenFailed = true
			h.ulog.Warn("Failed to open sync database, sync stays dormant").Err(err).Emit()
		}
		return nil
	}
	h.dbOpenFailed = false
	h.db.Store(db)
	if h.dbReady != nil {
		h.dbReady(db)
	}
	h.ulog.Info("Sync database opened").
		Field("origin_id", db.OriginID()).
		StructuredOnly().Emit()
	return db
}

func (h *SyncHandler) Name() string {
	return "sync"
}

// subscription returns the sync subscription for a workspace name, or nil.
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
// takes, read under one lock so the URL and the workspaces can never straddle
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

// syntheticNodeFor builds a WorkspaceNode for a subscribed workspace that code
// discovery did not yield (pure notes satellite: sync.toml subscribes to
// workspaces whose source trees don't exist under any grove path). A notebook
// workspace's location is fully determined by config — [notebooks.definitions]
// root_dir + path templates keyed only on the workspace NAME — so a node
// carrying just Name + a resolved NotebookName is enough for the locator.
//
// NotebookName resolution, in preference order:
//  1. a notebook definition whose resolved workspace root already exists on
//     disk (an existing replica or bootstrap-precreated dirs is the strongest
//     signal of which notebook the workspace belongs to);
//  2. the notebook referenced by a configured grove (what discovery's
//     config.ResolveNotebook would have produced for a child of that grove),
//     groves visited in sorted order for determinism;
//  3. empty — the locator then falls back to notebooks.rules.default and the
//     builtin default, exactly as it does for any node without a match.
func (h *SyncHandler) syntheticNodeFor(name string) *workspace.WorkspaceNode {
	cfg := h.cfg
	if cfg != nil && cfg.Notebooks != nil && len(cfg.Notebooks.Definitions) > 0 {
		for _, defName := range slices.Sorted(maps.Keys(cfg.Notebooks.Definitions)) {
			node := &workspace.WorkspaceNode{Name: name, NotebookName: defName}
			if root := h.nodeWorkspaceRoot(node); root != "" {
				if fi, err := os.Stat(root); err == nil && fi.IsDir() {
					return node
				}
			}
		}
	}
	if cfg != nil && cfg.Notebooks != nil && cfg.Notebooks.Definitions != nil {
		for _, groveName := range slices.Sorted(maps.Keys(cfg.Groves)) {
			nb := cfg.Groves[groveName].Notebook
			if nb == "" {
				continue
			}
			if _, ok := cfg.Notebooks.Definitions[nb]; !ok {
				continue
			}
			return &workspace.WorkspaceNode{Name: name, NotebookName: nb}
		}
	}
	return &workspace.WorkspaceNode{Name: name}
}

// nodeWorkspaceRoot resolves a node's workspace root purely from config, with
// no existence requirement (a pull pipeline must be able to materialize a
// replica into a tree that doesn't exist yet). It mirrors the root derivation
// computeWorkspaceWatches performs on stat-able trees: the "notes" content dir
// (parent of the inbox path) run through workspaceRootForDir. Returns "" when
// the locator fails or resolves a non-absolute path (a local-mode notebook on
// a synthetic node has no project path to anchor to — nothing to sync into).
func (h *SyncHandler) nodeWorkspaceRoot(node *workspace.WorkspaceNode) string {
	notesDir, err := h.locator.GetNotesDir(node, "inbox")
	if err != nil || !filepath.IsAbs(notesDir) {
		return ""
	}
	return workspaceRootForDir(filepath.Dir(notesDir))
}

// WorkspaceRoots resolves explicitly selected subscribed workspaces to their
// configured laptop notebook roots. It is used by the user-authorized incoming
// apply boundary; unlike configuredPullRoots it includes push-only laptop
// subscriptions and never invents a wildcard/default selection.
func (h *SyncHandler) WorkspaceRoots(names []string) (map[string]string, error) {
	roots := make(map[string]string, len(names))
	h.pathsMutex.RLock()
	for _, w := range h.watchedPaths {
		for _, name := range names {
			if w.workspace == name {
				roots[name] = w.root
			}
		}
	}
	h.pathsMutex.RUnlock()
	for _, name := range names {
		if h.subscription(name) == nil {
			return nil, fmt.Errorf("workspace %q is not a sync subscription", name)
		}
		if roots[name] == "" {
			roots[name] = h.nodeWorkspaceRoot(h.syntheticNodeFor(name))
		}
		if roots[name] == "" {
			return nil, fmt.Errorf("cannot resolve configured laptop root for workspace %q", name)
		}
	}
	return roots, nil
}

// configuredPullRoots derives workspace -> root for every pull = true
// subscription directly from sync.toml + notebook definitions, independent of
// code-workspace discovery. Pull targets are notebook workspaces whose paths
// are fully config-determined; requiring a .git under a grove path to
// materialize notes was accidental coupling (the empty-~/code satellite bug).
func (h *SyncHandler) configuredPullRoots() map[string]string {
	roots := make(map[string]string)
	for _, sub := range h.subscriptionsSnapshot() {
		if !sub.Pull || sub.Mode == config.SyncModeSearchOnly {
			continue
		}
		if root := h.nodeWorkspaceRoot(h.syntheticNodeFor(sub.Name)); root != "" {
			roots[sub.Name] = root
		}
	}
	return roots
}

// workspaceRootForDir derives the workspace root a content dir belongs to.
// Centralized notebook layouts follow <root>/workspaces/<name>/...; when the
// marker is absent the content dir's parent is the best available root.
func workspaceRootForDir(dir string) string {
	marker := string(filepath.Separator) + "workspaces" + string(filepath.Separator)
	if idx := strings.LastIndex(dir, marker); idx >= 0 {
		rest := dir[idx+len(marker):]
		if slash := strings.IndexByte(rest, filepath.Separator); slash > 0 {
			return dir[:idx+len(marker)+slash]
		}
		return dir
	}
	return filepath.Dir(dir)
}

// computeWorkspaceWatches enumerates every Included directory of one subscribed
// workspace as its own watch entry, recursively (Phase 2, the S1 fix). fsnotify
// is non-recursive, so every directory in the doc space needs an individual
// watch; DocSpace.WalkTree prunes excluded subtrees (.git/, .artifacts/, …)
// during the walk at O(included dirs) cost. syncWatch.root stays the workspace
// root in every entry — lookupWatch/flush compute wire paths against it, and
// that must not change with the walk root. Extracted from ComputeWatchPaths so
// the S1 reproduction can test it without the locator machinery.
func computeWorkspaceWatches(sub *config.SyncWorkspace, contentDirs []workspace.ContentDirectory) map[string]*syncWatch {
	watches := make(map[string]*syncWatch)
	if sub == nil {
		return watches
	}
	// Build the DocSpace once per subscription (per-workspace excludes + size
	// cap), shared across all of the workspace's watched dirs.
	space := syncdb.NewDocSpace(sub)

	// Derive the workspace root from the first stat-able content dir. The
	// "notes" entry from GetAllContentDirs is already the workspace root itself
	// (filepath.Dir of the inbox path), so workspaceRootForDir is a no-op there
	// and generalizes the plans/chats entries to the same root.
	var root string
	for _, dir := range contentDirs {
		if _, err := os.Stat(dir.Path); err != nil {
			continue
		}
		root = workspaceRootForDir(dir.Path)
		break
	}
	if root == "" {
		return watches
	}

	// Walk roots by mode: Full covers everything from the workspace root in one
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
			watches[abs] = &syncWatch{workspace: sub.Name, root: root, space: space}
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

// ComputeWatchPaths returns every Included directory of subscribed workspaces
// (recursively) so the non-recursive fsnotify backend covers the whole tree.
func (h *SyncHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*syncWatch)
	covered := make(map[string]bool) // subscription names covered by discovery

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}
		sub := h.subscription(node.Name)
		if sub == nil {
			continue
		}
		covered[node.Name] = true
		if sub.Mode == config.SyncModeSearchOnly {
			// search-only keeps no local replica — nothing to watch.
			continue
		}

		dirs, err := h.locator.GetAllContentDirs(node)
		if err != nil {
			continue
		}
		// GetAllContentDirs is now the root-resolution source, not the watch
		// list; the recursive walk (before pathsMutex — a 15k-dir walk must not
		// hold the lock) produces the actual watch set.
		maps.Copy(newWatches, computeWorkspaceWatches(sub, dirs))
	}

	// Subscriptions not covered by code discovery (pure notes satellite:
	// nothing under any grove path) are still config-locatable: resolve their
	// content dirs through a synthetic node so the workspace's own tree is
	// watched — local edits get captured and pushed just like on a machine
	// where discovery works. computeWorkspaceWatches only registers stat-able
	// dirs, so this is a no-op until the tree exists (for a pull replica, until
	// the pull pipeline materializes it; the periodic refresh picks it up
	// afterwards).
	//
	// This loop deliberately does NOT filter on sub.Pull. Push-only
	// subscriptions were the F8 residual: configuredPullRoots covers the
	// pull side only, so a push-only notebook workspace that discovery never
	// yields (no .git under any grove path — a bare notebook) got no watches
	// here and, because ensurePipelines derives its roots from this watch set,
	// no push pipeline either. It went silently dark in both directions. The
	// nodeWorkspaceRoot resolution below is the real gate: a subscription whose
	// root config cannot locate is still skipped. Pull remains strictly
	// opt-in — ensurePipelines still starts a pull loop only for sub.Pull, so
	// the legacy push-only invariant is untouched.
	for _, sub := range h.subscriptionsSnapshot() {
		if covered[sub.Name] || sub.Mode == config.SyncModeSearchOnly {
			continue
		}
		node := h.syntheticNodeFor(sub.Name)
		if h.nodeWorkspaceRoot(node) == "" {
			continue
		}
		dirs, err := h.locator.GetAllContentDirs(node)
		if err != nil {
			continue
		}
		maps.Copy(newWatches, computeWorkspaceWatches(&sub, dirs))
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.pathsMutex.Unlock()
	h.ensurePipelines()

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}
	return paths
}

// lookupWatch finds the subscription covering an absolute path (deepest
// watched directory wins) and the wire path relative to the workspace root.
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

// MatchesEvent applies the default exclusion manifest (plus per-workspace
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

// HandleEvents debounces matched filesystem events per path.
func (h *SyncHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	for _, event := range events {
		h.scheduleFlush(event.Name)
	}
	return nil
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
	if watch == nil || !watch.space.Included(rel) {
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
			h.recordDelete(ctx, watch.workspace, rel)
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

	// Big-file policy: a per-workspace MaxFileSize skip is a quiet, user-
	// configured policy (not an error). The server-ceiling skip is the
	// surfaced one and happens at push time (DrainOutbox).
	if watch.space.Route(rel, int64(len(content))) == syncdb.RouteSkip {
		h.ulog.Debug("sync skip: file over workspace size cap").
			Field("workspace", watch.workspace).
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
	reason, err := syncdb.InsertAndEnqueue(db, watch.workspace, rel, content, fi.ModTime())
	if err != nil {
		h.ulog.Warn("Failed to record sync change").Err(err).Field("path", rel).Log(ctx)
		return
	}
	if reason != "" {
		// The watcher surfaces quarantine as a sync_conflict SSE update
		// (addendum #8: the watcher keeps its Store broadcast; the reconcile
		// only logs + counts).
		h.ulog.Warn("Sync quarantine: document matches secret heuristic, not queued").
			Field("workspace", watch.workspace).
			Field("path", rel).
			Field("heuristic", reason).
			Log(ctx)
		h.broadcastConflict(&store.SyncConflictPayload{
			Kind:      "secret_quarantine",
			Workspace: watch.workspace,
			Path:      rel,
			Detail:    reason,
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
// sync_documents UNIQUE(workspace, path) constraint on delete-then-recreate.
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
		Workspace:   ws,
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
		if newCfg, err := config.LoadDefault(); err == nil {
			h.cfg = newCfg
			h.locator = workspace.NewNotebookLocator(newCfg)
		}
		if syncCfg, err := config.LoadSyncConfig(); err == nil {
			h.syncCfgMu.Lock()
			h.syncCfg = syncCfg // nil drops all subscriptions → handler goes dormant
			h.syncCfgMu.Unlock()
			// Wake immediately on the reload that first brings subscriptions
			// (a `grove join` on a machine that has never synced) rather than
			// waiting for the transport tick. Dormant on nil/empty config.
			h.ensureDB()
		}
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
		h.recordDelete(ctx, prevWatch.workspace, prevRel)
		return
	}

	doc, err := db.GetDocumentByPath(prevWatch.workspace, prevRel)
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
		Workspace:   newWatch.workspace,
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
}

// transportLoop connects to the sync server (retrying quietly — sync stays
// passive and the outbox accumulates until the server is reachable), then
// keeps per-workspace pipelines in step with discovered workspaces.
func (h *SyncHandler) transportLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
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

		h.clientMu.RLock()
		ready := h.client != nil
		h.clientMu.RUnlock()

		if !ready {
			h.syncCfgMu.RLock()
			cfg := h.syncCfg
			h.syncCfgMu.RUnlock()
			if cfg != nil {
				// DeviceID is this machine's durable identity (ULID in
				// state), distinct from OriginID (per-sync.db install id that
				// dies with the DB). It rides CapabilitiesRequest/PushRequest;
				// the server is free to keep discarding it — rendezvous stays
				// dumb — but the wire now carries "which machine" so a wiped
				// sync.db is diagnosable as same machine, new origin.
				client, err := syncdb.NewClientFromConfig(ctx, cfg, machine.ID(), db.OriginID(), "", h.ulog)
				if err != nil {
					dialFailures++
					if err.Error() != lastDialErr {
						lastDialErr = err.Error()
						h.ulog.Debug("sync server not reachable yet (suppressing repeats until the error changes)").Err(err).StructuredOnly().Log(ctx)
					}
				} else {
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
					h.ulog.Info("sync server connected").Field("server", cfg.Server).Field("failed_dials", dialFailures).StructuredOnly().Log(ctx)
					lastDialErr = ""
					dialFailures = 0
				}
			}
		} else {
			h.ensurePipelines()
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ensurePipelines spawns push/pull/anti-entropy loops for any subscribed
// workspace that has no running transport yet. Workspace roots come from two
// sources: the discovery-driven watch set (push-side real trees), and — for
// pull = true subscriptions — direct config resolution, so a pull replica
// spawns even when code-workspace discovery finds nothing.
// Idempotent; called on each transport tick and after watch-path updates.
func (h *SyncHandler) ensurePipelines() {
	h.clientMu.RLock()
	client := h.client
	h.clientMu.RUnlock()
	// A client only exists once the DB is open (transportLoop connects after
	// ensureDB), so db is non-nil here — the check keeps that ordering explicit
	// rather than assumed.
	db := h.database()
	if client == nil || db == nil || h.baseCtx == nil {
		return
	}

	// Unique workspace -> root from the current watch set.
	roots := make(map[string]string)
	h.pathsMutex.RLock()
	for _, w := range h.watchedPaths {
		roots[w.workspace] = w.root
	}
	h.pathsMutex.RUnlock()

	// Pull subscriptions are config-determined, not discovery-determined:
	// merge in roots derived from sync.toml + notebook definitions so pull
	// pipelines spawn even when the watch set is empty (pure notes satellite
	// with an empty ~/code, or a replica tree that doesn't exist yet — the
	// pull pipeline creates it). A discovery-derived root wins when both
	// exist; in a centralized notebook layout they resolve identically.
	for name, root := range h.configuredPullRoots() {
		if _, ok := roots[name]; !ok {
			roots[name] = root
		}
	}

	for name, root := range roots {
		h.pipelinesMu.Lock()
		_, running := h.pipelines[name]
		if running {
			h.pipelinesMu.Unlock()
			continue
		}
		pctx, cancel := context.WithCancel(h.baseCtx)
		h.pipelines[name] = cancel
		h.pipelinesMu.Unlock()

		sub := h.subscription(name)

		push := syncdb.NewPushPipeline(db, client, name, h.ulog, syncdb.PushConfig{})
		// Surface server-ceiling oversize skips as a sync_conflict SSE update
		// (convertToAPIUpdate already forwards UpdateSyncConflict). The quiet
		// per-workspace MaxFileSize skip stays in flush; this is the loud one.
		push.OnOversizeSkipped = func(ws, path string, size, limit int64) {
			h.broadcastConflict(&store.SyncConflictPayload{
				Kind:      "oversize_skipped",
				Workspace: ws,
				Path:      path,
				Detail:    fmt.Sprintf("%d bytes exceeds server blob ceiling %d", size, limit),
			})
		}
		// Surface a push-side divergence (S5): the merged server head was pushed
		// but the local file was left untouched, so it lags until the user runs
		// `nb sync adopt`. Same SSE surfacing as an oversize skip.
		push.OnDiverged = func(ws, path string) {
			h.broadcastConflict(&store.SyncConflictPayload{
				Kind:      "diverged",
				Workspace: ws,
				Path:      path,
				Detail:    "local file lags the merged server head; run `nb sync adopt` to take it",
			})
		}
		go h.runWithRecovery(pctx, name, "push", func() error {
			return push.RunPushLoop(pctx, root)
		})

		if sub != nil && sub.Pull {
			pull := syncdb.NewPullPipeline(sub, client, db, h.ulog)
			go h.runWithRecovery(pctx, name, "pull", func() error {
				return pull.RunPullLoop(pctx, root)
			})
		}

		// Build the reconcile with the same per-workspace DocSpace the watcher
		// uses (sub is already resolved above), so walk coverage and reconcile
		// coverage judge the doc space identically.
		ae := syncdb.NewAntiEntropyPass(db, client, name, root, syncdb.NewDocSpace(sub), h.ulog, syncdb.AntiEntropyConfig{})
		h.pipelinesMu.Lock()
		h.aePasses[name] = ae
		h.pipelinesMu.Unlock()
		go h.runWithRecovery(pctx, name, "anti-entropy", func() error {
			// One immediate pass (initial reconciliation), then the loop.
			if err := ae.Run(pctx); err != nil {
				return err
			}
			return ae.RunAntiEntropyLoop(pctx)
		})

		// Materialize the sync_state row immediately so /api/sync/status
		// reflects the subscription as soon as transport starts (readiness
		// probes key on this; rows otherwise appear only on first activity).
		if cur, err := db.GetWorkspaceCursor(name); err == nil {
			_ = db.UpdateWorkspaceCursor(name, cur)
		}

		h.ulog.Info("sync transport started").
			Field("workspace", name).
			Field("pull", sub != nil && sub.Pull).
			StructuredOnly().Log(pctx)
	}
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
		roots[w.workspace] = w.root
	}
	h.pathsMutex.RUnlock()
	for name, root := range h.configuredPullRoots() {
		if _, ok := roots[name]; !ok {
			roots[name] = root
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
// workspace, or for every running workspace when workspace is empty. Wired
// into the HTTP server (SetSyncKick) so POST /api/sync/repush can convert its
// state reset into re-pushes without waiting for the hourly tick. A workspace
// whose transport has not started yet is silently skipped — its initial pass
// runs at pipeline start anyway.
func (h *SyncHandler) KickAntiEntropy(workspace string) {
	h.pipelinesMu.Lock()
	defer h.pipelinesMu.Unlock()
	for name, ae := range h.aePasses {
		if workspace == "" || name == workspace {
			ae.Kick()
		}
	}
}

// runWithRecovery wraps a sync pipeline goroutine with panic recovery.
// If the goroutine panics, it logs the panic and exits gracefully rather than
// crashing the daemon. This ensures server restarts or protocol edge cases
// don't kill the entire sync handler.
func (h *SyncHandler) runWithRecovery(ctx context.Context, workspace, pipelineType string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			h.ulog.Error("sync pipeline panic (recovered)").
				Field("workspace", workspace).
				Field("pipeline", pipelineType).
				Field("panic", fmt.Sprint(r)).
				Log(ctx)
		}
	}()
	if err := fn(); err != nil {
		// Normal error exit (context cancelled, etc)
		h.ulog.Debug("sync pipeline stopped").
			Field("workspace", workspace).
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
