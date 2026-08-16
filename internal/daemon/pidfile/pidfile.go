// Package pidfile provides PID file management for the grove daemon.
//
// Acquire is the gate that makes exactly one process THE daemon for a given
// pidfile path. `groved start` does not fork — the invoked process IS the
// daemon — so every survivor of Acquire runs a complete daemon workload:
// collectors, fsnotify watchers, a signal-cli subprocess, and a socket bind
// that unlinks and re-creates whatever is already at the socket path. Two
// survivors therefore means two daemons, one of which serves a dead socket
// inode and appears in no census but `ps`.
//
// The original implementation read the file, decided nobody live owned it, and
// then wrote — three syscalls with two windows in between. Two clients
// auto-starting groved in the same instant (the normal shape of a fleet
// restart) both read "absent" and both wrote, and the machine ran two full
// daemons for hours. Acquisition is now an exclusive flock held for the life of
// the process, so the kernel arbitrates and exactly one starter wins.
//
// The lock is advisory, which matters in exactly one direction: a groved
// predating this file holds no lock, so its live PID in the file is still
// treated as an occupant (see acquire's occupant wait). That same wait is what
// lets `groved upgrade` hand off — the draining predecessor exits within
// milliseconds of unlinking its socket and the successor takes the lock the
// moment it does — while two cold starts, where nobody is leaving, resolve to
// one winner and one process that exits nonzero having touched nothing.
package pidfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/grovetools/core/pkg/process"
)

const (
	// DefaultAcquireWait bounds how long Acquire waits for an occupant to
	// leave. It covers the graceful-upgrade handoff (a predecessor that has
	// already unlinked its socket and is tearing down) without making a losing
	// cold start linger: a loser waits this long, having done nothing, and then
	// exits nonzero. The auto-start factory already expects a spawned groved
	// that lost the race to die "seconds later" (core's waitForStartingDaemon).
	DefaultAcquireWait = 5 * time.Second

	// AcquireWaitEnv overrides DefaultAcquireWait as a Go duration. Fixtures use
	// it to make the losing side of a deliberate race exit promptly; "0" makes
	// acquisition strictly non-blocking.
	AcquireWaitEnv = "GROVE_PIDFILE_WAIT"

	// acquirePoll is the retry cadence while waiting for the lock or for an
	// occupant to exit. Short enough that an upgrade handoff is imperceptible.
	acquirePoll = 50 * time.Millisecond
)

// AlreadyRunningError reports that another process owns the pidfile. PID is the
// occupant when it could be read (0 when the file was unreadable or empty).
type AlreadyRunningError struct {
	Path string
	PID  int
}

func (e *AlreadyRunningError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("daemon already running with PID %d", e.PID)
	}
	return fmt.Sprintf("pid file %s is held by another process", e.Path)
}

// IsAlreadyRunning reports whether err says another process owns the pidfile,
// as opposed to an I/O failure. A caller that races a start deliberately (a
// fixture, a client auto-start) wants to distinguish "I lost" from "the state
// directory is broken".
func IsAlreadyRunning(err error) bool {
	var are *AlreadyRunningError
	return errors.As(err, &are)
}

// held maps a pidfile path to its open, flock'd stable lock artifact. The
// lock is deliberately on path+".lock", not on the pidfile inode: pidfiles are
// routinely unlinked during pruning and upgrade, and unlinking a locked inode
// would otherwise let a second process create and lock a different inode at
// the same pathname. The lock artifact is never unlinked; closing the handle
// (or process death) releases it. Go opens it O_CLOEXEC, so children do not
// inherit the election.
var (
	heldMu sync.Mutex
	held   = map[string]*os.File{}
)

// Acquire takes exclusive ownership of the pidfile and writes the current PID.
// It returns an *AlreadyRunningError if another process owns it.
func Acquire(path string) error {
	return AcquireWait(path, acquireWaitFromEnv())
}

// AcquireWait is Acquire with an explicit bound on how long to wait for an
// occupant to leave. Zero makes it strictly non-blocking.
func AcquireWait(path string, wait time.Duration) error {
	return acquireWait(path, wait, process.List)
}

func acquireWait(path string, wait time.Duration, listProcesses func() ([]process.Entry, error)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: daemon runtime directory
		return fmt.Errorf("failed to create pid directory: %w", err)
	}

	heldMu.Lock()
	_, alreadyMine := held[path]
	heldMu.Unlock()
	if alreadyMine {
		return fmt.Errorf("pid file %s is already held by this process", path)
	}

	deadline := time.Now().Add(wait)
	for {
		f, err := lockPidFile(path)
		if err != nil {
			return err
		}
		if f != nil {
			// The stable lock is ours. A groved predating the lock protocol can
			// still be named by the pidfile. Wait only for a process positively
			// identified as groved; a recycled PID naming an unrelated process is
			// stale and must not block startup forever.
			occupant, occupied := liveGrovedOccupant(path, listProcesses)
			if !occupied {
				if err := writeOwnPID(path); err != nil {
					_ = f.Close()
					return err
				}
				heldMu.Lock()
				held[path] = f
				heldMu.Unlock()
				return nil
			}
			_ = f.Close()
			if !time.Now().Before(deadline) {
				return &AlreadyRunningError{Path: path, PID: occupant}
			}
		} else if !time.Now().Before(deadline) {
			// Contended: somebody else holds the lock. Read their PID for the
			// error message only — the lock, not the content, is the verdict.
			pid, _ := Read(path)
			return &AlreadyRunningError{Path: path, PID: pid}
		}

		time.Sleep(acquirePoll)
	}
}

