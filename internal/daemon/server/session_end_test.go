package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func TestHandleSessionEndDuplicateIsNoOp(t *testing.T) {
	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{{
			ID:     "job-1",
			Status: "running",
		}},
	})
	s := New(false)
	s.SetEngine(engine.New(st))

	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	postEnd := func(outcome string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/job-1/end", strings.NewReader(`{"outcome":"`+outcome+`"}`))
		rec := httptest.NewRecorder()
		s.handleSessionByID(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST end(%q) status = %d, body = %q", outcome, rec.Code, rec.Body.String())
		}
		return rec
	}

	postEnd("exited")
	select {
	case update := <-sub:
		if update.Type != store.UpdateSessionEnd {
			t.Fatalf("first update type = %q, want %q", update.Type, store.UpdateSessionEnd)
		}
	case <-time.After(time.Second):
		t.Fatal("first endpoint call did not publish session end")
	}
	first := st.GetSession("job-1")
	if first == nil || first.Status != "exited" || first.EndedAt == nil {
		t.Fatalf("first endpoint call left session = %+v", first)
	}

	postEnd("failed")
	second := st.GetSession("job-1")
	if second.Status != "exited" || second.EndedAt == nil || !second.EndedAt.Equal(*first.EndedAt) {
		t.Fatalf("duplicate endpoint call mutated session: first=%+v second=%+v", first, second)
	}
	select {
	case update := <-sub:
		t.Fatalf("duplicate endpoint call published update: %+v", update)
	case <-time.After(50 * time.Millisecond):
	}
}
