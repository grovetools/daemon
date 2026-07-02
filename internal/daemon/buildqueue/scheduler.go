// Package buildqueue implements the daemon's machine-wide build queue: a
// FIFO queue drained by a fixed-size worker pool so that at most
// [daemon.build] max_parallel build jobs run concurrently on the host,
// no matter how many `grove build` invocations submit work.
//
// Lifecycle events (queued/started/finished) are broadcast through the
// daemon store as build_* UpdateTypes; per-job OUTPUT never touches the
// store — it streams over the dedicated per-job SSE endpoint
// (GET /api/build/jobs/{id}/stream), buffered in a ring so late
// subscribers can replay the full history.
package buildqueue

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/taskexec"
	"github.com/grovetools/daemon/internal/daemon/store"
)

const (
	// ringCapacity bounds the per-job event ring replayed to late
	// subscribers. Oldest events are dropped once exceeded.
	ringCapacity = 10000
	// subscriberBuf is the per-subscriber channel buffer. Output events
	// are dropped (not blocked on) when a subscriber falls this far
	// behind, so one slow SSE consumer can't stall a machine-wide worker.
	subscriberBuf = 4096
	// queueCapacity bounds the FIFO submit queue.
	queueCapacity = 4096
	// retireAfter is how long finished jobs stay subscribable before the
	// scheduler forgets them.
	retireAfter = 10 * time.Minute
	// finishedSendTimeout bounds the blocking delivery of the terminal
	// event to a slow subscriber before its channel is closed anyway.
	finishedSendTimeout = 5 * time.Second
)

// Job statuses.
const (
	statusQueued   = "queued"
	statusRunning  = "running"
	statusFinished = "finished"
)

// DefaultMaxParallel returns the default machine-wide build concurrency:
// max(2, NumCPU/2).
func DefaultMaxParallel() int {
	n := runtime.NumCPU() / 2
	if n < 2 {
		n = 2
	}
	return n
}

// Scheduler owns the FIFO build queue and its worker pool.
type Scheduler struct {
	maxParallel int
	queue       chan *job
	store       *store.Store
	ulog        *grovelogging.UnifiedLogger

	mu   sync.Mutex
	jobs map[string]*job
}

// job tracks one submitted build job through its lifecycle.
type job struct {
	id  string
	req models.BuildJobRequest

	mu          sync.Mutex
	status      string
	cancelled   bool               // group-cancel requested
	cancel      context.CancelFunc // set while running
	ring        *eventRing
	subscribers map[chan models.BuildJobEvent]struct{}
}

// New creates a Scheduler. maxParallel <= 0 selects DefaultMaxParallel().
// st may be nil (tests); lifecycle events are then not broadcast.
func New(st *store.Store, maxParallel int) *Scheduler {
	if maxParallel <= 0 {
		maxParallel = DefaultMaxParallel()
	}
	return &Scheduler{
		maxParallel: maxParallel,
		queue:       make(chan *job, queueCapacity),
		store:       st,
		ulog:        grovelogging.NewUnifiedLogger("groved.buildqueue"),
		jobs:        make(map[string]*job),
	}
}

// MaxParallel returns the configured machine-wide concurrency cap.
func (s *Scheduler) MaxParallel() int {
	return s.maxParallel
}

// Start launches the worker pool. Workers exit when ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	for i := 0; i < s.maxParallel; i++ {
		go s.worker(ctx)
	}
}

// Submit enqueues a build job and returns its ID. The job's "queued" event
// is emitted immediately so subscribers always see a complete lifecycle.
func (s *Scheduler) Submit(req models.BuildJobRequest) (string, error) {
	if req.Dir == "" {
		return "", fmt.Errorf("build job requires a working directory")
	}
	if len(req.Command) == 0 && req.Verb == "" {
		return "", fmt.Errorf("build job requires a command or a verb")
	}

	j := &job{
		id:          fmt.Sprintf("build-%s", uuid.New().String()[:8]),
		req:         req,
		status:      statusQueued,
		ring:        newEventRing(ringCapacity),
		subscribers: make(map[chan models.BuildJobEvent]struct{}),
	}

	s.mu.Lock()
	s.jobs[j.id] = j
	s.mu.Unlock()

	j.emit(models.BuildJobEvent{Event: models.BuildEventQueued, JobID: j.id})
	s.publishLifecycle(j, store.UpdateBuildQueued, statusQueued, 0, 0)

	select {
	case s.queue <- j:
	default:
		s.finish(j, 1, "build queue full", false, 0)
		return "", fmt.Errorf("build queue full (%d jobs)", queueCapacity)
	}

	s.ulog.Info("Build job queued").
		Field("job_id", j.id).
		Field("workspace", req.Workspace).
		Field("verb", req.Verb).
		Field("group_id", req.GroupID).
		Log(context.Background())

	return j.id, nil
}

