package watcher

// The two-machine half of W3.5 (holistic review F1).
//
// The shipped gate was one-directional: a contested notespace lost its pull
// loop and kept its push loop and its anti-entropy sweep. Every test in the
// suite watched the machine that raised the collision, and that machine was
// genuinely protected — so nothing failed, and the OTHER machine's copy was
// being overwritten by content no operator had adopted.
//
// The scenario these tests reproduce is the one the gate exists for:
//
//	Machine A holds plans/x.md in a shared notebook and has pushed it.
//	Machine B holds a DIFFERENT plans/x.md that has never been synced.
//	B joins, pulls, and the gate contests.
//
// The server is the only channel between them, so "no unadopted content
// reaches machine A" is provable here as "no unadopted content reaches the
// server". Both are asserted: the push log stays empty for the contested
// notespace, AND the server's copy still holds A's bytes at the end.
//
// The fixture server below is machine A's already-pushed state plus the
// transport surface B talks to. It is hermetic: temp dirs, an ephemeral
// httptest listener, no ambient config, no real daemon.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/syncproto"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// peerServer is machine A's content as machine B can reach it, plus a log of
// everything B tried to send.
type peerServer struct {
	mu sync.Mutex
	// documents is the server's head state, keyed by wire path. It starts as
	// machine A's content; a push from B would replace it.
	documents map[string]string
	// pushedPaths records every path B pushed, in order. The empty case is the
	// assertion this file is built around.
	pushedPaths []string
	// pushedAt is when each pushedPaths entry arrived, index-aligned with it.
	// The birth-race case cannot be stated as "nothing was pushed" — a
	// pipeline that starts uncontested is entitled to push until the verdict
	// exists — so it is stated as "nothing was pushed AFTER the verdict".
	pushedAt []time.Time
	// servedEventsAt is when the contesting batch left this server, which is
	// the last moment before the gate can possibly have raised its verdict.
	// Using it as the verdict timestamp is the conservative direction: it can
	// only make a push look EARLIER than the verdict, never later.
	servedEventsAt time.Time
	// snapshots counts anti-entropy passes: the sweep that seeds untracked
	// local files into the outbox begins with a snapshot fetch, so a nonzero
	// count for a contested notespace means the outbound side ran at all.
	snapshots int
	// events is the batch B's pull loop is served, i.e. what A pushed.
	events []syncproto.SyncEvent
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (p *peerServer) pushed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.pushedPaths...)
}

func (p *peerServer) document(path string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.documents[path]
}

// pushedAfter is every path this server accepted at or after the given moment.
func (p *peerServer) pushedAfter(when time.Time) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var late []string
	for i, at := range p.pushedAt {
		if !at.Before(when) {
			late = append(late, p.pushedPaths[i])
		}
	}
	return late
}

// contestedAt is when the batch that contests was served; zero until it is.
func (p *peerServer) contestedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.servedEventsAt
}

func (p *peerServer) snapshotCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshots
}

func (p *peerServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sync/capabilities":
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
				ProtocolVersion: syncproto.ProtocolVersionLegacy,
				Capabilities:    syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersionLegacy}},
			})

		case "/sync/register":
			var req syncproto.RegisterRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(syncproto.RegisterResponse{NotespaceID: req.ProposedNotespaceID})

		case "/sync/events":
			// Machine A's write, served once. The cursor stops the loop from
			// re-serving it and contesting in a hot loop.
			p.mu.Lock()
			events := p.events
			p.mu.Unlock()
			cursor := int64(0)
			if r.URL.Query().Get("cursor") == "0" && len(events) > 0 {
				cursor = 1
				p.mu.Lock()
				if p.servedEventsAt.IsZero() {
					p.servedEventsAt = time.Now()
				}
				p.mu.Unlock()
			} else {
				events = nil
			}
			_ = json.NewEncoder(w).Encode(syncproto.PullResponse{Events: events, Cursor: cursor})

		case "/sync/snapshot":
			p.mu.Lock()
			p.snapshots++
			docs := make([]syncproto.DocumentSnapshot, 0, len(p.documents))
			for path, content := range p.documents {
				docs = append(docs, syncproto.DocumentSnapshot{
					ID: "doc-" + path, Path: path, Version: 1, Hash: hashOf(content), Size: int64(len(content)),
				})
			}
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(syncproto.SnapshotManifest{Documents: docs, Cursor: 1})

		case "/sync/push":
			var req syncproto.PushRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			resp := syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
			p.mu.Lock()
			for i, ev := range req.Events {
				p.pushedPaths = append(p.pushedPaths, ev.Path)
				p.pushedAt = append(p.pushedAt, time.Now())
				if len(ev.Content) > 0 {
					p.documents[ev.Path] = string(ev.Content)
				}
				resp.Results[i] = syncproto.PushResult{
					Status:     syncproto.PushStatusAccepted,
					DocumentID: "doc-" + ev.Path, Version: 2, Seq: int64(i + 1),
				}
			}
			resp.Cursor = int64(len(p.pushedPaths))
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)

		default:
			// Inventory, history and the blob tier are not reached by these
			// paths; a 404 here is louder than a silent empty answer if one
			// ever is.
			http.NotFound(w, r)
		}
	})
}

