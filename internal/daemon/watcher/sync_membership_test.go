package watcher

import (
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/syncproto"
)

// Notebook membership for contained notespaces (sync_membership.go).
//
// The regression these pin is the notebook lab's finding 5, found by probe 10:
// containment auto-registration wrote a notespace's IDENTITY to the server and
// stopped there, so the server held it unparented, `grove notebook pull` — which
// binds a notebook's MEMBERS — never learned of it, and a notespace created
// inside a shared notebook synced for exactly one machine.

const (
	notebookID      = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	otherNotebookID = "01ARZ3NDEKTSV4RRFFQ69G5FB2"
)

// sharedNotebookHarness is the state every probe-10 case starts from: a notebook
// this machine records as shared and stamped, held shared by the server too.
func sharedNotebookHarness(t *testing.T) *lifecycleHarness {
	t.Helper()
	lh := newLifecycleHarness(t)
	lh.stampNotebook(t, notebookID, "workshop")
	lh.serverNotebook(notebookID, "workshop", syncproto.NotebookShareStateShared)
	lh.share(true)
	return lh
}

// fakeMembershipClock is the hermetic clock the expiry cases step by hand.
// Only the reconcile pass reads it, and only from the calling goroutine, so an
// advance between passes is as ordered as the passes are.
type fakeMembershipClock struct{ now time.Time }

