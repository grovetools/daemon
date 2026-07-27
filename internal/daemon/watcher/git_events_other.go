//go:build !darwin

package watcher

import (
	"context"

	"github.com/grovetools/daemon/internal/daemon/store"
)

// Other platforms retain the unified fsnotify git-internal watches plus the
// hourly collector reconciler. The recursive owner is Darwin-specific because
// FSEvents is the only backend here that provides broad recursion without one
// watch descriptor per directory.
func runGlobalGitEvents(ctx context.Context, _ *store.Store, _ *GitHandler) error {
	<-ctx.Done()
	return nil
}
