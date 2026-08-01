package assistant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

// fakeFlow records every argv-equivalent call the supervisor makes and lets a
// test script the outcomes. It stands in for the whole flow CLI, which is the
// supervisor's only view of orchestration.
type fakeFlow struct {
	mu sync.Mutex

	jobs    []Job
	jobsErr error

	resumeErr error
	retryErr  error
	addErr    error
	runErr    error
	clawErr   error

	added *AddedJob

	calls []string
}

func (f *fakeFlow) record(format string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, format)
}

func (f *fakeFlow) history() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeFlow) PlanJobs(context.Context, string) ([]Job, error) {
	f.record("jobs")
	return f.jobs, f.jobsErr
}

func (f *fakeFlow) Resume(_ context.Context, jobPath string) error {
	f.record("resume " + filepath.Base(jobPath))
	return f.resumeErr
}

func (f *fakeFlow) Retry(_ context.Context, jobPath string) error {
	f.record("retry " + filepath.Base(jobPath))
	return f.retryErr
}

func (f *fakeFlow) AddJob(_ context.Context, req AddJobRequest) (*AddedJob, error) {
	f.record("add " + req.Title)
	if f.addErr != nil {
		return nil, f.addErr
	}
	if f.added != nil {
		return f.added, nil
	}
	return &AddedJob{Path: filepath.Join(req.PlanDir, "02-steward.md"), ID: "steward-new"}, nil
}

func (f *fakeFlow) Run(_ context.Context, jobPath string) error {
	f.record("run " + filepath.Base(jobPath))
	return f.runErr
}

func (f *fakeFlow) Claw(_ context.Context, _, jobFile, channel, _ string, _ int) error {
	f.record("claw " + jobFile + " " + channel)
	return f.clawErr
}

// newTestSupervisor wires an enabled supervisor over a fake flow CLI with a
// controllable clock and no live head.
func newTestSupervisor(t *testing.T, flow *fakeFlow) (*Supervisor, *time.Time) {
	t.Helper()
	planDir := t.TempDir()
	cfg := (&Config{Enabled: true, Plan: "steward", Provider: "grove-agent", HandoffMax: 20}).withDefaults()
	s := NewSupervisor(cfg, planDir, flow)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s.Clock = func() time.Time { return now }
	s.LiveHead = func() (Head, bool) { return Head{}, false }
	return s, &now
}

func agentJob(file, status string, depth int) Job {
	return Job{
		ID:           strings.TrimSuffix(file, ".md"),
		Title:        "steward",
		Status:       status,
		Type:         "interactive_agent",
		Filename:     file,
		FilePath:     filepath.Join("/plans/steward", file),
		HandoffDepth: depth,
		HandoffMax:   20,
	}
}

// TestEnsureLiveIsANoop: the whole point of an ensure-running loop is that a
// live chain costs nothing. No flow command may run.
func TestEnsureLiveIsANoop(t *testing.T) {
	flow := &fakeFlow{}
	s, _ := newTestSupervisor(t, flow)
	s.LiveHead = func() (Head, bool) { return Head{JobID: "steward-1", JobFile: "01-steward.md"}, true }

	status, err := s.Ensure(context.Background(), "test", false)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if status.State != models.AssistantStateLive {
		t.Errorf("state = %q, want live", status.State)
	}
	if status.HeadJobID != "steward-1" {
		t.Errorf("head = %q, want steward-1", status.HeadJobID)
	}
	if got := flow.history(); len(got) != 0 {
		t.Errorf("a live chain ran flow commands: %v", got)
	}
}

