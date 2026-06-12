// SyncHandler implements DomainHandler for the notebook sync protocol's
// Phase 0 client-side state: it watches subscribed notebook workspaces,
// hash-gates content changes, and records them in sync.db (identity map +
// outbox) for the Phase 1 push loop.
//
// Standing rules baked in here:
//
//   - Dark by default: the handler is only constructed and registered when
//     a sync config with workspace subscriptions exists (see cmd/groved.go);
//     no config, no watcher, no sync.db.
//   - Notebook-read-only: this handler never writes to the notebook tree.
//     All writes go to sync.db under ~/.local/share/grove.
//   - Global daemon only: sync.db is owned by the global daemon, like
//     memory.db; scoped daemons proxy /api/sync/* to global.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
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

// defaultSyncExclusionDirs are directory names excluded from sync anywhere
// in a document's path. These are tool-local or editor-local state that must
// never replicate (the protocol's default exclusion manifest).
var defaultSyncExclusionDirs = map[string]bool{
	".obsidian":   true, // Obsidian vault-local state
	".stfolder":   true, // Syncthing marker
	".stversions": true, // Syncthing versioning
	".cx":         true, // cx-local context state
	".artifacts":  true, // generated briefings/aggregated contexts
}

// syncExcluded reports whether a slash-normalized workspace-relative path is
// excluded by the protocol's default exclusion manifest or by per-workspace
// extra exclusion globs (matched against both the full relative path and the
// basename).
func syncExcluded(relPath string, extra []string) bool {
	relPath = strings.Trim(path.Clean(relPath), "/")
	base := path.Base(relPath)

	// Suffix/basename rules.
	switch {
	case base == ".DS_Store":
		return true
	case strings.Contains(base, ".sync-conflict-"): // Syncthing conflict copies
		return true
	case strings.HasSuffix(base, ".conflict.md"): // grove sync conflict copies
		return true
	case strings.HasSuffix(base, ".lock"): // flow plan locks etc.
		return true
	}

	// Directory-segment rules, including ".grove/rules" as a pair.
	segs := strings.Split(relPath, "/")
	for i, seg := range segs {
		if defaultSyncExclusionDirs[seg] {
			return true
		}
		if seg == ".grove" && i+1 < len(segs) && segs[i+1] == "rules" {
			return true
		}
	}

	// Per-workspace extra exclusions from sync config.
	for _, pattern := range extra {
		if ok, _ := path.Match(pattern, relPath); ok {
			return true
		}
		if ok, _ := path.Match(pattern, base); ok {
			return true
		}
		if strings.HasSuffix(pattern, "/") && strings.HasPrefix(relPath, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
	}
	return false
}

// secretPatterns are the quarantine heuristics: a document matching any of
// these is never queued to the outbox. Conservative, high-signal patterns
// only — quarantine is the backstop behind nb's Phase 1 frontmatter token
// cleanup, not a general secret scanner.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"github fine-grained token", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)},
	{"github token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`)},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"aws access key id", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"slack token", regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`)},
	{"openai project key", regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{20,}`)},
	{"anthropic key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
}

// scanForSecrets returns the name of the first matching secret heuristic.
func scanForSecrets(content []byte) (string, bool) {
	for _, p := range secretPatterns {
		if p.re.Match(content) {
			return p.name, true
		}
	}
	return "", false
}

// syncWatch maps a watched directory to its sync workspace subscription.
type syncWatch struct {
	workspace string   // sync workspace name
	root      string   // workspace root dir; wire paths are relative to this
	excludes  []string // per-workspace extra exclusion globs
}

// SyncHandler implements DomainHandler for sync change capture.
type SyncHandler struct {
	store   *store.Store
	cfg     *config.Config
	locator *workspace.NotebookLocator
	db      *syncdb.DB
	ulog    *logging.UnifiedLogger

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
	// cancel funcs, spawned lazily once workspaces are discovered.
	client      *syncdb.Client
	clientMu    sync.RWMutex
	pipelines   map[string]context.CancelFunc
	pipelinesMu sync.Mutex
	baseCtx     context.Context
}

// NewSyncHandler creates a SyncHandler. Callers gate construction on sync
// configuration presence (the dark gate lives at the registration site).
// quietMs/maxWaitMs <= 0 select the defaults (2s quiet / 15s max latency).
func NewSyncHandler(st *store.Store, cfg *config.Config, syncCfg *config.SyncConfig, db *syncdb.DB, quietMs, maxWaitMs int) *SyncHandler {
	if quietMs <= 0 {
		quietMs = defaultSyncQuietMs
	}
	if maxWaitMs <= 0 {
		maxWaitMs = defaultSyncMaxWaitMs
	}

	return &SyncHandler{
		store:        st,
		cfg:          cfg,
		locator:      workspace.NewNotebookLocator(cfg),
		db:           db,
		ulog:         logging.NewUnifiedLogger("groved.watcher.sync"),
		syncCfg:      syncCfg,
		watchedPaths: make(map[string]*syncWatch),
		quietMs:      quietMs,
		maxWaitMs:    maxWaitMs,
		timers:       make(map[string]*time.Timer),
		firstSeen:    make(map[string]time.Time),
		pipelines:    make(map[string]context.CancelFunc),
	}
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

// ComputeWatchPaths returns the content directories of subscribed workspaces.
func (h *SyncHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*syncWatch)

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}
		sub := h.subscription(node.Name)
		if sub == nil || sub.Mode == config.SyncModeSearchOnly {
			// search-only keeps no local replica — nothing to watch.
			continue
		}

		dirs, err := h.locator.GetAllContentDirs(node)
		if err != nil {
			continue
		}
		for _, dir := range dirs {
			if sub.Mode == config.SyncModePlansOnly && dir.Type != "plans" {
				continue
			}
			if _, err := os.Stat(dir.Path); err != nil {
				continue
			}
			newWatches[dir.Path] = &syncWatch{
				workspace: sub.Name,
				root:      workspaceRootForDir(dir.Path),
				excludes:  sub.Excludes,
			}
		}
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
	return !syncExcluded(rel, watch.excludes)
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
	if watch == nil || syncExcluded(rel, watch.excludes) {
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

	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	doc, err := h.db.GetDocumentByPath(watch.workspace, rel)
	if err != nil {
		h.ulog.Warn("Failed to query sync document").Err(err).Field("path", rel).Log(ctx)
		return
	}

	// Hash-gate: unchanged content never re-enters the outbox.
	if doc != nil && doc.ContentHash == hash {
		return
	}

	// Secret quarantine: drop the event before anything reaches the outbox.
	if reason, found := scanForSecrets(content); found {
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
		return
	}

	eventType := syncproto.EventDocumentCreated
	documentID := uuid.New().String()
	if doc != nil {
		eventType = syncproto.EventDocumentUpdated
		documentID = doc.DocumentID
	}

	if err := h.db.UpsertDocument(&syncdb.Document{
		DocumentID:  documentID,
		Workspace:   watch.workspace,
		Path:        rel,
		ContentHash: hash,
	}); err != nil {
		h.ulog.Warn("Failed to upsert sync document").Err(err).Field("path", rel).Log(ctx)
		return
	}

	if _, err := h.db.EnqueueOutbox(&syncdb.OutboxEntry{
		DocumentID:  documentID,
		Workspace:   watch.workspace,
		EventType:   eventType,
		Path:        rel,
		ContentHash: hash,
	}); err != nil {
		h.ulog.Warn("Failed to enqueue sync outbox entry").Err(err).Field("path", rel).Log(ctx)
	}
}

// recordDelete enqueues a deleted event for a tracked document and drops it
// from the identity map. Untracked paths are ignored.
func (h *SyncHandler) recordDelete(ctx context.Context, ws, rel string) {
	doc, err := h.db.GetDocumentByPath(ws, rel)
	if err != nil || doc == nil {
		return
	}
	if _, err := h.db.EnqueueOutbox(&syncdb.OutboxEntry{
		DocumentID: doc.DocumentID,
		Workspace:  ws,
		EventType:  syncproto.EventDocumentDeleted,
		Path:       rel,
	}); err != nil {
		h.ulog.Warn("Failed to enqueue sync delete").Err(err).Field("path", rel).Log(ctx)
		return
	}
	if err := h.db.DeleteDocument(doc.DocumentID); err != nil {
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
	if syncExcluded(newRel, newWatch.excludes) {
		// Moved out of sync scope — record as a delete of the old path.
		h.recordDelete(ctx, prevWatch.workspace, prevRel)
		return
	}

	doc, err := h.db.GetDocumentByPath(prevWatch.workspace, prevRel)
	if err != nil {
		h.ulog.Warn("Failed to query sync document for move").Err(err).Field("path", prevRel).Log(ctx)
		return
	}
	if doc == nil {
		// Old path untracked: the fsnotify create event on the new path
		// will capture it as document_created. Nothing to move.
		return
	}

	if err := h.db.MoveDocument(doc.DocumentID, newRel); err != nil {
		h.ulog.Warn("Failed to move sync document").Err(err).Field("path", newRel).Log(ctx)
		return
	}
	if _, err := h.db.EnqueueOutbox(&syncdb.OutboxEntry{
		DocumentID:  doc.DocumentID,
		Workspace:   newWatch.workspace,
		EventType:   syncproto.EventDocumentMoved,
		Path:        newRel,
		PrevPath:    prevRel,
		ContentHash: doc.ContentHash,
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

	for {
		h.clientMu.RLock()
		ready := h.client != nil
		h.clientMu.RUnlock()

		if !ready {
			h.syncCfgMu.RLock()
			cfg := h.syncCfg
			h.syncCfgMu.RUnlock()
			if cfg != nil {
				client, err := syncdb.NewClientFromConfig(ctx, cfg, "", h.db.OriginID(), "", h.ulog)
				if err != nil {
					h.ulog.Debug("sync server not reachable yet").Err(err).StructuredOnly().Log(ctx)
				} else {
					h.clientMu.Lock()
					h.client = client
					h.clientMu.Unlock()
					h.ulog.Info("sync server connected").Field("server", cfg.Server).StructuredOnly().Log(ctx)
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
// workspace that has been discovered but has no running transport yet.
// Idempotent; called on each transport tick and after watch-path updates.
func (h *SyncHandler) ensurePipelines() {
	h.clientMu.RLock()
	client := h.client
	h.clientMu.RUnlock()
	if client == nil || h.baseCtx == nil {
		return
	}

	// Unique workspace -> root from the current watch set.
	roots := make(map[string]string)
	h.pathsMutex.RLock()
	for _, w := range h.watchedPaths {
		roots[w.workspace] = w.root
	}
	h.pathsMutex.RUnlock()

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

		push := syncdb.NewPushPipeline(h.db, client, name, h.ulog, syncdb.PushConfig{})
		go h.runWithRecovery(pctx, name, "push", func() error {
			return push.RunPushLoop(pctx, root)
		})

		if sub != nil && sub.Pull {
			pull := syncdb.NewPullPipeline(sub, client, h.db, h.ulog)
			go h.runWithRecovery(pctx, name, "pull", func() error {
				return pull.RunPullLoop(pctx, root)
			})
		}

		ae := syncdb.NewAntiEntropyPass(h.db, client, name, root, h.ulog, syncdb.AntiEntropyConfig{})
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
		if cur, err := h.db.GetWorkspaceCursor(name); err == nil {
			_ = h.db.UpdateWorkspaceCursor(name, cur)
		}

		h.ulog.Info("sync transport started").
			Field("workspace", name).
			Field("pull", sub != nil && sub.Pull).
			StructuredOnly().Log(pctx)
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
