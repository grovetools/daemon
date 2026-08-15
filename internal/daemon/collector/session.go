package collector

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	aglogsession "github.com/grovetools/agentlogs/pkg/sessioninfo"
	"github.com/grovetools/agentlogs/pkg/usage"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/process"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/core/pkg/sessions/health"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
	"github.com/grovetools/flow/pkg/orchestration"
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

	// Transcript resolution failures back off exponentially, then become
	// permanent for this registration. A registration change clears the state.
	transcriptResolveInitialBackoff = 30 * time.Second
	transcriptResolveMaxBackoff     = 10 * time.Minute
	transcriptResolveMaxFailures    = 5

	// bashChildTTL bounds how long a live background bash job stays shown (F6).
	// Background bash has no reliable per-job completion hook: background_tasks[]
	// entries only ever report status "running", and a session with no subagent
	// never fires the SubagentStop that would list them at all. The accurate
	// clear is drop-reconciliation from a SubagentStop snapshot; this TTL is the
	// guaranteed floor for sessions that never fire one, capping a finished
	// bash's lingering display. Long enough that a genuinely long-running bg bash
	// (poll loops, daemons) stays visible for a useful window; a stuck count can
	// never outlive it.
	bashChildTTL = 10 * time.Minute
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
	model   string
	// Resolution state is scoped to registrationKey. Failures back off and
	// eventually become permanent until that key changes.
	resolvedTranscript string
	registrationKey    string
	resolveFailures    int
	nextResolveAttempt time.Time
	resolvePermanent   bool
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
	// Injectable seams keep backoff tests deterministic and avoid global scans.
	now               func() time.Time
	resolveTranscript func(string) (path, provider string, err error)
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
		now:        time.Now,
		resolveTranscript: func(spec string) (string, string, error) {
			info, err := aglogsession.Resolve(spec)
			if err != nil {
				return "", "", err
			}
			return info.LogFilePath, info.Provider, nil
		},
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
				// Remote (federated) sessions never enter the local liveness state
				// machine (C8): their PID/PTY/transcript belong to a satellite, so
				// IsProcessAlive would judge an unrelated local PID and the reaper
				// could signal it. Staleness for remote rows is derived from
				// satellite_status, not from this loop. Skip before anything reads
				// c.liveness or a PID for this row.
				if session.Origin != "" {
					continue
				}
				// Only verify sessions we think are active. "pending" is included
				// so a session whose agent process died before completing its first
				// turn (claude exited during startup) still gets reaped instead of
				// lingering forever. This is safe: a flow-registered intent that has
				// not spawned yet carries PID 0 AND has no crash-registry record, so
				// the pid==0 branch below continues without judging it; a
				// hook-registered session always has a registry record with a real
				// PID, so a genuinely dead pending session reaps through the normal
				// seenAlive/deadStrikes guards.
				if session.Status != "running" && session.Status != "idle" && session.Status != "pending_user" && session.Status != "pending" {
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
						Reason:  "process_dead",
					},
				}

				// The store now says interrupted, but the job file on
				// disk still says "running" — and flow tooling reads
				// the file, not the store. Reconcile it here so a
				// reaped agent can't leave a phantom running job behind
				// for anyone to trip over later.
				c.reconcileJobFile(ctx, session)

				// Clean up the crash recovery files. Prefer the native ID from the
				// recovered registry metadata when this daemon's record lacks it.
				//
				// Only the recovery state goes: metadata.json is the record
				// binding this job to its native session and transcript, and
				// consumers read it after the process is gone. A dead-PID
				// reading — from a PID that may be stale or may belong to a
				// launcher rather than the agent — is not licence to delete the
				// index. Age-based cleanup is sessions.PurgeStaleSessions' job.
				if registry != nil {
					nativeID := session.ClaudeSessionID
					if nativeID == "" && recovered != nil && recovered.ClaudeSessionID != "" {
						nativeID = recovered.ClaudeSessionID
					}
					if nativeID == "" {
						nativeID = session.ID
					}
					_ = registry.RemoveRecoveryFiles(nativeID)
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

			// Expire any live background bash children past the TTL floor before
			// deriving counts, so a finished bash on a session that never fires
			// SubagentStop still clears (F6 guaranteed-clear path). Cheap in-memory
			// pass; a no-op when no bash children are tracked.
			st.ExpireBashChildren(time.Now(), bashChildTTL)

			// Daemon-authoritative live-background-child count. Recomputed every
			// tick (cheap — pure in-memory map math, no transcript parsing) so a
			// session whose subagents/workflow-run children have all finished
			// self-clears to 0 without needing another SubagentStop (the F3
			// clearing guarantee). This is the authoritative source; the hook's
			// children_snapshot is a best-effort inter-tick bump that this
			// reconciles within one tick.
			c.refreshLiveChildren(st, activeSessions, updates)

			if d := time.Since(start); d > 1*time.Second {
				c.ulog.Debug("Slow PID verification detected").Field("duration", d).Log(ctx)
			}
		}
	}
}

