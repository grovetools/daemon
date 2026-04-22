package env

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProxyManager_DirectorStripsHostPort(t *testing.T) {
	pm := NewProxyManager()
	pm.Register("tier1-c", "api", 61320)

	tests := []struct {
		name     string
		reqHost  string
		wantHost string
	}{
		{"with :8443 suffix", "api.tier1-c.grove.local:8443", "127.0.0.1:61320"},
		{"no port (OS-redirect path)", "api.tier1-c.grove.local", "127.0.0.1:61320"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://example.invalid/", nil)
			req.Host = tt.reqHost
			req.URL = &url.URL{Scheme: "http", Host: tt.reqHost, Path: "/"}

			pm.directRequest(req)

			if req.URL.Host != tt.wantHost {
				t.Errorf("expected URL.Host %q, got %q", tt.wantHost, req.URL.Host)
			}
			if req.URL.Scheme != "http" {
				t.Errorf("expected scheme http, got %q", req.URL.Scheme)
			}
		})
	}
}

func TestProxyManager_DirectorLookupMiss(t *testing.T) {
	pm := NewProxyManager()

	req := httptest.NewRequest("GET", "http://unknown.foo.grove.local/", nil)
	req.Host = "unknown.foo.grove.local:8443"
	origURL := *req.URL

	pm.directRequest(req)

	if req.URL.Host != origURL.Host {
		t.Errorf("expected URL.Host unchanged on miss, got %q", req.URL.Host)
	}
}


func TestProxyManager_RegisterAndLookup(t *testing.T) {
	pm := NewProxyManager()

	pm.Register("demo", "web", 34012)

	target, ok := pm.Lookup("web.demo.grove.local")
	if !ok {
		t.Fatal("expected route to be registered")
	}
	if target != "127.0.0.1:34012" {
		t.Errorf("expected target 127.0.0.1:34012, got %s", target)
	}
}

func TestProxyManager_LookupMiss(t *testing.T) {
	pm := NewProxyManager()

	_, ok := pm.Lookup("unknown.demo.grove.local")
	if ok {
		t.Error("expected lookup miss for unregistered route")
	}
}

func TestProxyManager_Unregister(t *testing.T) {
	pm := NewProxyManager()

	pm.Register("demo", "web", 34012)
	pm.Register("demo", "api", 34013)
	pm.Register("other", "web", 34014)

	pm.Unregister("demo")

	if _, ok := pm.Lookup("web.demo.grove.local"); ok {
		t.Error("expected web.demo route to be unregistered")
	}
	if _, ok := pm.Lookup("api.demo.grove.local"); ok {
		t.Error("expected api.demo route to be unregistered")
	}

	// Other worktree should be unaffected
	if _, ok := pm.Lookup("web.other.grove.local"); !ok {
		t.Error("expected web.other route to still exist")
	}
}

func TestProxyManager_MultipleServices(t *testing.T) {
	pm := NewProxyManager()

	pm.Register("feature-x", "web", 10001)
	pm.Register("feature-x", "api", 10002)
	pm.Register("feature-x", "worker", 10003)

	tests := []struct {
		host   string
		target string
	}{
		{"web.feature-x.grove.local", "127.0.0.1:10001"},
		{"api.feature-x.grove.local", "127.0.0.1:10002"},
		{"worker.feature-x.grove.local", "127.0.0.1:10003"},
	}

	for _, tt := range tests {
		target, ok := pm.Lookup(tt.host)
		if !ok {
			t.Errorf("expected route for %s", tt.host)
			continue
		}
		if target != tt.target {
			t.Errorf("host %s: expected %s, got %s", tt.host, tt.target, target)
		}
	}
}
