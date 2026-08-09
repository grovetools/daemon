package watcher

import (
	"bufio"
	"context"
	"encoding/binary"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	coregit "github.com/grovetools/core/git"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/pkg/worktreeregistry"
	corestate "github.com/grovetools/core/state"
	"github.com/grovetools/core/util/frontmatter"
	"github.com/grovetools/daemon/internal/daemon/jobattr"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
	"github.com/grovetools/daemon/internal/enrichment"
	"github.com/grovetools/flow/pkg/orchestration"
)

// FlowHandler implements DomainHandler for watching plan directories.
// When plan files change, it triggers an immediate plan stats re-scan
// rather than waiting for the PlanCollector's polling interval.
type FlowHandler struct {
	store   *store.Store
	cfg     *config.Config
	locator *workspace.NotebookLocator
	ulog    *logging.UnifiedLogger

	// Maps watched path -> the workspace node that OWNS that path's plans.
	// Many workspaces of one ecosystem resolve to a single centralized plans
	// directory, so the value is chosen by sortPlanOwners rather than by
	// whichever workspace happened to be registered last (see
	// ComputeWatchPaths). watchedNodes is the node set the same computation
	// saw, indexed for frontmatter `worktree:` resolution; it is replaced
	// atomically with watchedPaths so a lookup can never mix generations.
	watchedPaths map[string]*workspace.WorkspaceNode
	watchedNodes *jobattr.Index
	pathsMutex   sync.RWMutex

	// Debounce timer + accumulated scope for the next refresh (guarded by
	// refreshMu). pendingAll forces a full disk rescan; pendingDirs holds the
	// plans directories fsnotify implicated since the last run. A trigger with
	// neither set is an overlay-only pass: it re-projects cached rows through
	// the current bindings/git/session state without touching plan files.
	refreshTimer    *time.Timer
	refreshDeadline time.Time
	refreshMu       sync.Mutex
	pendingAll      bool
	pendingDirs     map[string]struct{}
	refreshRunMu    sync.Mutex
	debounceMs      int

	// Per-plansDir scan results, so event-scoped refreshes only re-read the
	// affected directory instead of the whole portfolio. Guarded by
	// refreshRunMu (only touched inside runRefresh).
	dirCache map[string]*dirScanResult

	// Aggregated-PlanStats pass bookkeeping. The stats leg re-reads every
	// plans directory on disk, so it executes on its own goroutine with
	// trailing-run coalescing — never under refreshRunMu, where it would
	// delay synchronous lifecycle publishes behind that disk work.
	statsMu      sync.Mutex
	statsRunning bool
	statsQueued  bool
	// statsQueuedForce remembers that the coalesced debt came from a
	// correctness path (cold start, reconciliation ticker), so the trailing
	// run inherits that path's exemption from the rate floor instead of
	// being throttled as if it were churn.
	statsQueuedForce bool
	// statsMinInterval is the rate floor: the minimum wall-clock spacing
	// between the STARTS of two stats passes. Zero disables it, restoring
	// "every lifecycle event recounts the portfolio". statsLastRun is stamped
	// when a pass starts (rate of starts is what the floor bounds), and
	// statsFloorTimer carries the single trailing run owed while the floor
	// holds — the reason a floor can never lose an update.
	statsMinInterval time.Duration
	statsLastRun     time.Time
	statsFloorTimer  *time.Timer
	// clock and statsPass are the seams the floor's tests drive: a fake clock
	// makes the floor observable without sleeping through it, and an
	// injectable pass counts runs without a populated store behind them.
	clock     func() time.Time
	statsPass func(seq uint64)
	// jobMetaHash memoizes each job file's last-seen frontmatter, keyed by
	// resolved path. It is what lets a chat transcript append — a WRITE on a
	// file the index genuinely reads — be recognized as inert.
	jobMetaMu   sync.Mutex
	jobMetaHash map[string]uint64

	// statsSeq fences the async stats pass against index publishes that raced
	// it: a pass whose seq moved read disk state older than the last publish,
	// so its answer is discarded and recomputed rather than allowed to clobber
	// fresher lifecycle state.
	//
	// It counts STATS-RELEVANT publishes, not all of them, and that difference
	// is the whole point. Bumping on every publish made the fence a treadmill:
	// `runRefresh` also fires for overlay-only re-projections (a git delta
	// landing in the store re-runs the row projection without touching a plan
	// file), and at 600 workspaces a pass takes seconds, so a publish almost
	// always raced it and the pass's own duration guaranteed its rerun. What
	// the stats reader can observe is the plan set and each plan's lifecycle
	// and job counts (see statsRelevantDigest); only a publish that moved one
	// of those advances the seq.
	//
	// Under-counting is safe by construction and over-counting is merely
	// wasteful. Any publish that re-read a plans directory ALSO calls
	// kickPlanStats, so a running pass is already owed a trailing run through
	// statsQueued; a digest that failed to notice a change costs at most one
	// briefly-stale emission that the queued rerun then corrects. The seq's
	// remaining job is the publishes that kick nothing.
	statsSeq atomic.Uint64
	// lastStatsInput is the digest statsSeq was last bumped for. Guarded by
	// refreshRunMu — only runRefresh reads or writes it.
	lastStatsInput uint64
}

// defaultPlanStatsMinInterval is the rate floor applied when the daemon config
// says nothing. Plan rows in the TUI do NOT ride it — they are published by
// the synchronous index refresh on its 2s debounce — so what this bounds is
// how often aggregated per-workspace job counts can move. Half a minute is
// well inside the "nobody notices" band for a count, and it is the difference
// between one portfolio-wide recount per lifecycle event and one per minute.
const defaultPlanStatsMinInterval = 30 * time.Second

// dirScanResult is the disk-derived portion of one plans directory's rows.
// Selected/RunningSessions/bindings/git are overlays recomputed on every
// publish from live store state, so cached entries never pin them stale.
type dirScanResult struct {
	plans     []*orchestration.Plan
	summaries []models.PlanSummary
	// statsDigest folds the subset of these rows the aggregated-PlanStats
	// reader can observe. Computed at scan time because that is where the
	// disk-derived state is: every later stage layers overlays on top.
	statsDigest uint64
}

// NewFlowHandler creates a new FlowHandler instance.
func NewFlowHandler(st *store.Store, cfg *config.Config, debounceMs int) *FlowHandler {
	if debounceMs <= 0 {
		debounceMs = 2000
	}

	h := &FlowHandler{
		store:            st,
		cfg:              cfg,
		locator:          workspace.NewNotebookLocator(cfg),
		ulog:             logging.NewUnifiedLogger("groved.watcher.flow"),
		watchedPaths:     make(map[string]*workspace.WorkspaceNode),
		debounceMs:       debounceMs,
		statsMinInterval: defaultPlanStatsMinInterval,
		clock:            time.Now,
	}
	h.statsPass = h.refreshPlanStats
	return h
}

// SetPlanStatsMinInterval overrides the aggregated-PlanStats rate floor.
// A non-positive duration disables the floor, which is the behaviour that
// existed before it: every lifecycle event recounts the whole portfolio.
func (h *FlowHandler) SetPlanStatsMinInterval(d time.Duration) *FlowHandler {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	h.statsMinInterval = d
	return h
}

