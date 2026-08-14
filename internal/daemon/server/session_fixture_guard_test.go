package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// The user's Agents drawer once held "Env Dump" and "pi Headless E2E" — flow
// jobs that only ever existed inside a tend sandbox, registered on the REAL
// daemon because the scenario inherited the host's GROVE_HOST_DAEMON_SOCKET.
// Nothing ends a session whose process never existed, so they stayed for days.

func newSessionTestServer(t *testing.T, socketPath string) *Server {
	t.Helper()
	t.Setenv("GROVE_HOME", t.TempDir())
	s := New(false)
	s.SetEngine(engine.New(store.New()))
	s.socketPath = socketPath
	return s
}

func postIntent(t *testing.T, s *Server, intent store.SessionIntentPayload) int {
	t.Helper()
	body, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/intent", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSessionIntent(w, req)
	return w.Code
}

func TestSessionIntentFromFixtureRejectedByRealDaemon(t *testing.T) {
	// A real daemon: its socket lives in the user's state dir.
	s := newSessionTestServer(t, filepath.Join(t.TempDir(), "groved.sock"))

	fixtureHome := filepath.Join(os.TempDir(), "grove-tend-pi-headless-launch-3744334191", "home")
	code := postIntent(t, s, store.SessionIntentPayload{
		JobID:       "pi-headless-e2e",
		Title:       "pi Headless E2E",
		Provider:    "pi",
		Type:        "headless_agent",
		WorkDir:     filepath.Join(fixtureHome, "code", "pi-headless-project"),
		JobFilePath: filepath.Join(fixtureHome, "notebooks", "plans", "01-pi-headless.md"),
	})
	if code != http.StatusForbidden {
		t.Errorf("real daemon accepted a fixture session intent (status %d, want %d)", code, http.StatusForbidden)
	}
}

func TestSessionIntentFromFixtureAcceptedByFixtureDaemon(t *testing.T) {
	// A sandbox daemon: its own socket is in the fixture namespace, and the
	// scenario under test is asserting on exactly these sessions.
	s := newSessionTestServer(t, filepath.Join(os.TempDir(), "tend-pi-headless-launch-1", "grove", "groved.sock"))

	code := postIntent(t, s, store.SessionIntentPayload{
		JobID:   "pi-headless-e2e",
		Title:   "pi Headless E2E",
		WorkDir: filepath.Join(os.TempDir(), "grove-tend-pi-headless-launch-1", "home", "code", "proj"),
	})
	if code != http.StatusCreated {
		t.Errorf("fixture daemon refused its own sandbox session (status %d, want %d)", code, http.StatusCreated)
	}
}

func TestSessionIntentFromRealWorktreeAccepted(t *testing.T) {
	s := newSessionTestServer(t, filepath.Join(t.TempDir(), "groved.sock"))

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	code := postIntent(t, s, store.SessionIntentPayload{
		JobID:   "real-job",
		Title:   "Real Job",
		WorkDir: filepath.Join(home, ".local", "share", "grove", "worktrees", "grovetools-abc", "misc-fixes"),
	})
	if code != http.StatusCreated {
		t.Errorf("real session rejected (status %d, want %d)", code, http.StatusCreated)
	}
}