// contestedHarness is a lifecycleHarness whose client points at a peerServer
// rather than the register-only fixture.
func contestedHarness(t *testing.T) (*lifecycleHarness, *peerServer) {
	t.Helper()
	lh := newLifecycleHarness(t)
	peer := &peerServer{documents: map[string]string{}}
	server := httptest.NewServer(peer.handler())
	t.Cleanup(server.Close)
	lh.h.client = syncdb.NewClient(syncdb.ClientConfig{
		ServerURL: server.URL, Token: "fixture", DeviceID: "device-b", OriginID: "origin-b",
	})
	return lh, peer
}

// pushWindow is how long an uncontested pipeline needs to get local content to
// the server: the anti-entropy initial pass runs immediately at pipeline start,
// and the push loop drains on its first tick (PushConfig.CheckInterval, 5s).
// Waiting less than this proves nothing — the absence of a push would only mean
// the test asked too early.
const pushWindow = 7 * time.Second

// waitForOutbound polls until cond holds, over a window long enough for the
// outbound side to have acted several times.
func waitForOutbound(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * pushWindow)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// queueLocal puts an un-synced local file in the outbox the way the hydration
// pass does at pipeline birth: a create for a path sync.db holds no document
// row for. The field case had exactly this — two rows enqueued seconds before
// the gate contested, and drained to the server while the verdict waited for a
// reconcile.
func queueLocal(t *testing.T, db *syncdb.DB, notespaceID, rel, content string) {
	t.Helper()
	if _, err := db.EnqueueOutbox(&syncdb.OutboxEntry{
		DocumentID:  "local-" + rel,
		Notespace:   notespaceID,
		EventType:   syncproto.EventDocumentCreated,
		Path:        rel,
		ContentHash: hashOf(content),
	}); err != nil {
		t.Fatalf("EnqueueOutbox %s: %v", rel, err)
	}
}

// writeLocal drops an un-synced file into a notespace root: content this
// machine has never pushed and sync.db has never seen.
func writeLocal(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// F1: a contested notespace must not push, and must not sweep.
//
// Before the fix, startPipeline ran push and anti-entropy unconditionally and
// gated only the pull loop. Anti-entropy is the wider of the two holes:
// walkLocalTree seeds every UNTRACKED local file into the outbox, and a machine
// with pre-existing notes is made of untracked local files. So B's disputed
// plans/x.md was enqueued and uploaded while B's own tree sat safely protected,
// and machine A's copy was replaced by content no operator ever adopted.
func TestContestedNotespaceSendsNothingToTheServer(t *testing.T) {
	lh, peer := contestedHarness(t)
	root := lh.notespace(t, "alpha", idA)

	// Machine A's copy, already on the server.
	peer.mu.Lock()
	peer.documents["plans/x.md"] = "machine A's plan"
	peer.mu.Unlock()

	// Machine B's copy: same path, different content, never synced.
	writeLocal(t, root, "plans/x.md", "machine B's plan")

	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})

	// The verdict the pull gate reaches on this tree (its detection is pinned
	// in sync/adoption_test.go; what is under test here is what the daemon
	// does with the verdict).
	lh.h.MarkContested(idA, "adoption pending: colliding local notes")
	lh.h.ensurePipelines()

	state := lh.pipeline(idA)
	if state == nil {
		t.Fatal("a contested notespace lost its root binding")
	}
	// Reported, not fatal: the claim this test is really making is about what
	// reaches the server, and stopping here would leave that unproved.
	if state.push || state.pull {
		t.Errorf("a contested notespace kept a transport: push=%v pull=%v", state.push, state.pull)
	}

	// Long enough for the anti-entropy initial pass (immediate at pipeline
	// start) AND the push loop's first tick (PushConfig.CheckInterval, 5s), so
	// the window in which the pre-fix code uploaded has fully elapsed.
	time.Sleep(pushWindow)

	if pushed := peer.pushed(); len(pushed) != 0 {
		t.Errorf("a contested notespace uploaded %v; machine A's copy is being decided for it", pushed)
	}
	if n := peer.snapshotCount(); n != 0 {
		t.Errorf("anti-entropy swept a contested notespace %d time(s); walkLocalTree seeds exactly the disputed files", n)
	}
	if got := peer.document("plans/x.md"); got != "machine A's plan" {
		t.Errorf("the server holds %q; machine A's unadopted content was overwritten", got)
	}
}

