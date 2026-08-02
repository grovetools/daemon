package forgepoll

import (
	"github.com/grovetools/core/pkg/forge"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// reviewStatsFrom projects one cached repo state onto the workspace enrichment
// shape, alongside the local plan status the surface should read it with.
//
// The projection's whole job is to not lose the unknown/stale distinction on
// the way across. An entry the poller has never successfully fetched yields
// PRs == nil (absent counts), not an all-zero PRCounts (a claim that the forge
// reports nothing), and a rollup of forge.CheckStateUnknown, which is not green.
func reviewStatsFrom(state store.ForgeRepoState, planStatus string) *models.ReviewStats {
	stats := &models.ReviewStats{
		SchemaVersion: models.ReviewStatsSchemaVersion,
		Freshness:     freshnessOf(state.State),
		FetchedAt:     state.FetchedAt,
		Provider:      state.Provider,
		Repo:          state.Repo,
		Checks:        forge.CheckStateUnknown,
		PlanStatus:    planStatus,
		LastError:     state.LastError,
	}
	if !stats.Freshness.Known() {
		// Nothing below this line may be asserted: no counts, no rollup.
		return stats
	}

	counts := &models.PRCounts{}
	var openRollups []forge.Check
	for _, pr := range state.PRs {
		switch pr.State.Normalized() {
		case forge.PRStateOpen:
			counts.Open++
			if pr.IsDraft {
				counts.Draft++
			}
			// A PR with no cached rollup contributes an unknown check, which
			// forge.RollupState ranks above pending and success — so a repo we
			// only partly measured never rolls up green.
			rollup, ok := state.Checks[pr.Number]
			if !ok {
				openRollups = append(openRollups, forge.Check{State: forge.CheckStateUnknown})
				continue
			}
			openRollups = append(openRollups, forge.Check{State: rollup.State.Normalized()})
		case forge.PRStateMerged:
			counts.Merged++
		case forge.PRStateClosed:
			counts.Closed++
		default:
			counts.Unknown++
		}
	}
	counts.Truncated = len(state.PRs) >= prFetchLimit
	stats.PRs = counts

	// forge.RollupState answers CheckStateNone for an empty set, which is the
	// right answer here too: no open pull requests means there is nothing to
	// check, not that checks passed.
	stats.Checks = forge.RollupState(openRollups)
	return stats
}

// freshnessOf maps the cache's wire state onto the enrichment vocabulary.
// Anything unrecognized becomes unknown — the safe direction.
func freshnessOf(state string) models.ReviewFreshness {
	switch state {
	case store.ForgeStateFresh:
		return models.ReviewFreshnessFresh
	case store.ForgeStateStale:
		return models.ReviewFreshnessStale
	default:
		return models.ReviewFreshnessUnknown
	}
}

// reviewStatsEqual reports whether two projections are materially the same, so
// an unchanged sweep produces no workspace delta.
//
// Timestamps are deliberately NOT compared — the same rule the cache's
// fingerprint follows, and for the same reason. A successful re-poll that finds
// nothing new advances the clock and nothing else; comparing it would emit a
// delta on every sweep forever, which is precisely the duplicate-event churn
// the restart/duplicate discipline exists to prevent. Freshness IS compared, so
// a fresh→stale transition still propagates immediately. (This is why
// ReviewStats.FetchedAt documents itself as a lower bound.)
func reviewStatsEqual(a, b *models.ReviewStats) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.SchemaVersion != b.SchemaVersion ||
		a.Freshness != b.Freshness ||
		a.Provider != b.Provider ||
		a.Repo != b.Repo ||
		a.Checks.Normalized() != b.Checks.Normalized() ||
		a.PlanStatus != b.PlanStatus ||
		a.LastError != b.LastError {
		return false
	}
	if a.PRs == nil || b.PRs == nil {
		return a.PRs == b.PRs
	}
	return *a.PRs == *b.PRs
}
