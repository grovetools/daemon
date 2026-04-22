package env

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBootstrap_SkipsWhenPathPresent(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(tmp, "sentinel")
	cfg := map[string]interface{}{
		"bootstrap": map[string]interface{}{
			"missing_path": "node_modules",
			"command":      "touch " + sentinel,
		},
	}
	m := NewManager()
	if err := m.runServiceBootstrap(context.Background(), "web", cfg, tmp, os.Environ(), ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("bootstrap ran but precondition was satisfied; should have been skipped")
	}
}

func TestBootstrap_RunsWhenPathMissing(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "sentinel")
	cfg := map[string]interface{}{
		"bootstrap": map[string]interface{}{
			"missing_path": "node_modules",
			"command":      "touch " + sentinel,
		},
	}
	m := NewManager()
	if err := m.runServiceBootstrap(context.Background(), "web", cfg, tmp, os.Environ(), ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel missing; bootstrap did not run: %v", err)
	}
}

func TestBootstrap_FailurePropagates(t *testing.T) {
	tmp := t.TempDir()
	cfg := map[string]interface{}{
		"bootstrap": map[string]interface{}{
			"missing_path": "node_modules",
			"command":      "exit 1",
		},
	}
	m := NewManager()
	err := m.runServiceBootstrap(context.Background(), "web", cfg, tmp, os.Environ(), "")
	if err == nil {
		t.Fatal("expected error from failing bootstrap")
	}
}

func TestBootstrap_Timeout(t *testing.T) {
	tmp := t.TempDir()
	cfg := map[string]interface{}{
		"bootstrap": map[string]interface{}{
			"missing_path":    "node_modules",
			"command":         "sleep 10",
			"timeout_seconds": int64(1),
		},
	}
	m := NewManager()
	start := time.Now()
	err := m.runServiceBootstrap(context.Background(), "web", cfg, tmp, os.Environ(), "")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("bootstrap took too long to timeout: %v", elapsed)
	}
}

func TestBootstrap_CtxCancelKillsProcess(t *testing.T) {
	tmp := t.TempDir()
	cfg := map[string]interface{}{
		"bootstrap": map[string]interface{}{
			"missing_path":    "node_modules",
			"command":         "sleep 30",
			"timeout_seconds": int64(60),
		},
	}
	m := NewManager()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- m.runServiceBootstrap(ctx, "web", cfg, tmp, os.Environ(), "")
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bootstrap did not exit within 2s of ctx cancel")
	}
}

func TestBootstrap_NoopWhenBlockMissing(t *testing.T) {
	m := NewManager()
	if err := m.runServiceBootstrap(context.Background(), "web", map[string]interface{}{}, t.TempDir(), nil, ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}