// Cancel kills the running process groups of every job in groupID and
// drains that group's queued jobs. Returns the number of jobs affected.
func (s *Scheduler) Cancel(groupID string) int {
	s.mu.Lock()
	var targets []*job
	for _, j := range s.jobs {
		if j.req.GroupID == groupID {
			targets = append(targets, j)
		}
	}
	s.mu.Unlock()

	n := 0
	for _, j := range targets {
		j.mu.Lock()
		switch j.status {
		case statusQueued:
			// Drain: mark cancelled so the worker skips it, and finish it
			// now so subscribers unblock without waiting for a worker.
			j.cancelled = true
			j.mu.Unlock()
			s.finish(j, 1, "cancelled before start", true, 0)
			n++
		case statusRunning:
			j.cancelled = true
			cancel := j.cancel
			j.mu.Unlock()
			if cancel != nil {
				cancel() // taskexec SIGTERMs the whole process group
			}
			n++
		default:
			j.mu.Unlock()
		}
	}

	s.ulog.Info("Build group cancelled").
		Field("group_id", groupID).
		Field("jobs", n).
		Log(context.Background())

	return n
}

// Subscribe returns a job's replayed event history plus a live channel.
// The channel is closed when the job reaches a terminal state (a job that
// already finished returns its full history and an already-closed channel).
func (s *Scheduler) Subscribe(jobID string) ([]models.BuildJobEvent, <-chan models.BuildJobEvent, error) {
	s.mu.Lock()
	j, ok := s.jobs[jobID]
	s.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("build job %s not found", jobID)
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	history := j.ring.snapshot()
	ch := make(chan models.BuildJobEvent, subscriberBuf)
	if j.status == statusFinished {
		close(ch)
		return history, ch, nil
	}
	j.subscribers[ch] = struct{}{}
	return history, ch, nil
}

// Unsubscribe detaches a subscriber channel from a job. Safe to call after
// the job finished or was retired.
func (s *Scheduler) Unsubscribe(jobID string, ch <-chan models.BuildJobEvent) {
	s.mu.Lock()
	j, ok := s.jobs[jobID]
	s.mu.Unlock()
	if !ok {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for sub := range j.subscribers {
		if sub == ch {
			delete(j.subscribers, sub)
			return
		}
	}
}

func (s *Scheduler) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.queue:
			s.run(ctx, j)
		}
	}
}

// run executes one build job through taskexec, streaming output into the
// job's ring/subscribers and publishing lifecycle transitions.
func (s *Scheduler) run(ctx context.Context, j *job) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	j.mu.Lock()
	if j.status != statusQueued || j.cancelled {
		// Cancelled while queued — already finished by Cancel.
		j.mu.Unlock()
		return
	}
	j.status = statusRunning
	j.cancel = cancel
	j.mu.Unlock()

	j.emit(models.BuildJobEvent{Event: models.BuildEventStarted, JobID: j.id})
	s.publishLifecycle(j, store.UpdateBuildStarted, statusRunning, 0, 0)

	start := time.Now()
	_, err := taskexec.Run(jobCtx, taskexec.Options{
		Command: j.req.Command,
		Verb:    j.req.Verb,
		Dir:     j.req.Dir,
		Env:     j.req.Env,
		OnOutput: func(line string) {
			j.emit(models.BuildJobEvent{Event: models.BuildEventOutput, JobID: j.id, Line: line})
		},
	})
	duration := time.Since(start)

	j.mu.Lock()
	j.cancel = nil
	cancelled := j.cancelled || jobCtx.Err() != nil
	j.mu.Unlock()

	exitCode := 0
	errMsg := ""
	if err != nil {
		exitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
			exitCode = exitErr.ExitCode()
		}
		errMsg = err.Error()
	}
	if cancelled && errMsg == "" {
		errMsg = "cancelled"
	}

	s.finish(j, exitCode, errMsg, cancelled, duration)
}