// TestContinuationLadder walks each rung of the spec §3.3 ordering.
func TestContinuationLadder(t *testing.T) {
	cases := []struct {
		name    string
		jobs    []Job
		want    []string
		wantAct string
	}{
		{
			name:    "orphaned resumes in place",
			jobs:    []Job{agentJob("01-steward.md", "orphaned", 0)},
			want:    []string{"jobs", "resume 01-steward.md", "claw 01-steward.md signal"},
			wantAct: "resume",
		},
		{
			name:    "interrupted resumes in place",
			jobs:    []Job{agentJob("01-steward.md", "interrupted", 0)},
			want:    []string{"jobs", "resume 01-steward.md", "claw 01-steward.md signal"},
			wantAct: "resume",
		},
		{
			name:    "failed retries",
			jobs:    []Job{agentJob("01-steward.md", "failed", 0)},
			want:    []string{"jobs", "retry 01-steward.md", "claw 01-steward.md signal"},
			wantAct: "retry",
		},
		{
			name:    "pending retries",
			jobs:    []Job{agentJob("01-steward.md", "pending", 0)},
			want:    []string{"jobs", "retry 01-steward.md", "claw 01-steward.md signal"},
			wantAct: "retry",
		},
		{
			name:    "completed but unexhausted resumes",
			jobs:    []Job{agentJob("01-steward.md", "completed", 3)},
			want:    []string{"jobs", "resume 01-steward.md", "claw 01-steward.md signal"},
			wantAct: "resume",
		},
		{
			name:    "exhausted chain resets",
			jobs:    []Job{agentJob("01-steward.md", "completed", 20)},
			want:    []string{"jobs", "add steward", "run 02-steward.md", "claw 02-steward.md signal"},
			wantAct: "chain_reset",
		},
		{
			name:    "no jobs at all resets",
			jobs:    nil,
			want:    []string{"jobs", "add steward", "run 02-steward.md", "claw 02-steward.md signal"},
			wantAct: "chain_reset",
		},
		{
			name:    "a job flow still believes is running is left alone",
			jobs:    []Job{agentJob("01-steward.md", "running", 0)},
			want:    []string{"jobs"},
			wantAct: "",
		},
		{
			// flow PID-verifies only `running` jobs, so an idle job whose
			// agent died in a reboot reads idle forever. The daemon has
			// already told us no session is live, so this must be continued.
			name:    "an idle head with no live session is continued",
			jobs:    []Job{agentJob("01-steward.md", "idle", 0)},
			want:    []string{"jobs", "resume 01-steward.md", "claw 01-steward.md signal"},
			wantAct: "resume",
		},
		{
			name:    "a held chain is left alone — an operator parked it",
			jobs:    []Job{agentJob("01-steward.md", "hold", 0)},
			want:    []string{"jobs"},
			wantAct: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flow := &fakeFlow{jobs: tc.jobs}
			s, _ := newTestSupervisor(t, flow)

			status, err := s.Ensure(context.Background(), "test", false)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			got := flow.history()
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("calls = %v, want %v", got, tc.want)
			}
			if status.LastAction != tc.wantAct {
				t.Errorf("last action = %q, want %q", status.LastAction, tc.wantAct)
			}
			if tc.wantAct == "" && status.State != models.AssistantStateStarting {
				t.Errorf("state = %q, want starting", status.State)
			}
		})
	}
}

// TestChainHeadPicksTheDeepestLink: the head of a handoff chain is its deepest
// link, not the first job in the file listing.
func TestChainHeadPicksTheDeepestLink(t *testing.T) {
	jobs := []Job{
		agentJob("01-steward.md", "completed", 0),
		{Type: "chat", Filename: "02-brainstorm.md", Status: "completed"},
		agentJob("03-steward.md", "orphaned", 2),
		agentJob("02-steward.md", "completed", 1),
	}
	head, ok := chainHead(jobs)
	if !ok {
		t.Fatal("no head found")
	}
	if head.Filename != "03-steward.md" {
		t.Errorf("head = %q, want 03-steward.md", head.Filename)
	}
}

