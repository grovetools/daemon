package buildqueue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

// collectEvents drains a subscription (history + live channel) until the
// finished event arrives or the timeout elapses.
func collectEvents(t *testing.T, s *Scheduler, jobID string, timeout time.Duration) []models.BuildJobEvent {
	t.Helper()
	history, ch, err := s.Subscribe(jobID)
	if err != nil {
		t.Fatalf("subscribe %s: %v", jobID, err)
	}
	defer s.Unsubscribe(jobID, ch)

	events := history
	for _, ev := range history {
		if ev.Event == models.BuildEventFinished {
			return events
		}
	}
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
			if ev.Event == models.BuildEventFinished {
				return events
			}
		case <-deadline:
			t.Fatalf("timed out waiting for finished event on %s (got %d events)", jobID, len(events))
		}
	}
}

func finishedEvent(t *testing.T, events []models.BuildJobEvent) models.BuildJobEvent {
	t.Helper()
	for _, ev := range events {
		if ev.Event == models.BuildEventFinished {
			return ev
		}
	}
	t.Fatal("no finished event")
	return models.BuildJobEvent{}
}

func TestSchedulerRunsJobAndStreamsOutput(t *testing.T) {
	s := New(nil, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	jobID, err := s.Submit(models.BuildJobRequest{
		Workspace: "ws",
		Dir:       t.TempDir(),
		Command:   []string{"sh", "-c", "echo hello-build"},
		Env:       os.Environ(),
		GroupID:   "g1",
		Verb:      "build",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	events := collectEvents(t, s, jobID, 10*time.Second)

	var sawOutput bool
	for _, ev := range events {
		if ev.Event == models.BuildEventOutput && strings.Contains(ev.Line, "hello-build") {
			sawOutput = true
		}
	}
	if !sawOutput {
		t.Errorf("expected output event with hello-build, got: %+v", events)
	}
	fin := finishedEvent(t, events)
	if fin.ExitCode != 0 || fin.Cancelled {
		t.Errorf("expected clean finish, got %+v", fin)
	}
}

func TestSchedulerEnforcesConcurrencyCap(t *testing.T) {
	const maxParallel = 2
	const totalJobs = 6

	s := New(nil, maxParallel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// Each job touches a per-job "running" marker file, sleeps, then
	// removes it. Peak concurrency = max simultaneous marker files.
	dir := t.TempDir()
	var jobIDs []string
	for i := 0; i < totalJobs; i++ {
		script := fmt.Sprintf("touch %s/running-%d; sleep 0.3; rm %s/running-%d", dir, i, dir, i)
		id, err := s.Submit(models.BuildJobRequest{
			Workspace: fmt.Sprintf("ws%d", i),
			Dir:       dir,
			Command:   []string{"sh", "-c", script},
			Env:       os.Environ(),
			GroupID:   "cap-group",
			Verb:      "build",
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		jobIDs = append(jobIDs, id)
	}

	// Sample concurrency while the jobs drain.
	done := make(chan struct{})
	var mu sync.Mutex
	peak := 0
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				entries, _ := os.ReadDir(dir)
				n := 0
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "running-") {
						n++
					}
				}
				mu.Lock()
				if n > peak {
					peak = n
				}
				mu.Unlock()
			}
		}
	}()

	for _, id := range jobIDs {
		events := collectEvents(t, s, id, 20*time.Second)
		fin := finishedEvent(t, events)
		if fin.ExitCode != 0 {
			t.Errorf("job %s failed: %+v", id, fin)
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if peak > maxParallel {
		t.Errorf("concurrency cap violated: observed %d simultaneous jobs, cap is %d", peak, maxParallel)
	}
	if peak == 0 {
		t.Error("sampler never observed a running job; test is not measuring anything")
	}
}

func TestSchedulerCancelKillsRunningAndDrainsQueued(t *testing.T) {
	s := New(nil, 1) // single worker: job 2+ stay queued while job 1 runs
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	dir := t.TempDir()
	running, err := s.Submit(models.BuildJobRequest{
		Workspace: "running",
		Dir:       dir,
		Command:   []string{"sh", "-c", fmt.Sprintf("touch %s/started; sleep 30", dir)},
		Env:       os.Environ(),
		GroupID:   "cancel-group",
		Verb:      "build",
	})
	if err != nil {
		t.Fatalf("submit running: %v", err)
	}
	queued, err := s.Submit(models.BuildJobRequest{
		Workspace: "queued",
		Dir:       dir,
		Command:   []string{"sh", "-c", "echo should-never-run"},
		Env:       os.Environ(),
		GroupID:   "cancel-group",
		Verb:      "build",
	})
	if err != nil {
		t.Fatalf("submit queued: %v", err)
	}
	// A job in a DIFFERENT group must survive the cancel.
	other, err := s.Submit(models.BuildJobRequest{
		Workspace: "other",
		Dir:       dir,
		Command:   []string{"sh", "-c", "echo other-ran"},
		Env:       os.Environ(),
		GroupID:   "other-group",
		Verb:      "build",
	})
	if err != nil {
		t.Fatalf("submit other: %v", err)
	}

	// Wait for the first job's process to actually start.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(dir + "/started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first job never started")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if n := s.Cancel("cancel-group"); n != 2 {
		t.Errorf("expected 2 cancelled jobs, got %d", n)
	}

	runEvents := collectEvents(t, s, running, 15*time.Second)
	if fin := finishedEvent(t, runEvents); !fin.Cancelled {
		t.Errorf("running job should be cancelled, got %+v", fin)
	}

	queuedEvents := collectEvents(t, s, queued, 15*time.Second)
	fin := finishedEvent(t, queuedEvents)
	if !fin.Cancelled {
		t.Errorf("queued job should be cancelled, got %+v", fin)
	}
	for _, ev := range queuedEvents {
		if ev.Event == models.BuildEventStarted {
			t.Error("drained queued job must never start")
		}
	}

	otherEvents := collectEvents(t, s, other, 15*time.Second)
	if fin := finishedEvent(t, otherEvents); fin.Cancelled || fin.ExitCode != 0 {
		t.Errorf("other group's job should complete cleanly, got %+v", fin)
	}
}

func TestSubscribeAfterFinishReplaysHistory(t *testing.T) {
	s := New(nil, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	jobID, err := s.Submit(models.BuildJobRequest{
		Workspace: "ws",
		Dir:       t.TempDir(),
		Command:   []string{"sh", "-c", "echo late-subscriber-line"},
		Env:       os.Environ(),
		GroupID:   "g",
		Verb:      "build",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Let the job finish before subscribing.
	collectEvents(t, s, jobID, 10*time.Second)

	history, ch, err := s.Subscribe(jobID)
	if err != nil {
		t.Fatalf("late subscribe: %v", err)
	}
	if _, ok := <-ch; ok {
		t.Error("late subscriber channel should be closed immediately")
	}
	var sawOutput, sawFinished bool
	for _, ev := range history {
		if ev.Event == models.BuildEventOutput && strings.Contains(ev.Line, "late-subscriber-line") {
			sawOutput = true
		}
		if ev.Event == models.BuildEventFinished {
			sawFinished = true
		}
	}
	if !sawOutput || !sawFinished {
		t.Errorf("late subscriber missing history: output=%v finished=%v (%d events)", sawOutput, sawFinished, len(history))
	}
}

func TestSubmitValidation(t *testing.T) {
	s := New(nil, 1)
	if _, err := s.Submit(models.BuildJobRequest{Verb: "build"}); err == nil {
		t.Error("expected error for missing dir")
	}
	if _, err := s.Submit(models.BuildJobRequest{Dir: "/tmp"}); err == nil {
		t.Error("expected error for missing command and verb")
	}
}
