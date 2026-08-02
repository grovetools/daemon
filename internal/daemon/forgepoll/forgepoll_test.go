package forgepoll

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/forge"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// ---------------------------------------------------------------------------
// Fakes. Nothing in this file touches the network or a real `gh` (D7): the
// provider is a fake forge.Provider, and remotes are supplied through the
// ListRemotes seam rather than read off disk.
// ---------------------------------------------------------------------------

type fakeProvider struct {
	mu sync.Mutex

	prs    map[string][]forge.PullRequest
	checks map[string]forge.CheckRollup

	listErr   error
	checksErr error
	panicOn   bool

	listCalls   int
	checksCalls int
}

var _ forge.Provider = (*fakeProvider)(nil)

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		prs:    make(map[string][]forge.PullRequest),
		checks: make(map[string]forge.CheckRollup),
	}
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Capabilities() forge.Capabilities {
	return forge.Capabilities{forge.CapListPRs: forge.SupportSupported}
}

func (f *fakeProvider) ResolveRepo(remoteURL string) (forge.Repo, error) {
	return forge.ParseRemoteURL(remoteURL)
}

func (f *fakeProvider) ListPRs(_ context.Context, repo forge.Repo, _ forge.ListPROptions) ([]forge.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.panicOn {
		panic("provider exploded")
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]forge.PullRequest(nil), f.prs[repo.String()]...), nil
}

func (f *fakeProvider) GetPR(context.Context, forge.Repo, int) (forge.PullRequest, error) {
	return forge.PullRequest{}, errors.New("not used")
}

func (f *fakeProvider) Checks(_ context.Context, _ forge.Repo, ref string) (forge.CheckRollup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checksCalls++
	if f.checksErr != nil {
		return forge.UnknownRollup(ref), f.checksErr
	}
	if r, ok := f.checks[ref]; ok {
		return r, nil
	}
	return forge.CheckRollup{Ref: ref, State: forge.CheckStateNone}, nil
}

func (f *fakeProvider) setPRs(repo string, prs ...forge.PullRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[repo] = prs
}

func (f *fakeProvider) setRollup(ref string, state forge.CheckState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[ref] = forge.CheckRollup{Ref: ref, State: state}
}

func (f *fakeProvider) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
}

func (f *fakeProvider) heal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = nil
}

// fakeStore is the narrow storeAPI the poller writes through, recording every
// update so a test can assert on the SSE/delta traffic a sweep produced.
type fakeStore struct {
	mu      sync.Mutex
	state   store.State
	updates []store.Update
}

func newFakeStore(paths ...string) *fakeStore {
	ws := make(map[string]*models.EnrichedWorkspace, len(paths))
	for _, p := range paths {
		ws[p] = &models.EnrichedWorkspace{
			WorkspaceNode: &workspace.WorkspaceNode{Path: p, Name: p},
		}
	}
	return &fakeStore{state: store.State{Workspaces: ws}}
}

func (s *fakeStore) Get() store.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// ApplyUpdate mirrors the real store closely enough for these tests: it
// applies ReviewStats deltas onto the workspace map (so the next sweep's
// change comparison sees what a real daemon would) and records everything.
func (s *fakeStore) ApplyUpdate(u store.Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, u)
	if u.Type != store.UpdateWorkspacesDelta {
		return
	}
	deltas, ok := u.Payload.([]*models.WorkspaceDelta)
	if !ok {
		return
	}
	for _, d := range deltas {
		if ws, exists := s.state.Workspaces[d.Path]; exists && d.ReviewStats != nil {
			ws.ReviewStats = d.ReviewStats
		}
	}
}

func (s *fakeStore) drain() []store.Update {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.updates
	s.updates = nil
	return out
}

func (s *fakeStore) reviewStats(path string) *models.ReviewStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.state.Workspaces[path]
	if !ok {
		return nil
	}
	return ws.ReviewStats
}

// clock is a hand-cranked time source so staleness is exercised by advancing
// it, never by sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	poller   *Poller
	store    *fakeStore
	provider *fakeProvider
	clock    *clock
}

const (
	testPath   = "/ws/demo"
	testRemote = "git@github.com:grovetools/demo.git"
	testRepoID = "github.com/grovetools/demo"
)

