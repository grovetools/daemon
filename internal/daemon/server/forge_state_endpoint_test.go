package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/forge"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// fakeSnapshotter stands in for the poller's read seam.
type fakeSnapshotter struct{ repos []store.ForgeRepoState }

func (f fakeSnapshotter) Snapshot() []store.ForgeRepoState { return f.repos }

// decodeForgeState drives the handler and decodes its payload.
func decodeForgeState(t *testing.T, s *Server) models.ForgeStateSnapshot {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleGetForgeState(rec, httptest.NewRequest(http.MethodGet, "/api/forge/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got models.ForgeStateSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	return got
}

// TestForgeStateEndpoint_NoPollerIsDisabledNotEmpty is the endpoint's reason
// for existing in this shape. A daemon with no poller must answer 200 with
// enabled=false — NOT 404, and NOT an empty list, which every consumer would
// render as "there are no pull requests" (STATE.md D4).
func TestForgeStateEndpoint_NoPollerIsDisabledNotEmpty(t *testing.T) {
	got := decodeForgeState(t, New(false))
	if got.Enabled {
		t.Fatal("a daemon with no poller must report enabled=false")
	}
	if len(got.Repos) != 0 {
		t.Errorf("a disabled poller must carry no repos, got %d", len(got.Repos))
	}
	if got.Provider != "" {
		t.Errorf("Provider = %q, want empty when disabled", got.Provider)
	}
}

// TestForgeStateEndpoint_ServesTheCache proves a wired poller's cache reaches
// the wire intact, including the fields a consumer needs to date it.
func TestForgeStateEndpoint_ServesTheCache(t *testing.T) {
	fetched := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	s := New(false)
	s.SetForgeSnapshotter("github", fakeSnapshotter{repos: []store.ForgeRepoState{{
		Provider:  "github",
		Repo:      "github.com/acme/flow",
		State:     store.ForgeStateFresh,
		FetchedAt: fetched,
		PRs:       []forge.PullRequest{{Number: 7, Title: "t", State: forge.PRStateOpen, HeadSHA: "abc"}},
		Checks:    map[int]forge.CheckRollup{7: {Ref: "abc", State: forge.CheckStateFailure}},
	}}})

	got := decodeForgeState(t, s)
	if !got.Enabled || got.Provider != "github" {
		t.Fatalf("enabled=%v provider=%q, want true/github", got.Enabled, got.Provider)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(got.Repos))
	}
	repo := got.Repos[0]
	if repo.Repo != "github.com/acme/flow" || repo.State != models.ForgeStateFresh {
		t.Errorf("repo = %+v", repo)
	}
	if !repo.FetchedAt.Equal(fetched) {
		t.Errorf("FetchedAt = %v, want %v", repo.FetchedAt, fetched)
	}
	if len(repo.PRs) != 1 || repo.PRs[0].Number != 7 {
		t.Errorf("PRs = %+v", repo.PRs)
	}
	if repo.Checks[7].State != forge.CheckStateFailure {
		t.Errorf("check rollup = %+v, want failure", repo.Checks[7])
	}
}

// TestForgeStateEndpoint_NilPRsSurvivesTheWire pins the nil/empty distinction
// end to end. An entry that was never successfully fetched must arrive with PRs
// still nil: normalizing it to an empty slice anywhere on this path would turn
// "we have never asked" into "the forge says there are none".
func TestForgeStateEndpoint_NilPRsSurvivesTheWire(t *testing.T) {
	s := New(false)
	s.SetForgeSnapshotter("github", fakeSnapshotter{repos: []store.ForgeRepoState{
		{Repo: "github.com/acme/never", State: store.ForgeStateUnknown, PRs: nil},
		{Repo: "github.com/acme/empty", State: store.ForgeStateFresh, PRs: []forge.PullRequest{}},
	}})

	got := decodeForgeState(t, s)
	byRepo := map[string]models.ForgeRepoState{}
	for _, r := range got.Repos {
		byRepo[r.Repo] = r
	}
	if byRepo["github.com/acme/never"].PRs != nil {
		t.Error("a never-fetched entry must keep PRs nil across the wire")
	}
	if byRepo["github.com/acme/never"].State != models.ForgeStateUnknown {
		t.Errorf("state = %q, want unknown", byRepo["github.com/acme/never"].State)
	}
	// The empty case round-trips as nil too (encoding/json omitempty drops an
	// empty slice), which is why the daemon carries the distinction in State:
	// "fresh" plus no PRs is the affirmative "none", and a consumer reads it
	// from State, never from the slice's nil-ness alone.
	if byRepo["github.com/acme/empty"].State != models.ForgeStateFresh {
		t.Errorf("state = %q, want fresh", byRepo["github.com/acme/empty"].State)
	}
}

// TestForgeStateEndpoint_RejectsNonGET proves the read surface is a read: no
// verb here mutates anything, and anything but GET is refused outright.
func TestForgeStateEndpoint_RejectsNonGET(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		rec := httptest.NewRecorder()
		New(false).handleGetForgeState(rec, httptest.NewRequest(method, "/api/forge/state", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
	}
}

// TestSetForgeSnapshotter_NilClears proves the seam can be taken back down,
// and that doing so returns the endpoint to the honest "disabled" answer rather
// than to a stale cache.
func TestSetForgeSnapshotter_NilClears(t *testing.T) {
	s := New(false)
	s.SetForgeSnapshotter("github", fakeSnapshotter{repos: []store.ForgeRepoState{{Repo: "github.com/a/b"}}})
	if !decodeForgeState(t, s).Enabled {
		t.Fatal("expected enabled after wiring")
	}
	s.SetForgeSnapshotter("", nil)
	if decodeForgeState(t, s).Enabled {
		t.Fatal("clearing the seam must return the endpoint to disabled")
	}
}
