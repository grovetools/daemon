package env

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
)

// NativeSupervisor abstracts how the daemon spawns and stops bare local
// processes. The default backend (PGIDSupervisor) sets each process up in
// its own POSIX process group and uses signal(-pgid) to reap the entire
// tree on teardown — which survives daemon restarts on macOS because PGID
// tables are kernel-side and persist across reparent-to-init.
//
// The interface seam exists so a future launchd/systemd backend can replace
// the implementation without touching local_services.go or tunnels.go.
type NativeSupervisor interface {
	// Spawn starts cmd in a new process group and returns the PGID. The
	// caller retains the *exec.Cmd handle; this contract is only about
	// putting the child in a separately-signalable group and reporting
	// the resulting PGID for state-file persistence.
	Spawn(ctx context.Context, name string, cmd *exec.Cmd) (int, error)
	// Stop signals the process group identified by pgid. A pgid of 0 or
	// less is a no-op (defensive against zero-valued state-file entries).
	Stop(pgid int) error
}

// PGIDSupervisor is the default NativeSupervisor: it sets Setpgid: true on
// each spawned process and tears them down with kill(-pgid, SIGTERM).
type PGIDSupervisor struct{}

// NewPGIDSupervisor constructs a PGIDSupervisor. Stateless — kept as a
// constructor so future backends can match the shape.
func NewPGIDSupervisor() *PGIDSupervisor { return &PGIDSupervisor{} }

// Spawn arranges for cmd to be started in its own process group and starts
// the process. After a successful Spawn the caller can read cmd.Process for
// the PID; the returned int is the PGID (== PID of the group leader on
// freshly-Setpgid'd children).
func (s *PGIDSupervisor) Spawn(_ context.Context, _ string, cmd *exec.Cmd) (int, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if cmd.Process == nil {
		return 0, fmt.Errorf("supervisor: process is nil after start")
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// Fall back to using the PID itself — Setpgid: true makes the
		// child its own group leader, so PID == PGID at this point.
		return cmd.Process.Pid, nil
	}
	return pgid, nil
}

// Stop sends SIGTERM to the entire process group. macOS preserves the PGID
// across reparent-to-init when the original parent (the daemon) exits, so
// a fresh daemon with the persisted PGID can still tear down the tree.
func (s *PGIDSupervisor) Stop(pgid int) error {
	if pgid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		// ESRCH (no such process group) is expected when a process has
		// already exited cleanly. Treat that as a successful stop so the
		// caller can keep iterating without spurious errors.
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	return nil
}
