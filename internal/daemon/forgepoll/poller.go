// Package forgepoll is the global daemon's read-only forge poller.
//
// Webhooks cannot reach a laptop, so the daemon polls: it discovers the
// ecosystem's repositories from the workspaces it already tracks, asks a
// core/pkg/forge provider what the forge says about each one, caches the
// answers with explicit staleness, and fans the changes out over the existing
// SSE stream plus the workspace enrichment pipeline.
//
// Standing rules baked in here:
//
//   - Read-only. The provider interface has no mutation methods; nothing in
//     this package pushes, comments, merges, or closes anything.
//   - Off by default. Two independent gates must both open: an explicit
//     [forge.poll] enabled = true, AND the provider reporting its transport
//     available. A machine without `gh` gets a log line and no goroutine —
//     never an auth prompt, never an error.
//   - Unknown is never green, and a failure is never "no PRs". A failed poll
//     degrades a cache entry to stale and keeps the data it already had; it
//     never evicts the entry and never substitutes an empty result.
//   - Emit on change. A sweep that re-reads identical state broadcasts
//     nothing, so a daemon restart converges on the cache it had before
//     without a burst of duplicate-but-different events.
//   - Never wedge the daemon. Every sweep is bounded by a context timeout,
//     every provider call runs behind a recover, and no failure path returns
//     an error to the boot sequence.
package forgepoll

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/git"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/forge"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// Bounds that are policy rather than configuration. They exist so a
// pathological repo cannot turn a background convenience into a request storm.
const (
	// prFetchLimit is how many pull requests one repo contributes per sweep.
	// The provider clamps its own ceiling too (forge.MaxItems); this is the
	// tighter, poller-owned bound.
	prFetchLimit = 50
	// maxCheckedPRs caps how many open pull requests get a Checks call per
	// repo per sweep. Beyond it the rollup stays unknown — which is the honest
	// answer, and never a green one.
	maxCheckedPRs = 20
	// sweepTimeout bounds an entire sweep. A hung provider costs one sweep,
	// not the poller.
	sweepTimeout = 4 * time.Minute
	// maxBackoffSteps caps the per-repo exponential backoff at 2^n intervals.
	maxBackoffSteps = 4
)

// storeAPI is the narrow slice of the daemon store the poller uses. It exists
// so the poller can be tested against a fake without standing up a real store.
type storeAPI interface {
	Get() store.State
	ApplyUpdate(store.Update)
}

// Options configures a Poller. Only Store and Provider are required.
type Options struct {
	// Store is the daemon store the poller reads workspaces from and writes
	// updates to.
	Store storeAPI
	// Provider is the forge seam. This wave ships the GitHub implementation.
	Provider forge.Provider
	// Interval is the gap between sweeps.
	Interval time.Duration
	// StaleAfter is how long a successful fetch stays fresh.
	StaleAfter time.Duration
	// Hosts is the set of git hosts whose repos this provider may be asked
	// about. Empty means DefaultHosts.
	Hosts []string
	// RemoteName is the git remote a repo's forge identity is read from.
	// Empty means git.DefaultRemoteName ("origin").
	RemoteName string

	// Test seams. All optional; each falls back to the production behavior.
	Now         func() time.Time
	Rand        *rand.Rand
	ListRemotes func(repoPath string) ([]git.Remote, error)
}

// DefaultHosts is the host allowlist when [forge.poll] does not name one.
// Restricting by host is what keeps the poller from asking the GitHub provider
// about a repo whose origin points at some unrelated forge — ResolveRepo is
// pure parsing and will happily accept any host.
var DefaultHosts = []string{"github.com"}

// Poller sweeps the ecosystem's repositories on an interval.
type Poller struct {
	store    storeAPI
	provider forge.Provider
	cache    *cache

	interval   time.Duration
	staleAfter time.Duration
	hosts      map[string]bool
	remoteName string

	now         func() time.Time
	listRemotes func(string) ([]git.Remote, error)

	randMu sync.Mutex
	rand   *rand.Rand

	// backoff tracks consecutive failures per repo identity, so one dead repo
	// slows only itself.
	backoffMu sync.Mutex
	failures  map[string]int
	nextTry   map[string]time.Time

	ulog *logging.UnifiedLogger
}

