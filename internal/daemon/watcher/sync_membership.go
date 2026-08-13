package watcher

// Notebook membership for a contained notespace (P4 — the second half of W3.2).
//
// Containment auto-registration gives a notespace inside a shared notebook an
// IDENTITY on the server: `POST /sync/register` says "this id, this subject,
// this name, this kind exist". It said nothing about WHERE the notespace lives,
// so the server held it unparented — belonging to no notebook at all — and
// `grove notebook pull`, which binds a notebook's MEMBERS, could not see it.
// The result was a notespace that synced for exactly one machine: the one that
// created it. The notebook lab's probe 10 is the assertion that found it.
//
// This file is the missing half: after registration, the notespace is attached
// to the notebook that contains it, with the wire verb built for the leg —
// NotespaceReparentRequest with an empty from_notebook_id, whose own doc
// comment names it "unparented -> notebook   attach (adoption)".
//
// # Why this is not the daemon deciding membership
//
// Membership is an operator-shaped act and stays one. Every condition below has
// to hold, and each of them is a fact an operator wrote down or the server
// already holds:
//
//   - The notespace is physically inside the notebook, at
//     <notebook>/notespaces/<name> — the same containment test that gave it a
//     transport in the first place.
//   - The notebook is recorded shared on this machine (`share = true`, written
//     by `grove notebook share` or `grove notebook pull`) and carries a
//     .notebook.toml, so it has an identity to be a member OF.
//   - The server holds that notebook, and holds it SHARED. A notebook this
//     server has never been told about, or one that was unshared, is left
//     alone; the operator re-shares it.
//   - The notespace is UNPARENTED on the server. A notespace the server already
//     counts in some other notebook is never taken from it, no matter what
//     directory it sits in locally — that is a move, the move has a verb, and a
//     daemon that stole members would make the one mechanism two.
//
// So the daemon never CHANGES a membership; it only completes a registration
// that left one absent, into the notebook whose share the operator declared.
// The change is CAS'd like every other membership write: the attach carries the
// membership version it was decided against, and a version the server has since
// moved past is retried exactly once against the version the refusal reports —
// the same discipline `grove notespace move` uses, so a genuinely concurrent
// third writer still fails rather than being routed around.
//
// # Cost
//
// One inventory round trip per reconcile pass that has something to check, and
// nothing at all once every contained notespace's membership is confirmed: the
// verdicts are memoized per (notespace, containing notebook), so the steady
// state is free and a notespace that moves to another notebook is re-checked
// because the pair changed. A verdict that could not be settled — the server
// has not heard of the notebook yet, the notespace belongs elsewhere, the
// attach was refused — is retried, but no more often than
// membershipRetryInterval, so a condition only an operator can clear does not
// turn into a request per pass.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"

	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/syncproto"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// defaultMembershipRetryInterval bounds how often an unsettled membership is
// asked about again. The conditions that leave one unsettled are operator-shaped
// (share the notebook on this server, move the notespace back), so the retry is
// a backstop for the moment they are cleared, not a poll.
const defaultMembershipRetryInterval = 5 * time.Minute

// membershipVerdict is the last answer for one notespace's membership of one
// notebook. notebookID is part of the key, not just the value: a notespace that
// moved into a different notebook has a different question to answer, and a
// verdict about its old one must not answer it.
type membershipVerdict struct {
	notebookID string
	attached   bool
	at         time.Time
}

// containedMembership is one notespace whose membership of its containing
// notebook has not been confirmed yet.
type containedMembership struct {
	notespaceID string
	root        string
	notebookID  string
}

