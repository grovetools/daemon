package forgepoll

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/pkg/forge"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// entry is one repository's cached state.
//
// The invariant every method here preserves: a failed fetch degrades the entry
// and keeps its data. Nothing in this file ever deletes an entry or replaces
// good data with an empty slice on failure — that is D4's "unknown is never
// green, and stale is never no-PRs" written as code rather than as a comment.
type entry struct {
	repo     forge.Repo
	provider string

	// fetchedAt is the last SUCCESSFUL fetch. Zero means never.
	fetchedAt time.Time
	// lastAttemptAt is the last attempt, successful or not.
	lastAttemptAt time.Time
	// prs is nil until the first success. Non-nil-but-empty is a real answer.
	prs    []forge.PullRequest
	checks map[int]forge.CheckRollup
	// lastErr is the most recent failure, cleared by a success.
	lastErr string
}

// stateAt derives the entry's state at a point in time. It is a pure function
// of (has data, last attempt outcome, age) so the same entry read twice at the
// same instant always answers the same thing.
func (e *entry) stateAt(now time.Time, staleAfter time.Duration) string {
	if e.fetchedAt.IsZero() {
		// Never fetched successfully. A failed attempt does not make this
		// "stale" — there is no last known good value to be stale about.
		return store.ForgeStateUnknown
	}
	if e.lastErr != "" {
		return store.ForgeStateStale
	}
	if now.Sub(e.fetchedAt) > staleAfter {
		return store.ForgeStateStale
	}
	return store.ForgeStateFresh
}

// snapshot renders the entry onto the wire shape.
func (e *entry) snapshot(now time.Time, staleAfter time.Duration) store.ForgeRepoState {
	out := store.ForgeRepoState{
		Provider:      e.provider,
		Repo:          e.repo.String(),
		State:         e.stateAt(now, staleAfter),
		FetchedAt:     e.fetchedAt,
		LastAttemptAt: e.lastAttemptAt,
		LastError:     e.lastErr,
	}
	if e.prs != nil {
		out.PRs = append([]forge.PullRequest(nil), e.prs...)
	}
	if len(e.checks) > 0 {
		out.Checks = make(map[int]forge.CheckRollup, len(e.checks))
		for n, r := range e.checks {
			out.Checks[n] = r
		}
	}
	return out
}

// fingerprint is the entry's MATERIAL identity: everything a consumer would
// render, and nothing else.
//
// This is what makes emit-on-change work, and it is why lastAttemptAt is
// absent from it. A sweep that re-fetches identical data advances
// lastAttemptAt and must broadcast nothing; a daemon restart that re-fetches
// the same data converges on the same fingerprint it had before. fetchedAt IS
// included only through the derived state, not directly — otherwise every
// successful re-poll would look like a change.
func (e *entry) fingerprint(now time.Time, staleAfter time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s", e.provider, e.repo.String(), e.stateAt(now, staleAfter), e.lastErr)
	if e.prs == nil {
		b.WriteString("|nil")
		return b.String()
	}
	fmt.Fprintf(&b, "|%d", len(e.prs))
	// PRs arrive in the forge's order, which is not guaranteed stable across
	// calls; sort by number so a reordered-but-identical list is not a change.
	ordered := append([]forge.PullRequest(nil), e.prs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Number < ordered[j].Number })
	for _, pr := range ordered {
		rollup := forge.CheckStateUnknown
		if r, ok := e.checks[pr.Number]; ok {
			rollup = r.State.Normalized()
		}
		fmt.Fprintf(&b, ";%d:%s:%t:%s:%s:%s",
			pr.Number, pr.State.Normalized(), pr.IsDraft, pr.HeadSHA, pr.Title, rollup)
	}
	return b.String()
}

// cache holds one entry per repository, keyed by the fully qualified repo
// identity. It is safe for concurrent use: the poll loop writes it and the
// projection/Snapshot readers read it.
type cache struct {
	mu           sync.RWMutex
	entries      map[string]*entry
	fingerprints map[string]string
	staleAfter   time.Duration
}

func newCache(staleAfter time.Duration) *cache {
	return &cache{
		entries:      make(map[string]*entry),
		fingerprints: make(map[string]string),
		staleAfter:   staleAfter,
	}
}

// ensure returns the entry for a repo, creating an unknown one if absent.
// A newly created entry is itself a change (unknown is information: "this repo
// is now being watched, and we don't know anything about it yet").
func (c *cache) ensure(repo forge.Repo, provider string) {
	key := repo.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		c.entries[key] = &entry{repo: repo, provider: provider}
	}
}

// recordSuccess replaces an entry's data with a completed fetch.
func (c *cache) recordSuccess(repo forge.Repo, provider string, now time.Time, prs []forge.PullRequest, checks map[int]forge.CheckRollup) {
	key := repo.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &entry{repo: repo, provider: provider}
		c.entries[key] = e
	}
	if prs == nil {
		// Normalize to a non-nil empty slice: a successful fetch that found
		// nothing is an answer, and the nil/non-nil distinction is reserved for
		// "never fetched".
		prs = []forge.PullRequest{}
	}
	e.prs = prs
	e.checks = checks
	e.fetchedAt = now
	e.lastAttemptAt = now
	e.lastErr = ""
}

// recordFailure degrades an entry. It never evicts and never clears data: an
// entry that had good PRs keeps them and becomes stale; an entry that never
// had any stays unknown.
func (c *cache) recordFailure(repo forge.Repo, provider string, now time.Time, err error) {
	key := repo.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &entry{repo: repo, provider: provider}
		c.entries[key] = e
	}
	e.lastAttemptAt = now
	e.lastErr = errText(err)
}

// changed returns the entries whose fingerprint differs from the last time
// changed was called, and records the new fingerprints. Calling it twice with
// no intervening mutation returns nothing the second time — that is the
// emit-on-change contract.
func (c *cache) changed(now time.Time) []store.ForgeRepoState {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []store.ForgeRepoState
	for key, e := range c.entries {
		fp := e.fingerprint(now, c.staleAfter)
		if prev, ok := c.fingerprints[key]; ok && prev == fp {
			continue
		}
		c.fingerprints[key] = fp
		out = append(out, e.snapshot(now, c.staleAfter))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

// snapshot returns every entry, for consumers that need the whole cache rather
// than a change frame.
func (c *cache) snapshot(now time.Time) []store.ForgeRepoState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]store.ForgeRepoState, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.snapshot(now, c.staleAfter))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

// lookup returns the wire snapshot for one repo, and whether it is cached.
func (c *cache) lookup(repo forge.Repo, now time.Time) (store.ForgeRepoState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[repo.String()]
	if !ok {
		return store.ForgeRepoState{}, false
	}
	return e.snapshot(now, c.staleAfter), true
}

// errText renders an error for the cache without leaking anything a log line
// would not already carry. Provider errors are already structured (*forge.Error
// carries class/provider/op) and never embed a token — the github impl shells
// out to `gh` and never handles credentials at all.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
