package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func newTestServer() *Server {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	return New(logger.WithField("test", true))
}

func TestHandleEnvUp_POST(t *testing.T) {
	s := newTestServer()

	body := strings.NewReader(`{"provider":"native","plan_dir":"/tmp/test"}`)
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
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", resp.Header.Get("Content-Type"))
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

func TestHandleEnvDown_POST(t *testing.T) {
	s := newTestServer()

	body := strings.NewReader(`{"provider":"docker","plan_dir":"/tmp/test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/env/down", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleEnvDown(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["status"] != "stopped" {
		t.Errorf("expected status 'stopped', got %v", result["status"])
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", resp.Header.Get("Content-Type"))
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
