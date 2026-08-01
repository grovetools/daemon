package assistant

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreconfig "github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

const (
	// TickInterval is the slow periodic ensure. Everything that matters
	// (daemon start, pane focus, inbound Signal, session end) pokes the
	// supervisor directly; the tick exists to catch what no event announced —
	// a session that died without emitting an end, a plan edited by hand.
	TickInterval = 5 * time.Minute

	// LaunchGrace is how long a launched continuation has to register a live
	// session before the supervisor calls it a failure. Plan loading, context
	// assembly and provider startup all happen before the daemon sees a
	// session, so an ensure that fires seconds after a launch must not
	// conclude anything.
	LaunchGrace = 90 * time.Second

	// FastExitThreshold is the assistant's analogue of the signal-cli
	// supervisor's 10-second fast-exit window: a chain that dies less than
	// this after coming up did not do any work, so restarting it is a loop,
	// not a recovery. A standing assistant is meant to live for hours, so the
	// window is minutes rather than seconds.
	FastExitThreshold = 10 * time.Minute

	// MaxFastFailures is the circuit breaker's ceiling — consecutive
	// fast-failing continuations before supervision stops. Same value and
	// same reasoning as the signal-cli supervisor: past this the cause is a
	// config or environment problem that restarting can never fix.
	MaxFastFailures = 5

	// InitialBackoff / MaxBackoff bound the exponential retry window between
	// failed continuations.
	InitialBackoff = 30 * time.Second
	MaxBackoff     = 15 * time.Minute

	// ChainResetWindow is the rolling window MaxChainResetsPerDay applies to.
	ChainResetWindow = 24 * time.Hour
)

// ErrStopped is returned by Ensure when the circuit breaker has tripped. The
// breaker re-arms on daemon restart or on an explicit forced ensure.
var ErrStopped = errors.New("assistant supervision stopped")

// ErrDisabled is returned when no [assistant] block enabled the supervisor.
var ErrDisabled = errors.New("assistant supervisor is not enabled")

// Head identifies the live head of the assistant chain.
type Head struct {
	JobID   string
	JobFile string
}

// resolution is what the supervisor supervises: which ecosystem, which config,
// which plan directory. It is computed once, lazily, and then immutable.
//
// Lazy because the two deployments learn it at different times. A scoped daemon
// knows its ecosystem at construction (it is its own scope). The global daemon
// has to DISCOVER it, and discovery wants the workspace set the collectors
// publish after boot — so resolving eagerly in the daemon's wiring would either
// force a filesystem walk onto the boot path or read an empty store. Deferring
// to first use puts it on the collector goroutine, after Run has started.
type resolution struct {
	cfg     *Config
	planDir string
	// scope is the ecosystem root the config came from. Empty means the
	// supervisor never resolved one.
	scope string
	// candidates is every ecosystem that opted in, when more than one did.
	candidates []string
	// err explains a resolution that produced nothing supervisable. It is a
	// diagnosis, not a fatal: an unresolvable assistant is a disabled one.
	err error
}

func (r *resolution) enabled() bool {
	return r != nil && r.cfg.Active() && r.planDir != ""
}

// disabledResolution is what every unresolved read sees.
func disabledResolution() *resolution { return &resolution{cfg: &Config{}} }

// res returns the supervisor's resolution, computing it on first use.
//
// Cached in both directions: a resolution that found nothing is remembered too,
// so a daemon whose ecosystems have no [assistant] block does not re-walk the
// workspace tree on every tick. An operator who adds the block to grove.toml
// therefore restarts the daemon — or POSTs /api/assistant/ensure?force=1, which
// re-resolves (see Ensure).
func (s *Supervisor) res() *resolution {
	if r := s.resolved.Load(); r != nil {
		return r
	}
	return s.resolveNow()
}