// TestResumeFallsBackToRetry: `flow plan resume` gates on job status, so a
// crashed job it refuses must still be continued rather than dead-ending.
func TestResumeFallsBackToRetry(t *testing.T) {
	flow := &fakeFlow{
		jobs:      []Job{agentJob("01-steward.md", "orphaned", 0)},
		resumeErr: errors.New("cannot resume job: status is 'orphaned', must be 'completed'"),
	}
	s, _ := newTestSupervisor(t, flow)

	status, err := s.Ensure(context.Background(), "test", false)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := []string{"jobs", "resume 01-steward.md", "retry 01-steward.md", "claw 01-steward.md signal"}
	if got := flow.history(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if status.State != models.AssistantStateStarting {
		t.Errorf("state = %q, want starting", status.State)
	}
}

// TestReclawFailureDoesNotFailTheLaunch: a claw that cannot be re-applied is a
// Signal-reachability problem, not a reason to declare the launch failed and
// start backing off.
func TestReclawFailureDoesNotFailTheLaunch(t *testing.T) {
	flow := &fakeFlow{
		jobs:    []Job{agentJob("01-steward.md", "failed", 0)},
		clawErr: errors.New("no daemon"),
	}
	s, _ := newTestSupervisor(t, flow)

	status, err := s.Ensure(context.Background(), "test", false)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if status.LastAction != "retry" || status.RestartCount != 1 {
		t.Errorf("status = %+v, want a counted retry", status)
	}
	if status.LastError != "" {
		t.Errorf("last error = %q, want empty", status.LastError)
	}
}

// TestLaunchGraceSuppressesFailure: a session takes seconds to register, so an
// ensure that fires right after a launch must not conclude anything.
func TestLaunchGraceSuppressesFailure(t *testing.T) {
	flow := &fakeFlow{jobs: []Job{agentJob("01-steward.md", "failed", 0)}}
	s, now := newTestSupervisor(t, flow)

	if _, err := s.Ensure(context.Background(), "first", false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	before := len(flow.history())

	*now = now.Add(LaunchGrace / 2)
	status, err := s.Ensure(context.Background(), "second", false)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if status.State != models.AssistantStateStarting {
		t.Errorf("state = %q, want starting", status.State)
	}
	if status.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d, want 0 inside the launch grace", status.ConsecutiveFailures)
	}
	if len(flow.history()) != before {
		t.Errorf("a second continuation was launched inside the grace window: %v", flow.history())
	}
}

// TestBackoffAndCircuitBreaker: repeated failing continuations back off
// exponentially and eventually stop supervision permanently, surfacing a
// stopped state with the cause — never a silent loop.
func TestBackoffAndCircuitBreaker(t *testing.T) {
	flow := &fakeFlow{
		jobs:      []Job{agentJob("01-steward.md", "failed", 0)},
		retryErr:  errors.New("provider missing"),
		resumeErr: errors.New("nope"),
	}
	s, now := newTestSupervisor(t, flow)

	var status models.AssistantStatus
	backoff := InitialBackoff
	for i := 1; i < MaxFastFailures; i++ {
		var err error
		status, err = s.Ensure(context.Background(), "test", false)
		if err == nil {
			t.Fatalf("attempt %d: want a failure", i)
		}
		if status.State != models.AssistantStateBackoff {
			t.Fatalf("attempt %d: state = %q, want backoff", i, status.State)
		}
		if status.ConsecutiveFailures != i {
			t.Fatalf("attempt %d: failures = %d", i, status.ConsecutiveFailures)
		}
		if status.NextAttemptAt == nil || !status.NextAttemptAt.Equal(now.Add(backoff)) {
			t.Fatalf("attempt %d: next attempt = %v, want %v", i, status.NextAttemptAt, now.Add(backoff))
		}

		// Inside the window nothing runs.
		calls := len(flow.history())
		status, _ = s.Ensure(context.Background(), "too soon", false)
		if len(flow.history()) != calls {
			t.Fatalf("attempt %d: ran a continuation inside the backoff window", i)
		}

		backoff *= 2
		if backoff > MaxBackoff {
			backoff = MaxBackoff
		}
		*now = now.Add(backoff)
	}

	status, err := s.Ensure(context.Background(), "final", false)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("final error = %v, want ErrStopped", err)
	}
	if status.State != models.AssistantStateStopped {
		t.Fatalf("state = %q, want stopped", status.State)
	}
	if !strings.Contains(status.LastError, "provider missing") {
		t.Errorf("last error = %q, want the underlying cause", status.LastError)
	}

	// A stopped supervisor refuses further ensures and runs nothing.
	calls := len(flow.history())
	if _, err := s.Ensure(context.Background(), "again", false); !errors.Is(err, ErrStopped) {
		t.Errorf("post-stop error = %v, want ErrStopped", err)
	}
	if len(flow.history()) != calls {
		t.Error("a stopped supervisor kept running continuations")
	}

	// A FORCED ensure re-arms it — that is the operator's override.
	flow.retryErr = nil
	flow.resumeErr = nil
	*now = now.Add(time.Hour)
	status, err = s.Ensure(context.Background(), "forced", true)
	if err != nil {
		t.Fatalf("forced ensure: %v", err)
	}
	if status.State != models.AssistantStateStarting {
		t.Errorf("state after force = %q, want starting", status.State)
	}
}

