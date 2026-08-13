package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// frameCacheHarness is streamHarness with a handle on the server, so a test can
// read the cache's counters and toggle the escape hatch.
type frameCacheHarness struct {
	*streamHarness
	srvr *Server
}

func newFrameCacheHarness(t *testing.T, enabled bool) *frameCacheHarness {
	t.Helper()
	st := store.New()
	s := New(false)
	s.SetEngine(engine.New(st))
	s.frames.enabled = enabled
	srv := httptest.NewServer(http.HandlerFunc(s.handleStreamState))
	t.Cleanup(srv.Close)
	return &frameCacheHarness{streamHarness: &streamHarness{st: st, srv: srv}, srvr: s}
}

// drainProbe reads and discards frames until the reader has seen the probe
// waitForSubscriber publishes, so the marshal counters and the frames that
// follow describe only what the test itself published.
func drainProbe(t *testing.T, next func() *apiStateUpdate) {
	t.Helper()
	for range 20 {
		frame := next()
		if frame == nil {
			t.Fatal("stream ended before the probe frame arrived")
		}
		if frame.UpdateType == "config_reload" && frame.ConfigFile == "probe" {
			return
		}
	}
	t.Fatal("probe frame never arrived")
}

// marshalCounts snapshots the marshal accounting. misses is the number of
// marshals actually performed, which is the number the fan-out is supposed to
// hold flat as subscribers are added.
func marshalCounts() (hits, misses int64) {
	return telemetry.SSEMarshalCacheHits.Value(), telemetry.SSEMarshalCacheMisses.Value()
}

// TestOnePublishMarshalsOnceForManySubscribers is the acceptance property: with
// N subscribers of the same sequence, the daemon serializes the frame once and
// the other N-1 write the shared bytes.
func TestOnePublishMarshalsOnceForManySubscribers(t *testing.T) {
	const subscribers = 8

	h := newFrameCacheHarness(t, true)
	readers := make([]func() *apiStateUpdate, 0, subscribers)
	for range subscribers {
		_, next := h.subscribe(t, "types=config_reload")
		readers = append(readers, next)
	}
	h.waitForSubscriber(t)
	for _, next := range readers {
		drainProbe(t, next)
	}

	hits0, misses0 := marshalCounts()
	h.st.BroadcastConfigReload("shared.toml")

	// Every subscriber must actually receive the frame — a cache that shares
	// bytes by dropping deliveries would pass a counter check alone.
	var wg sync.WaitGroup
	for i, next := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frame := nextOfType(t, next, "config_reload")
			if frame.ConfigFile != "shared.toml" {
				t.Errorf("subscriber %d: config_file = %q, want shared.toml", i, frame.ConfigFile)
			}
		}()
	}
	wg.Wait()

	hits, misses := marshalCounts()
	if got := misses - misses0; got != 1 {
		t.Errorf("marshals for one publish across %d subscribers = %d, want 1", subscribers, got)
	}
	if got := hits - hits0; got != subscribers-1 {
		t.Errorf("cache hits = %d, want %d (every subscriber but the one that did the work)", got, subscribers-1)
	}
}

// TestFrameCacheDisabledMarshalsPerSubscriber pins the other arm of the A/B:
// with the escape hatch set, the daemon is back to one marshal per subscriber.
// This is what makes GROVE_SSE_FRAME_CACHE=0 a usable control on one binary.
func TestFrameCacheDisabledMarshalsPerSubscriber(t *testing.T) {
	const subscribers = 4

	h := newFrameCacheHarness(t, false)
	readers := make([]func() *apiStateUpdate, 0, subscribers)
	for range subscribers {
		_, next := h.subscribe(t, "types=config_reload")
		readers = append(readers, next)
	}
	h.waitForSubscriber(t)
	for _, next := range readers {
		drainProbe(t, next)
	}

	hits0, misses0 := marshalCounts()
	h.st.BroadcastConfigReload("private.toml")
	for _, next := range readers {
		nextOfType(t, next, "config_reload")
	}

	hits, misses := marshalCounts()
	if got := misses - misses0; got != subscribers {
		t.Errorf("marshals with the cache disabled = %d, want %d (one per subscriber)", got, subscribers)
	}
	if got := hits - hits0; got != 0 {
		t.Errorf("cache hits with the cache disabled = %d, want 0", got)
	}
}

