package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// newWorkflowTestServer wires a Server to a fresh store backed by a temp
// state dir.
func newWorkflowTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	t.Setenv("GROVE_HOME", t.TempDir())
	st := store.New()
	s := New(false)
	s.SetEngine(engine.New(st))
	return s, st
}

// TestConvertWorkflowUpdatesRoundTrip proves all three wire layers agree:
// store.Update → convertToAPIUpdate (apiStateUpdate) → JSON → core
// daemon.StateUpdate, with a DISTINCT update_type per workflow constant and
// the payload surviving intact.
func TestConvertWorkflowUpdatesRoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 10, 17, 7, 14, 0, time.UTC)
	cases := []struct {
		updateType store.UpdateType
		wantString string
		event      models.WorkflowEvent
	}{
		{store.UpdateWorkflowRunDiscovered, "workflow_run_discovered", models.WorkflowEvent{
			Kind: models.WorkflowRunDiscovered, JobID: "j1", ClaudeSessionID: "s1",
			RunID: "wf_1", Timestamp: ts, Source: models.WorkflowSourceJournal,
		}},
		{store.UpdateWorkflowAgentStarted, "workflow_agent_started", models.WorkflowEvent{
			Kind: models.WorkflowAgentStarted, JobID: "j1", ClaudeSessionID: "s1",
			RunID: "wf_1", AgentID: "a1", AgentType: "workflow-subagent",
			Timestamp: ts, Source: models.WorkflowSourceHooks,
		}},
		{store.UpdateWorkflowAgentCompleted, "workflow_agent_completed", models.WorkflowEvent{
			Kind: models.WorkflowAgentCompleted, JobID: "j1", ClaudeSessionID: "s1",
			RunID: "wf_1", AgentID: "a1", ResultSummary: "ok",
			LastMessage: "done", Timestamp: ts, Source: models.WorkflowSourceHooks,
		}},
		{store.UpdateWorkflowRunStale, "workflow_run_stale", models.WorkflowEvent{
			Kind: models.WorkflowRunStale, JobID: "j1", ClaudeSessionID: "s1",
			RunID: "wf_1", Timestamp: ts, Source: models.WorkflowSourceJournal,
		}},
	}

	seen := make(map[string]bool)
	for _, tc := range cases {
		t.Run(tc.wantString, func(t *testing.T) {
			payload := &store.WorkflowEventPayload{Event: tc.event, RunName: "flow", Phases: []string{"P1"}}
			apiUpdate := convertToAPIUpdate(store.Update{
				Type:    tc.updateType,
				Source:  tc.event.Source,
				Payload: payload,
			})
			if apiUpdate == nil {
				t.Fatal("convertToAPIUpdate returned nil — this workflow update would be silently invisible on SSE")
			}
			if apiUpdate.UpdateType != tc.wantString {
				t.Fatalf("update_type = %q, want %q", apiUpdate.UpdateType, tc.wantString)
			}
			if seen[apiUpdate.UpdateType] {
				t.Fatalf("update_type %q is not distinct", apiUpdate.UpdateType)
			}
			seen[apiUpdate.UpdateType] = true

			// Wire round-trip: SSE JSON → core daemon.StateUpdate.
			data, err := json.Marshal(apiUpdate)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var stateUpdate coredaemon.StateUpdate
			if err := json.Unmarshal(data, &stateUpdate); err != nil {
				t.Fatalf("unmarshal into core StateUpdate: %v", err)
			}
			if stateUpdate.UpdateType != tc.wantString {
				t.Errorf("core update_type = %q, want %q", stateUpdate.UpdateType, tc.wantString)
			}
			if stateUpdate.Payload == nil {
				t.Fatal("core StateUpdate dropped the payload")
			}

			// And the payload decodes back into the typed struct losslessly.
			payloadJSON, err := json.Marshal(stateUpdate.Payload)
			if err != nil {
				t.Fatalf("re-marshal payload: %v", err)
			}
			var got store.WorkflowEventPayload
			if err := json.Unmarshal(payloadJSON, &got); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if !reflect.DeepEqual(got.Event, tc.event) {
				t.Errorf("event round-trip mismatch:\n  in:  %+v\n  out: %+v", tc.event, got.Event)
			}
			if got.RunName != "flow" || len(got.Phases) != 1 {
				t.Errorf("enrichment round-trip mismatch: %+v", got)
			}
		})
	}
}

