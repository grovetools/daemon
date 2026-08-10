package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestConfigDegradedSocketServesStatusAndRejectsSubmissions(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	sock := shortSocketPath(t)
	s := New(false)
	s.SetConfigDegradation("/tmp/config/roots.toml: roots.bad: expected path")
	if err := s.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	client := unixHTTPClient(sock)
	assertConfigError := func(path string, wantStatus int, method, body string, key string) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, "http://unix"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s returned non-JSON degradation: %q: %v", path, raw, err)
		}
		nested, ok := got[key].(map[string]any)
		if !ok || nested["code"] != "config_load_failed" || nested["recovery"] != "fix the configuration and restart groved" {
			t.Fatalf("%s missing structured restart-only degradation: %#v", path, got)
		}
		if !strings.Contains(nested["message"].(string), "roots.toml") {
			t.Fatalf("%s lost path-qualified config error: %#v", path, got)
		}
		return got
	}

	health := assertConfigError("/health", http.StatusServiceUnavailable, http.MethodGet, "", "config_error")
	if health["degraded"] != true || health["status"] != "degraded" {
		t.Fatalf("health is not truthful: %#v", health)
	}
	cfg := assertConfigError("/api/config", http.StatusOK, http.MethodGet, "", "config_error")
	if cfg["degraded"] != true {
		t.Fatalf("config status is not degraded: %#v", cfg)
	}
	syncStatus := assertConfigError("/api/sync/status", http.StatusOK, http.MethodGet, "", "config_error")
	if syncStatus["enabled"] != false || syncStatus["degraded"] != true {
		t.Fatalf("sync status implies a pipeline is active: %#v", syncStatus)
	}

	// These assertions run with no runner or scheduler wired. The config gate
	// must win and return the same structured reason before either submission
	// can reach (or implicitly create) a pipeline.
	assertConfigError("/api/jobs", http.StatusServiceUnavailable, http.MethodPost, `{}`, "error")
	assertConfigError("/api/build/submit", http.StatusServiceUnavailable, http.MethodPost, `{}`, "error")
}