// lockPidFile takes the exclusive election lock on a stable sibling artifact,
// returning (nil, nil) when another process holds it. The artifact is never
// unlinked, so deleting or replacing the informational pidfile cannot create a
// second lock domain.
func lockPidFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644) //nolint:gosec // G302/G304: daemon lock path from config
	if err != nil {
		return nil, fmt.Errorf("failed to open pid file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to lock pid file: %w", err)
	}

	return f, nil
}

// liveGrovedOccupant recognizes only an old, non-locking groved process. A
// merely-live PID is insufficient because stale pidfiles survive crashes and
// PIDs are recycled.
func liveGrovedOccupant(path string, listProcesses func() ([]process.Entry, error)) (int, bool) {
	pid, err := Read(path)
	if err != nil || pid <= 0 || pid == os.Getpid() || !process.IsProcessAlive(pid) {
		return 0, false
	}
	procs, err := listProcesses()
	if err != nil {
		return 0, false
	}
	for _, p := range procs {
		if p.PID != pid {
			continue
		}
		return pid, isGrovedStartCommand(p.Args)
	}
	return 0, false
}

func isGrovedStartCommand(cmdline string) bool {
	fields := strings.Fields(cmdline)
	if len(fields) < 2 || filepath.Base(fields[0]) != "groved" {
		return false
	}
	for _, field := range fields[1:] {
		if field == "start" {
			return true
		}
	}
	return false
}

// writeOwnPID replaces the informational pidfile while the stable election
// lock is held.
func writeOwnPID(path string) error {
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil { //nolint:gosec // G304: daemon pid path from config
		return fmt.Errorf("failed to write pid file: %w", err)
	}
	return nil
}

// Release removes the PID file and drops the lock.
//
// The removal is ownership-checked. Two daemons legitimately overlap during a
// graceful upgrade, and an unconditional remove on the predecessor's teardown
// path would delete the SUCCESSOR's pidfile — leaving a running daemon that
// `groved status`, `groved stop` and every auto-start client believe is not
// there, and that the next start would happily duplicate.
func Release(path string) error {
	heldMu.Lock()
	f := held[path]
	delete(held, path)
	heldMu.Unlock()

	if f == nil {
		// Not acquired here (or already released): fall back to the content
		// check alone.
		return removeIfOurs(path)
	}
	// Remove our informational pidfile before dropping the stable lock. A
	// waiter cannot proceed until Close, so it cannot write a successor PID
	// that this cleanup then removes.
	defer func() { _ = f.Close() }()
	return removeIfOurs(path)
}

// removeIfOurs unlinks path only while it still names this process.
func removeIfOurs(path string) error {
	pid, err := Read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if pid != os.Getpid() {
		return nil
	}
	return os.Remove(path)
}

// Owns reports whether path still names this process, plus a human-readable
// reason when it does not. It is the cheap half of the daemon's post-bind
// self-check: a daemon whose pidfile has been taken over is invisible to every
// census and to `groved stop`, so it must not keep running as though it were
// the daemon of record.
func Owns(path string) (bool, string) {
	pid, err := Read(path)
	switch {
	case os.IsNotExist(err):
		return false, fmt.Sprintf("pid file %s no longer exists", path)
	case err != nil:
		return false, fmt.Sprintf("pid file %s unreadable: %v", path, err)
	case pid != os.Getpid():
		return false, fmt.Sprintf("pid file %s names pid %d, not us", path, pid)
	}

	return true, ""
}

// acquireWaitFromEnv reads AcquireWaitEnv, falling back to DefaultAcquireWait.
func acquireWaitFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(AcquireWaitEnv))
	if raw == "" {
		return DefaultAcquireWait
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return DefaultAcquireWait
	}
	return d
}

// Read returns the PID from the file, or 0 if not found/invalid.
func Read(path string) (int, error) {
	content, err := os.ReadFile(path) //nolint:gosec // G304: path from daemon config
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(content))
	return strconv.Atoi(pidStr)
}

// IsRunning checks if the daemon described by the pidfile is active.
//
// A pidfile that describes no PID at all — absent, empty, or corrupt — reports
// "not running" rather than an error. Acquire creates the file before it writes
// into it, so an empty one is the sub-millisecond residue of a starter that
// died mid-acquire, and every caller of this function means "is a daemon up?".
func IsRunning(path string) (bool, int, error) {
	pid, err := Read(path)
	if err != nil {
		var numErr *strconv.NumError
		if os.IsNotExist(err) || errors.As(err, &numErr) {
			return false, 0, nil
		}
		return false, 0, err
	}
	return process.IsProcessAlive(pid), pid, nil
}
