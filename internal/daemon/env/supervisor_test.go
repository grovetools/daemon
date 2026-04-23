package env

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

// TestReapPGIDs_KillsGrandchild reproduces the hybrid-api orphan leak: a
// service spawned as `sh -c 'sleep 60 & wait'` leaves a grandchild sleep
// behind once the shell is SIGTERMed by cancel()/exec.CommandContext, because
// the signal doesn't cross the process boundary. reapPGIDs must signal the
// whole group so the grandchild dies with it.
//
// Detection uses `pgrep -g <pgid>` which counts processes still in the group.
// After SIGTERM to -pgid the group must be empty; a plain SIGTERM to the
// shell leader would leave the orphaned sleep behind.
func TestReapPGIDs_KillsGrandchild(t *testing.T) {
	s := NewPGIDSupervisor()
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	pgid, err := s.Spawn(t.Context(), "leaker", cmd)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Give the shell time to fork the background sleep.
	time.Sleep(300 * time.Millisecond)

	countInGroup := func(pgid int) int {
		out, _ := exec.Command("pgrep", "-g", fmt.Sprintf("%d", pgid)).Output()
		s := strings.TrimSpace(string(out))
		if s == "" {
			return 0
		}
		return len(strings.Split(s, "\n"))
	}

	// Sanity: group should currently have at least 2 members (sh + sleep).
	// If pgrep can't see them we can't meaningfully assert post-reap.
	if n := countInGroup(pgid); n < 2 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skipf("expected >=2 processes in group %d before reap, pgrep saw %d", pgid, n)
	}

	// Mirror production: a background goroutine reaps the shell's exit
	// status so kill(-pgid, 0) → ESRCH promptly after SIGTERM succeeds,
	// matching what local_services.go does for real service commands.
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()

	reapPGIDs(t.Context(), nil, s, map[string]int{"leaker": pgid})
	<-waitDone

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countInGroup(pgid) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("grandchild survived reapPGIDs: %d processes still in pgid %d", countInGroup(pgid), pgid)
}