// The other half of the same contract: withholding push must PARK the local
// work, not discard it. Adoption releases it.
//
// This is what makes the two-sided gate a deferral rather than a data-loss
// bug: B's edits keep queuing behind the gate (the pipeline is still installed
// at its root, so the watch stays bound and flush keeps capturing), and the
// operator's adoption is what sends them.
func TestAdoptionReleasesTheParkedOutbox(t *testing.T) {
	lh, peer := contestedHarness(t)
	root := lh.notespace(t, "alpha", idA)

	peer.mu.Lock()
	peer.documents["plans/x.md"] = "machine A's plan"
	peer.mu.Unlock()
	writeLocal(t, root, "plans/x.md", "machine B's plan")

	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})
	lh.h.MarkContested(idA, "adoption pending: colliding local notes")
	lh.h.ensurePipelines()

	time.Sleep(500 * time.Millisecond)
	if pushed := peer.pushed(); len(pushed) != 0 {
		t.Fatalf("precondition: the contested notespace already pushed %v", pushed)
	}

	// The operator adopts. The next reconcile restores both directions.
	lh.h.ClearContested(idA)
	waitForLifecycle(t, "adoption to restore the outbound side", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && state.push && state.pull
	})

	waitForOutbound(t, "the parked local work to reach the server", func() bool {
		return peer.document("plans/x.md") == "machine B's plan"
	})
}

// And the mid-flight ordering: the collision is discovered by a pipeline that
// is ALREADY pushing. The verdict has to reach the outbound side on the next
// reconcile, or the disputed content leaves the machine in the window between
// the contest and whatever else happens to re-root the pipeline.
func TestAContestRaisedMidFlightStopsTheOutboundSide(t *testing.T) {
	lh, peer := contestedHarness(t)
	root := lh.notespace(t, "alpha", idA)
	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})

	lh.h.ensurePipelines()
	state := lh.pipeline(idA)
	if state == nil || !state.push || !state.pull {
		t.Fatalf("precondition: want both directions running, got %+v", state)
	}

	lh.h.MarkContested(idA, "adoption pending: colliding local notes")
	waitForLifecycle(t, "the contest to tear down BOTH directions", func() bool {
		lh.h.ensurePipelines()
		state := lh.pipeline(idA)
		return state != nil && !state.push && !state.pull
	})

	// The maintenance drain is the other outbound path — it reconciles and
	// drains the outbox directly, bypassing the pipeline whose absence is the
	// enforcement everywhere else. It must skip a contested notespace too.
	before := len(peer.pushed())
	if err := lh.h.BeginMaintenance(lh.h.baseCtx); err != nil {
		t.Fatalf("maintenance drain: %v", err)
	}
	if after := peer.pushed(); len(after) != before {
		t.Errorf("the maintenance drain pushed %v from a contested notespace", after[before:])
	}
}