func (c *fakeMembershipClock) time() time.Time         { return c.now }
func (c *fakeMembershipClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// membershipClock installs that clock and both membership floors, so every
// expiry below is a deliberate step rather than a wait. The base instant is
// fixed: nothing in these tests reads the wall clock.
func (lh *lifecycleHarness) membershipClock(retry, confirm time.Duration) *fakeMembershipClock {
	clock := &fakeMembershipClock{now: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	lh.h.membershipNow = clock.time
	lh.h.membershipRetryInterval = retry
	lh.h.membershipConfirmInterval = confirm
	return clock
}

// The headline: a notespace created inside a shared notebook is not only
// registered but ATTACHED, so the notebook's membership roll names it and the
// other machine's pull can find it.
func TestAContainedNotespaceBecomesAMemberOfItsNotebook(t *testing.T) {
	lh := sharedNotebookHarness(t)
	lh.notespace(t, "widget-personal", idBeta)
	lh.subscribe()

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()

	if lh.registrationCount(idBeta) != 1 {
		t.Fatalf("the contained notespace registered %d times, want 1", lh.registrationCount(idBeta))
	}
	book, version := lh.serverMembership(idBeta)
	if book != notebookID {
		t.Fatalf("the server holds the contained notespace in notebook %q, want %q — registered is not the same as contained", book, notebookID)
	}
	if version != 1 {
		t.Fatalf("membership version = %d after one attach, want 1", version)
	}
	attaches := lh.attachRequests()
	if len(attaches) != 1 {
		t.Fatalf("attach requests = %d, want exactly 1: %+v", len(attaches), attaches)
	}
	// The attach leg and only the attach leg: out of nowhere, into the notebook
	// that contains it.
	if attaches[0].FromNotebookID != "" {
		t.Fatalf("the daemon claimed to move the notespace out of %q; it only ever attaches an unparented one", attaches[0].FromNotebookID)
	}
	if attaches[0].ToNotebookID.String() != notebookID || attaches[0].NotespaceID.String() != idBeta {
		t.Fatalf("attach = %+v, want %s into %s", attaches[0], idBeta, notebookID)
	}
}

// Once settled, the question is not asked again: no attach, and no inventory
// round trip either, for as long as the notespace stays where it is.
func TestASettledMembershipCostsNothing(t *testing.T) {
	lh := sharedNotebookHarness(t)
	lh.notespace(t, "widget-personal", idBeta)
	lh.subscribe()

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()
	settled := lh.inventoryCount()

	for range 3 {
		lh.h.ensurePipelines()
	}
	if got := lh.inventoryCount(); got != settled {
		t.Fatalf("inventory requests = %d after three further passes, want the %d the first pass made", got, settled)
	}
	if attaches := lh.attachRequests(); len(attaches) != 1 {
		t.Fatalf("attach requests = %d, want the one that settled it: %+v", len(attaches), attaches)
	}
}

// A notespace the server already counts in the notebook is confirmed without
// asking for anything: the attach is what is idempotent about this, and the
// check in front of it is why the server is never asked to repeat itself.
func TestAnExistingMembershipIsConfirmedNotRewritten(t *testing.T) {
	lh := sharedNotebookHarness(t)
	lh.notespace(t, "widget", idBeta)
	lh.placeOnServer(idBeta, "widget", notebookID, 4)
	lh.subscribe()

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()

	if attaches := lh.attachRequests(); len(attaches) != 0 {
		t.Fatalf("a notespace that is already a member was re-attached: %+v", attaches)
	}
	if book, version := lh.serverMembership(idBeta); book != notebookID || version != 4 {
		t.Fatalf("membership = %s@%d, want it untouched at %s@4", book, version, notebookID)
	}
}

// The rule that keeps this from making the daemon a third writer of membership:
// a notespace the server counts in ANOTHER notebook is left exactly where it is,
// however the local directory reads. Taking it would be a move, and a move is an
// operator's verb.
func TestANotespaceBelongingElsewhereIsNeverTaken(t *testing.T) {
	lh := sharedNotebookHarness(t)
	lh.notespace(t, "widget", idBeta)
	lh.placeOnServer(idBeta, "widget", otherNotebookID, 2)
	lh.serverNotebook(otherNotebookID, "staging", syncproto.NotebookShareStateShared)
	lh.subscribe()

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()

	if attaches := lh.attachRequests(); len(attaches) != 0 {
		t.Fatalf("the daemon tried to take a notespace out of another notebook: %+v", attaches)
	}
	if book, _ := lh.serverMembership(idBeta); book != otherNotebookID {
		t.Fatalf("membership = %q, want it left in %q", book, otherNotebookID)
	}
}

// Consent is still the recorded local one. A notebook this machine does not
// record as shared gets no membership writes even when the server holds it
// shared, because nothing about it is in scope for this daemon at all.
func TestAnUnsharedNotebookGetsNoMembershipWrites(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.stampNotebook(t, notebookID, "workshop")
	lh.serverNotebook(notebookID, "workshop", syncproto.NotebookShareStateShared)
	root := lh.notespace(t, "widget", idBeta)
	lh.subscribe(config.SyncWorkspace{Name: "widget", Mode: config.SyncModeFull})
	lh.watch(map[string]string{"widget": root})

	lh.h.ensurePipelines()

	if attaches := lh.attachRequests(); len(attaches) != 0 {
		t.Fatalf("an unshared notebook's notespace was attached: %+v", attaches)
	}
	if got := lh.inventoryCount(); got != 0 {
		t.Fatalf("inventory requests = %d for a machine that shares nothing, want 0", got)
	}
}

// A shared notebook with no .notebook.toml has no identity to be a member of,
// and minting one is a verb's job. The daemon writes no notebook identity —
// here or anywhere — so it asks the server nothing.
func TestAnUnstampedNotebookIsNotAMembershipTarget(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.serverNotebook(notebookID, "workshop", syncproto.NotebookShareStateShared)
	lh.notespace(t, "widget", idBeta)
	lh.share(true)
	lh.subscribe()

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()

	if lh.registrationCount(idBeta) != 1 {
		t.Fatalf("the contained notespace registered %d times, want 1 — an unstamped notebook still syncs", lh.registrationCount(idBeta))
	}
	if attaches := lh.attachRequests(); len(attaches) != 0 {
		t.Fatalf("a notebook with no stamp was used as a membership target: %+v", attaches)
	}
	if got := lh.inventoryCount(); got != 0 {
		t.Fatalf("inventory requests = %d with no notebook identity to check against, want 0", got)
	}
}

// A notebook the server has never been told about is left for the operator to
// share, and the daemon backs off rather than asking once per reconcile pass —
// but it does ask again once the backoff is up, because sharing it is exactly
// the thing that makes the answer change.
func TestAnUnknownNotebookIsRetriedOnlyAfterTheBackoff(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.stampNotebook(t, notebookID, "workshop")
	lh.notespace(t, "widget", idBeta)
	lh.share(true)
	lh.subscribe()
	now := time.Now()
	lh.h.membershipNow = func() time.Time { return now }
	lh.h.membershipRetryInterval = time.Minute

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()
	if got := lh.inventoryCount(); got != 1 {
		t.Fatalf("inventory requests = %d on the first pass, want 1", got)
	}
	if attaches := lh.attachRequests(); len(attaches) != 0 {
		t.Fatalf("the daemon attached into a notebook the server does not hold: %+v", attaches)
	}

	lh.h.ensurePipelines()
	if got := lh.inventoryCount(); got != 1 {
		t.Fatalf("inventory requests = %d inside the backoff window, want the first pass's 1", got)
	}

	// The operator shares the notebook, and the backoff expires.
	lh.serverNotebook(notebookID, "workshop", syncproto.NotebookShareStateShared)
	now = now.Add(2 * time.Minute)
	lh.h.ensurePipelines()
	if book, _ := lh.serverMembership(idBeta); book != notebookID {
		t.Fatalf("membership = %q after the notebook was shared and the backoff expired, want %q", book, notebookID)
	}
}

// The server refuses an attach into an unshared notebook, and it is right to.
// The daemon does not work around it — re-sharing is an operator decision — and
// it does not send the request either, so the refusal is never the thing that
// stops it.
func TestAnUnsharedServerNotebookIsNotAttachedInto(t *testing.T) {
	lh := newLifecycleHarness(t)
	lh.stampNotebook(t, notebookID, "workshop")
	lh.serverNotebook(notebookID, "workshop", syncproto.NotebookShareStateUnshared)
	lh.notespace(t, "widget", idBeta)
	lh.share(true)
	lh.subscribe()

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()

	if attaches := lh.attachRequests(); len(attaches) != 0 {
		t.Fatalf("the daemon attached into an unshared notebook: %+v", attaches)
	}
	if book, _ := lh.serverMembership(idBeta); book != "" {
		t.Fatalf("membership = %q, want it left unparented", book)
	}
}

// The membership version the attach is CAS'd against is not in the inventory,
// so the first attempt asks at 0 and a refusal is the only place the real one
// exists. It is used exactly once.
func TestAStaleAttachIsRetriedAgainstTheVersionTheServerReports(t *testing.T) {
	lh := sharedNotebookHarness(t)
	lh.notespace(t, "widget", idBeta)
	// Unparented, but moved before: an attach at version 0 is refused with the
	// version the server holds.
	lh.placeOnServer(idBeta, "widget", "", 7)
	lh.subscribe()

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()

	attaches := lh.attachRequests()
	if len(attaches) != 2 {
		t.Fatalf("attach requests = %d, want the first attempt and one retry: %+v", len(attaches), attaches)
	}
	if attaches[0].ExpectedVersion != 0 || attaches[1].ExpectedVersion != 7 {
		t.Fatalf("attach versions = %d then %d, want 0 then the server's 7", attaches[0].ExpectedVersion, attaches[1].ExpectedVersion)
	}
	if attaches[0].IdempotencyKey == attaches[1].IdempotencyKey {
		t.Fatal("the retry reused the first attempt's idempotency key, so the server would have replayed the refusal")
	}
	if book, version := lh.serverMembership(idBeta); book != notebookID || version != 8 {
		t.Fatalf("membership = %s@%d, want %s@8", book, version, notebookID)
	}
}

// A confirmed membership is believed, but not forever. The passes in between
// ask nothing — including the ones past the FAILURE floor, which is a different
// and much shorter question — and then the confirmation expires, is revalidated
// against the server's own inventory, and goes quiet again on its new stamp.
func TestAConfirmedMembershipIsRevalidatedWhenItExpires(t *testing.T) {
	lh := sharedNotebookHarness(t)
	lh.notespace(t, "widget-personal", idBeta)
	lh.subscribe()
	clock := lh.membershipClock(time.Minute, 10*time.Minute)

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()
	if got := lh.inventoryCount(); got != 1 {
		t.Fatalf("inventory requests = %d on the pass that settled it, want 1", got)
	}

	// Well past the failure floor and well inside the confirmation's: a
	// confirmed answer is not re-asked on the cadence of an unsettled one.
	clock.advance(2 * time.Minute)
	lh.h.ensurePipelines()
	lh.h.ensurePipelines()
	if got := lh.inventoryCount(); got != 1 {
		t.Fatalf("inventory requests = %d inside the confirmation window, want the settling pass's 1", got)
	}

	clock.advance(9 * time.Minute)
	lh.h.ensurePipelines()
	if got := lh.inventoryCount(); got != 2 {
		t.Fatalf("inventory requests = %d after the confirmation expired, want it revalidated exactly once more", got)
	}
	// Revalidation is a question, not a write: the server still holds the
	// membership, so nothing is sent.
	if attaches := lh.attachRequests(); len(attaches) != 1 {
		t.Fatalf("attach requests = %d, want only the one that settled it: %+v", len(attaches), attaches)
	}
	if book, version := lh.serverMembership(idBeta); book != notebookID || version != 1 {
		t.Fatalf("membership = %s@%d, want it untouched at %s@1", book, version, notebookID)
	}

	// And the fresh confirmation restarts the floor rather than leaving the
	// notespace asking once per pass from here on.
	lh.h.ensurePipelines()
	if got := lh.inventoryCount(); got != 2 {
		t.Fatalf("inventory requests = %d after revalidation, want the confirmation to have re-stamped at 2", got)
	}
}

// The reason the confirmation expires at all: a server rebuilt, restored, or
// history-reset under a running daemon holds the notespace unparented again,
// and until this the memo answered for it forever — so the notespace reached no
// other machine until the daemon was restarted. Now the expiry finds it and the
// attach leg repairs it, with no restart and no operator verb.
func TestAMembershipTheServerLostIsReattachedAfterTheConfirmationExpires(t *testing.T) {
	lh := sharedNotebookHarness(t)
	lh.notespace(t, "widget-personal", idBeta)
	lh.subscribe()
	clock := lh.membershipClock(time.Minute, 10*time.Minute)

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()
	if book, _ := lh.serverMembership(idBeta); book != notebookID {
		t.Fatalf("membership = %q before the rebuild, want %q", book, notebookID)
	}

	// The server comes back with this notespace's identity and none of its
	// membership — registered, unparented, version 0, exactly as the register
	// leg leaves one.
	lh.placeOnServer(idBeta, "widget-personal", "", 0)

	lh.h.ensurePipelines()
	if attaches := lh.attachRequests(); len(attaches) != 1 {
		t.Fatalf("attach requests = %d inside the confirmation window, want the memo to still suppress repeats: %+v", len(attaches), attaches)
	}

	clock.advance(11 * time.Minute)
	lh.h.ensurePipelines()

	attaches := lh.attachRequests()
	if len(attaches) != 2 {
		t.Fatalf("attach requests = %d after the confirmation expired, want the settling one and the repair: %+v", len(attaches), attaches)
	}
	if attaches[1].FromNotebookID != "" || attaches[1].ToNotebookID.String() != notebookID {
		t.Fatalf("the repair = %+v, want the attach leg into %s", attaches[1], notebookID)
	}
	if book, version := lh.serverMembership(idBeta); book != notebookID || version != 1 {
		t.Fatalf("membership = %s@%d after the repair, want %s@1", book, version, notebookID)
	}
}

// The harder shape of the same rebuild: the server has not even re-registered
// the notespace yet when the confirmation expires. There is nothing to attach,
// so the expired confirmation becomes an UNSETTLED verdict — and an unsettled
// verdict waits the short failure floor, not another confirmation window, so the
// attach follows the registration that repairs it rather than trailing it by
// half an hour.
func TestAnExpiredConfirmationFallsBackToTheFailureFloor(t *testing.T) {
	lh := sharedNotebookHarness(t)
	lh.notespace(t, "widget-personal", idBeta)
	lh.subscribe()
	clock := lh.membershipClock(time.Minute, 10*time.Minute)

	lh.h.ComputeWatchPaths(nil)
	lh.h.ensurePipelines()

	// A server with no memory of this notespace at all.
	lh.forgetServerNotespace(idBeta)
	clock.advance(11 * time.Minute)
	lh.h.ensurePipelines()
	if attaches := lh.attachRequests(); len(attaches) != 1 {
		t.Fatalf("attach requests = %d against a server that does not hold the notespace, want only the settling one: %+v", len(attaches), attaches)
	}

	// Registration lands again, and one failure floor — not one confirmation
	// window — later the membership follows it.
	lh.placeOnServer(idBeta, "widget-personal", "", 0)
	lh.h.ensurePipelines()
	if book, _ := lh.serverMembership(idBeta); book != "" {
		t.Fatalf("membership = %q inside the retry backoff, want it still unparented", book)
	}
	clock.advance(2 * time.Minute)
	lh.h.ensurePipelines()
	if book, version := lh.serverMembership(idBeta); book != notebookID || version != 1 {
		t.Fatalf("membership = %s@%d once the retry floor was up, want %s@1", book, version, notebookID)
	}
}