// reconcileJobFile flips a reaped session's job file out of its
// "running" claim.
//
// The daemon's reaper has always marked the STORE interrupted, but the
// job file kept saying running forever — that is exactly the
// "87-commit.md running 23m" class of ghost, and it survived precisely
// because nobody was watching. The reaper is the right place for it:
// it has already assembled the evidence (seen-alive, N consecutive dead
// PID reads, past the grace window) that justifies the write.
//
// Which status it flips to mirrors the jobrunner's adoption
// philosophy: a turn-based job (chat/oneshot) is safely re-runnable so
// it gets the terminal "interrupted"; an agent job gets the
// non-terminal "orphaned" — "we lost it", not "it failed".
//
// Best-effort by design: a failure here must never stop the reap. Every
// outcome is logged with the evidence so a wrong flip is diagnosable.
func (c *SessionCollector) reconcileJobFile(ctx context.Context, session *models.Session) {
	if session.JobFilePath == "" {
		return
	}
	want := health.ReconciledStatusFor(session.Type)
	changed, err := orchestration.ReconcileJobFile(session.JobFilePath, want)
	switch {
	case err != nil:
		c.ulog.Warn("Failed to reconcile job file for reaped session").
			Err(err).
			Field("job_id", session.ID).
			Field("job_file", session.JobFilePath).
			Log(ctx)
	case changed:
		c.ulog.Info("Reconciled stuck job file after reaping its session").
			Field("job_id", session.ID).
			Field("job_file", session.JobFilePath).
			Field("new_status", want).
			Field("evidence", "session reaped: pid dead past grace window").
			Log(ctx)
	}
}

// isNonClaudeProvider reports whether a session-registry provider value
// denotes a non-Claude agent CLI (codex/pi/opencode). Empty means Claude:
// older records omit the provider, and only Claude runs predate the field
// (inverse of flow's isClaudeSessionProvider).
func isNonClaudeProvider(provider string) bool {
	return provider != "" && !strings.HasPrefix(provider, "claude")
}

