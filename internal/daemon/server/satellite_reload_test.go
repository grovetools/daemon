package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/grovetools/daemon/internal/daemon/satellite"
)

// TestSatelliteReloadHandler pins POST /api/satellites/reload's contract:
// POST-only; global-daemon-only (scoped → 400); 409 when satellites are
// disabled (no reloader wired — the boot registry load errored, or the
// ConnManager doesn't exist); 500 on a reload-time load failure; and the
// ReloadSummary JSON on success.
func TestSatelliteReloadHandler(t *testing.T) {
	post := func(s *Server) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/satellites/reload", nil)
		rec := httptest.NewRecorder()
		s.handleSatellitesReload(rec, req)
		return rec
	}

	t.Run("method not allowed", func(t *testing.T) {
		s := New(false)
		req := httptest.NewRequest(http.MethodGet, "/api/satellites/reload", nil)
		rec := httptest.NewRecorder()
		s.handleSatellitesReload(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET: status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("scoped daemon refused", func(t *testing.T) {
		s := New(false)
		s.SetScope("myscope")
		// A wired reloader must not rescue a scoped daemon — the scope gate
		// comes first.
		s.SetSatelliteReloader(func() (*satellite.ReloadSummary, error) {
			t.Fatal("reloader must not run on a scoped daemon")
			return nil, nil
		})
		if rec := post(s); rec.Code != http.StatusBadRequest {
			t.Fatalf("scoped: status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("satellites disabled", func(t *testing.T) {
		s := New(false) // global scope, no reloader wired
		if rec := post(s); rec.Code != http.StatusConflict {
			t.Fatalf("disabled: status = %d, want %d", rec.Code, http.StatusConflict)
		}
	})

	t.Run("reload-time load error", func(t *testing.T) {
		s := New(false)
		s.SetSatelliteReloader(func() (*satellite.ReloadSummary, error) {
			return nil, errors.New("boom")
		})
		if rec := post(s); rec.Code != http.StatusInternalServerError {
			t.Fatalf("load error: status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("success returns summary", func(t *testing.T) {
		want := &satellite.ReloadSummary{
			Added:     []string{"fresh"},
			Removed:   []string{"gone"},
			Changed:   []string{},
			Unchanged: []string{"stable"},
		}
		s := New(false)
		s.SetSatelliteReloader(func() (*satellite.ReloadSummary, error) {
			return want, nil
		})
		rec := post(s)
		if rec.Code != http.StatusOK {
			t.Fatalf("success: status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body)
		}
		var got satellite.ReloadSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode summary: %v", err)
		}
		if !reflect.DeepEqual(&got, want) {
			t.Fatalf("summary = %+v, want %+v", got, *want)
		}
	})
}
