package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grovetools/core/pkg/models"
	navbindings "github.com/grovetools/nav/pkg/bindings"
)

// seedNavBindings writes a known sessions.yml into a sandboxed GROVE_HOME
// and returns it. The sandbox also redirects the generated-bindings conf
// path, so no handler in these tests can touch real user state.
func seedNavBindings(t *testing.T) *models.NavSessionsFile {
	t.Helper()
	t.Setenv("GROVE_HOME", t.TempDir())

	file := &models.NavSessionsFile{
		Sessions: map[string]models.NavSessionConfig{
			"a": {Path: "/tmp/proj-a"},
			"b": {Path: "/tmp/proj-b"},
		},
		LockedKeys: []string{"v", "o"},
		Groups: map[string]models.NavGroupState{
			"work": {Sessions: map[string]models.NavSessionConfig{
				"e": {Path: "/tmp/proj-e"},
			}},
		},
		LastAccessedGroup: "work",
	}
	if err := navbindings.Save(navbindings.DefaultPath(), file); err != nil {
		t.Fatalf("failed to seed bindings: %v", err)
	}
	return file
}

// putJSON drives a handler directly and returns the recorder.
func putJSON(t *testing.T, handler http.HandlerFunc, url string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func assertStatus(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("response should be JSON: %v", err)
	}
	if resp["status"] != want {
		t.Fatalf("expected status %q, got %q", want, resp["status"])
	}
}

// A byte-identical re-PUT of a group's current state must not fall through
// to the persist + regenerate + reload + broadcast train. nav re-PUTs its
// state on every tab focus, so the no-op path is the hot path — and the
// handlers reach s.engine (nil here) only on real changes, which this test
// relies on: a missing short-circuit panics rather than silently passing.
func TestHandleNavGroupNoOpShortCircuit(t *testing.T) {
	seeded := seedNavBindings(t)
	s := New(false)

	w := putJSON(t, s.handleNavGroup, "/api/nav/groups/work", seeded.Groups["work"])
	assertStatus(t, w, "unchanged")

	w = putJSON(t, s.handleNavGroup, "/api/nav/groups/default",
		models.NavGroupState{Sessions: seeded.Sessions})
	assertStatus(t, w, "unchanged")
}

func TestHandleNavLockedKeysNoOpShortCircuit(t *testing.T) {
	seeded := seedNavBindings(t)
	s := New(false)

	w := putJSON(t, s.handleNavLockedKeys, "/api/nav/locked-keys", seeded.LockedKeys)
	assertStatus(t, w, "unchanged")
}

func TestHandleNavLastAccessedGroupNoOpShortCircuit(t *testing.T) {
	seedNavBindings(t)
	s := New(false)

	w := putJSON(t, s.handleNavLastAccessedGroup, "/api/nav/last-accessed",
		map[string]string{"group": "work"})
	assertStatus(t, w, "unchanged")
}