// attachContainedNotespaces settles the notebook membership of every notespace
// this pass resolved inside a shared notebook. It is called at the end of a
// reconcile pass, after registration: attaching a notespace the server has not
// been told about is refused, and would be, so registration comes first.
//
// It never gates a transport. A notespace that cannot be attached still syncs
// for this machine exactly as it did before; what it does not do is reach any
// other machine, which is the condition being reported and repaired here.
func (h *SyncHandler) attachContainedNotespaces(client *syncdb.Client, resolved map[string]resolvedNotespace) {
	if client == nil || h.baseCtx == nil {
		return
	}
	pending := h.pendingMemberships(resolved)
	if len(pending) == 0 {
		return
	}
	inventory, err := client.Inventory(h.baseCtx)
	if err != nil || inventory == nil {
		// The inventory is the whole basis for the decision — without it there
		// is no way to tell "unparented" from "already a member", and guessing
		// would mean sending an attach that the server refuses. The next pass
		// asks again; nothing is recorded, so nothing is backed off.
		h.ulog.Debug("notebook membership not checked: the server inventory could not be read").
			Err(err).StructuredOnly().Log(h.baseCtx)
		return
	}
	notespaces := make(map[string]syncproto.InventoryNotespace, len(inventory.Notespaces))
	for _, ns := range inventory.Notespaces {
		notespaces[ns.ID.String()] = ns
	}
	notebooks := make(map[string]syncproto.InventoryNotebook, len(inventory.Notebooks))
	for _, book := range inventory.Notebooks {
		notebooks[book.ID.String()] = book
	}
	for _, want := range pending {
		h.settleMembership(client, want, notespaces, notebooks)
	}
}

// pendingMemberships is the contained notespaces whose membership is not
// already settled, in a deterministic order. It reads only local state, which is
// what keeps a fully-settled machine from making any request at all.
func (h *SyncHandler) pendingMemberships(resolved map[string]resolvedNotespace) []containedMembership {
	var pending []containedMembership
	for _, id := range slices.Sorted(maps.Keys(resolved)) {
		r := resolved[id]
		notebookRoot, ok := containingNotebookRoot(r.root)
		if !ok || !h.notebookRecordedShared(notebookRoot) {
			continue
		}
		// The notebook's own identity is the .notebook.toml an operator verb
		// minted. A shared notebook with no stamp is not a thing to be a member
		// of yet, and minting one is `grove notebook share`'s job, not this
		// daemon's — it writes no notebook identity, here or anywhere.
		stamp, err := notespacepkg.LoadNotebook(notebookRoot)
		if err != nil || stamp == nil || stamp.ID == "" {
			continue
		}
		if h.membershipSettled(id, stamp.ID) {
			continue
		}
		pending = append(pending, containedMembership{notespaceID: id, root: r.root, notebookID: stamp.ID})
	}
	return pending
}

// membershipSettled reports whether this exact question — is notespace id a
// member of notebook notebookID — has an answer worth reusing.
func (h *SyncHandler) membershipSettled(id, notebookID string) bool {
	h.membershipMu.Lock()
	defer h.membershipMu.Unlock()
	verdict, ok := h.membership[id]
	if !ok || verdict.notebookID != notebookID {
		return false
	}
	if verdict.attached {
		return true
	}
	return h.membershipNowFunc()().Sub(verdict.at) < h.membershipRetry()
}

func (h *SyncHandler) recordMembership(id, notebookID string, attached bool) {
	h.membershipMu.Lock()
	defer h.membershipMu.Unlock()
	if h.membership == nil {
		h.membership = make(map[string]membershipVerdict)
	}
	h.membership[id] = membershipVerdict{notebookID: notebookID, attached: attached, at: h.membershipNowFunc()()}
}

func (h *SyncHandler) membershipRetry() time.Duration {
	if h.membershipRetryInterval > 0 {
		return h.membershipRetryInterval
	}
	return defaultMembershipRetryInterval
}

func (h *SyncHandler) membershipNowFunc() func() time.Time {
	if h.membershipNow != nil {
		return h.membershipNow
	}
	return time.Now
}

