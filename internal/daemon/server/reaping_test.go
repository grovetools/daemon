package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestLookupRegistryPID verifies the cross-daemon PID bridge: a session
// confirmed (and registered) by one daemon can have its real PID recovered by
// another daemon — the global registry is the shared link. This is what lets
// killSession SIGTERM, and the collector reap, a session whose in-store record
// carries PID 0.
func TestLookupRegistryPID(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())

	reg, err := sessions.NewFileSystemRegistry()
	if err != nil {
		t.Fatalf("NewFileSystemRegistry: %v", err)
	}
	const jobID = "coordinate-some-feature-abc123"
	if err := reg.Register(sessions.SessionMetadata{
		SessionID:       jobID,
		JobID:           jobID,
		ClaudeSessionID: "11111111-2222-3333-4444-555555555555",
		PID:             4242,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	md := lookupRegistryPID(jobID, "")
	if md == nil {
		t.Fatal("lookupRegistryPID returned nil for a registered job")
	}
	if md.PID != 4242 {
		t.Errorf("recovered PID = %d, want 4242", md.PID)
	}
	if md.ClaudeSessionID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("recovered native id = %q, want the registered UUID", md.ClaudeSessionID)
	}

	if got := lookupRegistryPID("no-such-job", ""); got != nil {
		t.Errorf("lookupRegistryPID for a missing job = %+v, want nil", got)
	}
}

func TestLookupRegistryPIDUsesExactAttempt(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	reg, err := sessions.NewFileSystemRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range []sessions.SessionMetadata{
		{SessionID: "job", JobID: "job", AttemptID: "attempt-old", PID: 111},
		{SessionID: "job", JobID: "job", AttemptID: "attempt-current", PID: 222},
	} {
		if err := reg.Register(md); err != nil {
			t.Fatal(err)
		}
	}
	if got := lookupRegistryPID("job", "attempt-current"); got == nil || got.PID != 222 {
		t.Fatalf("exact current attempt lookup = %+v, want PID 222", got)
	}
	if got := lookupRegistryPID("wrong-job", "attempt-current"); got != nil {
		t.Fatalf("point lookup accepted wrong JobID armor: %+v", got)
	}
}

func TestKillSessionEndsRealPendingIntentWithAttempt(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateSessionIntent, Payload: &store.SessionIntentPayload{
		JobID: "pending-job", AttemptID: "attempt-pending", Provider: "claude", Type: models.SessionTypeInteractiveAgent,
	}})
	s := New(false)
	s.SetEngine(engine.New(st))
	if err := s.killSession("pending-job"); err != nil {
		t.Fatalf("kill pending PID-0 intent: %v", err)
	}
	got := st.GetSession("pending-job")
	if got == nil || got.AttemptID != "attempt-pending" || got.Status != "interrupted" || got.EndedAt == nil {
		t.Fatalf("pending intent was not ended with identity preserved: %+v", got)
	}
}

func TestPersistConfirmationUpgradesExactAttemptRegistryRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)
	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateSessionIntent, Payload: &store.SessionIntentPayload{
		JobID: "job", AttemptID: "attempt", Provider: "codex", Type: models.SessionTypeHeadlessAgent,
		WorkDir: "/workspace", JobFilePath: "/plan/01-job.md", PlanName: "plan", Title: "Job",
	}})
	s := New(false)
	s.scope = "/ecosystem"
	s.SetEngine(engine.New(st))
	s.persistConfirmedSessionToRegistry(&store.SessionConfirmationPayload{
		JobID: "job", AttemptID: "attempt", NativeID: "native", PID: 4242, TranscriptPath: "/tmp/transcript.jsonl",
	})
	reg, err := sessions.NewFileSystemRegistry()
	if err != nil {
		t.Fatal(err)
	}
	md, err := reg.Find("attempt")
	if err != nil {
		t.Fatalf("Find exact attempt: %v", err)
	}
	if md.AttemptID != "attempt" || md.JobID != "job" || md.ClaudeSessionID != "native" || md.Status != "running" || md.Scope != "/ecosystem" || md.Type != models.SessionTypeHeadlessAgent || md.Provider != "codex" {
		t.Fatalf("confirmed registry metadata missing identity/status/scope/type: %+v", md)
	}
	entries, err := os.ReadDir(filepath.Join(paths.StateDir(), "hooks", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "attempt" {
		t.Fatalf("registry rows = %v, want exactly attempt/", entries)
	}
}

func TestSessionEndHTTPPayloadCarriesAttemptIdentity(t *testing.T) {
	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateSessionIntent, Payload: &store.SessionIntentPayload{
		JobID: "job", AttemptID: "current", Provider: "claude", Type: models.SessionTypeInteractiveAgent,
	}})
	s := New(false)
	s.SetEngine(engine.New(st))

	request := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/job/end", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleSessionByID(w, req)
		return w.Code
	}
	if code := request(`{"attempt_id":"stale","outcome":"interrupted"}`); code != http.StatusOK {
		t.Fatalf("stale end HTTP status = %d", code)
	}
	if got := st.GetSession("job"); got.Status != "pending" || got.EndedAt != nil {
		t.Fatalf("stale HTTP end mutated current attempt: %+v", got)
	}
	if code := request(`{"attempt_id":"current","outcome":"interrupted"}`); code != http.StatusOK {
		t.Fatalf("current end HTTP status = %d", code)
	}
	if got := st.GetSession("job"); got.Status != "interrupted" || got.EndedAt == nil {
		t.Fatalf("current HTTP end did not apply: %+v", got)
	}
}

func TestKillSessionRejectsSyntheticRow(t *testing.T) {
	st := store.New()
	st.ApplyUpdate(store.Update{Type: store.UpdateSessions, Payload: []*models.Session{{
		ID: "synthetic", Type: models.SessionTypeInteractiveAgent, Status: "pending", Synthetic: true, Provenance: "flow-job-fabricated",
	}}})
	s := New(false)
	s.SetEngine(engine.New(st))
	if err := s.killSession("synthetic"); err == nil {
		t.Fatal("kill accepted a synthetic row")
	}
}