func (h *FlowHandler) Name() string {
	return "flow"
}

// ComputeWatchPaths returns plan directories for all workspaces.
func (h *FlowHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*workspace.WorkspaceNode)

	nodes := make([]*workspace.WorkspaceNode, 0, len(workspaces))
	for _, ew := range workspaces {
		if ew != nil && ew.WorkspaceNode != nil {
			nodes = append(nodes, ew.WorkspaceNode)
		}
	}
	// Registration order decides who owns a plans directory, and the caller's
	// order is not an order at all: store.GetWorkspaces() materializes its
	// slice by ranging a map, so it is a fresh permutation on every refresh.
	// Every member repo and worktree of one ecosystem resolves to the SAME
	// centralized plans directory (NotebookLocator.getContextNodeForPath maps
	// them all onto the origin ecosystem's notebook workspace), so an
	// unordered registration attributed that directory — and therefore every
	// job discovered under it — to whichever workspace happened to land last.
	// That is how a grovetools plan job was persisted against a `tuimux`
	// checkout inside an unrelated worktree container.
	sortPlanOwners(nodes)

	claimed := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		plansDir, err := h.locator.GetPlansDir(node)
		if err != nil || plansDir == "" {
			continue
		}

		// Centralized notebook workspaces can be reached through aliases. Register
		// the resolved root because fsnotify reports target-path events on Darwin.
		resolved := resolveFlowWatchPath(plansDir)
		// First claim wins, and sortPlanOwners has already put the workspace
		// that should win first. Claiming explicitly (rather than relying on
		// addWatchRecursive's overwrite) keeps the rule readable and stops a
		// later node from silently re-owning the directory's subpaths.
		if _, dup := claimed[resolved]; dup {
			continue
		}
		claimed[resolved] = struct{}{}
		addWatchRecursive(resolved, node, newWatches)
	}

	h.pathsMutex.Lock()
	previous := h.watchedPaths
	h.watchedPaths = newWatches
	h.watchedNodes = jobattr.NewIndex(nodes)
	h.pathsMutex.Unlock()

	// Watch-registration boundary: a live daemon log must be able to prove
	// which plan directories the flow handler asked to cover. The set only
	// changes when plans/workspaces appear or disappear, so info is quiet.
	var added, removed []string
	for p := range newWatches {
		if _, ok := previous[p]; !ok {
			added = append(added, p)
		}
	}
	for p := range previous {
		if _, ok := newWatches[p]; !ok {
			removed = append(removed, p)
		}
	}
	if len(added) > 0 || len(removed) > 0 {
		sort.Strings(added)
		sort.Strings(removed)
		h.ulog.Info("Flow watch set changed").
			Field("watched", len(newWatches)).
			Field("added", strings.Join(added, ",")).
			Field("removed", strings.Join(removed, ",")).
			Log(context.Background())
	}

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}
	return paths
}

func (h *FlowHandler) MatchesEvent(event fsnotify.Event) bool {
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}

	eventPath := resolveFlowWatchPath(event.Name)
	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watchedPath := range h.watchedPaths {
		if eventPath == watchedPath || strings.HasPrefix(eventPath, watchedPath+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolveFlowWatchPath returns the stable filesystem spelling fsnotify uses.
//
// EvalSymlinks needs the path to exist, and the events that matter most to the
// index are precisely the ones whose leaf does not: a removed job file, the
// old name of a rename. Returning the unresolved spelling for those would fail
// every comparison against a watch set built from resolved paths whenever the
// notebook is reached through a symlink, so the deepest ancestor that DOES
// exist is resolved and the remainder re-attached.
func resolveFlowWatchPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolveFlowWatchPath(parent), filepath.Base(path))
}

// HandleEvents triggers a debounced plan stats refresh when plan files change.
// It also parses modified/created .md files to instantly discover new jobs.
//
// Two filters stand in front of the refresh, because a plans directory under a
// running agent is written into continuously and almost none of it can move
// anything this handler publishes:
//
//   - by path (classifyPlanEvent): only the files the index and the stats
//     recount actually open;
//   - by content (absorbJobFile): a job `.md` whose frontmatter came back
//     byte-identical. This is the loud one — an agent appends its chat
//     transcript to the very file whose frontmatter the index reads, and both
//     readers stop at the closing fence, so the append changes nothing.
//
// Only when something survives both is the refresh armed.
func (h *FlowHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	batch := h.classifyEvents(events)
	if len(batch.kept) == 0 {
		telemetry.RecordPlanStatsEvents(0, batch.suppressed)
		h.logSuppressed(ctx, batch, 0)
		return nil
	}
	h.ulog.Debug("Plan file changes detected").Field("count", len(batch.kept)).Log(ctx)

	var discoveredJobs []*models.JobInfo
	lifecycleChanged := false
	affectedDirs := make(map[string]struct{})
	inert := 0

	for _, classified := range batch.kept {
		event := classified.event
		relevant := true

		if classified.class == planEventConfig &&
			(event.Op&fsnotify.Write != 0 || event.Op&fsnotify.Create != 0 || event.Op&fsnotify.Rename != 0 || event.Op&fsnotify.Remove != 0) {
			lifecycleChanged = true
			// Event-match boundary: plan-config mutations are the hold/unhold
			// delivery proof and are rare, so log each one at info.
			h.ulog.Info("Plan lifecycle event received").
				Field("path", event.Name).
				Field("op", event.Op.String()).
				Log(ctx)
		}
		// A directory created directly under a plans dir is a new plan being
		// born (`flow plan init`, plan copy). Its config write happens before
		// fsnotify can watch the new directory, so this bare dir-create is the
		// ONLY signal we get — treat it as a lifecycle edge rather than
		// letting the new row wait out the enrichment debounce.
		if classified.class == planEventMembership && event.Op&fsnotify.Create != 0 &&
			!lifecycleChanged && isPlanDirCreate(classified.path) {
			lifecycleChanged = true
			h.ulog.Info("Plan directory created").
				Field("path", event.Name).
				Log(ctx)
		}
		// A job-file write only matters if its frontmatter moved. Removals and
		// renames are not content edits and always matter: they change the
		// plan's job set and its directory mtime.
		if classified.class == planEventJob && event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
			relevant = h.absorbJobFile(classified, &discoveredJobs)
		}

		if !relevant {
			inert++
			continue
		}
		affectedDirs[classified.plansDir] = struct{}{}
	}

	telemetry.RecordPlanStatsEvents(len(batch.kept)-inert, batch.suppressed+inert)
	h.logSuppressed(ctx, batch, inert)

	if len(discoveredJobs) > 0 {
		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdateJobsDiscovered,
			Source:  "flow_watcher",
			Payload: discoveredJobs,
		})
	}

	if lifecycleChanged {
		// Plan lifecycle edges are state transitions, not eventually-consistent
		// enrichment. A debounced rescan can collapse hold→unhold into one live
		// snapshot, so publish each observed config mutation synchronously.
		h.triggerLifecycleRefresh(affectedDirs)
	} else if len(affectedDirs) > 0 {
		h.scheduleRefresh(affectedDirs, false, time.Duration(h.debounceMs)*time.Millisecond)
	}
	return nil
}

