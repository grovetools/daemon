package collector

import (
	"context"
	"os"
	"time"

	"github.com/grovetools/agentlogs/pkg/usage"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/process"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// PtyKiller is satisfied by any type that can terminate an out-of-process PTY
// by ID. The tuimux ApiClient implements this interface.
type PtyKiller interface {
	KillPty(ptyID string) error
}

const (
	// sessionReapGracePeriod is how long after a session's start/last-activity
	// the liveness reaper leaves it completely alone. Agent startup can register
	// a short-lived intermediate PID (shell, grove meta-tool) before the real
	// agent process exists; we never judge liveness inside this window.
	sessionReapGracePeriod = 45 * time.Second

	// reapDeadStrikes is the number of consecutive polls a PID must read as dead
	// before the session is reaped, to absorb transient IsProcessAlive blips.
	reapDeadStrikes = 2

	// liveTokenRefreshInterval throttles per-agent transcript re-summarization.
	// Transcript parsing is expensive, so live token usage is recomputed at most
	// this often — never on every 2s liveness tick — mirroring flow's
	// runningTokenRefreshInterval (flow/pkg/tui/status/token_pane.go).
	liveTokenRefreshInterval = 4 * time.Second
)

// liveTokenSummary caches the last-computed token snapshot for one session,
// keyed by session ID inside the collector. mtime is the parent transcript's
// modification time at summarization; an unchanged mtime lets the next refresh
// skip the expensive re-parse entirely.
type liveTokenSummary struct {
	mtime   time.Time
	tokens  int64
	cost    float64
	ctxSize int64
}

// pidLiveness is per-session reaper bookkeeping carried across poll ticks.
// A PID is only eligible for reaping once it has been positively observed alive
// (seenAlive) — a never-confirmed-alive PID is more likely a slow/handoff
// startup than a crashed agent, so reaping it would race the starting agent.
type pidLiveness struct {
	seenAlive   bool
	deadStrikes int
}

// SessionCollector monitors active sessions in the store for process liveness.
// It also performs initial crash recovery on daemon startup.
//
// The daemon store is the single source of truth for session state.
// This collector only:
// 1. Recovers sessions from the filesystem crash-recovery registry on startup
// 2. Periodically verifies that active sessions' PIDs are still alive
// 3. Cleans up dead sessions (marks as interrupted, removes crash-recovery files)
type SessionCollector struct {
	interval  time.Duration
	ulog      *logging.UnifiedLogger
	ptyKiller PtyKiller
	// scope is this daemon's owning scope ("" == unscoped/global). The collector
	// only ever seeds and reaps sessions whose owning scope matches, so a
	// daemon can never reap another scope's agents.
	scope string
	// liveness tracks per-session reap bookkeeping (seen-alive + dead strikes)
	// across ticks. Only accessed from the single Run goroutine.
	liveness map[string]*pidLiveness
	// tokenCache memoizes the last live-token summary per session ID (with the
	// transcript mtime) so an unchanged transcript skips re-parsing. Only
	// accessed from the single Run goroutine.
	tokenCache map[string]liveTokenSummary
	// lastTokenRefresh throttles the live-token pass to liveTokenRefreshInterval,
	// decoupling expensive transcript parsing from the 2s liveness tick.
	lastTokenRefresh time.Time
}

// NewSessionCollector creates a new SessionCollector.
// Defaults to 2 seconds for PID verification. scope is the owning daemon scope
// ("" == unscoped/global) and bounds which sessions this collector seeds/reaps.
func NewSessionCollector(interval time.Duration, scope string) *SessionCollector {
	if interval == 0 {
		interval = 2 * time.Second
	}
	return &SessionCollector{
		interval:   interval,
		ulog:       logging.NewUnifiedLogger("groved.collector.session"),
		scope:      scope,
		liveness:   make(map[string]*pidLiveness),
		tokenCache: make(map[string]liveTokenSummary),
	}
}

// SetPtyKiller wires a PTY terminator into the collector. When set, the
// collector kills the out-of-process PTY when it detects a dead session PID
// so that treemux panes auto-close without requiring a daemon restart.
// Must be called before the engine starts the collector's Run goroutine.
func (c *SessionCollector) SetPtyKiller(killer PtyKiller) {
	c.ptyKiller = killer
}

// Name returns the collector's name.
func (c *SessionCollector) Name() string { return "session" }

// Run starts the session liveness verification loop.
func (c *SessionCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	// 1. Initial Crash Recovery
	// Load sessions that were running before the daemon started/restarted.
	// Scope-filtered: a daemon only adopts sessions whose owning scope matches
	// its own, so the operational store (and thus the shutdown kill-list and the
	// liveness reaper below) can never touch another scope's agents.
	recoveredSessions, err := sessions.RecoverSessionsForScope(c.scope)
	if err != nil {
		c.ulog.Warn("Failed to recover sessions from disk").Err(err).Log(ctx)
	} else if len(recoveredSessions) > 0 {
		c.ulog.Info("Recovered active sessions from crash registry").
			Field("count", len(recoveredSessions)).
			Log(ctx)
		updates <- store.Update{
			Type:    store.UpdateSessions,
			Source:  "session_recovery",
			Scanned: len(recoveredSessions),
			Payload: recoveredSessions,
		}
	}

	// Recovered sessions were alive when persisted, so a dead PID now means a
	// genuine crash (not a slow startup) — seed them as seen-alive so the loop
	// below reaps them. Runtime-registered sessions start seenAlive=false and
	// must be observed alive before they become reap-eligible (startup guard).
	for _, s := range recoveredSessions {
		c.liveness[s.ID] = &pidLiveness{seenAlive: true}
	}

	// 2. PID Verification Loop
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	registry, _ := sessions.NewFileSystemRegistry()

	c.ulog.Info("Session liveness collector started").Log(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			start := time.Now()

			// Get all active sessions from the canonical store
			activeSessions := st.GetSessions()

			// Track which sessions are still active this tick so we can prune
			// liveness bookkeeping for sessions that have since ended.
			activeIDs := make(map[string]struct{}, len(activeSessions))

			for _, session := range activeSessions {
				// Only verify sessions we think are active.
				if session.Status != "running" && session.Status != "idle" && session.Status != "pending_user" {
					continue
				}
				activeIDs[session.ID] = struct{}{}

				// Grace period: leave freshly-started sessions completely alone.
				// During agent startup the registered PID may be a short-lived
				// intermediate process (shell, grove meta-tool) that exits before
				// the real agent starts.
				if time.Since(session.LastActivity) < sessionReapGracePeriod && time.Since(session.StartedAt) < sessionReapGracePeriod {
					continue
				}

				// PID 0 means this daemon's record was never confirmed with a real
				// PID. That happens for genuinely unstarted intents AND for sessions
				// confirmed against a different scoped daemon (the filesystem
				// job-watcher synthesizes a PID-0 record here). For the latter the
				// real PID is in the global crash-recovery registry; recover it so
				// the session is reapable instead of lingering "running" forever.
				// A registry entry only exists post-confirmation, so its presence is
				// proof the session was alive — seed seenAlive=true so an
				// already-dead orphan reaps without needing a fresh alive reading.
				pid := session.PID
				var recovered *sessions.SessionMetadata
				if pid == 0 {
					if registry != nil {
						if md, err := registry.Find(session.ID); err == nil && md != nil && md.PID > 0 {
							// registry.Find is a global cross-scope point-lookup. Only
							// adopt its PID (and thus make this record reapable) when the
							// recovered record's owning scope matches ours; otherwise this
							// is another scope's confirmed session and reaping it would be
							// the cross-scope leak we're guarding against.
							if md.Scope == c.scope {
								pid = md.PID
								recovered = md
							}
						}
					}
					if pid == 0 {
						// No confirmed PID for our scope — a true unstarted intent or a
						// foreign-scope record we must not touch. Can't/shouldn't judge.
						continue
					}
				}

				ls := c.liveness[session.ID]
				if ls == nil {
					ls = &pidLiveness{seenAlive: recovered != nil}
					c.liveness[session.ID] = ls
				}

				if process.IsProcessAlive(pid) {
					// Confirmed alive — eligible for reaping only if it later dies.
					// Reset any transient dead strikes.
					ls.seenAlive = true
					ls.deadStrikes = 0
					continue
				}

				// PID reads dead. Only reap a PID we have positively observed alive:
				// a never-confirmed-alive PID is more likely a slow/handoff startup
				// than a crashed agent, and reaping it would race the starting agent.
				if !ls.seenAlive {
					continue
				}

				// Debounce: require N consecutive dead reads before reaping.
				ls.deadStrikes++
				if ls.deadStrikes < reapDeadStrikes {
					continue
				}

				c.ulog.Warn("Session process died unexpectedly").
					Field("job_id", session.ID).
					Field("pid", pid).
					Log(ctx)

				// Kill the out-of-process PTY so treemux panes get EOF and
				// auto-close. The process is already dead so this is best-effort.
				if c.ptyKiller != nil && session.PtyID != "" {
					if err := c.ptyKiller.KillPty(session.PtyID); err != nil {
						c.ulog.Debug("Failed to kill PTY for dead session").
							Err(err).
							Field("job_id", session.ID).
							Field("pty_id", session.PtyID).
							Log(ctx)
					}
				}

				// Update daemon state
				updates <- store.Update{
					Type:   store.UpdateSessionEnd,
					Source: "session_collector",
					Payload: &store.SessionEndPayload{
						JobID:   session.ID,
						Outcome: "interrupted",
					},
				}

				// Clean up the crash recovery files. Prefer the native ID from the
				// recovered registry metadata when this daemon's record lacks it.
				if registry != nil {
					nativeID := session.ClaudeSessionID
					if nativeID == "" && recovered != nil && recovered.ClaudeSessionID != "" {
						nativeID = recovered.ClaudeSessionID
					}
					if nativeID == "" {
						nativeID = session.ID
					}
					_ = registry.Unregister(nativeID)
				}

				delete(c.liveness, session.ID)
			}

			// Prune liveness bookkeeping for sessions no longer active.
			for id := range c.liveness {
				if _, ok := activeIDs[id]; !ok {
					delete(c.liveness, id)
				}
			}

			// Live per-agent token usage. Throttled to liveTokenRefreshInterval
			// and skipped entirely on unchanged transcripts, so the 2s liveness
			// tick never pays for transcript parsing.
			if time.Since(c.lastTokenRefresh) >= liveTokenRefreshInterval {
				c.lastTokenRefresh = time.Now()
				c.refreshLiveTokens(ctx, activeSessions, updates)
			}

			if d := time.Since(start); d > 1*time.Second {
				c.ulog.Debug("Slow PID verification detected").Field("duration", d).Log(ctx)
			}
		}
	}
}

