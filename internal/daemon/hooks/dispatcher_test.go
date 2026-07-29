package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// recorder stands in for the real executor so unit tests observe dispatch
// decisions without spawning shells.
type recorder struct {
	mu   sync.Mutex
	runs []recordedRun
	done chan struct{}
}

type recordedRun struct {
	hook config.HookCommand
	run  HookRun
}

func newRecorder() *recorder { return &recorder{done: make(chan struct{}, 64)} }

func (r *recorder) ExecuteHook(_ context.Context, hook config.HookCommand, run HookRun) {
	r.mu.Lock()
	r.runs = append(r.runs, recordedRun{hook: hook, run: run})
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
}

func (r *recorder) snapshot() []recordedRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRun(nil), r.runs...)
}

// waitForRuns blocks until n dispatches have been recorded, or fails.
func (r *recorder) waitForRuns(t *testing.T, n int) []recordedRun {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if got := r.snapshot(); len(got) >= n {
			return got
		}
		select {
		case <-r.done:
		case <-deadline:
			t.Fatalf("only %d hook runs after 3s, want %d", len(r.snapshot()), n)
		}
	}
}

// newTestDispatcher builds a dispatcher over a real store with a recorded
// runner. Using the real store keeps the subscribe/publish path honest — that
// is where sequence stamping and the buffered fan-out live.
func newTestDispatcher(t *testing.T, hooks []config.EventHook) (*store.Store, *Dispatcher, *recorder) {
	t.Helper()
	st := store.New()
	rec := newRecorder()
	cfg := &config.Config{Daemon: &config.DaemonConfig{Hooks: &config.DaemonHooks{OnEvent: hooks}}}
	d := NewDispatcher(st, nil, cfg, nil)
	d.runner = rec
	return st, d, rec
}

func hookFor(name string, events []string, filter string) config.EventHook {
	return config.EventHook{
		HookCommand: config.HookCommand{Name: name, Command: "true"},
		Events:      events,
		Filter:      filter,
	}
}

func TestDispatcherRunsMatchingHooksOnly(t *testing.T) {
	st, d, rec := newTestDispatcher(t, []config.EventHook{
		hookFor("jobs", []string{"job_*"}, ""),
		hookFor("notes", []string{"note_event"}, ""),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobCompleted,
		Source:  "jobrunner",
		Payload: &models.JobInfo{ID: "job-1", Status: "completed"},
	})

	runs := rec.waitForRuns(t, 1)
	if len(runs) != 1 {
		t.Fatalf("%d hooks ran, want only the matching one: %+v", len(runs), runs)
	}
	if runs[0].hook.Name != "jobs" {
		t.Fatalf("the wrong hook ran: %q", runs[0].hook.Name)
	}
	if runs[0].run.Key != "jobs" {
		t.Errorf("cancel/dedupe key = %q, want the hook name", runs[0].run.Key)
	}
}

func TestDispatcherDeliversEventOnStdinAndEnv(t *testing.T) {
	st, d, rec := newTestDispatcher(t, []config.EventHook{hookFor("jobs", []string{"job_completed"}, "")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobCompleted,
		Source:  "jobrunner",
		Payload: &models.JobInfo{ID: "job-9", PlanName: "plan-a", Status: "completed"},
	})

	run := rec.waitForRuns(t, 1)[0].run
	var ev Event
	if err := json.Unmarshal(run.Stdin, &ev); err != nil {
		t.Fatalf("stdin is not a decodable event: %v (%s)", err, run.Stdin)
	}
	if ev.Type != "job_completed" || ev.JobID != "job-9" || ev.Plan != "plan-a" {
		t.Fatalf("event on stdin = %+v", ev)
	}
	if ev.Seq == 0 {
		t.Error("the event carried no sequence number")
	}
	if run.Env["GROVE_JOB_ID"] != "job-9" || run.Env["GROVE_EVENT_TYPE"] != "job_completed" {
		t.Errorf("env = %v", run.Env)
	}
}

