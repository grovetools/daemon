package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/assistant"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestAssistantEndpointsWithoutSupervisor: an ecosystem with no [assistant]
// block never wires a supervisor, so the endpoints must answer honestly
// instead of panicking on a nil pointer.
func TestAssistantEndpointsWithoutSupervisor(t *testing.T) {
	s := New(false)

	rec := httptest.NewRecorder()
	s.handleAssistantEnsure(rec, httptest.NewRequest(http.MethodPost, "/api/assistant/ensure", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("ensure without a supervisor returned %d, want 503", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handleAssistantStatus(rec, httptest.NewRequest(http.MethodGet, "/api/assistant/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status without a supervisor returned %d, want 200", rec.Code)
	}
	var status models.AssistantStatus
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.State != models.AssistantStateDisabled {
		t.Errorf("state = %q, want disabled", status.State)
	}
}

// TestAssistantEnsureDisabled: a supervisor that exists but was never enabled
// reports a configuration answer (412), not a server error — the pane shows
// the difference.
func TestAssistantEnsureDisabled(t *testing.T) {
	s := New(false)
	s.SetAssistantSupervisor(assistant.NewSupervisor(nil, "", nil))

	rec := httptest.NewRecorder()
	s.handleAssistantEnsure(rec, httptest.NewRequest(http.MethodPost, "/api/assistant/ensure", nil))
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("ensure on a disabled supervisor returned %d, want 412", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not enabled") {
		t.Errorf("body = %q, want the reason", rec.Body.String())
	}
}

// TestAssistantEnsureRejectsGET: ensure mutates (it can launch an agent), so
// it is POST-only.
func TestAssistantEnsureRejectsGET(t *testing.T) {
	s := New(false)
	rec := httptest.NewRecorder()
	s.handleAssistantEnsure(rec, httptest.NewRequest(http.MethodGet, "/api/assistant/ensure", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET ensure returned %d, want 405", rec.Code)
	}
}

// TestConvertToAPIUpdateAssistantStatus: the wire rule — a supervisor status
// that never reaches SSE leaves the rail pane blind to a stopped assistant.
func TestConvertToAPIUpdateAssistantStatus(t *testing.T) {
	payload := &models.AssistantStatus{Enabled: true, State: models.AssistantStateStopped, Plan: "steward"}
	api := convertToAPIUpdate(store.Update{
		Type:    store.UpdateAssistantStatus,
		Source:  "assistant_supervisor",
		Payload: payload,
	})
	if api == nil {
		t.Fatal("convertToAPIUpdate dropped assistant_status")
	}
	if api.UpdateType != "assistant_status" {
		t.Errorf("update_type = %q", api.UpdateType)
	}
	if api.Payload == nil {
		t.Error("payload not forwarded")
	}
}