// TestFastExitTripsTheBreaker: a chain that comes up and dies again inside the
// fast-exit window did no work; that is the loop the breaker exists to stop.
func TestFastExitTripsTheBreaker(t *testing.T) {
	flow := &fakeFlow{jobs: []Job{agentJob("01-steward.md", "failed", 0)}}
	s, now := newTestSupervisor(t, flow)

	live := false
	s.LiveHead = func() (Head, bool) {
		if live {
			return Head{JobID: "steward-1", JobFile: "01-steward.md"}, true
		}
		return Head{}, false
	}

	for i := 1; i < MaxFastFailures; i++ {
		// It comes up…
		live = true
		if _, err := s.Ensure(context.Background(), "up", false); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		// …and dies a minute later.
		*now = now.Add(time.Minute)
		live = false
		status, _ := s.Ensure(context.Background(), "down", false)
		if status.ConsecutiveFailures != i {
			t.Fatalf("iteration %d: failures = %d, want %d", i, status.ConsecutiveFailures, i)
		}
		*now = now.Add(MaxBackoff)
	}

	live = true
	if _, err := s.Ensure(context.Background(), "up", false); err != nil {
		t.Fatalf("final up: %v", err)
	}
	*now = now.Add(time.Minute)
	live = false
	status, err := s.Ensure(context.Background(), "down", false)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("final error = %v, want ErrStopped", err)
	}
	if status.State != models.AssistantStateStopped {
		t.Errorf("state = %q, want stopped", status.State)
	}
}

