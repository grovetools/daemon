package watcher

import (
	"context"
	"fmt"
	"time"

	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// The stale-token trap (contract §3 P2b, scope 1).
//
// A sync server destroyed and recreated mints fresh tokens, so every token
// minted against the old one is dead. The daemon could not see that: an
// unauthorized handshake and an unreachable server both arrived as an
// undifferentiated error, transportLoop logged both at debug ("sync server not
// reachable yet") and retried every 10 seconds forever. Nothing on any surface
// said the word "token", and the retry never converged because no amount of
// retrying fixes a rejected credential.
//
// Worse for a server recreated UNDER a running daemon: transportLoop caches
// the connected client and never re-handshakes, so the pipelines kept using a
// client holding the dead token. Even replacing the token on disk changed
// nothing until the daemon was restarted, because nothing ever re-resolved it.
//
// This file is the state machine that closes both halves:
//
//   - Detection. syncdb.IsAuthError classifies a 401 from any client call.
//     transportLoop reports handshake rejections; a hook on the live client
//     (Client.SetAuthFailureHook) reports mid-run ones from push/pull/snapshot.
//   - One actionable error per episode. The first rejection logs at ERROR with
//     the server, the source, and the remediation, and broadcasts an
//     "auth_failed" sync update so the TUI shows it. Subsequent rejections in
//     the same episode are silent — the point is one error, not a flood.
//   - Recovery without a restart. A mid-run rejection marks the transport for
//     teardown; transportLoop drops the cached client and its pipelines, and
//     the next connect attempt re-resolves the token from config
//     (GROVE_SYNC_TOKEN > token_command > token — SyncConfig.ResolveToken), so
//     a re-minted token self-heals. Reconnect attempts back off while the
//     token stays rejected, instead of hammering a handshake per 10s.
//
// Nothing here touches notebook content: the recovery is reconnect-only.

const (
	// authSourceHandshake marks a rejection from the connect-time capabilities
	// handshake — no live client exists yet, so there is nothing to tear down.
	authSourceHandshake = "handshake"

	// authSourcePipeline marks a rejection observed by a running pipeline
	// (push, pull, snapshot, blob) through the client's auth-failure hook.
	authSourcePipeline = "pipeline"

	// defaultAuthRetryBase/Max bound the reconnect backoff while the token is
	// rejected. A dead token is not a transient blip: the flat 10s retry
	// bought nothing and cost a full HTTP handshake 8,640 times a day.
	defaultAuthRetryBase = 30 * time.Second
	defaultAuthRetryMax  = 10 * time.Minute
)

// authRetryBounds resolves the backoff window, honoring the test seams.
func (h *SyncHandler) authRetryBounds() (base, max time.Duration) {
	base, max = h.authRetryBase, h.authRetryMax
	if base <= 0 {
		base = defaultAuthRetryBase
	}
	if max < base {
		max = defaultAuthRetryMax
	}
	if max < base {
		max = base
	}
	return base, max
}

// authRemediation is the one thing an operator needs to be told. It mirrors
// the wording verifySatelliteSyncToken (grove/cmd/satellite_sync.go) prints
// for the same condition, so the two surfaces do not contradict each other.
const authRemediation = "mint a replacement on the server (`grove-syncd token create <description>`) " +
	"and point this machine's sync.toml at it (token / token_command / GROVE_SYNC_TOKEN); " +
	"for a satellite, fetch the token its bootstrap minted: " +
	"ssh <vm> 'sudo cat /root/laptop-sync.token' > <token file>"

// syncAuthState is the transport's view of "is our token being rejected". All
// fields are guarded by SyncHandler.authMu.
type syncAuthState struct {
	failing  bool      // the server is currently rejecting this machine's token
	since    time.Time // when the current episode started
	detail   string    // the last rejection's error text (surfaced in status)
	source   string    // where it was observed: handshake, push, pull, ...
	reported bool      // the actionable error has been emitted for this episode

	// backoff/retryAt throttle reconnect attempts while failing. Only
	// handshake rejections advance them — a rejection observed by a live
	// pipeline says nothing about how soon a reconnect is worth trying.
	backoff time.Duration
	retryAt time.Time

	// needsReset is set when a LIVE pipeline was rejected: the cached client
	// holds a dead token and must be dropped before any replacement token can
	// take effect. Consumed by transportLoop via takeTransportReset.
	needsReset bool
}

// noteAuthFailure records a token rejection and, once per episode, emits the
// single actionable error. source is authSourceHandshake or the pipeline that
// observed it. Safe to call from any goroutine and from the client hook, which
// runs inline on a pipeline's request path — it never blocks on I/O.
func (h *SyncHandler) noteAuthFailure(ctx context.Context, source string, err error) {
	if err == nil {
		return
	}

	h.authMu.Lock()
	if !h.auth.failing {
		h.auth.failing = true
		h.auth.since = time.Now()
		h.auth.reported = false
		h.auth.backoff = 0
	}
	h.auth.detail = err.Error()
	h.auth.source = source
	if source == authSourceHandshake {
		// Advance the reconnect backoff: this attempt just failed.
		base, max := h.authRetryBounds()
		switch {
		case h.auth.backoff == 0:
			h.auth.backoff = base
		case h.auth.backoff < max:
			h.auth.backoff *= 2
		}
		if h.auth.backoff > max {
			h.auth.backoff = max
		}
		h.auth.retryAt = time.Now().Add(h.auth.backoff)
	} else {
		// A live pipeline was rejected: the cached client is holding a dead
		// token and must be rebuilt before a replacement can be resolved.
		h.auth.needsReset = true
	}
	report := !h.auth.reported
	h.auth.reported = true
	h.authMu.Unlock()

	if !report {
		return
	}

	server := ""
	h.syncCfgMu.RLock()
	if h.syncCfg != nil {
		server = h.syncCfg.Server
	}
	h.syncCfgMu.RUnlock()

	h.ulog.Error("sync server rejected this machine's token — replication is stopped until it is replaced").
		Field("server", server).
		Field("source", source).
		Field("detail", err.Error()).
		Field("remediation", authRemediation).
		Log(ctx)

	h.broadcastConflict(&store.SyncConflictPayload{
		Kind:   "auth_failed",
		Detail: fmt.Sprintf("%s rejected this machine's sync token (%s); %s", serverLabel(server), source, authRemediation),
	})
}

// serverLabel renders the configured server for a message, tolerating an
// unset one (the config can be reloaded out from under the transport).
func serverLabel(server string) string {
	if server == "" {
		return "the sync server"
	}
	return server
}

// noteAuthSuccess clears an auth-failure episode after a handshake succeeds.
// It logs the recovery exactly once, so an operator who fixed the token sees
// confirmation without watching for an absence of errors.
func (h *SyncHandler) noteAuthSuccess(ctx context.Context) {
	h.authMu.Lock()
	recovered := h.auth.failing
	since := h.auth.since
	h.auth = syncAuthState{}
	h.authMu.Unlock()

	if recovered {
		h.ulog.Info("sync token accepted again — replication resumed").
			Field("rejected_for", time.Since(since).Round(time.Second).String()).
			Log(ctx)
	}
}

// clearAuthBackoff drops the reconnect backoff without clearing the episode:
// the failure is still real until a handshake proves otherwise, but the next
// tick gets to try. Called on a sync-config reload, which is usually the
// operator installing the replacement token.
func (h *SyncHandler) clearAuthBackoff() {
	h.authMu.Lock()
	h.auth.backoff = 0
	h.auth.retryAt = time.Time{}
	h.authMu.Unlock()
}

// authConnectDue reports whether a connect attempt is allowed on this tick.
// It is false only while an auth-failure episode is inside its backoff window;
// every other failure mode keeps the ordinary tick cadence.
func (h *SyncHandler) authConnectDue(now time.Time) bool {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	if !h.auth.failing || h.auth.retryAt.IsZero() {
		return true
	}
	return !now.Before(h.auth.retryAt)
}

// takeTransportReset consumes the "the cached client holds a dead token" flag
// set by a mid-run rejection. Returns true at most once per rejection.
func (h *SyncHandler) takeTransportReset() bool {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	reset := h.auth.needsReset
	h.auth.needsReset = false
	return reset
}

// AuthFailure reports the current token-rejection state for the sync status
// surface: a human-readable detail, when the episode started, and whether one
// is in progress. Empty/zero/false when the token is fine.
func (h *SyncHandler) AuthFailure() (detail string, since time.Time, failing bool) {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	if !h.auth.failing {
		return "", time.Time{}, false
	}
	return h.auth.detail, h.auth.since, true
}

// resetTransport drops the cached client and every running pipeline so the
// next transportLoop tick rebuilds both against a freshly resolved token. This
// is the recovery half of the stale-token fix: without it, replacing the token
// on disk did nothing until the daemon restarted, because the client that
// resolved it is constructed exactly once.
//
// Push-side safety: pipelines are cancelled, never drained-and-dropped. The
// outbox is in sync.db, so everything queued survives into the rebuilt
// pipelines; nothing local is deleted or rewritten.
func (h *SyncHandler) resetTransport(ctx context.Context) {
	h.pipelinesMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(h.pipelines))
	for _, cancel := range h.pipelines {
		cancels = append(cancels, cancel)
	}
	h.pipelines = make(map[string]context.CancelFunc)
	h.aePasses = make(map[string]*syncdb.AntiEntropyPass)
	h.pipelinesMu.Unlock()

	h.clientMu.Lock()
	had := h.client != nil
	h.client = nil
	h.clientMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	if had || len(cancels) > 0 {
		h.ulog.Warn("sync transport reset — reconnecting with a freshly resolved token").
			Field("pipelines_stopped", len(cancels)).
			Log(ctx)
	}
}

// kickAntiEntropyExcept requests an immediate anti-entropy pass on every
// running workspace but the named one. It is the fan-out for an epoch reset
// (contract §3 P2b, scope 2): CheckServerEpoch voids the synced state of ALL
// workspaces and clears their outboxes, but only the workspace whose pass
// detected the change sweeps in that cycle — the rest would sit voided, with
// nothing queued and nothing scheduled, until their own hourly tick.
func (h *SyncHandler) kickAntiEntropyExcept(workspace string) {
	h.pipelinesMu.Lock()
	defer h.pipelinesMu.Unlock()
	for name, ae := range h.aePasses {
		if name != workspace {
			ae.Kick()
		}
	}
}