// resolveNow runs the resolver and installs its answer. Callers that already
// hold s.mu must NOT call it: it takes s.mu to publish the state transition.
func (s *Supervisor) resolveNow() *resolution {
	s.resolveMu.Lock()
	if r := s.resolved.Load(); r != nil {
		s.resolveMu.Unlock()
		return r
	}
	r := disabledResolution()
	if s.resolve != nil {
		if got := s.resolve(); got != nil {
			r = got
		}
	}
	if r.cfg == nil {
		r.cfg = &Config{}
	}
	s.resolved.Store(r)
	s.resolveMu.Unlock()

	s.mu.Lock()
	if r.err != nil {
		s.lastError = r.err.Error()
	}
	if r.enabled() && s.state == models.AssistantStateDisabled {
		s.state = models.AssistantStateStarting
	}
	s.mu.Unlock()

	if hook := s.onResolved.Load(); hook != nil && *hook != nil {
		(*hook)(s.Status())
	}
	return r
}

// SetOnResolved registers a callback fired once, the moment the supervisor
// learns what it supervises. It is how the daemon claims the ecosystem's
// default claw (spec §3.4) without knowing the plan name at wiring time.
//
// A hook registered after resolution already happened fires immediately, so the
// order of daemon wiring versus an early inbound request cannot lose it.
func (s *Supervisor) SetOnResolved(fn func(models.AssistantStatus)) {
	s.onResolved.Store(&fn)
	if fn != nil && s.resolved.Load() != nil {
		fn(s.Status())
	}
}

// reresolve discards a cached resolution so the next read recomputes it. Only
// a forced ensure does this: the operator has changed grove.toml and asked.
//
// A supervisor built from an explicit Config (NewSupervisor) has no resolver to
// re-run, so its resolution is permanent and this is a no-op — dropping it
// would silently disable the supervisor on the first forced ensure.
//
// Dropping OUR cached answer is not enough. LoadConfig reads through
// config.LoadFrom, which keeps its own 2-second memo of the parsed cascade, so
// a re-resolution that lands inside that window would re-read the very config
// the operator just edited and conclude nothing changed — the force silently
// doing nothing, which is the one outcome an escape hatch may not have. The
// window is short, but "did my edit take?" must not depend on how fast the
// operator typed the curl.
func (s *Supervisor) reresolve() {
	if s.resolve == nil {
		return
	}
	coreconfig.ResetLoadCache()
	s.resolveMu.Lock()
	s.resolved.Store(nil)
	s.resolveMu.Unlock()
}

// Supervisor keeps an ecosystem's standing assistant chain alive.
//
// It is an ENSURE-RUNNING loop, not a hot restart loop: every trigger asks the
// same question ("is a live assistant session heading this plan?") and does the
// cheapest thing that makes the answer yes. A chain that is already live costs
// one map scan.
type Supervisor struct {
	flow FlowCLI
	ulog *logging.UnifiedLogger

	// resolve answers "what does this daemon supervise?". See resolution.
	resolve    func() *resolution
	resolveMu  sync.Mutex
	resolved   atomic.Pointer[resolution]
	onResolved atomic.Pointer[func(models.AssistantStatus)]

	// LiveHead reports the live head of the assistant chain. Injected by the
	// daemon so this package never reaches into the session store's locking.
	LiveHead func() (Head, bool)

	// Publish, when set, receives every status change so the daemon can put it
	// on the state stream. Called without the supervisor lock held.
	Publish func(models.AssistantStatus)

	// Clock is overridable for tests.
	Clock func() time.Time

	triggers chan string

	mu            sync.Mutex
	state         string
	lastAction    string
	lastActionAt  time.Time
	lastError     string
	restarts      int
	chainResets   int
	resetTimes    []time.Time
	fastFailures  int
	backoff       time.Duration
	nextAttemptAt time.Time
	lastLaunchAt  time.Time
	firstLiveAt   time.Time
	headJobID     string
	headJobFile   string
	stopped       bool
	ensuring      bool
}

// NewSupervisor builds a supervisor for a config that is already known. planDir
// must be the ABSOLUTE directory of the assistant plan — see FlowCLI for why
// the address is a directory and not `--at <plan>`.
func NewSupervisor(cfg *Config, planDir string, flow FlowCLI) *Supervisor {
	if cfg == nil {
		cfg = &Config{}
	}
	s := newSupervisor(flow, nil)
	s.resolved.Store(&resolution{cfg: cfg, planDir: planDir})
	if cfg.Active() && planDir != "" {
		s.state = models.AssistantStateStarting
	}
	return s
}

