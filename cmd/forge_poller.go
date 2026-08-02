package cmd

import (
	"context"

	"github.com/grovetools/core/config"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/forge/github"
	"github.com/grovetools/daemon/internal/daemon/forgepoll"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// startForgePoller starts the read-only forge poller if — and only if — both of
// its independent gates are open.
//
// DARK BY DEFAULT, twice over:
//
//  1. `[forge.poll] enabled = true` must be set explicitly. Absent config,
//     absent block, or enabled = false means no poller and no goroutine.
//  2. The provider's transport must be present. `gh` missing from PATH is
//     silence plus one log line — never a prompt, never a boot error. An
//     installed-but-unauthenticated `gh` clears this gate and then fails at
//     call time, which the poller records as unknown/stale (D4) rather than as
//     "no pull requests".
//
// It returns nothing and reports nothing upward: a poller that cannot start is
// a feature that stays off, not a daemon that fails to boot.
func startForgePoller(ctx context.Context, st *store.Store, cfg *config.Config, ulog *grovelogging.UnifiedLogger) {
	forgeCfg, err := cfg.Forge()
	if err != nil {
		ulog.Warn("Failed to parse [forge] config, forge poller disabled").Err(err).Log(ctx)
		return
	}
	if !forgeCfg.PollEnabled() {
		return
	}
	if !github.Available() {
		ulog.Info("Forge poller enabled but the gh CLI is not on PATH; poller stays off").Log(ctx)
		return
	}

	poller, err := forgepoll.New(forgepoll.Options{
		Store:      st,
		Provider:   github.New(),
		Interval:   forgeCfg.Poll.EffectiveInterval(),
		StaleAfter: forgeCfg.Poll.EffectiveStaleAfter(),
	})
	if err != nil {
		ulog.Warn("Failed to construct forge poller, forge state disabled").Err(err).Log(ctx)
		return
	}
	go poller.Start(ctx)
}