// TestLongLivedChainForgivesPastFailures: a chain that stays up past the
// fast-exit window is a real run, so the counters that were arming the breaker
// must reset.
func TestLongLivedChainForgivesPastFailures(t *testing.T) {
	flow := &fakeFlow{jobs: []Job{agentJob("01-steward.md", "failed", 0)}, retryErr: errors.New("boom")}
	s, now := newTestSupervisor(t, flow)

	if _, err := s.Ensure(context.Background(), "fail", false); err == nil {
		t.Fatal("want a failure")
	}

	flow.retryErr = nil
	s.LiveHead = func() (Head, bool) { return Head{JobID: "steward-1"}, true }
	if _, err := s.Ensure(context.Background(), "up", false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	*now = now.Add(FastExitThreshold + time.Minute)
	status, err := s.Ensure(context.Background(), "still up", false)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if status.ConsecutiveFailures != 0 || status.LastError != "" {
		t.Errorf("status = %+v, want the failure history forgiven", status)
	}
}

// TestChainResetBudget: resets are rate-limited, so an assistant whose every
// fresh chain dies escalates to a human instead of respawning forever.
func TestChainResetBudget(t *testing.T) {
	flow := &fakeFlow{jobs: []Job{agentJob("01-steward.md", "completed", 20)}}
	s, now := newTestSupervisor(t, flow)

	for i := 0; i < DefaultMaxChainResetsPerDay; i++ {
		if _, err := s.Ensure(context.Background(), "reset", false); err != nil {
			t.Fatalf("reset %d: %v", i, err)
		}
		*now = now.Add(LaunchGrace + time.Minute)
	}

	status, err := s.Ensure(context.Background(), "one too many", false)
	if err == nil || !strings.Contains(err.Error(), "chain-reset budget exhausted") {
		t.Fatalf("error = %v, want a budget refusal", err)
	}
	if status.ChainResets != DefaultMaxChainResetsPerDay {
		t.Errorf("chain resets = %d, want %d", status.ChainResets, DefaultMaxChainResetsPerDay)
	}

	// The window rolls: a day later the budget is back.
	*now = now.Add(ChainResetWindow + time.Minute)
	if _, err := s.Ensure(context.Background(), "next day", true); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// TestDisabledSupervisorDoesNothing: no [assistant] block means no supervision,
// which must be reported honestly rather than as a failure.
func TestDisabledSupervisorDoesNothing(t *testing.T) {
	flow := &fakeFlow{}
	s := NewSupervisor(&Config{}, "", flow)

	status, err := s.Ensure(context.Background(), "test", false)
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("error = %v, want ErrDisabled", err)
	}
	if status.Enabled || status.State != models.AssistantStateDisabled {
		t.Errorf("status = %+v, want a disabled report", status)
	}
	if len(flow.history()) != 0 {
		t.Errorf("a disabled supervisor ran flow commands: %v", flow.history())
	}

	// Enabled but plan-less is equally inert: a supervisor that guessed which
	// plan is the assistant could resume the wrong chain.
	s = NewSupervisor((&Config{Enabled: true}).withDefaults(), "/plans", flow)
	if s.Enabled() {
		t.Error("an [assistant] block with no plan must not activate the supervisor")
	}
}

// TestSeedPromptCarriesTheHandoffSpecAndMemoryDir: a chain reset is not a cold
// start — the new root job must arrive with its predecessor's continuation
// brief and a pointer at the memory directory.
func TestSeedPromptCarriesTheHandoffSpecAndMemoryDir(t *testing.T) {
	workspace := t.TempDir()
	planDir := filepath.Join(workspace, "plans", "steward")
	artifacts := filepath.Join(planDir, ".artifacts", "steward-old")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := "## Where I left off\n\nThe portfolio audit is half done.\n"
	if err := os.WriteFile(filepath.Join(artifacts, handoffSpecFile), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := (&Config{Enabled: true, Plan: "steward"}).withDefaults()
	s := NewSupervisor(cfg, planDir, &fakeFlow{})

	wantMemory := filepath.Join(workspace, "steward", "memory")
	if got := s.MemoryDir(); got != wantMemory {
		t.Errorf("memory dir = %q, want %q", got, wantMemory)
	}

	prompt := s.seedPrompt(&Job{ID: "steward-old", Filename: "01-steward.md"})
	if !strings.Contains(prompt, "The portfolio audit is half done.") {
		t.Errorf("seed prompt lost the handoff spec:\n%s", prompt)
	}
	if !strings.Contains(prompt, wantMemory) {
		t.Errorf("seed prompt lost the memory-dir pointer:\n%s", prompt)
	}
	if !strings.Contains(prompt, "chain reset") {
		t.Errorf("seed prompt does not say why it exists:\n%s", prompt)
	}

	// With no predecessor artifacts at all the prompt still stands on its own.
	empty := NewSupervisor(cfg, t.TempDir(), &fakeFlow{})
	if p := empty.seedPrompt(nil); !strings.Contains(p, "first job in the plan") {
		t.Errorf("cold-start seed prompt:\n%s", p)
	}
}

// TestSeedPromptFallsBackToTheNewestSpec: a predecessor killed before it could
// write its own spec still leaves the chain's last word somewhere under
// .artifacts.
func TestSeedPromptFallsBackToTheNewestSpec(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plans", "steward")
	older := filepath.Join(planDir, ".artifacts", "steward-1")
	newer := filepath.Join(planDir, ".artifacts", "steward-2")
	for _, d := range []string{older, newer} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(older, handoffSpecFile), []byte("OLDER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newer, handoffSpecFile), []byte("NEWER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(newer, handoffSpecFile), future, future); err != nil {
		t.Fatal(err)
	}

	s := NewSupervisor((&Config{Enabled: true, Plan: "steward"}).withDefaults(), planDir, &fakeFlow{})
	prompt := s.seedPrompt(&Job{ID: "steward-gone", Filename: "09-steward.md"})
	if !strings.Contains(prompt, "NEWER") {
		t.Errorf("fallback picked the wrong spec:\n%s", prompt)
	}
}

// TestPublishOnEveryEnsure: the pane and `groved health` read this snapshot, so
// every pass must push one.
func TestPublishOnEveryEnsure(t *testing.T) {
	flow := &fakeFlow{jobs: []Job{agentJob("01-steward.md", "failed", 0)}}
	s, _ := newTestSupervisor(t, flow)

	var published []models.AssistantStatus
	s.Publish = func(st models.AssistantStatus) { published = append(published, st) }

	if _, err := s.Ensure(context.Background(), "test", false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("published %d snapshots, want 1", len(published))
	}
	if published[0].Plan != "steward" || published[0].PlanDir == "" {
		t.Errorf("published status = %+v, want the plan identity", published[0])
	}
}

// TestTriggerCoalesces: a burst of triggers must collapse into one pending
// ensure — the pass is idempotent, so running it five times is pure waste.
func TestTriggerCoalesces(t *testing.T) {
	s, _ := newTestSupervisor(t, &fakeFlow{})
	for i := 0; i < 10; i++ {
		s.Trigger("burst")
	}
	if got := len(s.triggers); got != 1 {
		t.Errorf("pending triggers = %d, want 1", got)
	}
}
