package collector

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/claudetrust"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// WorkspaceCollector discovers workspaces and maintains the base workspace list.
//
// Discovery is deliberately global — it has no scope filter — so a scoped
// daemon still populates the store with every workspace on the filesystem.
// That lets clients like nav present a full worldview even when connected
// to a worktree-scoped daemon. The per-worktree work budget is enforced by
// the other collectors and watchers (git, plan, note, memory, skills),
// which each filter by scope against store.IsInScope.
type WorkspaceCollector struct {
	interval     time.Duration
	ulog         *logging.UnifiedLogger
	discoveryLog *logrus.Logger // Passed to workspace.GetProjects which requires *logrus.Logger
	refresh      chan chan struct{}
}

// NewWorkspaceCollector creates a new WorkspaceCollector with the specified interval.
// If interval is 0, defaults to 5 minutes.
func NewWorkspaceCollector(interval time.Duration) *WorkspaceCollector {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	discoveryLog := logrus.New()
	discoveryLog.SetLevel(logrus.WarnLevel)
	return &WorkspaceCollector{
		interval:     interval,
		ulog:         logging.NewUnifiedLogger("groved.collector.workspace"),
		discoveryLog: discoveryLog,
		refresh:      make(chan chan struct{}),
	}
}

// Refresh triggers an immediate workspace scan and blocks until it completes.
func (c *WorkspaceCollector) Refresh(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case c.refresh <- reply:
		select {
		case <-reply:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Name returns the collector's name.
func (c *WorkspaceCollector) Name() string { return "workspace" }

// Run starts the workspace discovery loop.
func (c *WorkspaceCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// lastSig fingerprints the discovered node SET (path + kind) from the previous
	// emit. Discovery runs every tick — as fast as every 10s while a TUI is
	// focused — but the set is unchanged the vast majority of ticks. Emitting
	// UpdateWorkspaces regardless made every idle tick fan out into a full
	// ecosystem-wide claude-settings reseed, a treemux SSE refetch, and a git
	// watcher watch-set recompute. Suppress the redundant emit when the set is
	// identical; per-workspace enrichment (git status, etc.) still flows via
	// UpdateWorkspacesDelta, and add/remove/kind changes still emit here.
	lastSig := ""

	scan := func(force bool) {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d > 1*time.Second {
				c.ulog.Debug("Slow workspace discovery detected").Field("duration", d).Log(ctx)
			}
		}()

		// Reconcile the worktree registry before discovery so stale entries
		// are pruned and newly-created XDG dirs are adopted in one pass.
		worktreeregistry.Reconcile(paths.WorktreesDir()) //nolint:errcheck // best-effort

		// Garbage-collect Claude folder-trust keys for worktrees that no
		// longer exist under WorktreesDir. SeedTrust is write-only, so
		// without this sweep every finished worktree leaks a dead
		// ~/.claude.json projects[] entry. The daemon runs unsandboxed, so
		// this privileged write succeeds.
		claudetrust.PruneOrphanTrust(paths.WorktreesDir()) //nolint:errcheck // best-effort

		// Discover base nodes globally — a scoped daemon still serves the
		// full workspace list so nav can show the whole worldview. Heavy
		// per-workspace enrichment is scope-filtered in the other collectors.
		nodes, err := workspace.GetProjects(c.discoveryLog)
		if err != nil {
			return
		}

		// Fingerprint the discovered set. A stable signature across ticks means
		// no worktree was added/removed/retyped, so re-broadcasting would only
		// trigger redundant downstream work.
		sigs := make([]string, 0, len(nodes))
		for _, node := range nodes {
			if node == nil {
				continue
			}
			sigs = append(sigs, node.Path+"\x00"+string(node.Kind))
		}
		sort.Strings(sigs)
		sig := strings.Join(sigs, "\n")
		if !force && sig == lastSig {
			return
		}
		lastSig = sig

		// 2. Convert to EnrichedWorkspace (initially empty enrichment)
		// Preserve existing enrichment data if available in the store
		currentState := st.Get() // Read lock
		enrichedMap := make(map[string]*models.EnrichedWorkspace)

		for _, node := range nodes {
			ew := &models.EnrichedWorkspace{WorkspaceNode: node}

			// Preserve existing data if we have it
			if existing, ok := currentState.Workspaces[node.Path]; ok {
				ew.GitStatus = existing.GitStatus
				// Preserve the daemon-computed per-file git cache. Without this,
				// every ~10s workspace rescan rebuilt the map and dropped
				// ChangedFiles/BlobHashes, so any client full-pull (/api/workspaces)
				// returned coarse-only data and the git-viewer cache-missed →
				// live git in the TUI. The git collector/watcher refresh these via
				// deltas; the rescan must not clobber them.
				ew.ChangedFiles = existing.ChangedFiles
				ew.BlobHashes = existing.BlobHashes
				ew.ChangedFilesComputed = existing.ChangedFilesComputed
				ew.NoteCounts = existing.NoteCounts
				ew.PlanStats = existing.PlanStats
				ew.ReleaseInfo = existing.ReleaseInfo
				ew.ActiveBinary = existing.ActiveBinary
				ew.CxStats = existing.CxStats
				ew.GitRemoteURL = existing.GitRemoteURL
			}
			enrichedMap[node.Path] = ew
		}

		updates <- store.Update{
			Type:    store.UpdateWorkspaces,
			Source:  "workspace",
			Payload: enrichedMap,
		}
	}

	// Initial scan — always emit to seed the store.
	scan(true)

	currentInterval := c.interval

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan(false)

			// Dynamically adjust interval based on active client focus.
			// When the TUI is open (focus set), scan more frequently to catch
			// worktree additions/removals quickly.
			focus := st.GetFocus()
			newInterval := c.interval
			if len(focus) > 0 {
				newInterval = 10 * time.Second
			}

			if newInterval != currentInterval {
				currentInterval = newInterval
				ticker.Reset(currentInterval)
			}
		case replyCh := <-c.refresh:
			// Explicit on-demand refresh always emits so callers that just
			// mutated the worktree set see it propagate even if discovery
			// races the filesystem change.
			scan(true)
			close(replyCh)
		}
	}
}