func newHarness(t *testing.T, remotes map[string][]git.Remote) *harness {
	t.Helper()
	paths := make([]string, 0, len(remotes))
	for p := range remotes {
		paths = append(paths, p)
	}
	st := newFakeStore(paths...)
	prov := newFakeProvider()
	clk := newClock()

	p, err := New(Options{
		Store:      st,
		Provider:   prov,
		Interval:   5 * time.Minute,
		StaleAfter: 15 * time.Minute,
		Now:        clk.now,
		// A fixed seed keeps jitter deterministic; the sweeps under test are
		// driven directly, so the jitter only matters for backoff arithmetic.
		Rand: rand.New(rand.NewSource(1)),
		ListRemotes: func(path string) ([]git.Remote, error) {
			r, ok := remotes[path]
			if !ok {
				return nil, errors.New("no such repo")
			}
			return r, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{poller: p, store: st, provider: prov, clock: clk}
}

func singleRepoHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, map[string][]git.Remote{
		testPath: {{Name: "origin", URL: testRemote}},
	})
}

func openPR(number int, headSHA string) forge.PullRequest {
	return forge.PullRequest{
		Number:  number,
		Title:   "a change",
		State:   forge.PRStateOpen,
		HeadSHA: headSHA,
		HeadRef: "feature",
	}
}

// forgeFrames extracts the forge_state payloads from a batch of updates.
func forgeFrames(updates []store.Update) []*store.ForgeStatePayload {
	var out []*store.ForgeStatePayload
	for _, u := range updates {
		if u.Type != store.UpdateForgeState {
			continue
		}
		if p, ok := u.Payload.(*store.ForgeStatePayload); ok {
			out = append(out, p)
		}
	}
	return out
}

func workspaceDeltas(updates []store.Update) []*models.WorkspaceDelta {
	var out []*models.WorkspaceDelta
	for _, u := range updates {
		if u.Type != store.UpdateWorkspacesDelta {
			continue
		}
		if d, ok := u.Payload.([]*models.WorkspaceDelta); ok {
			out = append(out, d...)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestSweepPopulatesReviewStats is the happy path: an enabled poller with a
// fake provider projects PR + checks state onto the workspace enrichment.
func TestSweepPopulatesReviewStats(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.setPRs(testRepoID,
		openPR(1, "sha1"),
		forge.PullRequest{Number: 2, State: forge.PRStateOpen, IsDraft: true, HeadSHA: "sha2"},
		forge.PullRequest{Number: 3, State: forge.PRStateMerged},
		forge.PullRequest{Number: 4, State: forge.PRStateClosed},
		forge.PullRequest{Number: 5, State: forge.ParsePRState("wat")},
	)
	h.provider.setRollup("sha1", forge.CheckStateSuccess)
	h.provider.setRollup("sha2", forge.CheckStateSuccess)

	h.poller.Sweep(context.Background())

	stats := h.store.reviewStats(testPath)
	if stats == nil {
		t.Fatal("ReviewStats not attached to the workspace")
	}
	if stats.SchemaVersion != models.ReviewStatsSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", stats.SchemaVersion, models.ReviewStatsSchemaVersion)
	}
	if stats.Freshness != models.ReviewFreshnessFresh {
		t.Errorf("Freshness = %q, want fresh", stats.Freshness)
	}
	if stats.Repo != testRepoID {
		t.Errorf("Repo = %q, want %q", stats.Repo, testRepoID)
	}
	if stats.PRs == nil {
		t.Fatal("PRs = nil on a fresh entry")
	}
	want := models.PRCounts{Open: 2, Draft: 1, Merged: 1, Closed: 1, Unknown: 1}
	if *stats.PRs != want {
		t.Errorf("PRs = %+v, want %+v", *stats.PRs, want)
	}
	if stats.Checks != forge.CheckStateSuccess {
		t.Errorf("Checks = %q, want success", stats.Checks)
	}
	if stats.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero on a fresh entry")
	}
	// Checks are fetched for OPEN pull requests only — a merged PR's CI state
	// is history and paying a round trip for it is how a poller becomes a
	// rate-limit problem.
	if h.provider.checksCalls != 2 {
		t.Errorf("Checks called %d times, want 2 (one per open PR)", h.provider.checksCalls)
	}
}

// TestUnknownBeforeFirstFetch pins D4's floor: before anything succeeds, the
// projection says unknown and carries NO counts. A consumer must be able to
// tell "don't know" from "no PRs".
func TestUnknownBeforeFirstFetch(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.fail(forge.Errorf(forge.ClassUnavailable, "fake", "ListPRs", nil, "gh is not logged in"))

	h.poller.Sweep(context.Background())

	stats := h.store.reviewStats(testPath)
	if stats == nil {
		t.Fatal("ReviewStats not attached — a failing poll must still say 'unknown'")
	}
	if stats.Freshness != models.ReviewFreshnessUnknown {
		t.Errorf("Freshness = %q, want unknown", stats.Freshness)
	}
	if stats.Freshness.Known() {
		t.Error("Known() = true for an unknown entry")
	}
	if stats.PRs != nil {
		t.Errorf("PRs = %+v on a never-fetched entry, want nil — zeros would read as 'no PRs'", *stats.PRs)
	}
	if stats.Checks.IsGreen() {
		t.Errorf("Checks = %q is green on a never-fetched entry", stats.Checks)
	}
	if stats.Checks != forge.CheckStateUnknown {
		t.Errorf("Checks = %q, want unknown", stats.Checks)
	}
	if stats.LastError == "" {
		t.Error("LastError is empty after a failed attempt")
	}
	if !stats.FetchedAt.IsZero() {
		t.Error("FetchedAt advanced on a failed attempt")
	}
}

// TestFailureDegradesToStaleAndKeepsData is the central D4 transition: a poll
// failure degrades a good entry, it never evicts it to "no PRs".
func TestFailureDegradesToStaleAndKeepsData(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.setPRs(testRepoID, openPR(1, "sha1"))
	h.provider.setRollup("sha1", forge.CheckStateSuccess)

	h.poller.Sweep(context.Background())
	fresh := h.store.reviewStats(testPath)
	if fresh.Freshness != models.ReviewFreshnessFresh || fresh.PRs.Open != 1 {
		t.Fatalf("setup: want one open PR and fresh, got %+v", fresh)
	}
	fetchedAt := fresh.FetchedAt

	h.provider.fail(errors.New("network is down"))
	h.clock.advance(6 * time.Minute)
	h.poller.Sweep(context.Background())

	stale := h.store.reviewStats(testPath)
	if stale.Freshness != models.ReviewFreshnessStale {
		t.Errorf("Freshness = %q after a failure, want stale", stale.Freshness)
	}
	if stale.PRs == nil {
		t.Fatal("PRs went nil on failure — the last known good data must survive")
	}
	if stale.PRs.Open != 1 {
		t.Errorf("Open = %d after a failure, want the retained 1 — a failure is not 'no PRs'", stale.PRs.Open)
	}
	if !stale.FetchedAt.Equal(fetchedAt) {
		t.Errorf("FetchedAt moved on a failed poll: %v -> %v", fetchedAt, stale.FetchedAt)
	}
	if stale.LastError == "" {
		t.Error("LastError is empty on a stale entry")
	}

	// And it recovers: a later success restores fresh. The wait clears the
	// repo's backoff window as well as the clock — a failed repo is not
	// retried on the very next sweep (see TestBackoffSkipsFailingRepo).
	h.provider.heal()
	h.clock.advance(30 * time.Minute)
	h.poller.Sweep(context.Background())
	if got := h.store.reviewStats(testPath).Freshness; got != models.ReviewFreshnessFresh {
		t.Errorf("Freshness = %q after recovery, want fresh", got)
	}
}

// TestAgeAloneDegradesToStale covers the other route into stale: nothing
// failed, the data simply got old. Reached without a failure so the two causes
// are pinned independently.
func TestAgeAloneDegradesToStale(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.setPRs(testRepoID, openPR(1, "sha1"))
	h.poller.Sweep(context.Background())
	if got := h.store.reviewStats(testPath).Freshness; got != models.ReviewFreshnessFresh {
		t.Fatalf("setup: Freshness = %q, want fresh", got)
	}

	// Age past StaleAfter without polling, then sweep with the repo held off by
	// nothing but the clock.
	h.clock.advance(20 * time.Minute)
	snap := h.poller.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() = %d entries, want 1", len(snap))
	}
	if snap[0].State != store.ForgeStateStale {
		t.Errorf("State = %q for data older than StaleAfter, want stale", snap[0].State)
	}
	if snap[0].LastError != "" {
		t.Errorf("LastError = %q on an aged-out (not failed) entry", snap[0].LastError)
	}
}

// TestUnchangedSweepEmitsNothing is the emit-on-change contract: re-reading
// identical state must produce no SSE frame and no workspace delta.
func TestUnchangedSweepEmitsNothing(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.setPRs(testRepoID, openPR(1, "sha1"))
	h.provider.setRollup("sha1", forge.CheckStateSuccess)

	h.poller.Sweep(context.Background())
	first := h.store.drain()
	if len(forgeFrames(first)) != 1 {
		t.Fatalf("first sweep emitted %d forge_state frames, want 1", len(forgeFrames(first)))
	}
	if len(workspaceDeltas(first)) != 1 {
		t.Fatalf("first sweep emitted %d workspace deltas, want 1", len(workspaceDeltas(first)))
	}

	// Two more identical sweeps, with the clock advancing (so fetched_at moves)
	// but the data unchanged. Both must be silent.
	for i := 0; i < 2; i++ {
		h.clock.advance(5 * time.Minute)
		h.poller.Sweep(context.Background())
	}
	quiet := h.store.drain()
	if n := len(forgeFrames(quiet)); n != 0 {
		t.Errorf("unchanged sweeps emitted %d forge_state frames, want 0", n)
	}
	if n := len(workspaceDeltas(quiet)); n != 0 {
		t.Errorf("unchanged sweeps emitted %d workspace deltas, want 0", n)
	}

	// A real change still gets through.
	h.provider.setPRs(testRepoID, openPR(1, "sha1"), openPR(2, "sha2"))
	h.clock.advance(5 * time.Minute)
	h.poller.Sweep(context.Background())
	changed := h.store.drain()
	if n := len(forgeFrames(changed)); n != 1 {
		t.Errorf("a changed sweep emitted %d forge_state frames, want 1", n)
	}
	if n := len(workspaceDeltas(changed)); n != 1 {
		t.Errorf("a changed sweep emitted %d workspace deltas, want 1", n)
	}
}

// TestReorderedPRsAreNotAChange guards the fingerprint: forges do not promise
// a stable list order, and a reshuffled-but-identical list is not news.
func TestReorderedPRsAreNotAChange(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.setPRs(testRepoID, openPR(1, "sha1"), openPR(2, "sha2"))
	h.poller.Sweep(context.Background())
	h.store.drain()

	h.provider.setPRs(testRepoID, openPR(2, "sha2"), openPR(1, "sha1"))
	h.clock.advance(5 * time.Minute)
	h.poller.Sweep(context.Background())

	if n := len(forgeFrames(h.store.drain())); n != 0 {
		t.Errorf("a reordered PR list emitted %d frames, want 0", n)
	}
}

// TestRestartConvergesWithoutDuplicateEvents is the restart discipline: a new
// poller over an unchanged forge converges on the same cache, and — because
// the daemon's workspace state survived — emits no workspace delta that
// contradicts what consumers already hold.
func TestRestartConvergesWithoutDuplicateEvents(t *testing.T) {
	remotes := map[string][]git.Remote{testPath: {{Name: "origin", URL: testRemote}}}
	h := newHarness(t, remotes)
	h.provider.setPRs(testRepoID, openPR(1, "sha1"))
	h.provider.setRollup("sha1", forge.CheckStateFailure)

	h.poller.Sweep(context.Background())
	before := h.poller.Snapshot()
	statsBefore := h.store.reviewStats(testPath)
	h.store.drain()

	// Restart: a brand new poller (empty cache, no fingerprints) over the same
	// store and the same forge.
	restarted, err := New(Options{
		Store:       h.store,
		Provider:    h.provider,
		Interval:    5 * time.Minute,
		StaleAfter:  15 * time.Minute,
		Now:         h.clock.now,
		Rand:        rand.New(rand.NewSource(1)),
		ListRemotes: func(p string) ([]git.Remote, error) { return remotes[p], nil },
	})
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	h.clock.advance(2 * time.Minute)
	restarted.Sweep(context.Background())

	after := restarted.Snapshot()
	if len(before) != len(after) {
		t.Fatalf("cache size %d before restart, %d after", len(before), len(after))
	}
	if before[0].Repo != after[0].Repo || before[0].State != after[0].State {
		t.Errorf("cache did not converge: %+v vs %+v", before[0], after[0])
	}

	updates := h.store.drain()
	// The restarted poller has no fingerprint history, so it re-announces the
	// state once over SSE — that is a resend, not a contradiction.
	if n := len(forgeFrames(updates)); n != 1 {
		t.Errorf("restart emitted %d forge_state frames, want exactly 1 resend", n)
	}
	// The workspace projection, however, is compared against the daemon state
	// that survived the poller — so it must be silent.
	if n := len(workspaceDeltas(updates)); n != 0 {
		t.Errorf("restart emitted %d workspace deltas for unchanged state, want 0", n)
	}
	if statsAfter := h.store.reviewStats(testPath); !reviewStatsEqual(statsBefore, statsAfter) {
		t.Errorf("ReviewStats diverged across restart: %+v vs %+v", statsBefore, statsAfter)
	}
}

// TestChecksFailureNeverRollsUpGreen: a PR whose checks call failed contributes
// unknown, and unknown outranks success in the rollup.
func TestChecksFailureNeverRollsUpGreen(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.setPRs(testRepoID, openPR(1, "sha1"), openPR(2, "sha2"))
	h.provider.setRollup("sha1", forge.CheckStateSuccess)
	h.provider.checksErr = errors.New("checks API is having a day")

	h.poller.Sweep(context.Background())

	stats := h.store.reviewStats(testPath)
	if stats.Freshness != models.ReviewFreshnessFresh {
		t.Errorf("Freshness = %q — a checks failure must not fail the PR list", stats.Freshness)
	}
	if stats.PRs.Open != 2 {
		t.Errorf("Open = %d, want 2 — the PR list succeeded", stats.PRs.Open)
	}
	if stats.Checks.IsGreen() {
		t.Errorf("Checks = %q is green despite an unreadable rollup", stats.Checks)
	}
	if stats.Checks != forge.CheckStateUnknown {
		t.Errorf("Checks = %q, want unknown", stats.Checks)
	}
}

// TestNoOpenPRsIsNoneNotGreen: zero open PRs means there is nothing to check,
// which is CheckStateNone — affirmatively not a pass.
func TestNoOpenPRsIsNoneNotGreen(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.setPRs(testRepoID, forge.PullRequest{Number: 9, State: forge.PRStateMerged})

	h.poller.Sweep(context.Background())

	stats := h.store.reviewStats(testPath)
	if stats.PRs == nil || stats.PRs.Open != 0 {
		t.Fatalf("PRs = %+v, want a non-nil zero-open count", stats.PRs)
	}
	if stats.Checks.IsGreen() {
		t.Errorf("Checks = %q is green with no open PRs", stats.Checks)
	}
	if stats.Checks != forge.CheckStateNone {
		t.Errorf("Checks = %q, want none", stats.Checks)
	}
}

// TestProviderPanicIsContained: a misbehaving provider costs one repo's
// freshness, never the daemon's goroutine.
func TestProviderPanicIsContained(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.panicOn = true

	h.poller.Sweep(context.Background()) // must not panic out

	stats := h.store.reviewStats(testPath)
	if stats == nil || stats.Freshness != models.ReviewFreshnessUnknown {
		t.Fatalf("ReviewStats = %+v after a provider panic, want an unknown entry", stats)
	}
	if stats.LastError == "" {
		t.Error("a contained panic left no LastError")
	}
}

// TestNonAllowlistedHostIsSkipped: ResolveRepo is pure parsing and accepts any
// host, so the poller — not the provider — is what keeps a GitHub client from
// being asked about someone else's forge.
func TestNonAllowlistedHostIsSkipped(t *testing.T) {
	h := newHarness(t, map[string][]git.Remote{
		"/ws/elsewhere": {{Name: "origin", URL: "git@git.example.com:team/thing.git"}},
	})
	h.poller.Sweep(context.Background())

	if h.provider.listCalls != 0 {
		t.Errorf("ListPRs called %d times for an off-allowlist host, want 0", h.provider.listCalls)
	}
	if got := h.store.reviewStats("/ws/elsewhere"); got != nil {
		t.Errorf("ReviewStats = %+v for an off-allowlist workspace, want nil", got)
	}
}

// TestWorkspaceWithoutRemoteIsSkipped: the local-first norm — a repo with no
// origin at all is not an error, it just has no forge identity.
func TestWorkspaceWithoutRemoteIsSkipped(t *testing.T) {
	h := newHarness(t, map[string][]git.Remote{"/ws/local-only": {}})
	h.poller.Sweep(context.Background())

	if h.provider.listCalls != 0 {
		t.Errorf("ListPRs called %d times for a remote-less repo, want 0", h.provider.listCalls)
	}
	if len(h.store.drain()) != 0 {
		t.Error("a remote-less workspace produced updates")
	}
}

// TestSharedRepoIsPolledOnce: a dozen worktrees of one repository are one
// repository. Polling per worktree would multiply every sweep by the number of
// open plans.
func TestSharedRepoIsPolledOnce(t *testing.T) {
	h := newHarness(t, map[string][]git.Remote{
		"/ws/a": {{Name: "origin", URL: testRemote}},
		"/ws/b": {{Name: "origin", URL: testRemote}},
		"/ws/c": {{Name: "origin", URL: testRemote}},
	})
	h.provider.setPRs(testRepoID, openPR(1, "sha1"))

	h.poller.Sweep(context.Background())

	if h.provider.listCalls != 1 {
		t.Errorf("ListPRs called %d times for three worktrees of one repo, want 1", h.provider.listCalls)
	}
	// All three worktrees still get the projection.
	for _, p := range []string{"/ws/a", "/ws/b", "/ws/c"} {
		if h.store.reviewStats(p) == nil {
			t.Errorf("workspace %s got no ReviewStats", p)
		}
	}
}

// TestBackoffSkipsFailingRepo: a repo that just failed is not retried on the
// next sweep, and one dead repo does not stall its neighbours.
func TestBackoffSkipsFailingRepo(t *testing.T) {
	h := singleRepoHarness(t)
	h.provider.fail(errors.New("rate limited"))

	h.poller.Sweep(context.Background())
	if h.provider.listCalls != 1 {
		t.Fatalf("setup: ListPRs called %d times, want 1", h.provider.listCalls)
	}

	// Immediately after the failure the repo is inside its backoff window.
	h.clock.advance(1 * time.Minute)
	h.poller.Sweep(context.Background())
	if h.provider.listCalls != 1 {
		t.Errorf("ListPRs called %d times inside the backoff window, want 1", h.provider.listCalls)
	}

	// Well past it, the poller tries again.
	h.clock.advance(30 * time.Minute)
	h.poller.Sweep(context.Background())
	if h.provider.listCalls != 2 {
		t.Errorf("ListPRs called %d times after the backoff elapsed, want 2", h.provider.listCalls)
	}
}

// TestPlanStatusRidesAlong: the projection carries the local plan status so a
// review surface can show forge state and plan state together.
func TestPlanStatusRidesAlong(t *testing.T) {
	h := singleRepoHarness(t)
	h.store.state.Workspaces[testPath].PlanStats = &models.PlanStats{PlanStatus: "running"}
	h.provider.setPRs(testRepoID, openPR(1, "sha1"))

	h.poller.Sweep(context.Background())

	if got := h.store.reviewStats(testPath).PlanStatus; got != "running" {
		t.Errorf("PlanStatus = %q, want %q", got, "running")
	}
}

// TestNewRejectsMissingDependencies keeps construction honest: only a
// programming mistake is an error. Operational conditions are runtime states.
func TestNewRejectsMissingDependencies(t *testing.T) {
	if _, err := New(Options{Provider: newFakeProvider()}); err == nil {
		t.Error("New() = nil error with no store")
	}
	if _, err := New(Options{Store: newFakeStore()}); err == nil {
		t.Error("New() = nil error with no provider")
	}
}

// TestSweepIsSafeWithNoWorkspaces: a daemon with nothing tracked yet must not
// emit, spin, or fail.
func TestSweepIsSafeWithNoWorkspaces(t *testing.T) {
	h := newHarness(t, map[string][]git.Remote{})
	h.poller.Sweep(context.Background())
	if n := len(h.store.drain()); n != 0 {
		t.Errorf("an empty ecosystem produced %d updates, want 0", n)
	}
	if n := len(h.poller.Snapshot()); n != 0 {
		t.Errorf("Snapshot() = %d entries for an empty ecosystem, want 0", n)
	}
}

// TestStartStopsOnContextCancel: the poll loop is a daemon-lifetime goroutine
// and must exit with its context.
func TestStartStopsOnContextCancel(t *testing.T) {
	h := singleRepoHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.poller.Start(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}
