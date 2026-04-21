package env

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestPGIDSupervisor_SpawnAndStop spawns a long-running sleep process via the
// supervisor, asserts the returned PGID is sensible (== child PID since it's
// the group leader), then calls Stop and verifies the process is gone.
func TestPGIDSupervisor_SpawnAndStop(t *testing.T) {
	s := NewPGIDSupervisor()
	cmd := exec.Command("sleep", "30")
	pgid, err := s.Spawn(t.Context(), "sleeper", cmd)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if pgid <= 0 {
		t.Fatalf("expected positive pgid, got %d", pgid)
	}
	if cmd.Process == nil {
		t.Fatalf("Spawn returned nil process")
	}
	// On a freshly-Setpgid'd child the group leader is the child itself.
	if pgid != cmd.Process.Pid {
		t.Errorf("pgid (%d) != pid (%d) — expected leader to be the child", pgid, cmd.Process.Pid)
	}

	if err := s.Stop(pgid); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Wait briefly for SIGTERM to take effect.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// good — process exited
	case <-time.After(2 * time.Second):
		t.Fatalf("process did not exit within 2s of Stop")
	}

	// After exit, signaling the (now-defunct) PGID should yield ESRCH which
	// the supervisor swallows; Stop must remain idempotent.
	if err := s.Stop(pgid); err != nil {
		t.Errorf("second Stop on dead pgid returned error: %v", err)
	}
}

// TestPGIDSupervisor_StopZeroIsNoop guards against accidentally signaling
// pgid 0 (the caller's own group), which would terminate the test runner.
func TestPGIDSupervisor_StopZeroIsNoop(t *testing.T) {
	s := NewPGIDSupervisor()
	if err := s.Stop(0); err != nil {
		t.Errorf("Stop(0) returned error: %v", err)
	}
	if err := s.Stop(-1); err != nil {
		t.Errorf("Stop(-1) returned error: %v", err)
	}
}

// TestPGIDSupervisor_PreservesSysProcAttr verifies that an existing
// SysProcAttr survives Spawn — the supervisor only sets the Setpgid bit.
func TestPGIDSupervisor_PreservesSysProcAttr(t *testing.T) {
	s := NewPGIDSupervisor()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Foreground: false}
	if _, err := s.Spawn(context.Background(), "sleeper", cmd); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer cmd.Process.Kill()
	defer cmd.Wait()

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Errorf("Setpgid not set on cmd.SysProcAttr: %+v", cmd.SysProcAttr)
	}
}
