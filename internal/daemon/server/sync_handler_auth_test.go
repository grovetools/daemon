package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// A rejected sync token is invisible in every counter the status payload
// carries: documents, outbox, cursors all stay exactly as they were while
// nothing replicates at all. That is the shape of the stale-token trap
// (contract §3 P2b), so the status endpoint reports it explicitly.
func TestHandleSyncStatusReportsTokenRejection(t *testing.T) {
	sandboxGroveHome(t)
	since := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Second)

	t.Run("reported while the token is rejected", func(t *testing.T) {
		s := newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))
		s.SetSyncAuthFailure(func() (string, time.Time, bool) {
			return "capabilities request rejected with status 401", since, true
		})

		out := getSyncStatus(t, s)
		if out.AuthError == "" {
			t.Fatal("a rejected token must be reported; the counters alone read as healthy")
		}
		if !out.AuthErrorSince.Equal(since) {
			t.Fatalf("AuthErrorSince = %v, want %v", out.AuthErrorSince, since)
		}
	})

	t.Run("absent while the token is good", func(t *testing.T) {
		s := newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))
		s.SetSyncAuthFailure(func() (string, time.Time, bool) { return "", time.Time{}, false })

		out := getSyncStatus(t, s)
		if out.AuthError != "" || !out.AuthErrorSince.IsZero() {
			t.Fatalf("healthy sync must not report a rejection: %q %v", out.AuthError, out.AuthErrorSince)
		}
	})

	t.Run("reported even when sync.db is not open", func(t *testing.T) {
		// A first-ever join whose token is already stale never gets a database
		// opened, which is exactly when the operator most needs to be told.
		s := New(false)
		s.SetSyncAuthFailure(func() (string, time.Time, bool) {
			return "capabilities request rejected with status 403", since, true
		})

		out := getSyncStatus(t, s)
		if out.Enabled {
			t.Fatal("no sync.db was wired; Enabled must stay false")
		}
		if out.AuthError == "" {
			t.Fatal("the rejection must be reported independently of the database")
		}
	})

	t.Run("nil seam is tolerated", func(t *testing.T) {
		// Sync not configured: the field simply does not appear, as before.
		out := getSyncStatus(t, New(false))
		if out.AuthError != "" {
			t.Fatalf("unwired seam produced an auth error: %q", out.AuthError)
		}
	})
}

func getSyncStatus(t *testing.T, s *Server) syncStatusResponse {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleSyncStatus(w, httptest.NewRequest(http.MethodGet, "/api/sync/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var out syncStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
