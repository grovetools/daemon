package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// streamHarness serves handleStreamState over loopback TCP. Driving the
// handler directly (rather than the whole unix-socket daemon) keeps these
// tests independent of the socket-path length limit that makes the
// Listen-based tests environment-sensitive.
type streamHarness struct {
	st  *store.Store
	srv *httptest.Server
}

func newStreamHarness(t *testing.T) *streamHarness {
	t.Helper()
	st := store.New()
	s := New(false)
	s.SetEngine(engine.New(st))
	srv := httptest.NewServer(http.HandlerFunc(s.handleStreamState))
	t.Cleanup(srv.Close)
	return &streamHarness{st: st, srv: srv}
}

// subscribe opens the SSE stream with the given query and returns the response
// plus a frame reader. Frames arrive decoded as apiStateUpdate.
func (h *streamHarness) subscribe(t *testing.T, query string) (*http.Response, func() *apiStateUpdate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	url := h.srv.URL + "/api/stream"
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	reader := bufio.NewReader(resp.Body)
	next := func() *apiStateUpdate {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			line, err := reader.ReadString('\n')
			if err != nil {
				return nil
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame apiStateUpdate
			if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "data: ")), &frame); err != nil {
				t.Fatalf("decode SSE frame %q: %v", line, err)
			}
			return &frame
		}
		return nil
	}
	return resp, next
}

// waitForSubscriber blocks until the handler has registered its store
// subscription, so a broadcast fired next is guaranteed to be delivered live.
func (h *streamHarness) waitForSubscriber(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		before := h.st.CurrentSeq()
		h.st.BroadcastConfigReload("probe")
		if h.st.CurrentSeq() > before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("store never accepted a probe broadcast")
}

func TestStreamAdvertisesItsFeatures(t *testing.T) {
	h := newStreamHarness(t)
	resp, _ := h.subscribe(t, "")

	if got := resp.Header.Get(StreamFeaturesHeader); got != StreamFeatures {
		t.Errorf("%s = %q, want %q", StreamFeaturesHeader, got, StreamFeatures)
	}
	if got := resp.Header.Get(StreamRingHeader); got != strconv.Itoa(store.RingSize) {
		t.Errorf("%s = %q, want %d", StreamRingHeader, got, store.RingSize)
	}
}

func TestStreamFramesCarrySequenceNumbers(t *testing.T) {
	h := newStreamHarness(t)
	_, next := h.subscribe(t, "")

	time.Sleep(100 * time.Millisecond)
	h.st.BroadcastConfigReload("a.toml")

	frame := nextOfType(t, next, "config_reload")
	if frame.Seq == 0 {
		t.Fatal("frame carried no seq — the cursor a client passes back as ?since= would be unusable")
	}
}

// nextOfType pulls frames until one of the wanted type arrives, skipping the
// "initial" snapshot (whose presence depends on ambient theme/workspace state).
func nextOfType(t *testing.T, next func() *apiStateUpdate, want string) *apiStateUpdate {
	t.Helper()
	for i := 0; i < 20; i++ {
		frame := next()
		if frame == nil {
			t.Fatalf("stream ended before a %q frame arrived", want)
		}
		if frame.UpdateType == want {
			return frame
		}
		if frame.UpdateType != "initial" {
			t.Fatalf("unexpected frame %q while waiting for %q", frame.UpdateType, want)
		}
	}
	t.Fatalf("no %q frame after 20 frames", want)
	return nil
}

func TestStreamTypeFilterDropsNonMatchingFrames(t *testing.T) {
	h := newStreamHarness(t)
	_, next := h.subscribe(t, "types=job_*,watcher_status")
	time.Sleep(100 * time.Millisecond)

	// config_reload must be filtered out; watcher_status must survive.
	h.st.BroadcastConfigReload("ignored.toml")
	h.st.ApplyUpdate(store.Update{Type: store.UpdateWatcherStatus, Source: "test", Payload: "up"})

	frame := next()
	if frame == nil {
		t.Fatal("no frame arrived")
	}
	if frame.UpdateType != "watcher_status" {
		t.Fatalf("first surviving frame = %q, want watcher_status (config_reload should have been filtered, "+
			"and the initial snapshot does not match the filter)", frame.UpdateType)
	}
}

func TestStreamRejectsAMalformedFilter(t *testing.T) {
	h := newStreamHarness(t)
	resp, err := http.Get(h.srv.URL + "/api/stream?types=job_[")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed glob", resp.StatusCode)
	}
}

func TestStreamRejectsAMalformedCursor(t *testing.T) {
	h := newStreamHarness(t)
	resp, err := http.Get(h.srv.URL + "/api/stream?since=soon")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a non-numeric cursor", resp.StatusCode)
	}
}

// The headline replay case: events published while the client was away are
// delivered on reconnect, in order, exactly once.
func TestStreamReplaysFromCursor(t *testing.T) {
	h := newStreamHarness(t)

	h.st.BroadcastConfigReload("one.toml")
	cursor := h.st.CurrentSeq()
	h.st.BroadcastConfigReload("two.toml")
	h.st.BroadcastConfigReload("three.toml")

	_, next := h.subscribe(t, "since="+strconv.FormatUint(cursor, 10))

	for _, want := range []string{"two.toml", "three.toml"} {
		frame := next()
		if frame == nil {
			t.Fatalf("replay ended before %s", want)
		}
		if frame.UpdateType == "initial" {
			t.Fatal("an exact resume must not re-send the initial snapshot")
		}
		if frame.ConfigFile != want {
			t.Fatalf("replayed %q, want %q", frame.ConfigFile, want)
		}
	}

	// Live frames continue after the replay, with no duplicate of the tail.
	h.waitForSubscriber(t)
	h.st.BroadcastConfigReload("four.toml")
	for {
		frame := next()
		if frame == nil {
			t.Fatal("live frame never arrived after the replay")
		}
		if frame.ConfigFile == "probe" {
			continue // waitForSubscriber's own broadcast
		}
		if frame.ConfigFile != "four.toml" {
			t.Fatalf("post-replay frame = %q, want four.toml (a duplicate means the watermark failed)", frame.ConfigFile)
		}
		return
	}
}