func TestHandleWorkflowEvent(t *testing.T) {
	s, st := newWorkflowTestServer(t)

	ev := models.WorkflowEvent{
		Kind:            models.WorkflowAgentStarted,
		JobID:           "job-1",
		ClaudeSessionID: "sess-1",
		RunID:           "wf_1",
		AgentID:         "a1",
		AgentType:       "workflow-subagent",
		Source:          models.WorkflowSourceHooks,
		// Timestamp deliberately zero — the server must stamp it.
	}
	body, _ := json.Marshal(ev)

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/event", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleWorkflowEvent(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", w.Code, w.Body.String())
	}

	runs := st.GetWorkflowRuns()
	run := runs["wf_1"]
	if run == nil {
		t.Fatal("event was not applied to the store")
	}
	agent := run.Agents["a1"]
	if agent == nil {
		t.Fatal("agent a1 missing")
	}
	if agent.StartedAt.IsZero() {
		t.Error("zero timestamp must be stamped server-side")
	}

	t.Run("unknown kind rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/event",
			bytes.NewReader([]byte(`{"kind":"bogus","agent_id":"a"}`)))
		w := httptest.NewRecorder()
		s.handleWorkflowEvent(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("GET rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/workflows/event", nil)
		w := httptest.NewRecorder()
		s.handleWorkflowEvent(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", w.Code)
		}
	})
}

func TestHandleGetWorkflows(t *testing.T) {
	s, st := newWorkflowTestServer(t)

	ts := time.Date(2026, 6, 10, 17, 0, 0, 0, time.UTC)
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkflowAgentStarted,
		Source: "journal",
		Payload: &store.WorkflowEventPayload{Event: models.WorkflowEvent{
			Kind: models.WorkflowAgentStarted, JobID: "job-1", ClaudeSessionID: "sess-1",
			RunID: "wf_1", AgentID: "a1", Timestamp: ts, Source: models.WorkflowSourceJournal,
		}},
	})
	// A run-less agent must carry a GENUINE spawn id — 'a' + 16 hex, the shape
	// Claude Code mints for a real Task spawn. Since d2e1dbb the store drops a
	// run-less started event whose agent id is any other shape, because that is
	// the phantom type-registration the harness fires once per agent definition
	// at session init (see isSpawnAgentID / store.TestPhantom…). This test was
	// written before that guard and kept a placeholder id, so it asserted the
	// ad-hoc bucket would fill from an event the store is right to discard.
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkflowAgentStarted,
		Source: "hooks",
		Payload: &store.WorkflowEventPayload{Event: models.WorkflowEvent{
			Kind: models.WorkflowAgentStarted, JobID: "job-2", ClaudeSessionID: "sess-2",
			AgentID: adhocSpawnAgentID, AgentType: "Explore", Timestamp: ts, Source: models.WorkflowSourceHooks,
		}},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	w := httptest.NewRecorder()
	s.handleGetWorkflows(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var snapshot struct {
		Runs  map[string]*models.WorkflowRunState    `json:"runs"`
		Adhoc map[string]map[string]*models.Subagent `json:"adhoc"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.Runs["wf_1"] == nil || snapshot.Runs["wf_1"].Agents["a1"] == nil {
		t.Errorf("snapshot missing run agent: %+v", snapshot.Runs)
	}
	if snapshot.Adhoc["job-2"][adhocSpawnAgentID] == nil {
		t.Errorf("snapshot missing ad-hoc agent: %+v", snapshot.Adhoc)
	}

	// The other half of the same contract: a phantom-shaped id reaches the
	// snapshot from nowhere. Pinned here, not only in the store, because this
	// handler is what the TUI reads and a regression would show up as agents
	// appearing that were never spawned.
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkflowAgentStarted,
		Source: "hooks",
		Payload: &store.WorkflowEventPayload{Event: models.WorkflowEvent{
			Kind: models.WorkflowAgentStarted, JobID: "job-3", ClaudeSessionID: "sess-3",
			AgentID: "a03e225", AgentType: "Explore", Timestamp: ts, Source: models.WorkflowSourceHooks,
		}},
	})
	w = httptest.NewRecorder()
	s.handleGetWorkflows(w, httptest.NewRequest(http.MethodGet, "/api/workflows", nil))
	snapshot.Adhoc = nil
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(snapshot.Adhoc["job-3"]) != 0 {
		t.Errorf("a phantom type-registration reached the snapshot: %+v", snapshot.Adhoc["job-3"])
	}
}

// adhocSpawnAgentID is a genuine spawn id: 'a' followed by exactly 16 hex
// digits. Any other shape is a phantom registration the store discards.
const adhocSpawnAgentID = "a62124203bfeb94f0"

func TestUnixOnlyMiddleware(t *testing.T) {
	handler := unixOnly(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	withLocalAddr := func(req *http.Request, addr net.Addr) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
	}

	t.Run("tcp listener rejected", func(t *testing.T) {
		req := withLocalAddr(httptest.NewRequest(http.MethodGet, "/api/workflows", nil),
			&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7777})
		w := httptest.NewRecorder()
		handler(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403 for TCP-origin request", w.Code)
		}
	})

	t.Run("unix socket allowed", func(t *testing.T) {
		req := withLocalAddr(httptest.NewRequest(http.MethodGet, "/api/workflows", nil),
			&net.UnixAddr{Name: "/tmp/groved.sock", Net: "unix"})
		w := httptest.NewRecorder()
		handler(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 for unix-socket request", w.Code)
		}
	})
}