func (h *FlowHandler) logSuppressed(ctx context.Context, batch classifiedPlanBatch, inert int) {
	if batch.suppressed+inert == 0 {
		return
	}
	h.ulog.Debug("Plan events suppressed as non-lifecycle").
		Field("unreadable_path", batch.suppressed).
		Field("unchanged_frontmatter", inert).
		Field("kept", len(batch.kept)-inert).
		Field("sample", batch.suppressedSample).
		Log(ctx)
}

// absorbJobFile reads one job file's frontmatter and reports whether it moved
// since this daemon last saw the file. A changed (or first-seen) frontmatter
// also publishes the job, which is how a new job reaches clients without
// waiting for the JobCollector's sweep.
func (h *FlowHandler) absorbJobFile(classified classifiedPlanEvent, jobs *[]*models.JobInfo) bool {
	raw, meta, parsed := jobFrontmatter(classified.event.Name)
	if !h.jobFrontmatterChanged(classified.path, raw) {
		return false
	}

	base := filepath.Base(classified.event.Name)
	if !parsed || meta.ID == "" || base == "spec.md" || base == "README.md" {
		// Nothing to publish, but the file is new to us (or genuinely
		// changed), so the refresh still runs.
		return true
	}

	submittedAt := meta.StartedAt
	if submittedAt.IsZero() {
		submittedAt = meta.UpdatedAt
	}
	if submittedAt.IsZero() {
		submittedAt = time.Now()
	}

	planDir := filepath.Dir(classified.event.Name)
	job := &models.JobInfo{
		ID:          meta.ID,
		Title:       meta.Title,
		Type:        models.JobType(meta.Type),
		Status:      meta.Status,
		PlanDir:     planDir,
		PlanName:    filepath.Base(planDir),
		JobFile:     base,
		SubmittedAt: submittedAt,
	}
	if len(meta.Channels) > 0 {
		job.Channels = meta.Channels
	}

	// Attribute the job to a workspace. This is the same computation the
	// JobCollector performs on its periodic sweep, and both publish under the
	// same store key with last-write-wins, so it MUST go through the shared
	// jobattr rule: any divergence makes a job's recorded workspace flip
	// depending on which producer ran most recently.
	//
	// The plans directory only identifies the plan's OWNER workspace (one
	// ecosystem, many members). The job's own frontmatter `worktree:` is the
	// higher authority for which checkout the job actually runs in, resolved
	// within the owner's ecosystem and deliberately degrading to the owner —
	// never to a stranger — when the name is missing, unknown, or ambiguous.
	h.pathsMutex.RLock()
	owner := h.ownerForPath(classified.path)
	index := h.watchedNodes
	h.pathsMutex.RUnlock()
	if owner != nil {
		job.WorkDir, job.Repo, job.Branch, _ = jobattr.JobWorkspace(
			index, owner, meta.Worktree, owner.Path, owner.Name)
	}

	*jobs = append(*jobs, job)
	return true
}

// maxFrontmatterBytes bounds what jobFrontmatter will treat as a frontmatter
// block. A job file's frontmatter is a few hundred bytes; anything past this
// is a file whose opening fence is not really frontmatter, and reading it
// would defeat the point of stopping at the closing fence.
const maxFrontmatterBytes = 64 << 10

// maxJobMetaMemo bounds the frontmatter memo. A portfolio has thousands of job
// files, not millions, but a daemon runs for weeks and job files come and go,
// so the table is dropped wholesale rather than grown without bound; the cost
// of a reset is one extra refresh per job file next written.
const maxJobMetaMemo = 20000

// jobFrontmatter returns one job file's raw frontmatter block and its parsed
// view. It stops at the closing fence, so a multi-megabyte chat transcript
// costs one small read — which is the entire reason the comparison is
// affordable on every write event.
//
// The RAW block is what gets compared, not the parsed DocMetadata: flow's own
// LoadJobMeta reads fields this parser does not, and a change to one of those
// (a `depends_on` edit, say) must not look unchanged here.
func jobFrontmatter(path string) (raw string, meta frontmatter.DocMetadata, ok bool) {
	file, err := os.Open(path) //nolint:gosec // G304: path from the watched plans tree
	if err != nil {
		return "", meta, false
	}
	defer func() { _ = file.Close() }()

	var block strings.Builder
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 8<<10), maxFrontmatterBytes)
	inside := false
	closed := false
	preamble := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			if !inside {
				inside = true
				continue
			}
			closed = true
			break
		}
		if !inside {
			// frontmatter.Parse gives up when no opening fence appears in the
			// first few lines; matching that keeps the two in agreement about
			// what counts as a job file.
			if preamble++; preamble > 5 {
				return "", meta, false
			}
			continue
		}
		block.WriteString(line)
		block.WriteByte('\n')
		if block.Len() > maxFrontmatterBytes {
			return "", meta, false
		}
	}
	if !closed {
		// A half-written file: no closing fence yet. Reporting "no
		// frontmatter" would memoize an empty block, and the completing write
		// then reads as a change — which is exactly the desired outcome.
		return "", meta, false
	}
	raw = block.String()
	meta, err = frontmatter.ParseString("---\n" + raw + "---\n")
	if err != nil {
		return raw, meta, false
	}
	return raw, meta, true
}

// jobFrontmatterChanged records path's frontmatter and reports whether it
// differs from what this daemon last saw there. A first sighting always counts
// as changed: the daemon cannot know whether it missed an edit while down.
func (h *FlowHandler) jobFrontmatterChanged(path, raw string) bool {
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(raw))
	digest := sum.Sum64()

	h.jobMetaMu.Lock()
	defer h.jobMetaMu.Unlock()
	if h.jobMetaHash == nil {
		h.jobMetaHash = make(map[string]uint64)
	}
	if len(h.jobMetaHash) >= maxJobMetaMemo {
		h.jobMetaHash = make(map[string]uint64)
	}
	previous, seen := h.jobMetaHash[path]
	h.jobMetaHash[path] = digest
	return !seen || previous != digest
}

