package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grovetools/core/config"
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
		defer func() { _ = res.Body.Close() }()
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
		defer func() { _ = res.Body.Close() }()
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
	sandbox := t.TempDir()
	home := filepath.Join(sandbox, "home")
	groveHome := filepath.Join(sandbox, "grove-home")
	for name, path := range map[string]string{
		"HOME": home, "GROVE_HOME": groveHome,
		"XDG_CONFIG_HOME": filepath.Join(sandbox, "config"),
		"XDG_DATA_HOME":   filepath.Join(sandbox, "data"),
		"XDG_STATE_HOME":  filepath.Join(sandbox, "state"),
		"XDG_CACHE_HOME":  filepath.Join(sandbox, "cache"),
	} {
		t.Setenv(name, path)
	}
	configDir := filepath.Join(groveHome, "config", "grove")
	notebookRoot := filepath.Join(home, "notebooks", "fixture")
	codeRoot := filepath.Join(home, "code", "fixture")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "notebooks.toml"), []byte(
		"default = \"fixture\"\n[notebooks.fixture]\nroot = \""+notebookRoot+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "roots.toml"), []byte(
		"[roots.fixture]\npath = \""+codeRoot+"\"\nnotebook = \"fixture\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetLoadCache()
	t.Cleanup(config.ResetLoadCache)

	s := New(false)
	mux := http.NewServeMux()
	s.registerDashboardRoutes(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/dashboard/state?probe=0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
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
