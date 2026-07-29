package hooks

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// The `[[daemon.hooks.on_event]]` dispatcher.
//
// Shape copied from server.StartSatelliteNotifier, the daemon's reference
// subscriber-with-side-effects: subscribe → filter → dedupe → act, on one
// goroutine that exits with its context. What it adds is generality — any
// store update type, any number of configured hooks — and the HookCommand
// lifecycle the skill-sync executor never honored.
//
// It deliberately does NOT use Part A's replay ring. See the comment on
// Dispatcher for why at-least-once delivery across restarts was punted.

// eventSubscriber is the slice of *store.Store the dispatcher needs, named so
// tests can drive it without a full store.
type eventSubscriber interface {
	Subscribe() chan store.Update
	Unsubscribe(chan store.Update)
}

// hookRunner is the executor seam. Tests substitute a recorder; production
// passes *Executor.
type hookRunner interface {
	ExecuteHook(ctx context.Context, hook config.HookCommand, run HookRun)
}

// dedupeCapacity bounds the terminal-event dedupe set. Terminal job events are
// the only ones deduped, so this is "how many distinct jobs can reach a
// terminal state before the oldest is forgotten" — generous for a laptop
// daemon, and bounded because the daemon is long-lived and the set would
// otherwise grow without limit.
const dedupeCapacity = 4096

// Dispatcher fans daemon lifecycle events out to configured shell hooks.
//
// AT-LEAST-ONCE DELIVERY IS NOT PROVIDED, deliberately. Part A gives clients a
// replay cursor, and it is tempting to have the dispatcher persist its cursor
// and replay across a daemon restart. It is not implemented because the ring
// is in-memory: a restart empties it AND resets the sequence, so the cursor a
// dispatcher persisted would land in the "reset" gap on every single restart
// and replay nothing. Real at-least-once needs a durable event log, which is a
// materially bigger design (retention, compaction, and a story for hooks that
// are not idempotent) and is out of scope here. What IS true today: hooks miss
// events that occur while the daemon is down, and events dropped when a
// subscriber channel overflows. Hooks that must not miss anything should
// reconcile against the REST API rather than trust the stream.
type Dispatcher struct {
	store      eventSubscriber
	runner     hookRunner
	ulog       *logging.UnifiedLogger
	now        func() time.Time
	reload     func() (*config.Config, error)
	onDispatch func(Event, string) // test seam: fires after each hook dispatch

	mu      sync.Mutex
	hooks   []compiledHook
	deduped map[string]struct{}
	dedupeQ []string
}

// compiledHook pairs a matcher with the command it triggers.
type compiledHook struct {
	matcher *Matcher
	command config.HookCommand
}

// NewDispatcher builds a dispatcher over a store and an executor. reload is
// consulted on config_reload events to pick up edited hooks without a daemon
// restart; a nil reload pins the hooks passed at construction.
func NewDispatcher(st *store.Store, executor *Executor, cfg *config.Config, reload func() (*config.Config, error)) *Dispatcher {
	d := &Dispatcher{
		store:   st,
		runner:  executor,
		ulog:    logging.NewUnifiedLogger("groved.hooks.event"),
		now:     time.Now,
		reload:  reload,
		deduped: make(map[string]struct{}),
	}
	d.SetConfig(context.Background(), cfg)
	return d
}

// SetConfig recompiles the hook set, reporting every configuration problem it
// finds. Reporting at (re)load rather than at dispatch time is the point: a
// typo'd event name or filter field is otherwise indistinguishable from "the
// event has not happened yet", which is the single most confusing failure a
// hook author can hit.
func (d *Dispatcher) SetConfig(ctx context.Context, cfg *config.Config) {
	var compiled []compiledHook
	if cfg != nil && cfg.Daemon != nil && cfg.Daemon.Hooks != nil {
		for _, hook := range cfg.Daemon.Hooks.OnEvent {
			matcher, problems := NewMatcher(hook)
			for _, problem := range problems {
				d.ulog.Warn("on_event hook configuration problem").
					Field("hook", matcher.Name).
					Field("problem", problem.Error()).
					Log(ctx)
			}
			if hook.Command == "" {
				d.ulog.Warn("on_event hook has no command; skipping").
					Field("hook", matcher.Name).Log(ctx)
				continue
			}
			if len(matcher.events) == 0 {
				continue // already reported; a hook with no events can never fire
			}
			compiled = append(compiled, compiledHook{matcher: matcher, command: hook.HookCommand})
		}
	}

	d.mu.Lock()
	d.hooks = compiled
	d.mu.Unlock()

	if len(compiled) > 0 {
		d.ulog.Info("on_event hooks armed").Field("count", len(compiled)).Log(ctx)
	}
}