// newSupervisor is the shared constructor. resolve may be nil, in which case
// the caller is expected to install a resolution directly.
func newSupervisor(flow FlowCLI, resolve func() *resolution) *Supervisor {
	if flow == nil {
		flow = &ExecFlowCLI{}
	}
	return &Supervisor{
		flow:     flow,
		ulog:     logging.NewUnifiedLogger("groved.assistant"),
		resolve:  resolve,
		triggers: make(chan string, 1),
		Clock:    time.Now,
		backoff:  InitialBackoff,
		state:    models.AssistantStateDisabled,
	}
}

// Name implements the collector.Collector interface.
func (s *Supervisor) Name() string { return "assistant_supervisor" }

// Enabled reports whether this supervisor supervises anything. It resolves on
// first call, so it is also the cheapest way to force resolution.
func (s *Supervisor) Enabled() bool { return s.res().enabled() }

// PlanDir is the absolute assistant plan directory.
func (s *Supervisor) PlanDir() string { return s.res().planDir }

// Plan is the assistant's home plan name, or "" when nothing is supervised.
// The channels manager asks for it on the inbound path to decide whether a
// clawing job is the default claw, which is why it reads through the same
// resolution the supervisor acts on: the two can never name different plans.
func (s *Supervisor) Plan() string { return s.res().cfg.Plan }

// Scope is the ecosystem root whose [assistant] block configured this
// supervisor. On a scoped daemon it is the daemon's own scope; on the global
// daemon it is the ecosystem discovery selected.
func (s *Supervisor) Scope() string { return s.res().scope }

func (s *Supervisor) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// Trigger requests an ensure pass. It never blocks: the channel holds one
// pending request, so a burst of triggers (daemon start plus a pane focus plus
// three inbound messages) coalesces into a single pass, which is exactly right
// for an idempotent ensure.
func (s *Supervisor) Trigger(reason string) {
	select {
	case s.triggers <- reason:
	default:
	}
}

// Run implements collector.Collector: it services triggers, ticks slowly, and
// watches session-end updates for the assistant plan.
func (s *Supervisor) Run(ctx context.Context, st *store.Store, _ chan<- store.Update) error {
	if !s.Enabled() {
		s.publish()
		return nil
	}

	// Daemon start is itself a trigger (spec §3.3).
	s.Trigger("daemon_start")

	if st != nil {
		go s.watchSessionEnds(ctx, st)
	}

	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case reason := <-s.triggers:
			s.ensure(ctx, reason, false)
		case <-ticker.C:
			s.ensure(ctx, "tick", false)
		}
	}
}

// watchSessionEnds turns a session-end for the assistant plan into a trigger.
// This is the event that matters most: it is what makes the pane's "starting
// assistant…" placeholder short-lived instead of permanent.
func (s *Supervisor) watchSessionEnds(ctx context.Context, st *store.Store) {
	sub := st.Subscribe()
	defer st.Unsubscribe(sub)

	for {
		select {
		case <-ctx.Done():
			return
		case u, ok := <-sub:
			if !ok {
				return
			}
			if u.Type != store.UpdateSessionEnd {
				continue
			}
			payload, _ := u.Payload.(*store.SessionEndPayload)
			if payload == nil {
				continue
			}
			// The store has already applied the end by the time subscribers
			// see it, so LiveHead inside ensure reads post-end state.
			if s.ownsSession(st, payload.JobID) {
				s.Trigger("session_end")
			}
		}
	}
}

// ownsSession reports whether jobID belongs to the assistant's plan.
func (s *Supervisor) ownsSession(st *store.Store, jobID string) bool {
	if jobID == "" {
		return false
	}
	sess := st.GetSession(jobID)
	if sess == nil {
		return false
	}
	return sess.PlanName == s.Plan()
}