// isLiveAgentSession reports whether a session is a live Claude agent worth
// summarizing: it must be active (running/idle/pending_user) and carry a
// ClaudeSessionID (the marker of a Claude transcript — plain shells never have
// one). Completed/failed sessions and non-agent shells are skipped.
func isLiveAgentSession(s *models.Session) bool {
	if s == nil || s.ClaudeSessionID == "" {
		return false
	}
	switch s.Status {
	case "running", "idle", "pending_user":
		return true
	default:
		return false
	}
}

// transcriptMtime returns the modification time of a session's parent transcript,
// or the zero time when unknown. A zero mtime never compares equal to a cached
// mtime, so such sessions are re-summarized each refresh (bounded by the
// throttle) rather than being skipped.
func transcriptMtime(transcriptPath string) time.Time {
	if transcriptPath == "" {
		return time.Time{}
	}
	info, err := os.Stat(transcriptPath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// refreshLiveTokens summarizes live per-agent token usage for the active agent
// sessions and emits a single in-place UpdateSessionTokens with whatever
// changed. It is called at most once per liveTokenRefreshInterval. Per session
// it skips the expensive re-parse when the transcript mtime is unchanged, and
// only includes a session in the update when its token snapshot actually
// changed — so an idle transcript produces no broadcast churn.
func (c *SessionCollector) refreshLiveTokens(ctx context.Context, activeSessions []*models.Session, updates chan<- store.Update) {
	var tokenUpdates []store.SessionTokenUpdate
	live := make(map[string]struct{})

	for _, s := range activeSessions {
		if !isLiveAgentSession(s) {
			continue
		}
		live[s.ID] = struct{}{}

		mtime := transcriptMtime(s.TranscriptPath)
		if cached, ok := c.tokenCache[s.ID]; ok && !mtime.IsZero() && mtime.Equal(cached.mtime) {
			// Transcript unchanged since the last summary; already applied.
			continue
		}

		slugDirs := usage.SlugDirsForSession(s.ClaudeSessionID, s.TranscriptPath)
		summary, err := usage.SummarizeSession(slugDirs, s.ClaudeSessionID, usage.CostModeCalculate)
		if err != nil {
			c.ulog.Debug("Failed to summarize live token usage").
				Field("job_id", s.ID).
				Field("claude_session_id", s.ClaudeSessionID).
				Err(err).
				Log(ctx)
			continue
		}

		// LiveTokens is the context-preferred magnitude: the peak single-turn
		// prompt size (Claude Code's /context analogue) when available, else the
		// cache-read-inflated cumulative total — mirroring flow's token pane.
		liveTokens := summary.ContextSize
		if liveTokens == 0 {
			liveTokens = summary.Usage.Total()
		}

		entry := liveTokenSummary{
			mtime:   mtime,
			tokens:  liveTokens,
			cost:    summary.CostUSD,
			ctxSize: summary.ContextSize,
		}

		prev, existed := c.tokenCache[s.ID]
		c.tokenCache[s.ID] = entry
		// Skip the broadcast when only the mtime moved but the numbers held.
		if existed && prev.tokens == entry.tokens && prev.cost == entry.cost && prev.ctxSize == entry.ctxSize {
			continue
		}

		tokenUpdates = append(tokenUpdates, store.SessionTokenUpdate{
			JobID:       s.ID,
			LiveTokens:  liveTokens,
			LiveCostUSD: summary.CostUSD,
			ContextSize: summary.ContextSize,
		})
	}

	// Drop cache entries for sessions that are no longer live agents.
	for id := range c.tokenCache {
		if _, ok := live[id]; !ok {
			delete(c.tokenCache, id)
		}
	}

	if len(tokenUpdates) > 0 {
		updates <- store.Update{
			Type:    store.UpdateSessionTokens,
			Source:  "session_token_collector",
			Scanned: len(tokenUpdates),
			Payload: &store.SessionTokensPayload{Updates: tokenUpdates},
		}
	}
}
