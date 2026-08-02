package store

import (
	"time"

	"github.com/grovetools/core/pkg/forge"
)

// Forge cache entry states, as carried on the wire by ForgeRepoState.State.
//
// They mirror models.ReviewFreshness and are deliberately the same three
// words: the SSE payload and the workspace enrichment must never disagree
// about whether the daemon knows something.
const (
	// ForgeStateUnknown means no successful fetch has ever landed for this
	// repo. PRs is nil, not empty — nothing about this repo may be rendered.
	ForgeStateUnknown = "unknown"
	// ForgeStateFresh means the last fetch succeeded inside the freshness
	// window.
	ForgeStateFresh = "fresh"
	// ForgeStateStale means a fetch succeeded once but the latest attempt
	// failed, or the data aged past the freshness window. The payload carries
	// the last known good data, dated by FetchedAt.
	ForgeStateStale = "stale"
)

// ForgeRepoState is the poller's cached knowledge of one repository, as it
// crosses the SSE wire.
//
// The load-bearing rule, and the reason PRs is a pointer: a poll failure
// degrades an entry to ForgeStateStale and keeps the data it already had. It
// never evicts the entry, and it never replaces the data with an empty slice —
// "we could not ask" must never render as "there are no pull requests".
type ForgeRepoState struct {
	// Provider is the forge provider name ("github").
	Provider string `json:"provider"`
	// Repo is the fully qualified "host/owner/name" identity. Slug alone is
	// ambiguous across forges.
	Repo string `json:"repo"`
	// State is one of the ForgeState* constants above.
	State string `json:"state"`
	// FetchedAt is when the carried data was last fetched SUCCESSFULLY. Zero
	// while State is unknown; it does NOT advance on a failed attempt.
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	// LastAttemptAt is when the poller last tried, successfully or not. The gap
	// between this and FetchedAt is how long the entry has been failing.
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	// PRs is nil when State is unknown, and a (possibly empty) slice otherwise.
	// An empty non-nil slice is the forge affirmatively reporting no pull
	// requests.
	PRs []forge.PullRequest `json:"prs,omitempty"`
	// Checks maps a pull request number to the rollup fetched for its head ref.
	// A PR absent from this map has an unknown rollup, never a green one.
	Checks map[int]forge.CheckRollup `json:"checks,omitempty"`
	// LastError is the most recent failure text, present whenever the last
	// attempt failed.
	LastError string `json:"last_error,omitempty"`
	// ConsecutiveFailures is how many sweeps in a row have failed for this
	// repo. Zero after any success.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// NextAttemptAt is when the poller will try this repo again. It is the
	// honest name for the quiet period after an outage: the per-repo backoff is
	// exponential (2^n intervals, capped), so a repo that failed a few times in
	// a row is deliberately not retried for minutes. Zero means "at the next
	// sweep". A surface that omits this renders that silence as a hang.
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
}

// ForgeStatePayload is the Payload of an UpdateForgeState broadcast: the repo
// entries whose state MATERIALLY changed on this sweep, never the whole cache.
//
// The poller emits only on change (a re-poll that returns identical data
// broadcasts nothing), so a subscriber that has seen every frame since the
// daemon booted holds the same cache the poller does. A subscriber that
// connected late reconciles the same way every other lossy-by-design update
// type here does: from the workspace enrichment it receives on connect.
//
// The daemon does NOT apply this to State — the poller owns its cache, and the
// projection that daemon state does keep (models.ReviewStats) arrives through
// the ordinary UpdateWorkspacesDelta path.
type ForgeStatePayload struct {
	// Repos are the changed entries, sorted by Repo for a deterministic frame.
	Repos []ForgeRepoState `json:"repos"`
}