// finish marks a job terminal, delivers the finished event, closes all
// subscriber channels, and schedules the job record for retirement.
func (s *Scheduler) finish(j *job, exitCode int, errMsg string, cancelled bool, duration time.Duration) {
	ev := models.BuildJobEvent{
		Event:      models.BuildEventFinished,
		JobID:      j.id,
		ExitCode:   exitCode,
		Error:      errMsg,
		Cancelled:  cancelled,
		DurationMs: duration.Milliseconds(),
	}

	j.mu.Lock()
	if j.status == statusFinished {
		j.mu.Unlock()
		return
	}
	j.status = statusFinished
	j.ring.append(ev)
	subs := make([]chan models.BuildJobEvent, 0, len(j.subscribers))
	for ch := range j.subscribers {
		subs = append(subs, ch)
	}
	j.subscribers = make(map[chan models.BuildJobEvent]struct{})
	j.mu.Unlock()

	// Deliver the terminal event with a bounded blocking send — unlike
	// output lines it must not be silently dropped, but a wedged consumer
	// can't be allowed to pin a worker goroutine forever either.
	for _, ch := range subs {
		select {
		case ch <- ev:
		case <-time.After(finishedSendTimeout):
		}
		close(ch)
	}

	status := statusFinished
	switch {
	case cancelled:
		status = "cancelled"
	case exitCode != 0:
		status = "failed"
	default:
		status = "succeeded"
	}
	s.publishLifecycle(j, store.UpdateBuildFinished, status, exitCode, duration.Milliseconds())

	s.ulog.Info("Build job finished").
		Field("job_id", j.id).
		Field("workspace", j.req.Workspace).
		Field("status", status).
		Field("exit_code", exitCode).
		Field("duration_ms", duration.Milliseconds()).
		Log(context.Background())

	// Keep the record around briefly for late subscribers, then drop it.
	time.AfterFunc(retireAfter, func() {
		s.mu.Lock()
		delete(s.jobs, j.id)
		s.mu.Unlock()
	})
}

// publishLifecycle broadcasts a build_* lifecycle transition through the
// daemon store (never output lines — those stay on the per-job stream).
func (s *Scheduler) publishLifecycle(j *job, t store.UpdateType, status string, exitCode int, durationMs int64) {
	if s.store == nil {
		return
	}
	s.store.ApplyUpdate(store.Update{
		Type:   t,
		Source: "buildqueue",
		Payload: &store.BuildEventPayload{
			JobID:      j.id,
			GroupID:    j.req.GroupID,
			Workspace:  j.req.Workspace,
			Dir:        j.req.Dir,
			Verb:       j.req.Verb,
			Status:     status,
			ExitCode:   exitCode,
			DurationMs: durationMs,
		},
	})
}

// emit appends a non-terminal event to the job's ring and fans it out to
// subscribers. Output events are dropped for subscribers whose buffer is
// full — the ring, not the channel, is the source of truth for history.
func (j *job) emit(ev models.BuildJobEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ring.append(ev)
	for ch := range j.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

// eventRing is a fixed-capacity ring of build events replayed to late
// subscribers. Appending beyond capacity overwrites the oldest event.
type eventRing struct {
	buf   []models.BuildJobEvent
	start int
	count int
}

func newEventRing(capacity int) *eventRing {
	return &eventRing{buf: make([]models.BuildJobEvent, capacity)}
}

func (r *eventRing) append(ev models.BuildJobEvent) {
	if r.count < len(r.buf) {
		r.buf[(r.start+r.count)%len(r.buf)] = ev
		r.count++
		return
	}
	r.buf[r.start] = ev
	r.start = (r.start + 1) % len(r.buf)
}

func (r *eventRing) snapshot() []models.BuildJobEvent {
	out := make([]models.BuildJobEvent, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.buf[(r.start+i)%len(r.buf)])
	}
	return out
}
