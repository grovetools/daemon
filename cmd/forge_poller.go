package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/grovetools/core/config"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/forge"
	"github.com/grovetools/core/pkg/forge/forgejo"
	"github.com/grovetools/core/pkg/forge/github"
	"github.com/grovetools/daemon/internal/daemon/forgepoll"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// forgeStore is the slice of the daemon store the poller needs. *store.Store
// satisfies it; declaring it here (rather than taking *store.Store) is what
// lets the wiring above be exercised end-to-end against a real forge — an
// httptest Forgejo, a real git remote, a real token_command — without standing
// up a Store that reads and writes the machine's state directory.
type forgeStore interface {
	Get() store.State
	ApplyUpdate(store.Update)
}

// startForgePoller starts the read-only forge poller if — and only if — both of
// its independent gates are open.
//
// DARK BY DEFAULT, twice over:
//
//  1. `[forge.poll] enabled = true` must be set explicitly. Absent config,
//     absent block, or enabled = false means no poller and no goroutine.
//  2. The selected provider must be constructible. For GitHub that means `gh`
//     on PATH; missing is silence plus one log line, never a prompt and never a
//     boot error. For a self-hosted forge it means a parseable `[forge] url`.
//     An installed-but-unauthenticated transport clears this gate and then
//     fails at call time, which the poller records as unknown/stale (D4) rather
//     than as "no pull requests".
//
// It reports nothing upward as an error: a poller that cannot start is a
// feature that stays off, not a daemon that fails to boot. It DOES return the
// running poller (nil when off) so the caller can wire its read seam onto the
// HTTP server — a nil return is what makes GET /api/forge/state answer
// "enabled: false" instead of an empty, indistinguishable-from-no-PRs list.
func startForgePoller(ctx context.Context, st forgeStore, cfg *config.Config, ulog *grovelogging.UnifiedLogger) *forgepoll.Poller {
	forgeCfg, err := cfg.Forge()
	if err != nil {
		ulog.Warn("Failed to parse [forge] config, forge poller disabled").Err(err).Log(ctx)
		return nil
	}
	if !forgeCfg.PollEnabled() {
		return nil
	}
	// Validate only once the poller has been asked for. A [forge] block nobody
	// consumes must never start failing daemon boots (the config-load path
	// still does not call this), but an operator who typed `enabled = true` has
	// asked for the feature and deserves to hear why it did not come up.
	if verr := forgeCfg.Validate(); verr != nil {
		ulog.Warn("Forge poller enabled but [forge] config is invalid; poller stays off").Err(verr).Log(ctx)
		return nil
	}

	opts, reason := forgePollerProviderOptions(forgeCfg)
	if opts.Provider == nil {
		ulog.Info("Forge poller enabled but no provider could be constructed; poller stays off").
			Field("reason", reason).
			Log(ctx)
		return nil
	}

	opts.Store = st
	opts.Interval = forgeCfg.Poll.EffectiveInterval()
	opts.StaleAfter = forgeCfg.Poll.EffectiveStaleAfter()

	poller, err := forgepoll.New(opts)
	if err != nil {
		ulog.Warn("Failed to construct forge poller, forge state disabled").Err(err).Log(ctx)
		return nil
	}
	// Everything logged here is derived from config the operator wrote, and
	// none of it is the token: the command string and its output stay inside
	// forgeTokenResolver.
	ulog.Info("Forge poller configured").
		Field("provider", opts.Provider.Name()).
		Field("hosts", strings.Join(opts.Hosts, ",")).
		Field("remote", opts.RemoteName).
		Field("token_command", forgeTokenCommandState(forgeCfg)).
		Log(ctx)

	go poller.Start(ctx)
	return poller
}

// forgePollerProviderOptions builds the provider half of the poller options
// from `[forge]`, plus the host allowlist and git remote that go with it.
//
// This is the function the pipeline-live trial found missing: `[forge]` url /
// remote_name / token_command used to be parse-only, the poller hard-coded
// github.New(), and Hosts/RemoteName were never set — so a self-hosted forge
// was unreachable by configuration and only pollable through a fake `gh` on
// PATH. The three axes now travel together, because they are one decision:
//
//   - forgejo → REST against `[forge] url`, identity read from the `[forge]
//     remote_name` remote (default "forge"), host allowlist = that URL's host.
//   - github  → `gh`, identity read from `origin`, host allowlist =
//     forgepoll.DefaultHosts. Byte-for-byte the pre-existing behavior.
//
// A nil Provider in the result means "do not start", with reason naming why.
func forgePollerProviderOptions(forgeCfg *config.ForgeConfig) (forgepoll.Options, string) {
	switch forgeCfg.EffectiveProvider() {
	case config.ForgeProviderForgejo:
		host := forgeCfg.Host()
		if host == "" {
			return forgepoll.Options{}, "provider forgejo needs a parseable [forge] url"
		}
		var providerOpts []forgejo.Option
		if cmd := strings.TrimSpace(forgeCfg.TokenCommand); cmd != "" {
			// Resolution is LAZY: the command runs on the first request a
			// sweep makes, not at boot. A secrets manager that prompts (touch
			// ID, a passphrase) must not be triggered by starting the daemon,
			// and a failing command must degrade one sweep rather than
			// suppress the poller entirely.
			providerOpts = append(providerOpts, forgejo.WithToken(newForgeTokenResolver(cmd).Token))
		}
		provider, err := forgejo.New(forgeCfg.URL, providerOpts...)
		if err != nil {
			return forgepoll.Options{}, fmt.Sprintf("provider forgejo: %v", err)
		}
		return forgepoll.Options{
			Provider:   provider,
			Hosts:      []string{host},
			RemoteName: forgeCfg.EffectiveRemoteName(),
		}, ""

	default:
		if !github.Available() {
			return forgepoll.Options{}, "the gh CLI is not on PATH"
		}
		// Hosts and RemoteName stay unset: forgepoll defaults them to
		// github.com and "origin", which is the enrollment model the GitHub
		// path has always used. `[forge] remote_name` is the SELF-HOSTED
		// forge's remote and must not be applied here.
		return forgepoll.Options{Provider: github.New()}, ""
	}
}

// forgeTokenCommandState renders whether a token command is configured, for the
// boot log line. It renders presence, never the command itself.
func forgeTokenCommandState(forgeCfg *config.ForgeConfig) string {
	if forgeCfg != nil && strings.TrimSpace(forgeCfg.TokenCommand) != "" {
		return "configured"
	}
	return "none"
}

// Compile-time proof that the two providers this file constructs both satisfy
// the read-only seam. It is here rather than in the provider packages because
// this is the file that chooses between them.
var (
	_ forge.Provider = (*forgejo.Provider)(nil)
	_ forge.Provider = (*github.Provider)(nil)
)
