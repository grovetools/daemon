package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/forge"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestConvertForgeStateReachesWire proves the forge_state wire layers agree:
// store.Update → convertToAPIUpdate → JSON. A missing case here would drop the
// poller's broadcasts silently — the whole "one poller, many consumers" data
// flow ends at this switch.
func TestConvertForgeStateReachesWire(t *testing.T) {
	fetched := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	payload := &store.ForgeStatePayload{Repos: []store.ForgeRepoState{{
		Provider:      "github",
		Repo:          "github.com/grovetools/demo",
		State:         store.ForgeStateStale,
		FetchedAt:     fetched,
		LastAttemptAt: fetched.Add(5 * time.Minute),
		PRs:           []forge.PullRequest{{Number: 7, State: forge.PRStateOpen, HeadSHA: "abc"}},
		Checks:        map[int]forge.CheckRollup{7: {Ref: "abc", State: forge.CheckStateFailure}},
		LastError:     "network is down",
	}}}

	apiUpdate := convertToAPIUpdate(store.Update{
		Type:    store.UpdateForgeState,
		Source:  "forge_poller",
		Scanned: 1,
		Payload: payload,
	})
	if apiUpdate == nil {
		t.Fatal("convertToAPIUpdate dropped forge_state — the event never reaches the SSE wire")
	}
	if apiUpdate.UpdateType != "forge_state" {
		t.Fatalf("update_type = %q, want %q", apiUpdate.UpdateType, "forge_state")
	}

	data, err := json.Marshal(apiUpdate)
	if err != nil {
		t.Fatalf("marshal apiStateUpdate: %v", err)
	}
	var wire struct {
		UpdateType string `json:"update_type"`
		Payload    struct {
			Repos []store.ForgeRepoState `json:"repos"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal wire JSON: %v", err)
	}
	if wire.UpdateType != "forge_state" {
		t.Errorf("wire update_type = %q", wire.UpdateType)
	}
	if len(wire.Payload.Repos) != 1 {
		t.Fatalf("wire carried %d repos, want 1", len(wire.Payload.Repos))
	}
	got := wire.Payload.Repos[0]
	if got.State != store.ForgeStateStale {
		t.Errorf("state = %q, want stale — staleness must survive the wire", got.State)
	}
	if !got.FetchedAt.Equal(fetched) {
		t.Errorf("fetched_at = %v, want %v", got.FetchedAt, fetched)
	}
	if got.Checks[7].State != forge.CheckStateFailure {
		t.Errorf("check rollup = %q, want failure", got.Checks[7].State)
	}
	if got.LastError != "network is down" {
		t.Errorf("last_error = %q", got.LastError)
	}
}

// TestReviewStatsSurvivesWorkspaceDeltaWire covers the poller's other channel:
// the ReviewStats projection rides the ordinary workspaces_delta path, and the
// unknown/no-PRs distinction has to survive JSON to be worth anything.
func TestReviewStatsSurvivesWorkspaceDeltaWire(t *testing.T) {
	cases := []struct {
		name  string
		stats *models.ReviewStats
	}{
		{
			name: "unknown carries no counts",
			stats: &models.ReviewStats{
				SchemaVersion: models.ReviewStatsSchemaVersion,
				Freshness:     models.ReviewFreshnessUnknown,
				Checks:        forge.CheckStateUnknown,
				LastError:     "gh is not logged in",
			},
		},
		{
			name: "fresh with zero PRs carries an empty count",
			stats: &models.ReviewStats{
				SchemaVersion: models.ReviewStatsSchemaVersion,
				Freshness:     models.ReviewFreshnessFresh,
				PRs:           &models.PRCounts{},
				Checks:        forge.CheckStateNone,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiUpdate := convertToAPIUpdate(store.Update{
				Type:    store.UpdateWorkspacesDelta,
				Source:  "forge_poller",
				Payload: []*models.WorkspaceDelta{{Path: "/ws/demo", ReviewStats: tc.stats}},
			})
			if apiUpdate == nil {
				t.Fatal("convertToAPIUpdate dropped the workspaces_delta")
			}
			data, err := json.Marshal(apiUpdate)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var wire struct {
				Deltas []*models.WorkspaceDelta `json:"workspace_deltas"`
			}
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(wire.Deltas) != 1 || wire.Deltas[0].ReviewStats == nil {
				t.Fatalf("review stats did not survive the wire: %+v", wire.Deltas)
			}
			got := wire.Deltas[0].ReviewStats
			if got.Freshness != tc.stats.Freshness {
				t.Errorf("freshness = %q, want %q", got.Freshness, tc.stats.Freshness)
			}
			if (got.PRs == nil) != (tc.stats.PRs == nil) {
				t.Errorf("PRs presence flipped across the wire: got %v, want %v", got.PRs, tc.stats.PRs)
			}
			if got.Checks.IsGreen() {
				t.Errorf("checks = %q decoded as green", got.Checks)
			}
		})
	}
}