// isPlanDirCreate reports whether a membership-class create event is a
// just-created DIRECTORY, i.e. a new plan row rather than a stray file
// dropped into a plans root. Everything else the old spelling of this check
// established — direct child of a watched plans directory (or of its .archive
// container), not hidden — the classifier has already proven, so what is left
// is the one probe that needs the filesystem.
func isPlanDirCreate(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// planEventClass is what one filesystem event under a plans directory can
// change in the two things hanging off this handler: the plan index
// (scanPlansDir → loadIndexedPlans → orchestration.LoadPlanLenient) and the
// aggregated PlanStats enrichment (enrichment.countPlanStats →
// processPlanCounts).
//
// Both readers open a strictly bounded, shallow set of files, and neither
// descends below a plan root: they enumerate the plans directory, then read
// each plan's config and its TOP-LEVEL `.md` files. Everything deeper — the
// `.artifacts/` tree a running agent writes into continuously, job logs, chat
// transcripts, `.claude/` — is invisible to both, which is why an allowlist
// (rather than a denylist of known-noisy names) is the honest filter here: a
// new kind of agent output cannot silently start costing a portfolio recount.
type planEventClass int

const (
	// planEventNone is an event neither reader can observe. Dropped before it
	// arms the refresh debounce.
	planEventNone planEventClass = iota
	// planEventJob is a top-level plan `.md`: job frontmatter, and therefore
	// the job's status, type and channels as well as the plan's job counts.
	planEventJob
	// planEventConfig is a plan's config file: hold/unhold, worktree binding,
	// finished status. This is the only class published synchronously.
	planEventConfig
	// planEventMembership is an entry directly under a plans directory (or its
	// .archive container) — a plan appearing, disappearing or being renamed.
	planEventMembership
	// planEventOther is allowlisted lifecycle input that is none of the above.
	// Today that is `rules/`: nothing in this file reads it, but it is written
	// once per job rather than continuously, so allowing it costs nothing and
	// keeps a future reader of those files honest.
	planEventOther
)

const (
	// planConfigFilename mirrors orchestration's constant of the same name.
	// legacyPlanConfigFilename is the older spelling enrichment's
	// processPlanCounts still reads for the finished check.
	planConfigFilename       = ".grove-plan.yml"
	legacyPlanConfigFilename = "config.yml"
	archiveDirName           = ".archive"
	planRulesDirName         = "rules"
)

// classifiedPlanEvent is one surviving event plus the work already done to
// judge it: the resolved path and the plans directory that owns it.
type classifiedPlanEvent struct {
	event fsnotify.Event
	path  string
	// plansDir is the UNRESOLVED spelling, keyed the way runRefresh keys its
	// targets map. A resolved scope key would silently match nothing there.
	plansDir string
	class    planEventClass
}

// classifiedPlanBatch is one HandleEvents batch after path triage.
type classifiedPlanBatch struct {
	kept             []classifiedPlanEvent
	suppressed       int
	suppressedSample string
}

// classifyEvents resolves each event to its owning plans directory and judges
// what it can change. It replaces the old affectedPlansDirs, doing the same
// owner resolution once for the whole batch instead of once per consumer, and
// memoizing GetPlansDir per owner node.
func (h *FlowHandler) classifyEvents(events []fsnotify.Event) classifiedPlanBatch {
	var batch classifiedPlanBatch

	type plansDirPair struct{ raw, resolved string }
	resolvedFor := make(map[*workspace.WorkspaceNode]plansDirPair)

	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()
	for _, event := range events {
		path := resolveFlowWatchPath(event.Name)
		// One event belongs to exactly one plans directory, so the enclosing
		// watch entry has to be the most specific one — first-match-over-a-map
		// could scope the follow-up rescan to a different directory than the
		// one that actually changed and leave the real edit unindexed.
		owner := h.ownerForPath(path)
		if owner == nil {
			batch.suppress(event)
			continue
		}
		dirs, memoized := resolvedFor[owner]
		if !memoized {
			if raw, err := h.locator.GetPlansDir(owner); err == nil && raw != "" {
				dirs = plansDirPair{raw: raw, resolved: resolveFlowWatchPath(raw)}
			}
			resolvedFor[owner] = dirs
		}
		if dirs.raw == "" {
			batch.suppress(event)
			continue
		}
		class := classifyPlanEvent(dirs.resolved, path)
		if class == planEventNone {
			batch.suppress(event)
			continue
		}
		batch.kept = append(batch.kept, classifiedPlanEvent{
			event: event, path: path, plansDir: dirs.raw, class: class,
		})
	}
	return batch
}

func (b *classifiedPlanBatch) suppress(event fsnotify.Event) {
	b.suppressed++
	if b.suppressedSample == "" {
		b.suppressedSample = event.Name
	}
}

// classifyPlanEvent judges one resolved event path against the plans directory
// that owns it. plansDir must be the RESOLVED spelling, since eventPath is.
func classifyPlanEvent(plansDir, eventPath string) planEventClass {
	rel, err := filepath.Rel(plansDir, eventPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// The path is not under the plans root we resolved for its owner, so
		// nothing below can be reasoned about. Errs toward doing the work: a
		// spurious refresh is cheaper than a lifecycle edge dropped on the
		// floor by a path shape this function did not anticipate.
		return planEventOther
	}
	if rel == "." {
		return planEventMembership
	}
	parts := strings.Split(rel, string(filepath.Separator))

	// .archive is a container, not a plan (loadIndexedPlans descends exactly
	// one level into it), so shift past it and let archived plans classify
	// through the same rules as live ones.
	if parts[0] == archiveDirName {
		if len(parts) == 1 {
			return planEventMembership
		}
		parts = parts[1:]
	}

	if len(parts) == 1 {
		// A direct child of the plans root. Only non-hidden entries can be
		// plans — loadIndexedPlans and countPlanStats both skip dot-prefixed
		// names — so the plans root's own hidden clutter (`.flow-jobs.lock`,
		// `.init-*.output.log`, `.DS_Store`) changes nothing either reader
		// sees. A non-hidden entry may be a plan appearing or disappearing.
		if strings.HasPrefix(parts[0], ".") {
			return planEventNone
		}
		return planEventMembership
	}

	if len(parts) == 2 {
		return classifyPlanRootEntry(parts[1])
	}

	// Below a plan root. Both readers skip directory entries outright, so the
	// entire subtree is unreadable to them: this is where the `.artifacts/`
	// write storm that motivated the filter lives.
	if parts[1] == planRulesDirName {
		return planEventOther
	}
	return planEventNone
}

// classifyPlanRootEntry judges one entry sitting directly in a plan directory.
// The allowlist is exactly what the two readers open there.
func classifyPlanRootEntry(name string) planEventClass {
	switch {
	case name == planConfigFilename || name == legacyPlanConfigFilename:
		return planEventConfig
	case strings.HasSuffix(name, ".md"):
		return planEventJob
	case name == planRulesDirName:
		return planEventOther
	default:
		// Job lock files, `.init-journal.json`, the `.artifacts` directory
		// entry itself. Suppressing these means a plan row's UpdatedAt (the
		// plan directory's mtime) can lag such a create until the next
		// lifecycle event or the 5-minute reconciliation ticker — a timestamp,
		// not a state, and the price of not recounting the portfolio every
		// time a job takes a lock.
		return planEventNone
	}
}

func (h *FlowHandler) HandleStoreUpdate(update store.Update) {
	switch update.Type {
	case store.UpdateConfigReload:
		newCfg, err := config.LoadDefault()
		if err != nil {
			h.ulog.Error("Failed to reload config").Err(err).Log(context.Background())
			return
		}
		h.cfg = newCfg
		h.locator = workspace.NewNotebookLocator(newCfg)

	case store.UpdateWorkspaces:
		// Workspace discovery just (re)populated the watch set — this is the
		// cold-start edge. Without it the first populated index build waits for
		// the 5-minute reconciliation ticker or a coincidental plan-file event:
		// OnStart's refresh usually fires before discovery completes and finds
		// no plans directories at all. The UnifiedWatcher recomputes watch
		// paths before broadcasting this update to handlers, so a short
		// coalescing delay is all that's needed.
		h.scheduleRefresh(nil, true, workspaceRefreshDelay)

	case store.UpdateWorkspacesDelta:
		// Git enrichment landing in the store is what fills the cheap cached
		// git column on rows (applyCachedPlanGit). Re-project cached rows
		// through the fresh state; the store suppresses no-change broadcasts,
		// so quiet deltas cost one in-memory pass and no SSE traffic. The flow
		// watcher's own PlanStats deltas carry no GitStatus and cannot loop.
		if deltas, ok := update.Payload.([]*models.WorkspaceDelta); ok {
			for _, delta := range deltas {
				if delta != nil && delta.GitStatus != nil {
					h.scheduleRefresh(nil, false, time.Duration(h.debounceMs)*time.Millisecond)
					break
				}
			}
		}
	}
}

// workspaceRefreshDelay coalesces bursts of workspace-set changes while still
// making the first populated snapshot land promptly after discovery.
const workspaceRefreshDelay = 250 * time.Millisecond

func (h *FlowHandler) OnStart(ctx context.Context) {
	// Kick off a first refresh so /api/plans has a snapshot to serve
	// before any filesystem event arrives. The PlanCollector still
	// handles aggregated PlanStats; this populates the deep cache.
	// When workspace discovery already ran (restart, late registration) the
	// watch set is populated and there is no reason to sit out the full
	// debounce; otherwise the UpdateWorkspaces edge in HandleStoreUpdate is
	// what delivers the first populated snapshot.
	if h.store != nil && len(h.store.GetWorkspaces()) > 0 {
		h.scheduleRefresh(nil, true, workspaceRefreshDelay)
	} else {
		h.triggerRefresh()
	}
	// fsnotify is an acceleration path, not a completeness guarantee. Periodic
	// reconciliation repairs missed/coalesced events and advances freshness.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.triggerRefresh()
			}
		}
	}()
}

