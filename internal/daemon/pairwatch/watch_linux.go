//go:build linux

package pairwatch

import (
	"context"

	"github.com/grovetools/core/logging"
	"golang.org/x/sys/unix"
)

func watch(ctx context.Context, pid int, onDeath func()) {
	ulog := logging.NewUnifiedLogger("groved.pairwatch")
	ulog.Info("pairwatch.Watch started").Field("pid", pid).Field("backend", "pidfd").Log(ctx)

	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		ulog.Warn("pidfd_open unavailable, falling back to poll").Err(err).Field("pid", pid).Log(ctx)
		watchPoll(ctx, pid, onDeath)
		return
	}
	defer unix.Close(fd)

	done := make(chan struct{})
	go func() {
		defer close(done)
		pfds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(pfds, -1)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				ulog.Warn("poll failed").Err(err).Field("pid", pid).Log(ctx)
				return
			}
			if n > 0 {
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return
	case <-done:
		ulog.Info("parent process died, triggering shutdown").Field("pid", pid).Log(ctx)
		onDeath()
	}
}
