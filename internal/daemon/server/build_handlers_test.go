package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/buildqueue"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// newBuildTestServer returns a Server wired with a live build scheduler
// (no store — lifecycle broadcast is exercised by the buildqueue tests).
func newBuildTestServer(t *testing.T) (*Server, context.CancelFunc) {
	t.Helper()
	s := New(false)
	sched := buildqueue.New(nil, 2)
	ctx, cancel := context.WithCancel(context.Background())
	sched.Start(ctx)
	s.SetBuildScheduler(sched)
	return s, cancel
}

func TestBuildSubmitAndStreamHandlers(t *testing.T) {
	s, cancel := newBuildTestServer(t)
	defer cancel()

	// Submit a quick job over the HTTP handler.
	body, _ := json.Marshal(models.BuildJobRequest{
		Workspace: "ws",
		Dir:       t.TempDir(),
		Command:   []string{"sh", "-c", "echo handler-test-line"},
		Env:       os.Environ(),
		GroupID:   "hg",
		Verb:      "build",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/build/submit", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.handleBuildSubmit(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp models.BuildSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.JobID == "" {
		t.Fatalf("bad submit response %q: %v", rec.Body.String(), err)
	}

	// Give the job time to finish so the stream handler exercises the
	// history-replay + terminal-close path.
	deadline := time.Now().Add(10 * time.Second)
	var streamBody string
	for {
		streamReq := httptest.NewRequest(http.MethodGet, "/api/build/jobs/"+resp.JobID+"/stream", nil)
		streamRec := httptest.NewRecorder()
		s.handleBuildJobSubpath(streamRec, streamReq)
		if streamRec.Code != http.StatusOK {
			t.Fatalf("stream returned %d: %s", streamRec.Code, streamRec.Body.String())
		}
		streamBody = streamRec.Body.String()
		if strings.Contains(streamBody, `"event":"finished"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished; last stream body: %q", streamBody)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, want := range []string{`"event":"queued"`, `"event":"started"`, "handler-test-line", "event: build"} {
		if !strings.Contains(streamBody, want) {
			t.Errorf("stream body missing %q:\n%s", want, streamBody)
		}
	}
}

func TestBuildCancelHandler(t *testing.T) {
	s, cancel := newBuildTestServer(t)
	defer cancel()

	body, _ := json.Marshal(models.BuildCancelRequest{GroupID: "nonexistent"})
	req := httptest.NewRequest(http.MethodPost, "/api/build/cancel", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.handleBuildCancel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel returned %d: %s", rec.Code, rec.Body.String())
	}

	// Missing group_id is a client error.
	req = httptest.NewRequest(http.MethodPost, "/api/build/cancel", strings.NewReader("{}"))
	rec = httptest.NewRecorder()
	s.handleBuildCancel(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("cancel without group_id returned %d, want 400", rec.Code)
	}
}

func TestBuildEndpointsWithoutScheduler(t *testing.T) {
	s := New(false)

	req := httptest.NewRequest(http.MethodPost, "/api/build/submit", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	s.handleBuildSubmit(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("submit without scheduler returned %d, want 503", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/build/jobs/x/stream", nil)
	rec = httptest.NewRecorder()
	s.handleBuildJobSubpath(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("stream without scheduler returned %d, want 503", rec.Code)
	}
}

func TestConvertToAPIUpdateBuildEvents(t *testing.T) {
	// The wire rule: every build_* store update must map to a distinct
	// update_type or SSE consumers silently never see it.
	payload := &store.BuildEventPayload{JobID: "b1", GroupID: "g", Status: "queued"}
	for _, typ := range []store.UpdateType{store.UpdateBuildQueued, store.UpdateBuildStarted, store.UpdateBuildFinished} {
		u := store.Update{Type: typ, Source: "buildqueue", Payload: payload}
		api := convertToAPIUpdate(u)
		if api == nil {
			t.Fatalf("convertToAPIUpdate dropped %s", typ)
		}
		if api.UpdateType != string(typ) {
			t.Errorf("update_type for %s = %q", typ, api.UpdateType)
		}
		if api.Payload == nil {
			t.Errorf("payload for %s not forwarded", typ)
		}
	}
}