// A cursor from a previous daemon incarnation must produce a gap signal, not
// silence and not a bogus replay.
func TestStreamSignalsAGapForAFutureCursor(t *testing.T) {
	h := newStreamHarness(t)
	h.st.BroadcastConfigReload("one.toml")

	_, next := h.subscribe(t, "since=999999")

	frame := next()
	if frame == nil {
		t.Fatal("no frame arrived")
	}
	if frame.UpdateType != StreamGapUpdateType {
		t.Fatalf("first frame = %q, want %q", frame.UpdateType, StreamGapUpdateType)
	}
	payload, ok := frame.Payload.(map[string]any)
	if !ok {
		t.Fatalf("gap payload = %T, want an object", frame.Payload)
	}
	if payload["reason"] != store.ReplayGapReset {
		t.Errorf("gap reason = %v, want %q", payload["reason"], store.ReplayGapReset)
	}
	if payload["ring_size"] != float64(store.RingSize) {
		t.Errorf("gap ring_size = %v, want %d", payload["ring_size"], store.RingSize)
	}
}

// The gap frame is a control frame: it describes the stream, so ?types= must
// never suppress it — a client that filtered itself into silence would never
// learn it needs to reconcile.
func TestStreamGapSurvivesTheTypeFilter(t *testing.T) {
	h := newStreamHarness(t)
	h.st.BroadcastConfigReload("one.toml")

	_, next := h.subscribe(t, "since=999999&types=job_*")

	frame := next()
	if frame == nil {
		t.Fatal("no frame arrived")
	}
	if frame.UpdateType != StreamGapUpdateType {
		t.Fatalf("first frame = %q, want the gap control frame to bypass ?types=", frame.UpdateType)
	}
}

func TestTypeFilterMatching(t *testing.T) {
	cases := []struct {
		raw        string
		updateType string
		want       bool
	}{
		{"", "job_completed", true},
		{"   ", "job_completed", true},
		{",,", "job_completed", true},
		{"job_*", "job_completed", true},
		{"job_*", "note_event", false},
		{"job_*,note_event", "note_event", true},
		{" job_* , note_event ", "job_completed", true},
		{"*", "anything", true},
		{"job_completed", "job_completed", true},
		{"job_completed", "job_failed", false},
	}
	for _, tc := range cases {
		f, err := parseTypeFilter(tc.raw)
		if err != nil {
			t.Fatalf("parseTypeFilter(%q): %v", tc.raw, err)
		}
		if got := f.matches(tc.updateType); got != tc.want {
			t.Errorf("parseTypeFilter(%q).matches(%q) = %v, want %v", tc.raw, tc.updateType, got, tc.want)
		}
	}

	if _, err := parseTypeFilter("job_["); err == nil {
		t.Error("parseTypeFilter accepted a malformed glob")
	}
}

// representativePayloads supplies a well-typed payload for the handful of
// update types whose conversion type-asserts before building a frame. Every
// other type passes its payload through, so a nil payload is representative.
var representativePayloads = map[store.UpdateType]any{
	store.UpdateWorkspaces:      map[string]*models.EnrichedWorkspace{},
	store.UpdateWorkspacesDelta: []*models.WorkspaceDelta{{Path: "/tmp/ws"}},
	store.UpdatePlanIndexDelta:  &models.PlanIndexDelta{},
	store.UpdateTaskResult:      &store.TaskResultPayload{Workspace: "/tmp/ws", Verb: "build", Result: &models.TaskResult{}},
	store.UpdateBootPhase:       &coredaemon.BootStatus{Phase: "boot"},
}

// Every store update type must either reach the SSE wire or be declared in
// apiUpdateSkipList with a reason. A type in neither bucket is a silent drop —
// exactly the failure mode this job set out to remove.
func TestEveryUpdateTypeIsConvertedOrDeclared(t *testing.T) {
	for _, typ := range store.AllUpdateTypes() {
		converts := convertUpdatePayload(store.Update{Type: typ, Payload: representativePayloads[typ]}) != nil
		_, declared := apiUpdateSkipList[typ]

		switch {
		case converts && declared:
			t.Errorf("store update type %q both converts to SSE and is declared omitted in apiUpdateSkipList", typ)
		case !converts && !declared:
			t.Errorf("store update type %q neither converts to SSE nor is declared in apiUpdateSkipList — "+
				"add a case to convertUpdatePayload, or declare the omission", typ)
		}
	}
}

// A skip-list entry for a type nobody declares is dead documentation.
func TestSkipListNamesOnlyRealTypes(t *testing.T) {
	for typ, reason := range apiUpdateSkipList {
		if !store.IsKnownUpdateType(typ) {
			t.Errorf("apiUpdateSkipList declares unknown update type %q", typ)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("apiUpdateSkipList entry %q has no reason — the point of the list is the why", typ)
		}
	}
}
