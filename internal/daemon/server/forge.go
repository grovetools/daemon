package server

import (
	"encoding/json"
	"net/http"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// The read-only forge surface: GET /api/forge/state.
//
// The poller (internal/daemon/forgepoll) owns the cache and every network call
// into a forge. This endpoint owns nothing — it renders whatever the poller
// currently holds. That split is the whole point: a surface may poll THIS
// endpoint as often as it likes without turning a user's keystroke into a forge
// request, which is what keeps "the daemon owns polling" true rather than
// aspirational.
//
// It is deliberately unconditional about failure: a daemon with no poller
// answers 200 with enabled=false, NOT 404 and not an empty list. An empty list
// and "the poller is off" are different facts, and a consumer that cannot tell
// them apart renders "no pull requests" for a machine that simply never asked
// (STATE.md D4).

// forgeSnapshotter is the narrow seam the poller satisfies. It exists as an
// interface rather than a *forgepoll.Poller so the server package does not
// depend on the poller (and so tests can wire a fixture).
type forgeSnapshotter interface {
	// Snapshot returns every cached repository entry, sorted by repo identity.
	Snapshot() []store.ForgeRepoState
}

// forgeReadSeam is what SetForgeSnapshotter stores: the seam plus the provider
// name, held together so a reader can never see one without the other.
type forgeReadSeam struct {
	provider string
	snap     forgeSnapshotter
}

// SetForgeSnapshotter wires the running forge poller's read seam. Called once
// at boot, and only on the global daemon and only when the poller actually
// started — leaving it nil is exactly how the endpoint learns to answer
// "enabled: false". Atomic like the other late-wired boot deps.
func (s *Server) SetForgeSnapshotter(provider string, snap forgeSnapshotter) {
	if snap == nil {
		s.forgeSource.Store(nil)
		return
	}
	s.forgeSource.Store(&forgeReadSeam{provider: provider, snap: snap})
}

// handleGetForgeState serves GET /api/forge/state.
func (s *Server) handleGetForgeState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	snapshot := models.ForgeStateSnapshot{}
	if src := s.forgeSource.Load(); src != nil && src.snap != nil {
		snapshot.Enabled = true
		snapshot.Provider = src.provider
		snapshot.Repos = convertForgeRepoStates(src.snap.Snapshot())
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

// convertForgeRepoStates maps the daemon's internal cache entries onto the
// public wire shape.
//
// The two structs are field-for-field identical today, so this looks like
// ceremony — it is not. store.ForgeRepoState is an INTERNAL type free to grow a
// field the daemon needs and no client should see; models.ForgeRepoState is the
// published contract. Writing the mapping out means adding a field to one is a
// compile-time decision about whether it belongs in the other, rather than an
// accidental disclosure via struct tags.
func convertForgeRepoStates(in []store.ForgeRepoState) []models.ForgeRepoState {
	if in == nil {
		return nil
	}
	out := make([]models.ForgeRepoState, 0, len(in))
	for _, e := range in {
		out = append(out, models.ForgeRepoState{
			Provider:      e.Provider,
			Repo:          e.Repo,
			State:         e.State,
			FetchedAt:     e.FetchedAt,
			LastAttemptAt: e.LastAttemptAt,
			// PRs is passed through by reference-of-slice deliberately: nil must
			// stay nil. A nil PRs means "never successfully fetched", and
			// normalizing it to an empty slice here would tell the client the
			// forge reported zero pull requests.
			PRs:       e.PRs,
			Checks:    e.Checks,
			LastError: e.LastError,
		})
	}
	return out
}
