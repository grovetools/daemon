// Package pairwatch watches for the death of a "paired" parent process and
// invokes a callback when it exits.
//
// Typical usage from the daemon:
//
//	pairwatch.Watch(ctx, parentPID, func() {
//	    stop <- syscall.SIGTERM
//	})
//
// Watch is non-blocking: it spawns its own goroutine and returns immediately.
// The goroutine honors ctx cancellation. If the target PID is invalid or
// already dead, the onDeath callback fires promptly.
//
// The implementation is selected per platform:
//   - darwin: kqueue + EVFILT_PROC + NOTE_EXIT (kernel-level, zero latency)
//   - linux:  pidfd_open + poll (kernel-level, zero latency)
//   - other:  100ms polling loop via syscall.Kill(pid, 0) (fallback)
package pairwatch

import (
	"context"
	"syscall"
	"time"

	"github.com/grovetools/core/logging"
)

// Watch starts a goroutine that calls onDeath exactly once when the process
// identified by pid exits or when the platform-specific watcher can no longer
// observe it. If ctx is canceled before the parent dies, the watcher returns
// without invoking onDeath.
//
// pid must be > 0. Callers are responsible for ensuring onDeath is safe to
// invoke from a background goroutine.
func Watch(ctx context.Context, pid int, onDeath func()) {
	if pid <= 0 || onDeath == nil {
		return
	}
	go watch(ctx, pid, onDeath)
}

// watchPoll is the cross-platform fallback. It ticks every 100ms and issues
// a null signal (syscall.Kill(pid, 0)); ESRCH indicates the process is gone.
// It is invoked directly on generic platforms and used as a fallback when the
// darwin/linux kernel APIs fail to initialize.
func watchPoll(ctx context.Context, pid int, onDeath func()) {
	ulog := logging.NewUnifiedLogger("groved.pairwatch")
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := syscall.Kill(pid, 0)
			if err == syscall.ESRCH {
				ulog.Info("parent process died, triggering shutdown").Field("pid", pid).Log(ctx)
				onDeath()
				return
			}
		}
	}
}
