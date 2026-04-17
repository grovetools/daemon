package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	daemonenv "github.com/grovetools/daemon/internal/daemon/env"
)

func newTestServer() *Server {
	s := New()
	s.SetEnvManager(daemonenv.NewManager())
	return s
}

func TestHandleEnvUp_POST(t *testing.T) {
	s := newTestServer()

	// Native provider requires a workspace — without one it returns a JSON error
	body := strings.NewReader(`{"provider":"native","plan_dir":"/tmp/test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/env/up", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleEnvUp(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500 (no workspace), got %d", resp.StatusCode)
	}

	// Error response should still be valid JSON
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("error response should be valid JSON: %v", err)
	}
	if result["status"] != "failed" {
		t.Errorf("expected status 'failed', got %v", result["status"])
	}
	if result["error"] == nil || result["error"] == "" {
		t.Error("expected non-empty error field in response")
	}
}

func TestHandleEnvUp_WithWorkspace(t *testing.T) {
	s := newTestServer()

	// Valid request with workspace and no services — should succeed with "running" status
	body := strings.NewReader(`{
		"provider":"native",
		"plan_dir":"/tmp/test",
		"workspace":{"name":"test-wt","path":"/tmp/test-wt"}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/env/up", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleEnvUp(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["status"] != "running" {
		t.Errorf("expected status 'running', got %v", result["status"])
	}
}

func TestHandleEnvUp_MethodNotAllowed(t *testing.T) {
	s := newTestServer()

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/env/up", nil)
			w := httptest.NewRecorder()

			s.handleEnvUp(w, req)

			if w.Result().StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 for %s, got %d", method, w.Result().StatusCode)
			}
		})
	}
}

func TestHandleEnvUp_InvalidBody(t *testing.T) {
	s := newTestServer()

	body := strings.NewReader(`{invalid json}`)
	req := httptest.NewRequest(http.MethodPost, "/api/env/up", body)
	w := httptest.NewRecorder()

	s.handleEnvUp(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Result().StatusCode)
	}
}

func TestHandleEnvUp_UnsupportedProvider(t *testing.T) {
	s := newTestServer()

	body := strings.NewReader(`{"provider":"kubernetes"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/env/up", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleEnvUp(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500 for unsupported provider, got %d", resp.StatusCode)
	}

	// Should still be valid JSON
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("error response should be valid JSON: %v", err)
	}
	if result["status"] != "failed" {
		t.Errorf("expected status 'failed', got %v", result["status"])
	}
}

func TestHandleEnvUp_NoManager(t *testing.T) {
	s := New()
	// Intentionally do NOT set env manager

	body := strings.NewReader(`{"provider":"native"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/env/up", body)
	w := httptest.NewRecorder()

	s.handleEnvUp(w, req)

	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when env manager not set, got %d", w.Result().StatusCode)
	}
}

func TestHandleEnvDown_POST(t *testing.T) {
	s := newTestServer()

	// Docker provider requires a workspace — without one it returns a JSON error
	body := strings.NewReader(`{"provider":"docker","plan_dir":"/tmp/test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/env/down", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleEnvDown(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500 (no workspace), got %d", resp.StatusCode)
	}

	// Error response should still be valid JSON
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("error response should be valid JSON: %v", err)
	}
	if result["status"] != "failed" {
		t.Errorf("expected status 'failed', got %v", result["status"])
	}
}

func TestHandleEnvDown_WithWorkspace(t *testing.T) {
	s := newTestServer()

	// Down on a non-running environment should return "stopped"
	body := strings.NewReader(`{
		"provider":"native",
		"plan_dir":"/tmp/test",
		"workspace":{"name":"test-wt","path":"/tmp/test-wt"}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/env/down", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleEnvDown(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["status"] != "stopped" {
		t.Errorf("expected status 'stopped', got %v", result["status"])
	}
}

func TestHandleEnvDown_MethodNotAllowed(t *testing.T) {
	s := newTestServer()

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/env/down", nil)
			w := httptest.NewRecorder()

			s.handleEnvDown(w, req)

			if w.Result().StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 for %s, got %d", method, w.Result().StatusCode)
			}
		})
	}
}
