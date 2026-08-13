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
