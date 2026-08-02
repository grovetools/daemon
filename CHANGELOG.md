# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **A first-ever sync subscription no longer needs a daemon restart.** The
  global daemon decided at boot whether to build a `SyncHandler` at all, so a
  `grove join` on a machine that had never synced wrote a valid `sync.toml`
  that nothing acted on until `groved` was restarted. The handler is now
  constructed unconditionally and stays dormant — no watches, no `sync.db`, no
  transport — until a config reload brings subscriptions, at which point it
  opens `sync.db` in place and wires it into the HTTP server.
- **Push-only notebook workspaces sync outbound again.** `ComputeWatchPaths`
  resolved config-derived (synthetic) workspace roots only for `pull = true`
  subscriptions. A bare notebook workspace that code discovery never yields got
  no watches, and since pipeline roots derive from the watch set, no push
  pipeline either — it was silently dark in both directions. Pull remains
  strictly opt-in; only capture and push coverage changed.

### Added
- Initial implementation of daemon
- Basic command structure
- E2E test framework