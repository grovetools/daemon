//go:build !darwin && !linux

package pairwatch

import "context"

func watch(ctx context.Context, pid int, onDeath func()) {
	watchPoll(ctx, pid, onDeath)
}