// Ensure runs one ensure pass synchronously and returns the resulting status.
// force re-arms a tripped circuit breaker; an ordinary request (the rail
// pane's) must not, or the breaker would be defeated by the very UI it exists
// to inform.
//
// A forced ensure also RE-RESOLVES what is supervised. Resolution is otherwise
// cached for the daemon's lifetime, so this is the operator's way to pick up an
// [assistant] block that was added to grove.toml since boot without restarting
// a daemon that is hosting live agents:
//
//	curl -X POST --unix-socket <sock> 'http://localhost/api/assistant/ensure?force=1'
func (s *Supervisor) Ensure(ctx context.Context, reason string, force bool) (models.AssistantStatus, error) {
	if force {
		s.reresolve()
	}
	if !s.Enabled() {
		return s.Status(), ErrDisabled
	}
	err := s.ensure(ctx, reason, force)
	return s.Status(), err
}

// Status returns the current public snapshot.
//
// Resolution happens BEFORE the lock is taken: resolveNow publishes a state
// transition under s.mu, so resolving from inside the locked section would
// deadlock the first status read.
func (s *Supervisor) Status() models.AssistantStatus {
	r := s.res()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked(r)
}

func (s *Supervisor) statusLocked(r *resolution) models.AssistantStatus {
	st := models.AssistantStatus{
		Enabled:             r.enabled(),
		State:               s.state,
		Plan:                r.cfg.Plan,
		PlanDir:             r.planDir,
		Scope:               r.scope,
		Candidates:          r.candidates,
		HeadJobID:           s.headJobID,
		HeadJobFile:         s.headJobFile,
		LastAction:          s.lastAction,
		LastError:           s.lastError,
		RestartCount:        s.restarts,
		ChainResets:         s.chainResets,
		ConsecutiveFailures: s.fastFailures,
	}
	if !r.enabled() {
		st.State = models.AssistantStateDisabled
	}
	if !s.lastActionAt.IsZero() {
		t := s.lastActionAt
		st.LastActionAt = &t
	}
	if s.state == models.AssistantStateBackoff && !s.nextAttemptAt.IsZero() {
		t := s.nextAttemptAt
		st.NextAttemptAt = &t
	}
	return st
}

func (s *Supervisor) publish() {
	if s.Publish == nil {
		return
	}
	s.Publish(s.Status())
}

// ensure is the whole loop. It is serialized: a second trigger arriving mid-pass
// is dropped rather than racing a launch that is already underway.
func (s *Supervisor) ensure(ctx context.Context, reason string, force bool) error {
	s.mu.Lock()
	if s.ensuring {
		s.mu.Unlock()
		return nil
	}
	if force {
		// A forced ensure re-arms the breaker and clears the backoff window:
		// the operator has looked at the failure and asked again.
		s.stopped = false
		s.fastFailures = 0
		s.backoff = InitialBackoff
		s.nextAttemptAt = time.Time{}
		s.lastLaunchAt = time.Time{}
	}
	if s.stopped {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrStopped, s.lastError)
	}
	s.ensuring = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.ensuring = false
		s.mu.Unlock()
		s.publish()
	}()

	return s.ensureLocked(ctx, reason)
}