// triggerRefresh schedules a full debounced rescan of every plans directory.
func (h *FlowHandler) triggerRefresh() {
	h.scheduleRefresh(nil, true, time.Duration(h.debounceMs)*time.Millisecond)
}

// scheduleRefresh merges the requested scope into the pending set and (re)arms
// the debounce timer. all=true forces every plans directory to rescan; dirs
// scopes the disk work to the named plans directories; neither means an
// overlay-only re-projection of cached rows.
func (h *FlowHandler) scheduleRefresh(dirs map[string]struct{}, all bool, delay time.Duration) {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	if all {
		h.pendingAll = true
	}
	for dir := range dirs {
		if h.pendingDirs == nil {
			h.pendingDirs = make(map[string]struct{})
		}
		h.pendingDirs[dir] = struct{}{}
	}

	// Earliest-deadline coalescing: the scope above is already merged, so a
	// slower trigger must never push out an armed faster one (e.g. a git
	// delta's 2s debounce arriving after the 250ms cold-start edge). This
	// also bounds storm latency — a steady event stream fires at most one
	// debounce interval after its first event instead of deferring forever.
	deadline := time.Now().Add(delay)
	if h.refreshTimer != nil {
		if !h.refreshDeadline.After(deadline) {
			return
		}
		h.refreshTimer.Stop()
	}
	h.refreshDeadline = deadline
	h.refreshTimer = time.AfterFunc(delay, h.refresh)
}

// takePendingScope drains the accumulated refresh scope and cancels any armed
// timer, merging extra scope from a synchronous (lifecycle) caller.
func (h *FlowHandler) takePendingScope(extra map[string]struct{}) (bool, map[string]struct{}) {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	if h.refreshTimer != nil {
		h.refreshTimer.Stop()
		h.refreshTimer = nil
	}
	all := h.pendingAll
	dirs := h.pendingDirs
	h.pendingAll = false
	h.pendingDirs = nil
	for dir := range extra {
		if dirs == nil {
			dirs = make(map[string]struct{})
		}
		dirs[dir] = struct{}{}
	}
	return all, dirs
}

// triggerLifecycleRefresh cancels a pending eventually-consistent refresh and
// publishes this observed plan-config transition before HandleEvents returns.
func (h *FlowHandler) triggerLifecycleRefresh(dirs map[string]struct{}) {
	all, merged := h.takePendingScope(dirs)
	h.runRefresh(all, merged)
}

// refresh is the debounce-timer callback: drain the pending scope and run.
func (h *FlowHandler) refresh() {
	all, dirs := h.takePendingScope(nil)
	h.runRefresh(all, dirs)
}

// runRefresh rebuilds and publishes the plan index, then updates the
// aggregated PlanStats enrichment. Ordering is deliberate: the row projection
// is built from cheap per-directory scans and already-collected daemon state,
// and is published BEFORE the PlanStats pass, which recounts every plans
// directory in the portfolio and must never gate first-row availability.
func (h *FlowHandler) runRefresh(all bool, scopeDirs map[string]struct{}) {
	h.refreshRunMu.Lock()
	defer h.refreshRunMu.Unlock()

	ctx := context.Background()
	start := time.Now()

	state := h.store.Get()

	// Snapshot the watch set into unique plansDir -> workspace node targets.
	// The first entry wins per directory, so the keys are visited in sorted
	// order: the representative decides each row's WorkspaceRoot and selected
	// plan, and those must not differ between two refreshes of an unchanged
	// watch set.
	h.pathsMutex.RLock()
	watched := make([]string, 0, len(h.watchedPaths))
	for path := range h.watchedPaths {
		watched = append(watched, path)
	}
	sort.Strings(watched)
	targets := make(map[string]*workspace.WorkspaceNode)
	for _, path := range watched {
		wsNode := h.watchedPaths[path]
		plansDir, err := h.locator.GetPlansDir(wsNode)
		if err != nil || plansDir == "" {
			continue
		}
		if _, dup := targets[plansDir]; !dup {
			targets[plansDir] = wsNode
		}
	}
	h.pathsMutex.RUnlock()

	// Boot ordering: before workspace discovery has populated the store there
	// is nothing to index yet. Publishing the empty snapshot here would be a
	// lie ("scanned, zero plans") that a genuinely empty portfolio later can't
	// be distinguished from; the UpdateWorkspaces edge re-triggers us.
	if len(targets) == 0 && len(state.Workspaces) == 0 {
		h.ulog.Debug("Skipping plan index refresh before workspace discovery").Log(ctx)
		return
	}

	if h.dirCache == nil {
		h.dirCache = make(map[string]*dirScanResult)
	}

	// Rescan only what the scope implicates (plus cache misses); reuse the
	// cached disk scan for everything else.
	scanAt := time.Now()
	rescanned := 0
	for plansDir := range targets {
		_, affected := scopeDirs[plansDir]
		if _, cached := h.dirCache[plansDir]; cached && !all && !affected {
			continue
		}
		h.dirCache[plansDir] = scanPlansDir(plansDir, planWorkspaceRoot(targets[plansDir]), scanAt)
		rescanned++
	}
	for dir := range h.dirCache {
		if _, ok := targets[dir]; !ok {
			delete(h.dirCache, dir)
		}
	}
	scanDone := time.Now()

	// Merge cached scans and re-apply the live overlays (selection, running
	// sessions, registry bindings, cached git) from current store state.
	plansByDir := make(map[string][]*orchestration.Plan, len(targets))
	var summaries []models.PlanSummary
	registryEntries, registryErr := worktreeregistry.ListAll()
	for plansDir, wsNode := range targets {
		result := h.dirCache[plansDir]
		if result == nil {
			continue
		}
		selectedPlan, _ := corestate.GetString(planWorkspaceRoot(wsNode), coreplan.StateKey)
		for _, base := range result.summaries {
			row := base
			row.RunningSessions = countRunningSessions(state.Sessions, row.PlanName)
			if !row.Archived {
				row.Selected = selectedPlan == row.PlanName
			}
			summaries = append(summaries, row)
		}
		plansByDir[plansDir] = result.plans
	}
	summaries = applyQualifiedPlanBindings(summaries, registryEntries, registryErr)
	summaries = applyCachedPlanGit(summaries, state.Workspaces)

	if len(plansByDir) > 0 {
		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdatePlans,
			Source:  "flow_watcher",
			Scanned: len(plansByDir),
			Payload: plansByDir,
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].PlanDir < summaries[j].PlanDir })
	h.store.ApplyUpdate(store.Update{
		Type: store.UpdatePlanIndexSnapshot, Source: "flow_watcher", Scanned: len(summaries),
		Payload: &models.PlanIndexSnapshot{ScannedAt: scanAt, Plans: summaries},
	})
	publishDone := time.Now()

	// Fence the async stats pass, but only against publishes it can actually
	// be wrong about. The digest covers the disk-derived plan state the stats
	// reader opens; an overlay-only re-projection leaves it identical and so
	// cannot invalidate a pass that is mid-flight.
	statsInput := statsInputDigest(targets, h.dirCache)
	statsMoved := statsInput != h.lastStatsInput
	if statsMoved {
		h.lastStatsInput = statsInput
		h.statsSeq.Add(1)
	}

	// Aggregated PlanStats enrichment. Overlay-only passes changed nothing on
	// disk, so the recount is skipped for them. The pass runs asynchronously:
	// holding refreshRunMu across its disk reads would queue the next
	// synchronous lifecycle publish behind them.
	if all || rescanned > 0 || len(scopeDirs) > 0 {
		// `all` is the exemption from the rate floor, and it is exactly the
		// right one: a full rescan is only ever requested by OnStart, the
		// UpdateWorkspaces cold-start edge and the 5-minute reconciliation
		// ticker. File churn always arrives scoped, so it can never buy itself
		// the exemption.
		h.kickPlanStats(all)
	}

	elapsed := time.Since(start)
	entry := h.ulog.Debug("Plan index refresh")
	if elapsed > time.Second {
		entry = h.ulog.Info("Slow plan index refresh")
	}
	entry.Field("rows", len(summaries)).
		Field("dirs", len(targets)).
		Field("rescanned", rescanned).
		Field("full", all).
		Field("stats_moved", statsMoved).
		Field("scan_ms", scanDone.Sub(start).Milliseconds()).
		Field("publish_ms", publishDone.Sub(scanDone).Milliseconds()).
		Field("total_ms", elapsed.Milliseconds()).
		Log(ctx)
}

