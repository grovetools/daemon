//go:build darwin

package pairwatch

import (
	"context"

	"github.com/grovetools/core/logging"
	"golang.org/x/sys/unix"
)

func watch(ctx context.Context, pid int, onDeath func()) {
	ulog := logging.NewUnifiedLogger("groved.pairwatch")
	ulog.Info("pairwatch.Watch started").Field("pid", pid).Field("backend", "kqueue").Log(ctx)

	kq, err := unix.Kqueue()
	if err != nil {
		ulog.Warn("kqueue unavailable, falling back to poll").Err(err).Field("pid", pid).Log(ctx)
		watchPoll(ctx, pid, onDeath)
		return
	}
	defer unix.Close(kq)

	change := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}

	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		ulog.Warn("kevent register failed, falling back to poll").Err(err).Field("pid", pid).Log(ctx)
		watchPoll(ctx, pid, onDeath)
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		events := make([]unix.Kevent_t, 1)
		for {
			n, err := unix.Kevent(kq, nil, events, nil)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				ulog.Warn("kevent wait failed").Err(err).Field("pid", pid).Log(ctx)
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