func TestDispatcherFilterNarrowsByField(t *testing.T) {
	st, d, rec := newTestDispatcher(t, []config.EventHook{
		hookFor("only-alpha", []string{"job_*"}, "plan=alpha"),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	st.ApplyUpdate(store.Update{Type: store.UpdateJobFailed, Payload: &models.JobInfo{ID: "j1", PlanName: "beta"}})
	st.ApplyUpdate(store.Update{Type: store.UpdateJobFailed, Payload: &models.JobInfo{ID: "j2", PlanName: "alpha"}})

	runs := rec.waitForRuns(t, 1)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1 — the filtered-out event should not have fired", len(runs))
	}
	var ev Event
	_ = json.Unmarshal(runs[0].run.Stdin, &ev)
	if ev.JobID != "j2" {
		t.Fatalf("the filter let the wrong job through: %q", ev.JobID)
	}
}

// The satellite_notify problem, generalized: the store synthesizes a terminal
// job event from every federated snapshot that shows the transition, so the
// same job_completed can arrive repeatedly. `notify-send` is not idempotent.
func TestDispatcherDedupesTerminalJobEvents(t *testing.T) {
	st, d, rec := newTestDispatcher(t, []config.EventHook{hookFor("notify", []string{"job_*"}, "")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	for i := 0; i < 3; i++ {
		st.ApplyUpdate(store.Update{
			Type:    store.UpdateJobCompleted,
			Payload: &models.JobInfo{ID: "job-dup", Status: "completed"},
		})
	}
	// A different job, and a different terminal type for the same job, are
	// distinct deliveries.
	st.ApplyUpdate(store.Update{Type: store.UpdateJobCompleted, Payload: &models.JobInfo{ID: "job-other"}})
	st.ApplyUpdate(store.Update{Type: store.UpdateJobFailed, Payload: &models.JobInfo{ID: "job-dup"}})

	runs := rec.waitForRuns(t, 3)
	time.Sleep(100 * time.Millisecond) // let any duplicate through if dedupe is broken
	runs = rec.snapshot()
	if len(runs) != 3 {
		ids := make([]string, 0, len(runs))
		for _, r := range runs {
			var ev Event
			_ = json.Unmarshal(r.run.Stdin, &ev)
			ids = append(ids, ev.Type+"/"+ev.JobID)
		}
		t.Fatalf("%d runs (%v), want 3: one per (job, terminal type)", len(runs), ids)
	}
}

// Non-terminal events legitimately repeat — two builds of the same workspace,
// a note edited twice — so dedupe must not touch them.
func TestDispatcherDoesNotDedupeNonTerminalEvents(t *testing.T) {
	st, d, rec := newTestDispatcher(t, []config.EventHook{hookFor("watch", []string{"job_started"}, "")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	for i := 0; i < 3; i++ {
		st.ApplyUpdate(store.Update{Type: store.UpdateJobStarted, Payload: &models.JobInfo{ID: "job-1"}})
	}
	runs := rec.waitForRuns(t, 3)
	if len(runs) < 3 {
		t.Fatalf("%d runs, want 3 — job_started is not terminal and must not be deduped", len(runs))
	}
}

// Two hooks watching the same terminal event must BOTH fire: dedupe is per
// hook, not global.
func TestDedupeIsPerHook(t *testing.T) {
	st, d, rec := newTestDispatcher(t, []config.EventHook{
		hookFor("a", []string{"job_completed"}, ""),
		hookFor("b", []string{"job_completed"}, ""),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	st.ApplyUpdate(store.Update{Type: store.UpdateJobCompleted, Payload: &models.JobInfo{ID: "j"}})

	runs := rec.waitForRuns(t, 2)
	names := map[string]bool{}
	for _, r := range runs {
		names[r.hook.Name] = true
	}
	if !names["a"] || !names["b"] {
		t.Fatalf("both hooks must fire for the same event, got %v", names)
	}
}

func TestDedupeSetIsBounded(t *testing.T) {
	d := &Dispatcher{deduped: make(map[string]struct{})}
	for i := 0; i < dedupeCapacity+50; i++ {
		d.alreadyDelivered("h", Event{Type: "job_completed", JobID: string(rune('a'+i%26)) + strings.Repeat("x", i%7) + itoa(i)})
	}
	if len(d.deduped) > dedupeCapacity {
		t.Fatalf("dedupe set grew to %d, want at most %d", len(d.deduped), dedupeCapacity)
	}
	if len(d.dedupeQ) != len(d.deduped) {
		t.Fatalf("eviction queue (%d) and set (%d) disagree", len(d.dedupeQ), len(d.deduped))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A config_reload must re-read the hook set. Without this a user editing
// grove.toml would have to restart the daemon for a hook to take effect —
// and the daemon is the long-lived process nobody restarts.
func TestDispatcherReloadsHooksOnConfigReload(t *testing.T) {
	st := store.New()
	rec := newRecorder()

	empty := &config.Config{Daemon: &config.DaemonConfig{Hooks: &config.DaemonHooks{}}}
	withHook := &config.Config{Daemon: &config.DaemonConfig{Hooks: &config.DaemonHooks{
		OnEvent: []config.EventHook{hookFor("late", []string{"job_completed"}, "")},
	}}}

	d := NewDispatcher(st, nil, empty, func() (*config.Config, error) { return withHook, nil })
	d.runner = rec

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	// Before the reload the hook does not exist.
	st.ApplyUpdate(store.Update{Type: store.UpdateJobCompleted, Payload: &models.JobInfo{ID: "before"}})
	time.Sleep(100 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a hook ran before it was configured: %+v", got)
	}

	st.BroadcastConfigReload("grove.toml")
	// The reload is applied by the dispatch loop; wait for it to take.
	deadline := time.Now().Add(3 * time.Second)
	for !d.HasHooks() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !d.HasHooks() {
		t.Fatal("config_reload did not arm the newly configured hook")
	}

	st.ApplyUpdate(store.Update{Type: store.UpdateJobCompleted, Payload: &models.JobInfo{ID: "after"}})
	runs := rec.waitForRuns(t, 1)
	var ev Event
	_ = json.Unmarshal(runs[0].run.Stdin, &ev)
	if ev.JobID != "after" {
		t.Fatalf("hook fired for %q, want the post-reload event", ev.JobID)
	}
}

func TestDispatcherSkipsHooksWithoutACommand(t *testing.T) {
	cfg := &config.Config{Daemon: &config.DaemonConfig{Hooks: &config.DaemonHooks{
		OnEvent: []config.EventHook{{
			HookCommand: config.HookCommand{Name: "no-command"},
			Events:      []string{"job_completed"},
		}},
	}}}
	d := NewDispatcher(store.New(), nil, cfg, nil)
	if d.HasHooks() {
		t.Fatal("a hook with no command was armed")
	}
}

// The end-to-end path: a real store update drives a real Executor, which
// spawns a real shell that reads the event off stdin. Nothing is stubbed.
func TestIntegrationStoreUpdateFiresARealHook(t *testing.T) {
	dir := t.TempDir()
	stdinFile := filepath.Join(dir, "event.json")
	envFile := filepath.Join(dir, "env.txt")
	st := store.New()
	cfg := &config.Config{Daemon: &config.DaemonConfig{Hooks: &config.DaemonHooks{
		OnEvent: []config.EventHook{{
			HookCommand: config.HookCommand{
				Name: "capture",
				// Both channels at once: the JSON payload from stdin, and the
				// GROVE_* projection from the environment.
				Command: "cat > " + stdinFile + "; echo \"$GROVE_JOB_ID $GROVE_EVENT_TYPE\" > " + envFile,
				Timeout: 10,
			},
			Events: []string{"job_*"},
			Filter: "plan=integration",
		}},
	}}}

	d := NewDispatcher(st, NewExecutor(cfg), cfg, nil)
	if !d.HasHooks() {
		t.Fatal("the hook was not armed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	// A non-matching event first: the file must not exist because of it.
	st.ApplyUpdate(store.Update{Type: store.UpdateJobCompleted, Payload: &models.JobInfo{ID: "skip", PlanName: "other"}})
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateJobCompleted,
		Source:  "jobrunner",
		Payload: &models.JobInfo{ID: "job-real", PlanName: "integration", Status: "completed"},
	})

	var stdinData, envData []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stdinData, _ = os.ReadFile(stdinFile)
		envData, _ = os.ReadFile(envFile)
		if len(stdinData) > 0 && len(envData) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(stdinData) == 0 || len(envData) == 0 {
		t.Fatalf("the hook never wrote its output files — no real process ran (stdin %q, env %q)", stdinData, envData)
	}

	var ev Event
	if err := json.Unmarshal(stdinData, &ev); err != nil {
		t.Fatalf("stdin was not the event JSON: %v (%q)", err, stdinData)
	}
	if ev.JobID != "job-real" || ev.Plan != "integration" || ev.Type != "job_completed" {
		t.Fatalf("the hook received the wrong event: %+v", ev)
	}
	if ev.Seq == 0 {
		t.Error("the delivered event carried no sequence number")
	}
	if got := strings.TrimSpace(string(envData)); got != "job-real job_completed" {
		t.Fatalf("GROVE_* environment did not reach the hook: %q", got)
	}

	// The non-matching event must not have fired the hook: exactly one
	// invocation happened, so the stdin file holds one event, not two.
	if strings.Count(string(stdinData), "\"type\"") != 1 {
		t.Fatalf("the filtered-out event also ran the hook: %q", stdinData)
	}
}
