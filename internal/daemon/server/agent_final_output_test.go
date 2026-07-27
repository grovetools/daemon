package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func newAgentTestServer(t *testing.T) *Server {
	t.Helper()
	s := New(false)
	s.SetEngine(engine.New(store.New()))
	return s
}

func postFinalOutput(t *testing.T, s *Server, jobID, text string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+jobID+"/final_output", strings.NewReader(text))
	rec := httptest.NewRecorder()
	s.handleAgentByID(rec, req)
	return rec
}

func getCapture(t *testing.T, s *Server, jobID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+jobID+"/capture", nil)
	rec := httptest.NewRecorder()
	s.handleAgentByID(rec, req)
	return rec
}

// connectTerminal registers a WebSocket primary on the server's terminal hub so
// HasConnections() reports true, i.e. the capture handler takes the SSE
// round-trip tier instead of failing fast. Returns the live connection.
func connectTerminal(t *testing.T, s *Server) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.terminalHub.HandleWS))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial terminal hub: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.WriteJSON(map[string]any{"type": "register"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !s.terminalHub.HasConnections() {
		if time.Now().After(deadline) {
			t.Fatal("terminal hub never reported a connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return conn
}

func TestFinalOutputStoreExpiresAfterTTL(t *testing.T) {
	st := newFinalOutputStore()
	now := time.Now()
	st.now = func() time.Time { return now }

	st.Put("job-1", "extension load failed")
	if got, ok := st.Get("job-1"); !ok || got != "extension load failed" {
		t.Fatalf("Get() = %q, %v; want retained snapshot", got, ok)
	}

	now = now.Add(finalOutputTTL + time.Second)
	if got, ok := st.Get("job-1"); ok {
		t.Fatalf("Get() after TTL = %q, %v; want dropped", got, ok)
	}
	if st.Len() != 0 {
		t.Fatalf("Len() after TTL = %d; want 0", st.Len())
	}
}

func TestFinalOutputStoreBoundsSize(t *testing.T) {
	st := newFinalOutputStore()
	// The tail is the part that explains the exit, so trimming must drop the
	// head — a snapshot ending in the error is the whole point.
	text := strings.Repeat("x", finalOutputMaxBytes) + "PANIC"
	st.Put("job-1", text)

	got, ok := st.Get("job-1")
	if !ok {
		t.Fatal("snapshot not retained")
	}
	if len(got) != finalOutputMaxBytes {
		t.Fatalf("len = %d; want %d", len(got), finalOutputMaxBytes)
	}
	if !strings.HasSuffix(got, "PANIC") {
		t.Fatalf("trimmed snapshot lost its tail: %q", got[len(got)-16:])
	}
}

func TestFinalOutputStoreBoundsEntryCount(t *testing.T) {
	st := newFinalOutputStore()
	for i := 0; i < finalOutputMaxEntries+10; i++ {
		st.Put(fmt.Sprintf("job-%03d", i), "out")
	}

	if st.Len() != finalOutputMaxEntries {
		t.Fatalf("Len() = %d; want %d", st.Len(), finalOutputMaxEntries)
	}
	// FIFO: the oldest snapshots are the ones evicted.
	if _, ok := st.Get("job-000"); ok {
		t.Fatal("oldest entry survived eviction")
	}
	newest := fmt.Sprintf("job-%03d", finalOutputMaxEntries+9)
	if _, ok := st.Get(newest); !ok {
		t.Fatalf("newest entry %s was evicted", newest)
	}
}

func TestFinalOutputStoreIgnoresEmptyAndReplacesInPlace(t *testing.T) {
	st := newFinalOutputStore()
	st.Put("job-1", "first")
	st.Put("job-1", "second")
	st.Put("job-1", "")

	if got, _ := st.Get("job-1"); got != "second" {
		t.Fatalf("Get() = %q; want %q (empty push must not clobber)", got, "second")
	}
	if st.Len() != 1 {
		t.Fatalf("Len() = %d; want 1 (replace must not duplicate)", st.Len())
	}
}

// The incident shape: the agent PTY is gone and no terminal is connected, so
// both live capture tiers are dead. The death-rattle snapshot must answer.
func TestAgentCaptureServesRetainedSnapshotWhenNoLiveTierAvailable(t *testing.T) {
	s := newAgentTestServer(t)

	rec := postFinalOutput(t, s, "job-1", "Error: failed to load extension\nHint: start without extensions")
	if rec.Code != http.StatusOK {
		t.Fatalf("final_output returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["status"] != "retained" {
		t.Fatalf("final_output body = %q (err %v)", rec.Body.String(), err)
	}

	capRec := getCapture(t, s, "job-1")
	if capRec.Code != http.StatusOK {
		t.Fatalf("capture returned %d: %s", capRec.Code, capRec.Body.String())
	}
	if !strings.Contains(capRec.Body.String(), "failed to load extension") {
		t.Fatalf("capture body = %q; want the retained snapshot", capRec.Body.String())
	}
	if src := capRec.Header().Get("X-Grove-Capture-Source"); src != "final_output" {
		t.Fatalf("X-Grove-Capture-Source = %q; want final_output", src)
	}
}

func TestAgentCaptureStillErrorsWithoutSnapshot(t *testing.T) {
	s := newAgentTestServer(t)

	rec := getCapture(t, s, "job-unknown")
	if rec.Code == http.StatusOK {
		t.Fatalf("capture returned 200 with no live tier and no snapshot: %q", rec.Body.String())
	}
}

// The retained snapshot is the LAST tier: a terminal that actually answers
// wins, because its screen is current and the snapshot is a post-mortem.
func TestAgentCapturePrefersLiveTerminalOverRetainedSnapshot(t *testing.T) {
	s := newAgentTestServer(t)
	connectTerminal(t, s)
	postFinalOutput(t, s, "job-1", "STALE SNAPSHOT")

	// Stand in for groveterm: answer the pending capture waiter.
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			s.captureWaitersMu.Lock()
			_, pending := s.captureWaiters["job-1"]
			s.captureWaitersMu.Unlock()
			if pending {
				postFinalOutputResponse(s, "job-1", "LIVE SCREEN")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	rec := getCapture(t, s, "job-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("capture returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "LIVE SCREEN" {
		t.Fatalf("capture body = %q; want the live screen", got)
	}
	if src := rec.Header().Get("X-Grove-Capture-Source"); src != "" {
		t.Fatalf("live capture tagged as %q; want untagged", src)
	}
}

// A terminal is connected but the agent's panel already unmounted, so the SSE
// waiter can only time out — exactly the 5s dead end from the incident.
func TestAgentCaptureFallsBackToSnapshotWhenWaiterTimesOut(t *testing.T) {
	s := newAgentTestServer(t)
	connectTerminal(t, s)
	postFinalOutput(t, s, "job-1", "readPTYLoop terminal error")

	rec := getCapture(t, s, "job-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("capture returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "readPTYLoop terminal error" {
		t.Fatalf("capture body = %q; want the retained snapshot", got)
	}
}

// The endpoint itself must bound what it accepts; a runaway pane cannot push
// unbounded bytes into the daemon.
func TestAgentFinalOutputEndpointBoundsBodySize(t *testing.T) {
	s := newAgentTestServer(t)
	postFinalOutput(t, s, "job-1", strings.Repeat("y", finalOutputMaxBytes*2))

	got, ok := s.finalOutputs.Get("job-1")
	if !ok {
		t.Fatal("snapshot not retained")
	}
	if len(got) != finalOutputMaxBytes {
		t.Fatalf("retained %d bytes; want %d", len(got), finalOutputMaxBytes)
	}
}

func postFinalOutputResponse(s *Server, jobID, text string) {
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+jobID+"/capture_response", strings.NewReader(text))
	s.handleAgentByID(httptest.NewRecorder(), req)
}