// kickPlanStats starts (or queues onto) the async aggregated-PlanStats pass.
// At most one pass runs at a time; kicks during a run coalesce into exactly
// one trailing run, which re-reads disk and so converges on the final state.
//
// force bypasses the rate floor and is reserved for the correctness paths
// (see runRefresh).
func (h *FlowHandler) kickPlanStats(force bool) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	h.startOrDeferStatsLocked(force)
}

// startOrDeferStatsLocked runs the pass now when nothing forbids it, and
// otherwise records the debt so that exactly one trailing run happens later —
// folded into the pass already running, or armed on the floor timer. Both
// deferrals converge, which is what makes the floor a rate limit rather than
// a sampling policy: no update is ever lost, only delayed. Callers hold
// statsMu.
func (h *FlowHandler) startOrDeferStatsLocked(force bool) {
	if h.statsRunning {
		h.statsQueued = true
		h.statsQueuedForce = h.statsQueuedForce || force
		return
	}
	if wait := h.statsFloorWaitLocked(force); wait > 0 {
		h.statsQueued = true
		h.statsQueuedForce = h.statsQueuedForce || force
		if h.statsFloorTimer == nil {
			h.statsFloorTimer = time.AfterFunc(wait, h.statsFloorExpired)
		}
		telemetry.RecordPlanStatsDeferred()
		return
	}
	h.statsQueued = false
	h.statsQueuedForce = false
	h.statsRunning = true
	h.statsLastRun = h.clock()
	go h.planStatsLoop()
}

// statsFloorWaitLocked reports how much longer the rate floor forbids a pass.
func (h *FlowHandler) statsFloorWaitLocked(force bool) time.Duration {
	if force || h.statsMinInterval <= 0 || h.statsLastRun.IsZero() {
		return 0
	}
	if elapsed := h.clock().Sub(h.statsLastRun); elapsed < h.statsMinInterval {
		return h.statsMinInterval - elapsed
	}
	return 0
}

// statsFloorExpired is the trailing run the floor promised.
func (h *FlowHandler) statsFloorExpired() {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	h.statsFloorTimer = nil
	if !h.statsQueued {
		return
	}
	h.startOrDeferStatsLocked(h.statsQueuedForce)
}

func (h *FlowHandler) planStatsLoop() {
	for {
		seq := h.statsSeq.Load()
		h.statsPass(seq)

		h.statsMu.Lock()
		// Two things can owe a rerun. A queued kick is a request for one. A
		// moved statsSeq means a publish changed plan state under this pass
		// while it read, so refreshPlanStats threw its answer away and the
		// recount still has to happen — the invariant being that emitted stats
		// never lag the last published lifecycle state.
		//
		// The seq arm is the narrow one now: it fences stats-relevant
		// publishes only, and every publish that re-read a plans directory has
		// already queued a kick, so what it uniquely catches is the rare
		// publish that moved the plan set without kicking.
		queued := h.statsQueued
		owed := queued || h.statsSeq.Load() != seq
		force := h.statsQueuedForce
		h.statsQueued = false
		h.statsQueuedForce = false
		h.statsRunning = false
		if !owed {
			h.statsMu.Unlock()
			return
		}
		telemetry.RecordPlanStatsRerun(queued)
		if wait := h.statsFloorWaitLocked(force); wait > 0 {
			// The rerun is owed but the floor forbids it now. Handing it to
			// the timer rather than spinning here is what turns a continuous
			// event stream into one pass per interval.
			h.statsQueued = true
			h.statsQueuedForce = force
			if h.statsFloorTimer == nil {
				h.statsFloorTimer = time.AfterFunc(wait, h.statsFloorExpired)
			}
			telemetry.RecordPlanStatsDeferred()
			h.statsMu.Unlock()
			return
		}
		h.statsRunning = true
		h.statsLastRun = h.clock()
		h.statsMu.Unlock()
	}
}

