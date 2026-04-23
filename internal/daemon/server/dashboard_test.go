package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardStaticAssets validates that the embedded SPA is wired onto
// the mux and the shell HTML references the JS + CSS bundle.
func TestDashboardStaticAssets(t *testing.T) {
	s := New(false)
	mux := http.NewServeMux()
	s.registerDashboardRoutes(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("index", func(t *testing.T) {
		res, err := http.Get(srv.URL + "/dashboard")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("status = %d", res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), "grove env dashboard") {
			t.Errorf("index.html missing title: %q", string(body)[:200])
		}
		if !strings.Contains(string(body), "dashboard.js") {
			t.Error("index.html missing JS reference")
		}
	})

	t.Run("assets", func(t *testing.T) {
		res, err := http.Get(srv.URL + "/dashboard/assets/dashboard.js")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("status = %d", res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), "/api/dashboard/state") {
			t.Errorf("dashboard.js missing poll URL")
		}
	})
}

// TestDashboardStateEndpointJSON confirms /api/dashboard/state returns a
// JSON document with the expected top-level shape even when the daemon
// has no env state yet (fresh boot).
func TestDashboardStateEndpointJSON(t *testing.T) {
	s := New(false)
	mux := http.NewServeMux()
	s.registerDashboardRoutes(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/dashboard/state?probe=0")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, body = %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "generated_at") {
		t.Errorf("payload missing generated_at: %s", body)
	}
}