// New builds a Poller from Options. It returns an error only for a
// programming mistake (no store, no provider); every operational condition —
// no repos, no network, an unauthenticated CLI — is a runtime state the poller
// represents as unknown, not a construction failure.
func New(opts Options) (*Poller, error) {
	if opts.Store == nil {
		return nil, errNoStore
	}
	if opts.Provider == nil {
		return nil, errNoProvider
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = config.DefaultForgePollInterval
	}
	staleAfter := opts.StaleAfter
	if staleAfter < interval {
		staleAfter = interval
	}

	hosts := opts.Hosts
	if len(hosts) == 0 {
		hosts = DefaultHosts
	}
	hostSet := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		hostSet[normalizeHost(h)] = true
	}

	p := &Poller{
		store:       opts.Store,
		provider:    opts.Provider,
		cache:       newCache(staleAfter),
		interval:    interval,
		staleAfter:  staleAfter,
		hosts:       hostSet,
		remoteName:  opts.RemoteName,
		now:         opts.Now,
		listRemotes: opts.ListRemotes,
		rand:        opts.Rand,
		failures:    make(map[string]int),
		nextTry:     make(map[string]time.Time),
		ulog:        logging.NewUnifiedLogger("groved.forgepoll"),
	}
	if p.remoteName == "" {
		p.remoteName = git.DefaultRemoteName
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.listRemotes == nil {
		p.listRemotes = git.ListRemotes
	}
	if p.rand == nil {
		p.rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return p, nil
}

// Start runs the poll loop until ctx is done. It sweeps once immediately (so a
// freshly booted daemon does not sit blind for a whole interval), then on the
// jittered interval.
func (p *Poller) Start(ctx context.Context) {
	p.ulog.Info("Forge poller started").
		Field("provider", p.provider.Name()).
		Field("interval", p.interval.String()).
		Field("stale_after", p.staleAfter.String()).
		Log(ctx)

	for {
		p.Sweep(ctx)

		select {
		case <-ctx.Done():
			return
		case <-time.After(p.jittered(p.interval)):
		}
	}
}

// Sweep runs one poll pass: discover, fetch what is due, broadcast what
// changed, project onto the workspace enrichment. It is exported so tests can
// drive passes deterministically instead of racing a ticker.
//
// It never returns an error and never panics out: a provider that misbehaves
// costs one repo's freshness, not the daemon.
func (p *Poller) Sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()

	targets := p.discover()
	now := p.now()

	for _, repo := range uniqueRepos(targets) {
		p.cache.ensure(repo, p.provider.Name())
		if !p.due(repo, now) {
			continue
		}
		p.fetch(ctx, repo, p.now())
	}

	p.broadcast(ctx)
	p.project(ctx, targets)
}

// Snapshot returns the whole cache as it stands, with each entry's live
// backoff state stamped on. It is the seam a read-only HTTP surface (the
// git-viewer PRs page) reads through; nothing in this wave mutates through it.
func (p *Poller) Snapshot() []store.ForgeRepoState {
	return p.withBackoff(p.cache.snapshot(p.now()))
}

// withBackoff stamps each entry with the poller's per-repo failure count and
// next attempt time.
//
// Backoff lives here rather than in the cache because it is the POLLER's
// schedule, not the forge's answer. Stamping it on the way out is what lets a
// surface say "quiet until 14:51" instead of showing a stale row that looks
// stuck: after a few failures the next try is 2^n intervals away, so restoring
// connectivity does not produce fresh data on the next sweep, and a consumer
// with no view of the schedule cannot tell that designed silence apart from a
// wedged poller.
func (p *Poller) withBackoff(states []store.ForgeRepoState) []store.ForgeRepoState {
	if len(states) == 0 {
		return states
	}
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	for i := range states {
		key := states[i].Repo
		states[i].ConsecutiveFailures = p.failures[key]
		states[i].NextAttemptAt = p.nextTry[key]
	}
	return states
}

// ProviderName reports which forge provider this poller is running. The HTTP
// read surface publishes it alongside the cache so a consumer can name the
// source of a stale or unknown entry instead of attributing it to "the forge".
func (p *Poller) ProviderName() string { return p.provider.Name() }