func (s *Supervisor) ensureLocked(ctx context.Context, reason string) error {
	now := s.now()

	// 1. A live head is the whole goal. Everything below only runs when the
	//    answer is no.
	if head, ok := s.liveHead(); ok {
		s.onLive(head, now)
		return nil
	}

	s.mu.Lock()
	s.headJobID, s.headJobFile = "", ""
	firstLive := s.firstLiveAt
	lastLaunch := s.lastLaunchAt
	s.firstLiveAt = time.Time{}
	nextAttempt := s.nextAttemptAt
	s.mu.Unlock()

	// 2. A launch that has not had its grace period yet is not a failure. The
	//    session simply has not registered.
	if !lastLaunch.IsZero() && now.Sub(lastLaunch) < LaunchGrace {
		s.setState(models.AssistantStateStarting, "")
		return nil
	}

	// 3. A chain that came up and died again inside the fast-exit window did
	//    no work — that is the breaker's signal, exactly as a signal-cli
	//    daemon exiting within seconds of start is.
	if !firstLive.IsZero() && now.Sub(firstLive) < FastExitThreshold {
		if s.countFailure(now, fmt.Sprintf("assistant chain died %s after starting", now.Sub(firstLive).Round(time.Second))) {
			return fmt.Errorf("%w: %s", ErrStopped, s.Status().LastError)
		}
	}

	// 4. Respect the backoff window.
	if !nextAttempt.IsZero() && now.Before(nextAttempt) {
		s.setState(models.AssistantStateBackoff, "")
		return nil
	}

	s.ulog.Info("Ensuring the assistant chain").
		Field("reason", reason).
		Field("plan", s.Plan()).
		Field("plan_dir", s.PlanDir()).
		Log(ctx)

	action, err := s.continueChain(ctx)
	if err != nil {
		s.ulog.Error("Assistant continuation failed").Err(err).
			Field("action", action).
			Field("plan", s.Plan()).
			Field("event", "assistant.down").
			Log(ctx)
		if s.countFailure(s.now(), err.Error()) {
			return fmt.Errorf("%w: %s", ErrStopped, err.Error())
		}
		return err
	}

	if action == actionNone {
		// flow believes a job is running; the daemon has not seen its session
		// yet. Wait it out rather than launching a second head.
		s.setState(models.AssistantStateStarting, "")
		return nil
	}

	s.mu.Lock()
	s.restarts++
	s.lastLaunchAt = s.now()
	s.lastAction = string(action)
	s.lastActionAt = s.lastLaunchAt
	s.lastError = ""
	s.state = models.AssistantStateStarting
	s.mu.Unlock()

	s.ulog.Success("Assistant continuation launched").
		Field("action", string(action)).
		Field("plan", s.Plan()).
		Field("event", "assistant.up").
		Log(ctx)
	return nil
}

// onLive records a live head and resets the failure machinery once the chain
// has outlived the fast-exit window.
func (s *Supervisor) onLive(head Head, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headJobID = head.JobID
	s.headJobFile = head.JobFile
	s.state = models.AssistantStateLive
	if s.firstLiveAt.IsZero() {
		s.firstLiveAt = now
	}
	if now.Sub(s.firstLiveAt) >= FastExitThreshold {
		// Long enough to count as a real run: forgive the past.
		s.fastFailures = 0
		s.backoff = InitialBackoff
		s.nextAttemptAt = time.Time{}
		s.lastError = ""
	}
	// A live head means the last launch did its job.
	s.lastLaunchAt = time.Time{}
}

// countFailure records a failed or fast-exiting continuation, advances the
// backoff and trips the breaker at the ceiling. It returns true when the
// breaker tripped.
func (s *Supervisor) countFailure(now time.Time, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.fastFailures++
	s.lastError = reason
	s.lastLaunchAt = time.Time{}
	s.firstLiveAt = time.Time{}

	if s.fastFailures >= MaxFastFailures {
		s.stopped = true
		s.state = models.AssistantStateStopped
		s.ulog.Error("Assistant failing repeatedly; supervision stopped").
			Field("event", "assistant.stopped").
			Field("consecutive_failures", s.fastFailures).
			Field("restart_count", s.restarts).
			Field("reason", reason).
			StructuredOnly().
			Log(context.Background())
		return true
	}

	s.nextAttemptAt = now.Add(s.backoff)
	s.state = models.AssistantStateBackoff
	s.backoff *= 2
	if s.backoff > MaxBackoff {
		s.backoff = MaxBackoff
	}
	return false
}

func (s *Supervisor) setState(state, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	if lastErr != "" {
		s.lastError = lastErr
	}
}

func (s *Supervisor) liveHead() (Head, bool) {
	if s.LiveHead == nil {
		return Head{}, false
	}
	return s.LiveHead()
}

// action names a continuation rung.
type action string

const (
	actionNone       action = "none"
	actionResume     action = "resume"
	actionRetry      action = "retry"
	actionChainReset action = "chain_reset"
)

