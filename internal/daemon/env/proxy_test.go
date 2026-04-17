package env

import (
	"testing"
)

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