// refreshPlanStats recomputes the aggregated per-workspace PlanStats. It runs
// off the refresh mutex so its cost never delays row publishes. seq fences
// staleness: results computed from plan state older than the latest index
// publish are discarded (the trailing loop run recomputes them). Only publishes
// that moved plan state the counts derive from advance the seq, so a git-delta
// re-projection landing mid-pass no longer throws the pass's work away.
//
// The workspace set comes from the store, never from a fresh
// workspace.DiscoverAll: this pass fires on a 2s debounce behind plan-file
// events, and re-walking/re-classifying every workspace on disk each time was
// the daemon's dominant allocation (and therefore GC, and therefore CPU) load.
func (h *FlowHandler) refreshPlanStats(seq uint64) {
	planStats := enrichment.FetchPlanStatsMap(h.store.WorkspaceNodes(), h.locator)
	if h.statsSeq.Load() != seq {
		return
	}

	state := h.store.Get()
	var deltas []*models.WorkspaceDelta
	for k, v := range state.Workspaces {
		if stats, ok := planStats[k]; ok {
			if !store.PlanStatsEqual(v.PlanStats, stats) {
				deltas = append(deltas, &models.WorkspaceDelta{
					Path:      k,
					PlanStats: stats,
				})
			}
		}
	}

	if len(deltas) > 0 {
		h.store.ApplyUpdate(store.Update{
			Type:    store.UpdateWorkspacesDelta,
			Source:  "flow_watcher",
			Scanned: len(deltas),
			Payload: deltas,
		})
	}
}

// sortPlanOwners orders workspaces so the first node claiming a shared plans
// directory is the one that should own it.
//
// The preference is the same one NotebookLocator.ScanForAllPlans applies for
// the JobCollector ("prefer main projects over worktrees"), expressed through
// planWorkspaceRoot: a node that IS its own plan-workspace root is the
// workspace the centralized plans directory is named after, while members and
// worktrees merely inherit it. Path is the tiebreak purely so the result is
// reproducible across daemon restarts — never a coin flip that files the same
// job under a different workspace on the next scan.
func sortPlanOwners(nodes []*workspace.WorkspaceNode) {
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		ownsA := planWorkspaceRoot(a) == a.Path
		ownsB := planWorkspaceRoot(b) == b.Path
		if ownsA != ownsB {
			return ownsA
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})
}

// ownerForPath returns the workspace node owning the watched path that most
// specifically contains eventPath, or nil when the path is unwatched.
//
// Most-specific wins: the watch set holds a plans directory AND each of its
// plan subdirectories, so any job file matches several entries at once. Taking
// whichever entry map iteration yielded first was arbitrary twice over — it
// ignored prefix length, and it re-rolled on every event. Callers must hold
// pathsMutex.
func (h *FlowHandler) ownerForPath(eventPath string) *workspace.WorkspaceNode {
	var best string
	var owner *workspace.WorkspaceNode
	for watchedPath, wsNode := range h.watchedPaths {
		if eventPath != watchedPath && !strings.HasPrefix(eventPath, watchedPath+string(filepath.Separator)) {
			continue
		}
		if owner != nil && len(watchedPath) <= len(best) {
			continue
		}
		best, owner = watchedPath, wsNode
	}
	return owner
}

// planWorkspaceRoot returns the canonical owner identity for plan rows. Many
// ecosystem members resolve to the same centralized plans directory; the
// representative node is picked deterministically by sortPlanOwners, and this
// still normalizes it, so row identity is the parent ecosystem even when the
// representative is a child checkout.
func planWorkspaceRoot(node *workspace.WorkspaceNode) string {
	if node == nil {
		return ""
	}
	if node.RootEcosystemPath != "" {
		return node.RootEcosystemPath
	}
	if node.Kind == workspace.KindEcosystemRoot {
		return node.Path
	}
	if node.IsWorktree() && node.ParentProjectPath != "" {
		return node.ParentProjectPath
	}
	return node.Path
}

// scanPlansDir reads one plans directory from disk into its cacheable base
// rows. Live overlays (Selected, RunningSessions, bindings, git) are
// deliberately NOT stamped here — runRefresh re-applies them on every publish
// so cached entries can never pin them stale.
func scanPlansDir(plansDir, workspaceRoot string, scanAt time.Time) *dirScanResult {
	indexed := loadIndexedPlans(plansDir)
	result := &dirScanResult{plans: make([]*orchestration.Plan, 0, len(indexed))}
	for _, indexedPlan := range indexed {
		p := indexedPlan.plan
		if !indexedPlan.archived {
			result.plans = append(result.plans, p)
		}
		summary := summarizePlan(p, plansDir, workspaceRoot, "", nil, scanAt)
		summary.Archived = indexedPlan.archived
		if indexedPlan.archived {
			summary.Lifecycle = "finished"
			summary.Selected = false
		}
		result.summaries = append(result.summaries, summary)
	}
	result.statsDigest = statsRelevantDigest(result.summaries)
	return result
}

// statsRelevantDigest folds one directory's rows down to what the aggregated
// PlanStats reader can see, so two scans that differ only in ways invisible to
// it compare equal.
//
// enrichment.countPlanStats enumerates the non-hidden plan directories under a
// plans root and, per plan, reads the config for the finished flag and each
// top-level job's frontmatter status. That is exactly the plan SET, each plan's
// LIFECYCLE and its JOB COUNTS — everything else on a summary (worktree
// binding, notes, repos, mtime) is either an overlay or invisible to the
// counts. Archived plans are excluded because countPlanStats never descends
// into `.archive`; a plan being archived still moves the digest, since its live
// entry disappears from the set.
//
// A row here is disk-derived by construction: scanPlansDir runs before
// runRefresh layers on selection, sessions, bindings and git.
func statsRelevantDigest(summaries []models.PlanSummary) uint64 {
	rows := make([]string, 0, len(summaries))
	var row strings.Builder
	for _, summary := range summaries {
		if summary.Archived {
			continue
		}
		row.Reset()
		row.WriteString(summary.PlanName)
		row.WriteByte(0)
		row.WriteString(summary.Lifecycle)
		statuses := make([]string, 0, len(summary.JobCounts))
		for status := range summary.JobCounts {
			statuses = append(statuses, status)
		}
		sort.Strings(statuses)
		for _, status := range statuses {
			row.WriteByte(0)
			row.WriteString(status)
			row.WriteByte('=')
			row.WriteString(strconv.Itoa(summary.JobCounts[status]))
		}
		rows = append(rows, row.String())
	}
	// Sorted so the digest is a property of the plan set, not of the order
	// os.ReadDir happened to return it in.
	sort.Strings(rows)

	sum := fnv.New64a()
	for _, r := range rows {
		_, _ = sum.Write([]byte(r))
		_, _ = sum.Write([]byte{'\n'})
	}
	return sum.Sum64()
}

