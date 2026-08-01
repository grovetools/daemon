package channels

import (
	"context"
	"fmt"
	"time"

	"github.com/grovetools/notify/pkg/channels"
)

// Ensure-on-inbound (assistant-pane spec §3.4).
//
// A text arriving for an assistant that is not currently up is not an error and
// not junk — it is mail, and the whole promise of a STANDING claw is that mail
// waits for the recipient rather than bouncing. So inbound that resolves to the
// ecosystem's assistant while no claw is registered pokes the supervisor's
// ensure, parks the message in a small bounded buffer, and delivers it the
// moment the assistant re-registers its claw.
//
// The bounds are deliberate and small. A buffer that held minutes of traffic
// would turn "the assistant is starting" into "the assistant silently ate your
// afternoon"; the sender gets a Signal reply on every path that does not end in
// delivery.
const (
	// AssistantQueueMax is how many messages may wait at once. Past this the
	// NEWEST message is refused rather than the oldest evicted: the oldest is
	// the one that triggered the launch and is closest to being delivered, and
	// refusing the newest lets us tell its sender so immediately.
	AssistantQueueMax = 5

	// AssistantQueueTTL bounds the wait for the assistant to attach. It matches
	// the supervisor's own LaunchGrace: that is the window in which a launch
	// either registers a live session or is counted a failure, so waiting
	// longer would only queue behind a launch the supervisor has already
	// given up on.
	AssistantQueueTTL = 90 * time.Second

	// assistantAttachPoll is how often the flush re-reads the claw registration
	// while waiting. The registration is a state.json write by another daemon,
	// so there is nothing to subscribe to; a two-second read of a small file for
	// at most 90 seconds after an inbound message is not a cost worth avoiding.
	assistantAttachPoll = 2 * time.Second
)

// queuedInbound is one parked message plus everything needed to deliver it, or
// to apologise for it, later.
type queuedInbound struct {
	msg  channels.InboundMessage
	text string
	at   time.Time
}

// queueForAssistant parks msg for the assistant and starts (or joins) the flush
// that will deliver it. It never blocks the inbound path: the supervisor poke
// and the wait both happen on the flush goroutine.
func (m *Manager) queueForAssistant(msg channels.InboundMessage, text, reason string) {
	m.mu.Lock()
	if len(m.assistantQueue) >= AssistantQueueMax {
		m.mu.Unlock()
		m.ulog.Warn("Inbound refused — the assistant's queue is full").
			Field("queued", AssistantQueueMax).
			Field("sender", msg.Source).
			Log(m.ctx)
		m.recordInbound(msg.Source, "assistant_queue_full", "", "assistant queue full", false)
		m.replyOverSignal(msg.Source, msg.GroupID,
			fmt.Sprintf("Assistant is still starting and %d messages are already waiting — your message was not queued. Try again shortly.", AssistantQueueMax))
		return
	}
	m.assistantQueue = append(m.assistantQueue, queuedInbound{msg: msg, text: text, at: time.Now()})
	start := !m.assistantFlushing
	if start {
		m.assistantFlushing = true
	}
	depth := len(m.assistantQueue)
	m.mu.Unlock()

	m.ulog.Info("Inbound queued for the assistant").
		Field("reason", reason).
		Field("sender", msg.Source).
		Field("queued", depth).
		Log(m.ctx)
	m.recordInbound(msg.Source, "assistant_queued", "", "", false)

	if start {
		go m.flushAssistantQueue(reason)
	}
}