// TestFrameCacheDoesNotChangeTheWire compares the bytes a subscriber receives
// with the cache on and off. Phase 1 is a serialization change only: any
// difference here is a wire-format regression.
func TestFrameCacheDoesNotChangeTheWire(t *testing.T) {
	frames := func(enabled bool) []string {
		h := newFrameCacheHarness(t, enabled)
		_, next := h.subscribe(t, "types=config_reload,workspaces_delta")
		h.waitForSubscriber(t)
		drainProbe(t, next)

		h.st.BroadcastConfigReload("wire.toml")
		h.st.ApplyUpdate(store.Update{
			Type:   store.UpdateWorkspacesDelta,
			Source: "test",
			Payload: []*models.WorkspaceDelta{
				{Path: "/repo/a", NoteCounts: &models.NoteCounts{Inbox: 3}},
				{Path: "/repo/b", NoteCounts: &models.NoteCounts{Inbox: 4}},
			},
		})

		var out []string
		for range 2 {
			frame := next()
			if frame == nil {
				t.Fatal("stream ended early")
			}
			// Seq is process-monotonic and differs between the two harnesses;
			// zero it so the comparison is about payload shape.
			frame.Seq = 0
			encoded, err := json.Marshal(frame)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			out = append(out, string(encoded))
		}
		return out
	}

	on, off := frames(true), frames(false)
	if len(on) != len(off) {
		t.Fatalf("frame count differs: cached %d, uncached %d", len(on), len(off))
	}
	for i := range on {
		if on[i] != off[i] {
			t.Errorf("frame %d differs\n cached: %s\nuncached: %s", i, on[i], off[i])
		}
	}
}

// TestFrameCacheDoesNotMarshalFilteredFrames guards the property job 85 bought:
// a subscriber that declared no interest in an event must not pay for its JSON.
// Caching conversion is free (every subscriber already converted before it
// could read the type), but the marshal must stay behind the filter.
func TestFrameCacheDoesNotMarshalFilteredFrames(t *testing.T) {
	h := newFrameCacheHarness(t, true)
	_, next := h.subscribe(t, "types=note_event")
	h.waitForSubscriber(t)
	time.Sleep(100 * time.Millisecond)

	_, misses0 := marshalCounts()
	h.st.BroadcastConfigReload("ignored.toml")
	time.Sleep(150 * time.Millisecond)

	if _, misses := marshalCounts(); misses != misses0 {
		t.Errorf("marshals = %d for an event no subscriber declared, want 0", misses-misses0)
	}

	// And the subscriber is still live: the filtered event was dropped, not the
	// connection.
	h.st.ApplyUpdate(store.Update{Type: store.UpdateNoteEvent, Source: "test", Payload: &models.NoteEvent{Event: models.NoteEventCreated}})
	if frame := nextOfType(t, next, "note_event"); frame == nil {
		t.Fatal("declared frame never arrived")
	}
}

// TestPathFilteredSubscriberGetsItsOwnPrunedFrame pins the split: a path filter
// that actually drops rows produces a frame belonging to that subscriber alone,
// which must be serialized separately and must not be published to the shared
// slot for that sequence.
func TestPathFilteredSubscriberGetsItsOwnPrunedFrame(t *testing.T) {
	h := newFrameCacheHarness(t, true)
	_, all := h.subscribe(t, "types=workspaces_delta")
	_, scoped := h.subscribe(t, "types=workspaces_delta&paths=/repo/a")
	time.Sleep(100 * time.Millisecond)

	h.st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkspacesDelta,
		Source: "test",
		Payload: []*models.WorkspaceDelta{
			{Path: "/repo/a", NoteCounts: &models.NoteCounts{Inbox: 1}},
			{Path: "/repo/b", NoteCounts: &models.NoteCounts{Inbox: 2}},
		},
	})

	unfiltered := nextOfType(t, all, "workspaces_delta")
	if len(unfiltered.WorkspaceDeltas) != 2 {
		t.Errorf("unfiltered subscriber saw %d deltas, want 2", len(unfiltered.WorkspaceDeltas))
	}
	pruned := nextOfType(t, scoped, "workspaces_delta")
	if len(pruned.WorkspaceDeltas) != 1 || pruned.WorkspaceDeltas[0].Path != "/repo/a" {
		t.Fatalf("path-filtered subscriber saw %d deltas (want 1, /repo/a): %+v", len(pruned.WorkspaceDeltas), pruned.WorkspaceDeltas)
	}
}

