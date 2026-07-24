package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func validSubjobEvent() models.SubjobEvent {
	return models.SubjobEvent{
		SchemaVersion: 1,
		Kind:          models.SubjobReportReady,
		PlanKey:       strings.Repeat("a", 64),
		ParentJobID:   "parent",
		ChildJobID:    "child",
		ReportSHA256:  strings.Repeat("b", 64),
	}
}

func TestHandleSubjobEventAndFilteredSnapshot(t *testing.T) {
	s, st := newWorkflowTestServer(t)
	ev := validSubjobEvent()
	body, _ := json.Marshal(ev)
	w := httptest.NewRecorder()
	s.handleSubjobEvent(w, httptest.NewRequest(http.MethodPost, "/api/subjobs/event", bytes.NewReader(body)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("publish status = %d, body=%s", w.Code, w.Body.String())
	}
	if got := st.GetSubjobSnapshot(ev.PlanKey, ev.ParentJobID).Reports[ev.ChildJobID]; got == nil || got.State != models.SubjobReportReady {
		t.Fatalf("event was not folded: %+v", got)
	}

	w = httptest.NewRecorder()
	s.handleGetSubjobs(w, httptest.NewRequest(http.MethodGet, "/api/subjobs?plan_key="+ev.PlanKey+"&parent_job_id="+ev.ParentJobID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body=%s", w.Code, w.Body.String())
	}
	var snapshot models.SubjobSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil || snapshot.Reports[ev.ChildJobID] == nil {
		t.Fatalf("snapshot = %+v, err=%v", snapshot, err)
	}

	w = httptest.NewRecorder()
	s.handleGetSubjobs(w, httptest.NewRequest(http.MethodGet, "/api/subjobs?plan_key="+ev.PlanKey+"&parent_job_id=other", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("filtered snapshot status = %d", w.Code)
	}
	snapshot = models.SubjobSnapshot{}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil || len(snapshot.Reports) != 0 {
		t.Fatalf("snapshot leaked another parent: %+v, err=%v", snapshot, err)
	}
}

func TestSubjobHandlersRejectInvalidAndTCPRequests(t *testing.T) {
	s, _ := newWorkflowTestServer(t)
	for name, body := range map[string]string{
		"unknown field": `{"schema_version":1,"extra":true}`,
		"invalid kind":  `{"schema_version":1,"kind":"bogus","plan_key":"` + strings.Repeat("a", 64) + `","parent_job_id":"p","child_job_id":"c","report_sha256":"` + strings.Repeat("b", 64) + `"}`,
		"trailing json": string(mustJSON(t, validSubjobEvent())) + `{}`,
		"oversized":     strings.Repeat("x", (16<<10)+1),
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleSubjobEvent(w, httptest.NewRequest(http.MethodPost, "/api/subjobs/event", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
		})
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/subjobs", nil)
	s.handleGetSubjobs(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing filters status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/subjobs/event", bytes.NewReader(mustJSON(t, validSubjobEvent())))
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{}))
	unixOnly(s.handleSubjobEvent)(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("TCP request status = %d, want 403", w.Code)
	}
}

func TestConvertSubjobUpdatesRoundTrip(t *testing.T) {
	for _, typ := range []store.UpdateType{store.UpdateSubjobReportReady, store.UpdateSubjobJoined} {
		ev := validSubjobEvent()
		if typ == store.UpdateSubjobJoined {
			ev.Kind = models.SubjobJoined
		}
		apiUpdate := convertToAPIUpdate(store.Update{Type: typ, Source: "flow", Payload: &ev})
		if apiUpdate == nil || apiUpdate.UpdateType != string(typ) {
			t.Fatalf("conversion dropped %s: %+v", typ, apiUpdate)
		}
		wire, err := json.Marshal(apiUpdate)
		if err != nil {
			t.Fatal(err)
		}
		var got coredaemon.StateUpdate
		if err := json.Unmarshal(wire, &got); err != nil || got.UpdateType != string(typ) || got.Payload == nil {
			t.Fatalf("wire update = %+v, err=%v", got, err)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