// target binds a workspace path to the repo identity its remote resolves to.
// Several worktrees of the same repository share one identity, and the poller
// fetches that repository once.
type target struct {
	path string
	repo forge.Repo
}

// discover resolves every tracked workspace's remote into a repo identity.
// A workspace with no remote, an unparseable remote, or a remote on a host
// this provider does not serve is silently skipped — that is the normal state
// for most of a local-first ecosystem, not an error.
func (p *Poller) discover() []target {
	state := p.store.Get()
	out := make([]target, 0, len(state.Workspaces))
	for path, ws := range state.Workspaces {
		if ws == nil || ws.WorkspaceNode == nil || path == "" {
			continue
		}
		url := p.remoteURL(path)
		if url == "" {
			continue
		}
		repo, err := p.provider.ResolveRepo(url)
		if err != nil || repo.IsZero() {
			continue
		}
		if !p.hosts[normalizeHost(repo.Host)] {
			continue
		}
		out = append(out, target{path: path, repo: repo})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// remoteURL returns the fetch URL of the configured remote, or "".
func (p *Poller) remoteURL(repoPath string) string {
	remotes, err := p.listRemotes(repoPath)
	if err != nil {
		return ""
	}
	for _, r := range remotes {
		if r.Name == p.remoteName {
			return r.URL
		}
	}
	return ""
}

// uniqueRepos collapses targets onto the repositories to poll, in a stable
// order so a sweep is deterministic.
func uniqueRepos(targets []target) []forge.Repo {
	seen := make(map[string]bool, len(targets))
	out := make([]forge.Repo, 0, len(targets))
	for _, t := range targets {
		key := t.repo.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t.repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// fetch polls one repository and records the outcome. A failure degrades the
// cache entry; it never clears it.
func (p *Poller) fetch(ctx context.Context, repo forge.Repo, now time.Time) {
	prs, checks, err := p.read(ctx, repo)
	if err != nil {
		p.cache.recordFailure(repo, p.provider.Name(), now, err)
		p.penalize(repo, now)
		p.ulog.Debug("Forge poll failed; entry degraded to stale").
			Field("repo", repo.String()).
			Field("class", string(forge.ClassOf(err))).
			Err(err).
			Log(ctx)
		return
	}
	p.cache.recordSuccess(repo, p.provider.Name(), now, prs, checks)
	p.clearPenalty(repo)
}

// read performs the provider calls for one repo behind a panic guard. A
// provider is third-party-ish code from this package's point of view; a panic
// in one must not take the daemon's goroutine with it.
func (p *Poller) read(ctx context.Context, repo forge.Repo) (prs []forge.PullRequest, checks map[int]forge.CheckRollup, err error) {
	defer func() {
		if r := recover(); r != nil {
			prs, checks = nil, nil
			err = panicError(repo, r)
		}
	}()

	prs, err = p.provider.ListPRs(ctx, repo, forge.ListPROptions{
		State: forge.StateAll,
		Limit: prFetchLimit,
	})
	if err != nil {
		return nil, nil, err
	}

	// Checks are fetched for open pull requests only: a merged or closed PR's
	// CI state is history, and paying a round trip per historical PR is how a
	// poller becomes a rate-limit problem. A PR with no entry in this map has
	// an unknown rollup, which is exactly what it is.
	checks = make(map[int]forge.CheckRollup)
	checked := 0
	for _, pr := range prs {
		if pr.State.Normalized() != forge.PRStateOpen {
			continue
		}
		if checked >= maxCheckedPRs {
			break
		}
		ref := pr.HeadSHA
		if ref == "" {
			ref = pr.HeadRef
		}
		if ref == "" {
			continue
		}
		checked++
		rollup, cerr := p.provider.Checks(ctx, repo, ref)
		if cerr != nil {
			// One PR's checks failing is not the repo's PR list failing. Record
			// the rollup as unknown and keep going — the alternative, failing
			// the whole repo, would degrade good PR data over a CI hiccup.
			checks[pr.Number] = forge.UnknownRollup(ref)
			continue
		}
		checks[pr.Number] = rollup
	}
	return prs, checks, nil
}

// broadcast emits an SSE frame for the entries that materially changed. No
// change, no frame — that is the restart/duplicate discipline.
func (p *Poller) broadcast(ctx context.Context) {
	changed := p.withBackoff(p.cache.changed(p.now()))
	if len(changed) == 0 {
		return
	}
	// NOTE: backoff is stamped onto the frame but deliberately absent from the
	// cache fingerprint. A repo failing the same way twice is not new
	// information and must not generate a frame per sweep; the current schedule
	// is always readable from GET /api/forge/state, which is where a surface
	// asks "how long is this quiet period".
	p.store.ApplyUpdate(store.Update{
		Type:    store.UpdateForgeState,
		Source:  "forge_poller",
		Scanned: len(changed),
		Payload: &store.ForgeStatePayload{Repos: changed},
	})
	p.ulog.Debug("Broadcast forge state change").Field("repos", len(changed)).Log(ctx)
}

// project computes models.ReviewStats for every tracked workspace and applies
// the ones that changed, through the same UpdateWorkspacesDelta path
// NoteCounts uses.
func (p *Poller) project(ctx context.Context, targets []target) {
	if len(targets) == 0 {
		return
	}
	now := p.now()
	state := p.store.Get()

	var deltas []*models.WorkspaceDelta
	for _, t := range targets {
		ws, ok := state.Workspaces[t.path]
		if !ok || ws == nil {
			continue
		}
		cached, ok := p.cache.lookup(t.repo, now)
		if !ok {
			continue
		}
		stats := reviewStatsFrom(cached, planStatusOf(ws))
		if reviewStatsEqual(ws.ReviewStats, stats) {
			continue
		}
		deltas = append(deltas, &models.WorkspaceDelta{Path: t.path, ReviewStats: stats})
	}
	if len(deltas) == 0 {
		return
	}
	p.store.ApplyUpdate(store.Update{
		Type:    store.UpdateWorkspacesDelta,
		Source:  "forge_poller",
		Scanned: len(deltas),
		Payload: deltas,
	})
	p.ulog.Debug("Applied review stats deltas").Field("workspaces", len(deltas)).Log(ctx)
}

func planStatusOf(ws *models.EnrichedWorkspace) string {
	if ws == nil || ws.PlanStats == nil {
		return ""
	}
	return ws.PlanStats.PlanStatus
}

// due reports whether a repo's backoff window has elapsed.
func (p *Poller) due(repo forge.Repo, now time.Time) bool {
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	next, ok := p.nextTry[repo.String()]
	return !ok || !now.Before(next)
}

// penalize extends a failing repo's backoff exponentially, capped, and
// jittered so a fleet of repos failing on the same outage does not retry in
// lockstep.
func (p *Poller) penalize(repo forge.Repo, now time.Time) {
	key := repo.String()
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	n := p.failures[key] + 1
	p.failures[key] = n
	steps := n
	if steps > maxBackoffSteps {
		steps = maxBackoffSteps
	}
	p.nextTry[key] = now.Add(p.jittered(p.interval * time.Duration(1<<steps)))
}

func (p *Poller) clearPenalty(repo forge.Repo) {
	key := repo.String()
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	delete(p.failures, key)
	delete(p.nextTry, key)
}

// jittered spreads a duration by ±10% so sweeps do not align across restarts
// or across repos coming off backoff together.
func (p *Poller) jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	p.randMu.Lock()
	f := p.rand.Float64()
	p.randMu.Unlock()
	spread := float64(d) * 0.2 * (f - 0.5)
	out := time.Duration(float64(d) + spread)
	if out <= 0 {
		return d
	}
	return out
}

// normalizeHost matches the normalization forge.ParseRemoteURL applies to
// Repo.Host, so a configured host compares equal to a resolved one.
func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSpace(h))
}

var (
	errNoStore    = errors.New("forgepoll: no store supplied")
	errNoProvider = errors.New("forgepoll: no provider supplied")
)

// panicError turns a recovered provider panic into a classified forge error,
// so it lands in the cache looking like every other failure rather than as an
// unexplained blank.
func panicError(repo forge.Repo, r any) error {
	return forge.Errorf(forge.ClassPermanent, "forgepoll", "Sweep", nil,
		"provider panicked polling %s: %v", repo.String(), r)
}