// HasHooks reports whether any hook is configured. groved uses it to skip
// subscribing at all — an idle subscriber still costs a channel and a
// non-blocking send per store update.
func (d *Dispatcher) HasHooks() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.hooks) > 0
}

// Start subscribes and dispatches until ctx is cancelled. It returns
// immediately; the loop runs on its own goroutine, like StartSatelliteNotifier.
func (d *Dispatcher) Start(ctx context.Context) {
	if d.store == nil {
		return
	}
	ch := d.store.Subscribe()
	go func() {
		defer d.store.Unsubscribe(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-ch:
				if !ok {
					return
				}
				d.handle(ctx, update)
			}
		}
	}()
}

// handle dispatches one store update. Hooks run on their own goroutines: a
// hook is arbitrary user shell with a timeout measured in seconds, and
// blocking here would back the subscription's 100-deep channel up and start
// dropping events for every OTHER hook.
func (d *Dispatcher) handle(ctx context.Context, update store.Update) {
	// A config reload may have changed the hook set. Recompile before
	// dispatching the event itself, so an edited hook sees its own reload.
	if update.Type == store.UpdateConfigReload && d.reload != nil {
		if fresh, err := d.reload(); err == nil {
			d.SetConfig(ctx, fresh)
		} else {
			d.ulog.Warn("Could not reload config for on_event hooks; keeping the previous set").
				Err(err).Log(ctx)
		}
	}

	d.mu.Lock()
	hooks := d.hooks
	d.mu.Unlock()
	if len(hooks) == 0 {
		return
	}

	ev := NewEvent(update, d.now())
	payload, err := json.Marshal(ev)
	if err != nil {
		d.ulog.Warn("Could not encode event for on_event hooks").
			Field("update_type", ev.Type).Err(err).Log(ctx)
		return
	}
	env := ev.Env()

	for _, hook := range hooks {
		if !hook.matcher.Matches(ev) {
			continue
		}
		if d.alreadyDelivered(hook.matcher.Name, ev) {
			d.ulog.Debug("Suppressed a duplicate terminal event").
				Field("hook", hook.matcher.Name).
				Field("job", ev.JobID).
				Field("update_type", ev.Type).Log(ctx)
			continue
		}
		d.ulog.Debug("Dispatching on_event hook").
			Field("hook", hook.matcher.Name).
			Field("update_type", ev.Type).
			Field("seq", ev.Seq).Log(ctx)

		command, name := hook.command, hook.matcher.Name
		go d.runner.ExecuteHook(ctx, command, HookRun{Env: env, Stdin: payload, Key: name})
		if d.onDispatch != nil {
			d.onDispatch(ev, name)
		}
	}
}

// alreadyDelivered implements the terminal-job dedupe and records the
// delivery. It is the satellite_notify problem generalized: the store
// synthesizes a per-job terminal event from EVERY federated snapshot that
// shows the transition, so a job that drops out of one snapshot and reappears
// terminal in a later one fires again. Lease release is idempotent;
// `notify-send` and `curl` are not.
//
// Only terminal job events are deduped. Deduping everything would break the
// legitimate repeat cases — two builds of the same workspace, a note edited
// twice — and there is no general key for "the same event happened again".
func (d *Dispatcher) alreadyDelivered(hookName string, ev Event) bool {
	if !isTerminalJobEvent(ev.Type) || ev.JobID == "" {
		return false
	}
	key := hookName + "\x00" + ev.Type + "\x00" + ev.JobID

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, seen := d.deduped[key]; seen {
		return true
	}
	d.deduped[key] = struct{}{}
	d.dedupeQ = append(d.dedupeQ, key)
	if len(d.dedupeQ) > dedupeCapacity {
		evict := d.dedupeQ[0]
		d.dedupeQ = d.dedupeQ[1:]
		delete(d.deduped, evict)
	}
	return false
}

// isTerminalJobEvent reports whether a type is a job's final transition.
// Deliberately excludes job_orphaned, which is non-terminal by design: "the
// daemon cannot see this job" is a claim about the daemon, not a verdict on
// the work.
func isTerminalJobEvent(updateType string) bool {
	switch store.UpdateType(updateType) {
	case store.UpdateJobCompleted, store.UpdateJobFailed, store.UpdateJobCancelled:
		return true
	}
	return false
}