// continueChain picks and performs the cheapest continuation (spec §3.3):
//
//  1. orphaned/interrupted  → flow plan resume (pi resumes in place: same
//     session uuid, same transcript, same context)
//  2. failed/pending        → flow plan retry -r
//  3. running               → nothing; `flow status` PID-verifies running jobs
//     (job_verify.go), so a job that STILL reads running
//     after that pass is believed alive, and launching a
//     second head would fork the chain
//  4. anything else, chain  → flow plan resume (continue in place; the agent's
//     not exhausted            own handoff monitor hands off when it fills up)
//  5. exhausted or no job   → chain reset: flow plan add --json + flow plan
//     run --yes --background, seeded from the last
//     handoff spec plus the memory-dir pointer
//
// idle and pending_user deliberately land on rung 4, not rung 3. flow verifies
// only `running` jobs, so an idle job whose agent died in a reboot reads idle
// forever; treating that as "believed alive" would leave the assistant
// permanently down in the most ordinary failure there is. We only reach this
// function when the daemon session store — the same authority the rail pane
// reads — has already said no live session exists.
func (s *Supervisor) continueChain(ctx context.Context) (action, error) {
	jobs, err := s.flow.PlanJobs(ctx, s.PlanDir())
	if err != nil {
		return actionNone, fmt.Errorf("read assistant plan: %w", err)
	}

	head, ok := chainHead(jobs)
	if !ok {
		return s.chainReset(ctx, nil)
	}

	switch strings.ToLower(head.Status) {
	case "orphaned", "interrupted":
		if err := s.resumeOrRetry(ctx, head); err != nil {
			return actionResume, err
		}
		s.reclaw(ctx, head)
		return actionResume, nil

	case "failed", "pending", "blocked", "needs_review":
		if err := s.flow.Retry(ctx, head.FilePath); err != nil {
			return actionRetry, err
		}
		s.reclaw(ctx, head)
		return actionRetry, nil

	case "running":
		// Not ours to touch: `flow status` PID-verifies running jobs, so one
		// that still reads running really is believed alive.
		return actionNone, nil

	case "hold":
		// An operator deliberately parked this chain. Ensuring it back to life
		// would be the supervisor overruling a human.
		return actionNone, nil

	default: // completed, idle, pending_user, abandoned, todo…
		if s.exhausted(head) {
			return s.chainReset(ctx, &head)
		}
		if err := s.resumeOrRetry(ctx, head); err != nil {
			return actionResume, err
		}
		s.reclaw(ctx, head)
		return actionResume, nil
	}
}

// resumeOrRetry prefers resume — it is the only continuation that keeps the
// transcript and the context — and falls back to retry when flow refuses.
// `flow plan resume` gates on status: completed jobs resume, a crashed job in
// some other status does not, and the fallback is what keeps the crash case
// from dead-ending on that gate.
func (s *Supervisor) resumeOrRetry(ctx context.Context, head Job) error {
	resumeErr := s.flow.Resume(ctx, head.FilePath)
	if resumeErr == nil {
		return nil
	}
	s.ulog.Warn("Assistant resume refused; falling back to retry").
		Err(resumeErr).
		Field("job", jobFileName(head)).
		Log(ctx)
	if err := s.flow.Retry(ctx, head.FilePath); err != nil {
		return fmt.Errorf("resume failed (%v) and retry failed: %w", resumeErr, err)
	}
	return nil
}

// exhausted reports whether head sits at the end of its handoff budget, which
// is the condition a chain reset exists for: the bound stays a real bound on a
// runaway chain, while a supervised, logged, rate-limited reset keeps the
// assistant itself immortal.
func (s *Supervisor) exhausted(head Job) bool {
	max := head.HandoffMax
	if max <= 0 {
		max = s.res().cfg.HandoffMax
	}
	if max <= 0 {
		return false
	}
	return head.HandoffDepth >= max
}