// flushAssistantQueue owns the queue until it is empty. The outer loop closes a
// race rather than being decorative: a message can arrive between a drain and
// the flag being cleared, and without the re-check it would sit in the buffer
// with nobody coming for it.
func (m *Manager) flushAssistantQueue(reason string) {
	for {
		m.runAssistantFlush(reason)

		m.mu.Lock()
		if len(m.assistantQueue) == 0 {
			m.assistantFlushing = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
	}
}

// runAssistantFlush performs one ensure-and-wait cycle. It always empties the
// queue: every message either gets delivered or gets its sender an answer.
func (m *Manager) runAssistantFlush(reason string) {
	ctx := m.flushContext()

	// A claw that is already live means the assistant came back on its own
	// between the enqueue and here. Nothing to ensure.
	if jobID, ok := m.liveDefaultClaw(); ok {
		m.deliverQueued(ctx, jobID)
		return
	}

	if err := m.ensureAssistant(ctx, reason); err != nil {
		m.ulog.Error("Assistant ensure failed for queued inbound").Err(err).
			Field("reason", reason).Log(ctx)
		m.failQueued(fmt.Sprintf("assistant could not be started: %v", err))
		return
	}

	deadline := time.Now().Add(AssistantQueueTTL)
	for {
		if jobID, ok := m.liveDefaultClaw(); ok {
			m.deliverQueued(ctx, jobID)
			return
		}
		if !time.Now().Before(deadline) {
			m.failQueued(fmt.Sprintf("assistant did not attach within %s", AssistantQueueTTL))
			return
		}
		select {
		case <-ctx.Done():
			m.failQueued("daemon is shutting down")
			return
		case <-time.After(assistantAttachPoll):
		}
	}
}

// flushContext returns the manager's lifetime context, falling back to a
// background one so a Manager built without Start (as in tests) still flushes.
func (m *Manager) flushContext() context.Context {
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// takeQueued removes and returns everything waiting.
func (m *Manager) takeQueued() []queuedInbound {
	m.mu.Lock()
	defer m.mu.Unlock()
	queued := m.assistantQueue
	m.assistantQueue = nil
	return queued
}

// deliverQueued hands every parked message to the now-registered claw, oldest
// first so a multi-message thought arrives in the order it was typed.
//
// A message that expired while waiting is NOT delivered: an assistant reading
// "what's the state of the worktrees?" long after the sender gave up and asked
// somewhere else is worse than an honest miss, and the reply says so.
func (m *Manager) deliverQueued(ctx context.Context, jobID string) {
	for _, q := range m.takeQueued() {
		if time.Since(q.at) > AssistantQueueTTL {
			m.ulog.Warn("Queued inbound expired before the assistant attached").
				Field("sender", q.msg.Source).
				Field("waited", time.Since(q.at).Round(time.Second).String()).
				Log(ctx)
			m.recordInbound(q.msg.Source, "assistant_queue_expired", jobID, "queued message expired", false)
			m.replyOverSignal(q.msg.Source, q.msg.GroupID,
				"Assistant took too long to start — your earlier message was not delivered. Send it again.")
			continue
		}
		m.ulog.Info("Delivering queued inbound to the assistant").
			Field("job_id", jobID).
			Field("sender", q.msg.Source).
			Field("waited", time.Since(q.at).Round(time.Second).String()).
			Log(ctx)
		m.deliverInbound(ctx, jobID, "assistant_queued", q.msg, q.text, true)
	}
}

// failQueued drops everything waiting and tells each sender why. Answering is
// the point: the alternative is a phone that got no reply and no error, which
// is indistinguishable from an assistant that read the message and ignored it.
func (m *Manager) failQueued(reason string) {
	for _, q := range m.takeQueued() {
		m.recordInbound(q.msg.Source, "assistant_queue_failed", "", reason, false)
		m.replyOverSignal(q.msg.Source, q.msg.GroupID,
			"Assistant unavailable — "+reason+". Your message was not delivered.")
	}
}

// liveDefaultClaw returns the assistant job currently holding the default claw,
// and is the queue's attach signal: the supervisor re-claws after every
// (re)launch, so a claw appearing here means a new assistant session is up and
// reachable — with nothing to subscribe to and no polling of the supervisor.
//
// Two facts have to agree, the registration in state.json and the live active
// set. Requiring both is what makes a fossil registration — a daemon killed
// before it could unregister — heal into the ensure path instead of addressing
// mail to a session that no longer exists.
func (m *Manager) liveDefaultClaw() (string, bool) {
	claw := LoadDefaultClaw()
	if claw == nil || claw.JobID == "" {
		return "", false
	}
	if !m.getActiveSessionIDs()[claw.JobID] {
		return "", false
	}
	return claw.JobID, true
}

// replyOverSignal sends a one-off message back to a sender. Only the daemon
// that owns signal-cli runs the inbound cascade, so the channel is local here
// by construction; a nil channel means signal is down, and there is nothing
// useful to do about that on this path.
func (m *Manager) replyOverSignal(recipient, groupID, message string) {
	m.mu.Lock()
	ch := m.signalChannel
	m.mu.Unlock()
	if ch == nil {
		return
	}
	_, _ = ch.Send(context.Background(), channels.OutboundMessage{
		Recipient: recipient,
		GroupID:   groupID,
		Message:   message,
	})
}