// isLiveAgentSession reports whether a session is a live agent worth
// summarizing: it must be active (running/idle/pending_user) and identify an
// agent transcript — a ClaudeSessionID (Claude), an explicit TranscriptPath,
// or a non-claude provider whose transcript is resolvable by job ID (opencode
// registers with neither an ID nor a path). Plain shells carry none of the
// three and are skipped, as are completed/failed sessions.
func isLiveAgentSession(s *models.Session) bool {
	if s == nil {
		return false
	}
	if s.ClaudeSessionID == "" && s.TranscriptPath == "" && !isNonClaudeProvider(s.Provider) {
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
		// Remote sessions carry satellite-side transcript paths that don't exist
		// locally (C8); summarizing them would spin the resolve-throttle on a path
		// that can never resolve. Skip before isLiveAgentSession.
		if s.Origin != "" {
			continue
		}
		if !isLiveAgentSession(s) {
			continue
		}
		live[s.ID] = struct{}{}
		cached := c.ensureTokenRegistration(s)

		// The transcript we expect to summarize: the registered path, else the
		// path a previous refresh resolved for a non-claude session.
		knownPath := s.TranscriptPath
		if knownPath == "" {
			knownPath = cached.resolvedTranscript
		}
		mtime := transcriptMtime(knownPath)
		if cached, ok := c.tokenCache[s.ID]; ok && !mtime.IsZero() && mtime.Equal(cached.mtime) {
			// Transcript unchanged since the last summary; already applied.
			// Counted as "considered but not parsed" so the telemetry tab can
			// show the mtime short-circuit working: a considered rate that
			// tracks the parse rate means the daemon is rescanning transcripts
			// it has already read, which is exactly the incident this counter
			// exists to catch.
			telemetry.RecordTranscriptParse(false, 0)
			continue
		}

		parseStart := time.Now()
		summary, usedPath, err := c.summarizeLiveSession(s)
		telemetry.RecordTranscriptParse(true, time.Since(parseStart))
		if err != nil {
			if !errors.Is(err, errResolveThrottled) && !errors.Is(err, errResolvePermanent) {
				c.ulog.Debug("Failed to summarize live token usage").
					Field("job_id", s.ID).
					Field("provider", s.Provider).
					Field("claude_session_id", s.ClaudeSessionID).
					Err(err).
					Log(ctx)
			}
			continue
		}
		if usedPath != knownPath {
			// A fresh resolution found the transcript this tick; stat the real
			// path so the next refresh's mtime comparison is meaningful.
			mtime = transcriptMtime(usedPath)
		}

		// LiveTokens is the context-preferred magnitude: the peak single-turn
		// prompt size (Claude Code's /context analogue) when available, else the
		// cache-read-inflated cumulative total — mirroring flow's token pane.
		liveTokens := summary.ContextSize
		if liveTokens == 0 {
			liveTokens = summary.Usage.Total()
		}

		// The cost-dominant model: summarize sorts ModelBreakdown by cost
		// descending, so index 0 is the model that did the session's real work.
		model := ""
		if len(summary.ModelBreakdown) > 0 {
			model = summary.ModelBreakdown[0].Model
		}

		// Re-read after summarizeLiveSession: it stores resolution/backoff state
		// that the new entry must keep.
		prev, existed := c.tokenCache[s.ID]
		entry := liveTokenSummary{
			mtime:              mtime,
			tokens:             liveTokens,
			cost:               summary.CostUSD,
			ctxSize:            summary.ContextSize,
			model:              model,
			resolvedTranscript: prev.resolvedTranscript,
			registrationKey:    prev.registrationKey,
			resolveFailures:    prev.resolveFailures,
			nextResolveAttempt: prev.nextResolveAttempt,
			resolvePermanent:   prev.resolvePermanent,
		}
		c.tokenCache[s.ID] = entry
		// Skip the broadcast when only the mtime moved but the numbers held.
		if existed && prev.tokens == entry.tokens && prev.cost == entry.cost && prev.ctxSize == entry.ctxSize && prev.model == entry.model {
			continue
		}

		tokenUpdates = append(tokenUpdates, store.SessionTokenUpdate{
			JobID:       s.ID,
			LiveTokens:  liveTokens,
			LiveCostUSD: summary.CostUSD,
			ContextSize: summary.ContextSize,
			Model:       model,
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

// refreshLiveChildren reconciles each active session's derived live-child count
// with the daemon's own authoritative view (store.LiveChildCounts, computed
// from WorkflowRuns + AdhocSubagents) and emits a children_snapshot for every
// session whose count changed. Because the derivation self-clears (completed
// children drop out), this is what returns an idle-busy agent to truly-idle
// once its background subagents/workflow finish — even if no further
// SubagentStop hook ever fires (F3). It reuses the existing children_snapshot
// apply path (applyChildrenSnapshot sets Session.LiveChildren unconditionally),
// so a zero count clears a previously-nonzero value. Emitted updates are keyed
// by JobID == the session's store key, so applyChildrenSnapshot's primary
// lookup lands directly. Only changed sessions are emitted, so a steady state
// produces no broadcast churn.
//
// Reconciliation with the hook snapshot: the daemon derivation is authoritative
// and wins each tick. The hook's turn-boundary snapshot still provides an
// immediate bump between ticks, but for a child kind the daemon cannot see
// (background bash — no liveness event exists), the derivation lowers the count
// back to its floor within one tick. That is the accepted F2 bash residual.
func (c *SessionCollector) refreshLiveChildren(st *store.Store, activeSessions []*models.Session, updates chan<- store.Update) {
	counts := st.LiveChildCounts()
	for _, s := range activeSessions {
		// Remote sessions have no local workflow/subagent children to count (C8);
		// the local derivation is always 0 for them and would emit spurious
		// zeroing snapshots against the satellite's real counts. Skip them.
		if s.Origin != "" {
			continue
		}
		want := counts[s.ID]
		if want == s.LiveChildren {
			continue
		}
		updates <- store.Update{
			Type:    store.UpdateWorkflowChildrenSnapshot,
			Source:  "session_child_collector",
			Scanned: 1,
			Payload: &store.WorkflowEventPayload{Event: models.WorkflowEvent{
				Kind:         models.WorkflowChildrenSnapshot,
				JobID:        s.ID,
				LiveChildren: want,
			}},
		}
	}
}

// Resolution wait states are expected and suppressed from per-tick logs.
var (
	errResolveThrottled = errors.New("transcript resolution throttled")
	errResolvePermanent = errors.New("transcript permanently unresolvable for this registration")
)

func transcriptRegistrationKey(s *models.Session) string {
	return strings.Join([]string{s.Provider, s.TranscriptPath, s.ClaudeSessionID, s.PlanDirectory, s.JobFilePath}, "\x00")
}

// ensureTokenRegistration resets all transcript-resolution state when the
// daemon receives changed registration metadata for the same job ID.
func (c *SessionCollector) ensureTokenRegistration(s *models.Session) liveTokenSummary {
	key := transcriptRegistrationKey(s)
	cached, ok := c.tokenCache[s.ID]
	if !ok || cached.registrationKey != key {
		cached = liveTokenSummary{registrationKey: key}
		c.tokenCache[s.ID] = cached
	}
	return cached
}

func transcriptResolveBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := transcriptResolveInitialBackoff
	for i := 1; i < failures && d < transcriptResolveMaxBackoff; i++ {
		d *= 2
	}
	return min(d, transcriptResolveMaxBackoff)
}

// summarizeLiveSession computes one live session's usage summary, branched on
// provider. Claude sessions go through slug-dir discovery (subagent-inclusive,
// as before); non-claude sessions summarize their single transcript via the
// provider-routed agentlogs summarizer, resolving the transcript by job ID
// when the registration lacked a path (caching the resolved path and applying
// the negative cache on failure). The second return is the transcript path
// actually used, for the caller's mtime bookkeeping.
func (c *SessionCollector) summarizeLiveSession(s *models.Session) (usage.Summary, string, error) {
	if !isNonClaudeProvider(s.Provider) {
		slugDirs := usage.SlugDirsForSession(s.ClaudeSessionID, s.TranscriptPath)
		summary, err := usage.SummarizeSession(slugDirs, s.ClaudeSessionID, usage.CostModeCalculate)
		return summary, s.TranscriptPath, err
	}

	provider := s.Provider
	path := s.TranscriptPath
	if path == "" {
		cached := c.ensureTokenRegistration(s)
		path = cached.resolvedTranscript
		if path == "" {
			if cached.resolvePermanent {
				return usage.Summary{}, "", errResolvePermanent
			}
			now := c.now()
			if now.Before(cached.nextResolveAttempt) {
				return usage.Summary{}, "", errResolveThrottled
			}
			resolvedPath, resolvedProvider, err := c.resolveTranscript(s.ID)
			if err == nil && resolvedPath == "" {
				err = errors.New("session resolved without a transcript path")
			}
			if err != nil {
				cached.resolveFailures++
				if cached.resolveFailures >= transcriptResolveMaxFailures {
					cached.resolvePermanent = true
				} else {
					cached.nextResolveAttempt = now.Add(transcriptResolveBackoff(cached.resolveFailures))
				}
				c.tokenCache[s.ID] = cached
				return usage.Summary{}, "", err
			}
			path = resolvedPath
			if resolvedProvider != "" {
				provider = resolvedProvider
			}
			cached.resolvedTranscript = path
			cached.resolveFailures = 0
			cached.nextResolveAttempt = time.Time{}
			cached.resolvePermanent = false
			c.tokenCache[s.ID] = cached
		}
	}
	summary, err := usage.SummarizeSessionTranscript(path, provider, usage.CostModeCalculate)
	return summary, path, err
}