// TestInitialFrameSharedAcrossReconnectStorm covers the snapshot cache: clients
// subscribing at the same sequence with the same filter share one marshal of
// the largest frame the endpoint writes.
func TestInitialFrameSharedAcrossReconnectStorm(t *testing.T) {
	h := newFrameCacheHarness(t, true)
	h.st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkspaces,
		Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{
			"/repo/a": {},
		},
	})

	hits0 := telemetry.SSEInitialCacheHits.Value()
	misses0 := telemetry.SSEInitialCacheMisses.Value()

	const clients = 5
	for range clients {
		_, next := h.subscribe(t, "types=initial")
		if frame := next(); frame == nil || frame.UpdateType != coredaemon.StreamTypeInitial {
			t.Fatalf("subscriber did not receive the snapshot: %+v", frame)
		}
	}

	hits := telemetry.SSEInitialCacheHits.Value() - hits0
	misses := telemetry.SSEInitialCacheMisses.Value() - misses0
	if misses != 1 {
		t.Errorf("snapshot built %d times for %d simultaneous clients, want 1", misses, clients)
	}
	if hits != clients-1 {
		t.Errorf("snapshot cache hits = %d, want %d", hits, clients-1)
	}
}

// TestInitialFrameCacheKeyedByFilter proves the snapshot cache cannot hand a
// filtered subscriber another subscriber's frame: the path allow-list is part
// of the key, because it is part of the bytes.
func TestInitialFrameCacheKeyedByFilter(t *testing.T) {
	c := &frameCache{enabled: true, budget: frameCacheDefaultBudget}

	build := func(body string) func() ([]byte, bool, error) {
		return func() ([]byte, bool, error) { return []byte(body), true, nil }
	}
	wide, _, _ := c.initial(initialFrameKey(7, nil), build("wide"))
	narrow, _, _ := c.initial(initialFrameKey(7, []string{"/repo/a"}), build("narrow"))
	if string(wide) != "wide" || string(narrow) != "narrow" {
		t.Fatalf("filters collided: wide=%q narrow=%q", wide, narrow)
	}

	// Same key inside the TTL reuses; a moved sequence does not.
	again, _, _ := c.initial(initialFrameKey(7, nil), build("rebuilt"))
	if string(again) != "wide" {
		t.Errorf("same (seq, filter) rebuilt the frame: %q", again)
	}
	moved, _, _ := c.initial(initialFrameKey(8, nil), build("fresh"))
	if string(moved) != "fresh" {
		t.Errorf("a moved sequence served a stale snapshot: %q", moved)
	}
}

// TestFrameCacheHonorsItsByteBudget pins the memory bound: the slot count alone
// does not cap what the ring retains, so a run of large frames must release old
// ones rather than pin 32 of them.
func TestFrameCacheHonorsItsByteBudget(t *testing.T) {
	const budget = 64 << 10
	c := &frameCache{enabled: true, budget: budget}

	big := strings.Repeat("x", 8<<10)
	for seq := uint64(1); seq <= frameCacheSlots; seq++ {
		u := store.Update{Type: store.UpdateConfigReload, Source: "test", Seq: seq, Payload: big}
		api, ok := c.convert(u)
		if !ok {
			t.Fatalf("seq %d did not convert", seq)
		}
		if _, err := c.marshal(seq, api); err != nil {
			t.Fatalf("seq %d marshal: %v", seq, err)
		}
	}

	if retained := c.retained.Load(); retained > budget {
		t.Errorf("retained %d bytes over a %d budget — the ring is unbounded in bytes", retained, budget)
	}
}