// statsInputDigest folds the per-directory digests of the CURRENT target set
// into the one value runRefresh compares across publishes. Keying on targets
// rather than on the cache means a plans directory leaving the watch set moves
// the digest even though nothing was rescanned.
func statsInputDigest(targets map[string]*workspace.WorkspaceNode, cache map[string]*dirScanResult) uint64 {
	dirs := make([]string, 0, len(targets))
	for dir := range targets {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	sum := fnv.New64a()
	var digest [8]byte
	for _, dir := range dirs {
		result := cache[dir]
		if result == nil {
			continue
		}
		_, _ = sum.Write([]byte(dir))
		binary.LittleEndian.PutUint64(digest[:], result.statsDigest)
		_, _ = sum.Write(digest[:])
	}
	return sum.Sum64()
}

// countRunningSessions mirrors summarizePlan's live-session overlay for rows
// merged from the per-directory cache.
func countRunningSessions(sessions map[string]*models.Session, planName string) int {
	running := 0
	for _, session := range sessions {
		if session != nil && session.PlanName == planName && session.EndedAt == nil {
			running++
		}
	}
	return running
}

type indexedPlanEntry struct {
	plan     *orchestration.Plan
	archived bool
}

// loadIndexedPlans recognizes only direct live plan directories and direct
// children of the archive container. Hidden organizational directories are
// never themselves plans, and archived plans remain separately identifiable
// as read-only rows in daemon clients.
//
// Loading is lenient: one malformed or half-written job file must degrade to
// a row with fewer jobs, never to the plan silently vanishing from the index
// (the pilot's burst-insert plans were dropped exactly that way).
func loadIndexedPlans(plansDir string) []indexedPlanEntry {
	ulog := logging.NewUnifiedLogger("groved.watcher.flow")
	var indexed []indexedPlanEntry
	loadChildren := func(parent string, archived bool) {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			p, problems := orchestration.LoadPlanLenient(filepath.Join(parent, entry.Name()))
			for _, problem := range problems {
				ulog.Debug("Plan indexed with degraded jobs").
					Field("plan_dir", filepath.Join(parent, entry.Name())).
					Err(problem).
					Log(context.Background())
			}
			if p != nil {
				indexed = append(indexed, indexedPlanEntry{plan: p, archived: archived})
			}
		}
	}
	loadChildren(plansDir, false)
	loadChildren(filepath.Join(plansDir, ".archive"), true)
	return indexed
}

func summarizePlan(p *orchestration.Plan, plansDir, workspaceRoot, selectedPlan string, sessions map[string]*models.Session, scannedAt time.Time) models.PlanSummary {
	lifecycle := "live"
	worktree := ""
	notes := ""
	var repos []string
	if p.Config != nil {
		worktree = p.Config.Worktree
		notes = p.Config.Notes
		repos = append(repos, p.Config.Repos...)
		switch p.Config.Status {
		case "hold", "review", "finished":
			lifecycle = p.Config.Status
		}
	}
	counts := make(map[string]int)
	for _, job := range p.Jobs {
		counts[string(job.Status)]++
	}
	running := 0
	for _, session := range sessions {
		if session != nil && session.PlanName == p.Name && session.EndedAt == nil {
			running++
		}
	}
	updatedAt := scannedAt
	if info, err := os.Stat(p.Directory); err == nil {
		updatedAt = info.ModTime()
	}
	return models.PlanSummary{
		PlanDir: p.Directory, PlanName: p.Name, WorkspaceRoot: workspaceRoot,
		PlansDir: plansDir, Lifecycle: lifecycle, Selected: selectedPlan == p.Name,
		Worktree: worktree, Repositories: repos, Notes: notes, JobCounts: counts,
		RunningSessions: running, UpdatedAt: updatedAt, ScannedAt: scannedAt,
	}
}

// applyQualifiedPlanBindings enriches summaries from the one canonical
// registry-backed resolver. Bare plan names are deliberately never used as a
// join key: duplicate slugs in different notebook workspaces must retain their
// own container association. The caller's registry listing (and its error) is
// passed through so the resolver never repeats the scan; on a failed listing
// the resolver runs its own, preserving the "registry unavailable" marking.
func applyQualifiedPlanBindings(summaries []models.PlanSummary, entries []*worktreeregistry.Entry, listErr error) []models.PlanSummary {
	requests := make([]coreplan.BindingRequest, 0, len(summaries))
	for _, summary := range summaries {
		requests = append(requests, coreplan.BindingRequest{
			PlanDir:            summary.PlanDir,
			WorkspaceRoot:      summary.WorkspaceRoot,
			ConfiguredWorktree: summary.Worktree,
			Archived:           summary.Archived,
		})
	}
	if listErr != nil {
		return applyResolvedPlanBindings(summaries, entries, coreplan.ResolvePlanBindings(requests))
	}
	return applyResolvedPlanBindings(summaries, entries, coreplan.ResolvePlanBindingsWithEntries(requests, entries))
}

// applyCachedPlanGit projects only already-collected daemon status into cheap
// rows. It never invokes Git. Ecosystem rows aggregate their declared member
// checkouts; selected-row detail remains the only live Git path in Flow.
func applyCachedPlanGit(summaries []models.PlanSummary, workspaces map[string]*models.EnrichedWorkspace) []models.PlanSummary {
	byPath := make(map[string]*coregit.StatusInfo, len(workspaces))
	for path, workspace := range workspaces {
		if workspace == nil || workspace.GitStatus == nil || workspace.GitStatus.StatusInfo == nil {
			continue
		}
		byPath[filepath.Clean(path)] = workspace.GitStatus.StatusInfo
		if workspace.WorkspaceNode != nil {
			byPath[filepath.Clean(workspace.Path)] = workspace.GitStatus.StatusInfo
		}
	}
	for i := range summaries {
		if summaries[i].WorktreePath == "" {
			continue
		}
		paths := []string{summaries[i].WorktreePath}
		if len(summaries[i].Repositories) > 0 {
			paths = paths[:0]
			for _, repo := range summaries[i].Repositories {
				paths = append(paths, filepath.Join(summaries[i].WorktreePath, repo))
			}
		}
		aggregate := &coregit.StatusInfo{}
		found := false
		for _, path := range paths {
			status := byPath[filepath.Clean(path)]
			if status == nil {
				continue
			}
			found = true
			aggregate.IsDirty = aggregate.IsDirty || status.IsDirty
			aggregate.ModifiedCount += status.ModifiedCount
			aggregate.UntrackedCount += status.UntrackedCount
			aggregate.StagedCount += status.StagedCount
			aggregate.AheadCount += status.AheadMainCount
			aggregate.BehindCount += status.BehindMainCount
		}
		if found {
			summaries[i].GitStatus = aggregate
		}
	}
	return summaries
}

func applyResolvedPlanBindings(summaries []models.PlanSummary, entries []*worktreeregistry.Entry, bindings map[string]coreplan.PlanBinding) []models.PlanSummary {
	entriesByPath := make(map[string]*worktreeregistry.Entry, len(entries))
	for _, entry := range entries {
		if entry != nil {
			entriesByPath[filepath.Clean(entry.AbsPath)] = entry
		}
	}
	for i := range summaries {
		binding := bindings[coreplan.NewPlanKey(summaries[i].PlanDir).String()]
		summaries[i].BindingHealth = string(binding.Health)
		summaries[i].BindingReason = binding.Reason
		summaries[i].RegistryID = binding.RegistryID
		if !binding.Valid() {
			continue
		}
		summaries[i].WorktreePath = binding.ContainerPath
		entry := entriesByPath[filepath.Clean(binding.ContainerPath)]
		if entry == nil {
			continue
		}
		if len(summaries[i].Repositories) == 0 {
			summaries[i].Repositories = append([]string(nil), entry.Repos...)
		}
		summaries[i].Anchor = entry.AnchorOverride
		if summaries[i].Anchor == "" {
			summaries[i].Anchor = entry.Owner
		}
	}
	return summaries
}