// settleMembership decides one notespace's membership against the server's own
// inventory, and attaches it when — and only when — the server holds it
// belonging to nothing.
func (h *SyncHandler) settleMembership(client *syncdb.Client, want containedMembership,
	notespaces map[string]syncproto.InventoryNotespace, notebooks map[string]syncproto.InventoryNotebook,
) {
	ns, held := notespaces[want.notespaceID]
	if !held {
		// Registration has not landed (it failed this pass, or the server was
		// recreated under this daemon). There is no identity to attach yet, and
		// the registration path is what repairs that.
		h.ulog.Debug("notebook membership deferred: the server does not hold this notespace yet").
			Field("notespace_id", want.notespaceID).
			Field("root", want.root).
			StructuredOnly().Log(h.baseCtx)
		h.recordMembership(want.notespaceID, want.notebookID, false)
		return
	}
	if ns.NotebookID.String() == want.notebookID {
		h.recordMembership(want.notespaceID, want.notebookID, true)
		return
	}
	if ns.NotebookID != "" {
		// Locally it sits in one notebook and the server counts it in another.
		// That is a move half-made or a move made elsewhere, and either way the
		// repair is an operator's: this daemon does not take a notespace out of
		// a notebook.
		h.ulog.Warn("notespace is inside one notebook and belongs to another on the server; membership left alone").
			Field("notespace_id", want.notespaceID).
			Field("root", want.root).
			Field("containing_notebook_id", want.notebookID).
			Field("server_notebook_id", ns.NotebookID.String()).
			Log(h.baseCtx)
		h.recordMembership(want.notespaceID, want.notebookID, false)
		return
	}
	book, known := notebooks[want.notebookID]
	if !known {
		h.ulog.Debug("notebook membership deferred: the server does not hold the containing notebook").
			Field("notespace_id", want.notespaceID).
			Field("notebook_id", want.notebookID).
			StructuredOnly().Log(h.baseCtx)
		h.recordMembership(want.notespaceID, want.notebookID, false)
		return
	}
	if book.ShareState == syncproto.NotebookShareStateUnshared {
		// The server would refuse an attach into an unshared notebook, and it is
		// right to: re-sharing it is the operator's decision, not a thing to
		// work around from here.
		h.ulog.Debug("notebook membership deferred: the containing notebook is unshared on the server").
			Field("notespace_id", want.notespaceID).
			Field("notebook_id", want.notebookID).
			StructuredOnly().Log(h.baseCtx)
		h.recordMembership(want.notespaceID, want.notebookID, false)
		return
	}

	version, err := h.attachNotespace(client, want)
	if err != nil {
		h.ulog.Warn("notespace could not be attached to the notebook that contains it").
			Err(err).
			Field("notespace_id", want.notespaceID).
			Field("root", want.root).
			Field("notebook_id", want.notebookID).
			Field("notebook_name", book.Name).
			Log(h.baseCtx)
		h.recordMembership(want.notespaceID, want.notebookID, false)
		return
	}
	h.recordMembership(want.notespaceID, want.notebookID, true)
	h.ulog.Info("notespace attached to the shared notebook that contains it").
		Field("notespace_id", want.notespaceID).
		Field("root", want.root).
		Field("notebook_id", want.notebookID).
		Field("notebook_name", book.Name).
		Field("membership_version", version).
		Log(h.baseCtx)
}

// attachNotespace sends the attach, once, with one retry against the version a
// refusal reports.
//
// The first attempt asks at version 0, which is what an unparented notespace's
// membership version is when nothing has ever moved it — the ordinary case for
// a notespace this daemon has just registered. A notespace that has been moved
// before carries a higher one, and the inventory does not report it, so the
// refusal is the only place that fact exists: it is used ONCE, and a second
// refusal is a real failure rather than a loop.
func (h *SyncHandler) attachNotespace(client *syncdb.Client, want containedMembership) (int64, error) {
	attempt := func(version int64) (*syncproto.NotespaceReparentResponse, error) {
		// The version is part of the idempotency key for the same reason grove's
		// is: the retry asks a DIFFERENT question, and replaying the first
		// question's answer would hide the one fact the retry exists to use.
		sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d", want.notespaceID, want.notebookID, version))
		return client.ReparentNotespace(h.baseCtx, syncproto.NotespaceReparentRequest{
			RequestIdentity: syncproto.RequestIdentity{
				IdempotencyKey: "daemon-attach-" + hex.EncodeToString(sum[:]),
			},
			NotespaceID:     syncproto.NotespaceID(want.notespaceID),
			ToNotebookID:    syncproto.NotebookID(want.notebookID),
			ExpectedVersion: version,
		})
	}
	version := int64(0)
	resp, err := attempt(version)
	if resp != nil && resp.Error != nil && resp.Error.Code == syncproto.ErrorStaleResolution &&
		resp.Error.CurrentVersion != version {
		version = resp.Error.CurrentVersion
		resp, err = attempt(version)
	}
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, fmt.Errorf("the server answered the attach of %s with nothing", want.notespaceID)
	}
	if resp.NotespaceID.String() != want.notespaceID || resp.ToNotebookID.String() != want.notebookID {
		return 0, fmt.Errorf("the attach of %s into %s was answered for notespace %s in notebook %s",
			want.notespaceID, want.notebookID, resp.NotespaceID, resp.ToNotebookID)
	}
	return resp.Version, nil
}