// The birth race, end to end: the pipeline that raises the verdict is the one
// that is already draining, and the verdict has to stop it BY ITSELF.
//
// This is the field case (2026-08-14, machine A, canary-nb-test) reproduced
// against the fixture server. There, the sequence was:
//
//	12:02:20  transport started, push:true pull:true
//	12:02:20  the hydration pass enqueued 2 outbox rows
//	12:02:20  the pull gate contested — verdict raised
//	12:02:29  the reconcile finally stopped the transport
//	12:02:40  it restarted with push:false pull:false
//
// and in the nine seconds between the verdict and the reconcile, the push loop
// drained both rows to the server. The gate's own decision — which copy of the
// divergent path wins — had already been made for the operator, in favour of
// this machine, by a loop that had not been told.
//
// So the test never reconciles after the verdict. Nothing calls
// ensurePipelines, the pipelineState still says push:true at the end (asserted,
// because a teardown would prove the OLD mechanism rather than this one), and
// the claim is exactly the ticket's: nothing reaches the server after the
// verdict timestamp.
func TestAContestOnTheFirstPullBatchStopsTheOutboundSideWithoutAReconcile(t *testing.T) {
	lh, peer := contestedHarness(t)
	root := lh.notespace(t, "alpha", idA)

	// Machine A's copy of the divergent path: on the server, and in the batch
	// this machine is about to be served.
	const serverCopy = "machine A's plan"
	peer.mu.Lock()
	peer.documents["plans/x.md"] = serverCopy
	peer.events = []syncproto.SyncEvent{{
		Type:        syncproto.EventDocumentCreated,
		Path:        "plans/x.md",
		DocumentID:  "doc-plans/x.md",
		ContentHash: hashOf(serverCopy),
		Size:        int64(len(serverCopy)),
	}}
	peer.mu.Unlock()

	// Machine B's side: the divergent copy plus an ordinary un-synced note,
	// both already queued for push before the transport starts. A non-empty
	// outbox at pipeline birth is the precondition the race needs, and it is
	// the ordinary state of a machine that has just hydrated.
	writeLocal(t, root, "plans/x.md", "machine B's plan")
	writeLocal(t, root, "notes/local.md", "machine B's note")
	queueLocal(t, lh.db, idA, "plans/x.md", "machine B's plan")
	queueLocal(t, lh.db, idA, "notes/local.md", "machine B's note")

	lh.subscribe(config.SyncWorkspace{Name: "alpha", Role: config.SyncRolePeer, Pull: true})
	lh.watch(map[string]string{"alpha": root})

	// Nothing is contested yet: this pipeline is born with both directions, as
	// the real one was, and its own pull loop raises the verdict a moment later.
	lh.h.ensurePipelines()
	state := lh.pipeline(idA)
	if state == nil || !state.push || !state.pull {
		t.Fatalf("precondition: want a pipeline born with both directions, got %+v", state)
	}

	waitForOutbound(t, "the pull gate to contest on its first batch", func() bool {
		_, contested := lh.h.isContested(idA)
		return contested
	})
	verdict := peer.contestedAt()
	if verdict.IsZero() {
		t.Fatal("the contesting batch was never served; the gate cannot have run on it")
	}

	// Past the push loop's first tick (PushConfig.CheckInterval, 5s) and the
	// anti-entropy initial pass — the whole window the field incident uploaded
	// in — with no reconcile anywhere in it.
	time.Sleep(pushWindow)

	if late := peer.pushedAfter(verdict); len(late) != 0 {
		t.Errorf("the outbox drained %v after the verdict; the contested copy is being published while the operator is still being asked", late)
	}
	if got := peer.document("plans/x.md"); got != serverCopy {
		t.Errorf("the server holds %q; machine A's copy of the divergent path was replaced by unadopted content", got)
	}

	// The enforcement above must have come from the verdict, not from a
	// teardown: the pipeline is still installed exactly as it was born.
	if state := lh.pipeline(idA); state == nil || !state.push {
		t.Fatalf("the pipeline was torn down after all (%+v); this test proves the SYNCHRONOUS gate, and a reconcile in the window would prove nothing about it", state)
	}

	// And the work is parked, not lost — the queued rows are still owed to the
	// server, and adoption is what releases them.
	queued, err := lh.db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if queued == 0 {
		t.Error("the contested notespace's outbox was emptied; withholding push must park local work, not discard it")
	}
}
