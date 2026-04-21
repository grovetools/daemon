package env

import (
	"bytes"
	"context"
	"testing"
	"text/template"
	"time"
)

func TestTunnelContext_Template(t *testing.T) {
	tmpl, err := template.New("test").Parse("gcloud iap --local-host-port=localhost:{{.AllocatedPort}}")
	if err != nil {
		t.Fatalf("template parse error: %v", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, TunnelContext{AllocatedPort: 12345}); err != nil {
		t.Fatalf("template execute error: %v", err)
	}

	expected := "gcloud iap --local-host-port=localhost:12345"
	if buf.String() != expected {
		t.Errorf("expected %q, got %q", expected, buf.String())
	}
}

func TestTunnelManager_StartAndStop(t *testing.T) {
	tm := NewTunnelManager()

	ctx := context.Background()

	// Start a simple sleep command as a tunnel
	pgid, err := tm.Start(ctx, "demo", "db", "sleep 60", 5432, "", nil, "")
	if err != nil {
		t.Fatalf("failed to start tunnel: %v", err)
	}
	if pgid <= 0 {
		t.Errorf("expected non-zero pgid from supervisor.Spawn, got %d", pgid)
	}

	// Verify tunnel is tracked
	tm.mu.Lock()
	_, exists := tm.tunnels["demo/db"]
	tm.mu.Unlock()
	if !exists {
		t.Fatal("expected tunnel to be tracked")
	}

	// Stop all tunnels for the worktree
	tm.StopAll("demo")

	// Give process cleanup a moment
	time.Sleep(50 * time.Millisecond)

	tm.mu.Lock()
	_, exists = tm.tunnels["demo/db"]
	tm.mu.Unlock()
	if exists {
		t.Error("expected tunnel to be removed after StopAll")
	}
}

func TestTunnelManager_StopAllSelectiveByWorktree(t *testing.T) {
	tm := NewTunnelManager()

	ctx := context.Background()

	tm.Start(ctx, "wt-a", "db", "sleep 60", 5432, "", nil, "")
	tm.Start(ctx, "wt-b", "db", "sleep 60", 5433, "", nil, "")

	tm.StopAll("wt-a")

	tm.mu.Lock()
	_, aExists := tm.tunnels["wt-a/db"]
	_, bExists := tm.tunnels["wt-b/db"]
	tm.mu.Unlock()

	if aExists {
		t.Error("expected wt-a tunnel to be stopped")
	}
	if !bExists {
		t.Error("expected wt-b tunnel to still be running")
	}

	// Cleanup
	tm.StopAll("wt-b")
}

func TestTunnelManager_InvalidTemplate(t *testing.T) {
	tm := NewTunnelManager()

	ctx := context.Background()
	_, err := tm.Start(ctx, "demo", "bad", "{{.Invalid", 5432, "", nil, "")
	if err == nil {
		t.Error("expected error for invalid template")
	}
}