// chainReset creates and launches a fresh root job, seeded from the previous
// chain's last handoff spec plus a pointer to the assistant memory directory.
func (s *Supervisor) chainReset(ctx context.Context, predecessor *Job) (action, error) {
	if err := s.checkResetBudget(); err != nil {
		return actionChainReset, err
	}

	r := s.res()
	prompt := s.seedPrompt(predecessor)
	added, err := s.flow.AddJob(ctx, AddJobRequest{
		PlanDir:          r.planDir,
		Title:            r.cfg.Plan,
		Provider:         r.cfg.Provider,
		Model:            r.cfg.Model,
		Skills:           r.cfg.Skills,
		CoordMode:        DefaultCoordMode,
		HandoffThreshold: r.cfg.HandoffThreshold,
		HandoffMax:       r.cfg.HandoffMax,
		Prompt:           prompt,
	})
	if err != nil {
		return actionChainReset, fmt.Errorf("create assistant root job: %w", err)
	}
	if err := s.flow.Run(ctx, added.Path); err != nil {
		return actionChainReset, fmt.Errorf("launch assistant root job %s: %w", added.Path, err)
	}

	s.mu.Lock()
	s.chainResets++
	s.resetTimes = append(s.resetTimes, s.now())
	s.mu.Unlock()

	s.reclaw(ctx, Job{FilePath: added.Path, Filename: filepathBase(added.Path)})
	return actionChainReset, nil
}

// checkResetBudget enforces the rolling chain-reset rate limit. Burning through
// it is not a transient failure — it means every fresh chain is dying — so it
// is reported as an error, which the caller counts toward the breaker.
func (s *Supervisor) checkResetBudget() error {
	// Resolved BEFORE the lock: resolveNow publishes its state transition
	// under s.mu, so reading the config from inside the locked section could
	// deadlock the very first pass.
	maxResets := s.res().cfg.MaxChainResetsPerDay

	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := s.now().Add(-ChainResetWindow)
	kept := s.resetTimes[:0]
	for _, t := range s.resetTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.resetTimes = kept

	if len(s.resetTimes) >= maxResets {
		return fmt.Errorf("chain-reset budget exhausted: %d resets in the last %s (max %d)",
			len(s.resetTimes), ChainResetWindow, maxResets)
	}
	return nil
}

// reclaw re-applies the channel and autonomous flags after a (re)launch. The
// channels manager auto-unclaws on session end, so without this every handoff
// and every restart would silently drop the assistant off Signal. Best-effort:
// a claw failure must not undo a successful launch, so it is logged and the
// continuation still counts as a success.
func (s *Supervisor) reclaw(ctx context.Context, head Job) {
	r := s.res()
	if r.cfg.Channel == "" {
		return
	}
	jobFile := jobFileName(head)
	if jobFile == "" {
		return
	}
	if err := s.flow.Claw(ctx, r.planDir, jobFile, r.cfg.Channel, r.cfg.SignalTarget, r.cfg.IdleMinutes); err != nil {
		s.ulog.Warn("Failed to re-claw the assistant after launch").
			Err(err).
			Field("job", jobFile).
			Field("channel", r.cfg.Channel).
			Log(ctx)
		return
	}
	s.ulog.Info("Re-clawed the assistant").
		Field("job", jobFile).
		Field("channel", r.cfg.Channel).
		Log(ctx)
}

// chainHead returns the newest interactive_agent job in the plan: the tail of
// the handoff chain. Jobs are ordered by their numeric filename prefix, which
// is what `flow plan add` increments for every successor, with handoff depth
// and update time as tie-breakers.
func chainHead(jobs []Job) (Job, bool) {
	agents := make([]Job, 0, len(jobs))
	for _, j := range jobs {
		if j.Type == "interactive_agent" {
			agents = append(agents, j)
		}
	}
	if len(agents) == 0 {
		return Job{}, false
	}
	sort.SliceStable(agents, func(i, k int) bool {
		if agents[i].HandoffDepth != agents[k].HandoffDepth {
			return agents[i].HandoffDepth < agents[k].HandoffDepth
		}
		fi, fk := jobFileName(agents[i]), jobFileName(agents[k])
		if fi != fk {
			return fi < fk
		}
		return agents[i].UpdatedAt.Before(agents[k].UpdatedAt)
	})
	return agents[len(agents)-1], true
}
