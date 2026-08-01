package channels

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/daemon/internal/daemon/store"
	notifychannels "github.com/grovetools/notify/pkg/channels"
)

// fakeChannel captures outbound replies so a test can assert what a sender was
// told when their message did not get through.
type fakeChannel struct {
	mu   sync.Mutex
	sent []notifychannels.OutboundMessage
}

func (f *fakeChannel) Name() string { return "signal" }
func (f *fakeChannel) Start(context.Context, func(notifychannels.InboundMessage)) error {
	return nil
}
func (f *fakeChannel) Send(_ context.Context, req notifychannels.OutboundMessage) (*notifychannels.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, req)
	return &notifychannels.SendResult{Timestamp: 1}, nil
}
func (f *fakeChannel) Stop(context.Context) error { return nil }
func (f *fakeChannel) Status() notifychannels.ChannelStatus {
	return notifychannels.ChannelStatus{IsAlive: true}
}
func (f *fakeChannel) messages() []notifychannels.OutboundMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notifychannels.OutboundMessage(nil), f.sent...)
}
func (f *fakeChannel) contains(substr string) bool {
	for _, m := range f.messages() {
		if strings.Contains(m.Message, substr) {
			return true
		}
	}
	return false
}

// delivery records what SendInput was asked to inject.
type delivery struct {
	jobID string
	input string
}

type inboundHarness struct {
	m       *Manager
	channel *fakeChannel

	mu        sync.Mutex
	delivered []delivery
	sendErr   error
}

// newInboundHarness builds a Manager wired for the inbound cascade with no
// daemon and no signal-cli: activeSessions is populated in memory (so
// lookupInboundRoute finds no cross-daemon route and delivery stays local) and
// state.json lives under a temp XDG_STATE_HOME.
func newInboundHarness(t *testing.T, activeJobs ...string) *inboundHarness {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	h := &inboundHarness{channel: &fakeChannel{}}
	m := NewManager(store.New(), SignalConfig{Enabled: true}, HAConfig{}, "", "/tmp/groved-test.sock")
	m.ctx = t.Context()
	m.signalChannel = h.channel
	for _, id := range activeJobs {
		m.activeSessions[id] = true
	}
	m.SendInput = func(_ context.Context, jobID, message string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.sendErr != nil {
			return h.sendErr
		}
		h.delivered = append(h.delivered, delivery{jobID: jobID, input: message})
		return nil
	}
	h.m = m
	return h
}

func (h *inboundHarness) deliveries() []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]delivery(nil), h.delivered...)
}

// registerEndpoint claims the ecosystem's assistant endpoint the way a scoped
// daemon does at boot. The socket matches the manager's own so ensureAssistant
// stays in-process instead of dialling anything.
func (h *inboundHarness) registerEndpoint(t *testing.T, plan string) {
	t.Helper()
	h.m.RegisterDefaultClaw(plan)
	if !LoadDefaultClaw().IsEndpoint() {
		t.Fatal("RegisterDefaultClaw wrote no endpoint")
	}
}

func inbound(text string) notifychannels.InboundMessage {
	return notifychannels.InboundMessage{Channel: "signal", Source: "+15550100", Message: text}
}

// waitFor polls cond until it holds or the deadline passes. The ensure-on-
// inbound path is asynchronous by design — the inbound cascade must never block
// on a launch — so the tests that exercise it have to wait for it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The phase-3 headline (spec §3.4): an untagged message with several claws
// active used to be dropped at the "multiple active agents" branch. With a
// default claw registered it lands on the assistant, which is the agent whose
// job is exactly the sort of thing people text in.
func TestInboundFallsBackToDefaultClaw(t *testing.T) {
	h := newInboundHarness(t, "assistant-1", "feature-claw-1", "feature-claw-2")
	h.registerEndpoint(t, "steward")
	h.m.markDefaultClawJob("assistant-1")

	h.m.handleInbound(inbound("make me a plan for the satellite retry work"))

	got := h.deliveries()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want 1 — the message was dropped instead of routed to the default claw", len(got))
	}
	if got[0].jobID != "assistant-1" {
		t.Fatalf("delivered to %q, want assistant-1", got[0].jobID)
	}
	if !strings.Contains(got[0].input, "[via Signal from ") {
		t.Fatalf("input %q lost its provenance tag — the action policy keys on it", got[0].input)
	}
	if !strings.Contains(got[0].input, "satellite retry work") {
		t.Fatalf("input %q dropped the message body", got[0].input)
	}
	if h.channel.contains("Multiple agents active") {
		t.Fatal("sender was told to disambiguate even though a default claw was registered")
	}
}

// A single active claw still wins on its own: the default-claw fallback is a
// LAST resort, and hijacking the one-agent case would break every ad-hoc claw.
func TestInboundSingleActiveStillWinsOverDefaultClaw(t *testing.T) {
	h := newInboundHarness(t, "feature-claw-1")
	h.registerEndpoint(t, "steward")
	h.m.markDefaultClawJob("assistant-1") // registered, but not currently active

	h.m.handleInbound(inbound("status?"))

	got := h.deliveries()
	if len(got) != 1 || got[0].jobID != "feature-claw-1" {
		t.Fatalf("deliveries = %+v, want the single active claw to keep the message", got)
	}
}

// An ecosystem that never opted in keeps the old behaviour exactly: several
// claws, no default, so the sender is asked to disambiguate rather than having
// their message silently steered somewhere.
func TestInboundWithoutDefaultClawStillAsksToDisambiguate(t *testing.T) {
	h := newInboundHarness(t, "feature-claw-1", "feature-claw-2")

	h.m.handleInbound(inbound("hello?"))

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("deliveries = %+v, want none", got)
	}
	if !h.channel.contains("Multiple agents active") {
		t.Fatalf("sender got %+v, want the disambiguation reply", h.channel.messages())
	}
}

// A registration whose session is gone — a daemon killed before it could
// unregister — must not be trusted as an address. The message goes to the
// ensure path instead, which is what will actually produce a live assistant.
func TestInboundIgnoresUnregisteredDefaultClaw(t *testing.T) {
	h := newInboundHarness(t, "feature-claw-1", "feature-claw-2")
	h.registerEndpoint(t, "steward")
	h.m.markDefaultClawJob("assistant-ghost") // stale: not in the active set

	ensured := make(chan string, 1)
	h.m.EnsureAssistant = func(_ context.Context, reason string) error {
		ensured <- reason
		return nil
	}

	h.m.handleInbound(inbound("what's up?"))

	select {
	case <-ensured:
	case <-time.After(5 * time.Second):
		t.Fatal("a stale default claw was treated as live; the supervisor was never poked")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("deliveries = %+v, want none — the ghost claw was addressed anyway", got)
	}
}

// Ensure-on-inbound end to end (spec §3.4): no live assistant, so the message
// is parked, the supervisor is poked, and the message is delivered the moment
// the assistant re-registers its claw — which is what the supervisor's re-claw
// does after every launch.
func TestEnsureOnInboundQueuesAndDeliversOnAttach(t *testing.T) {
	h := newInboundHarness(t)
	h.registerEndpoint(t, "steward")

	h.m.EnsureAssistant = func(context.Context, string) error {
		// Stand in for the supervisor: the launch succeeds, and the claw
		// appears a moment later, exactly as `flow agent claw` would land it.
		go func() {
			time.Sleep(50 * time.Millisecond)
			h.m.mu.Lock()
			h.m.activeSessions["assistant-1"] = true
			h.m.mu.Unlock()
			h.m.markDefaultClawJob("assistant-1")
		}()
		return nil
	}

	h.m.handleInbound(inbound("first"))
	h.m.handleInbound(inbound("second"))

	waitFor(t, "both queued messages to be delivered", func() bool { return len(h.deliveries()) == 2 })

	got := h.deliveries()
	if got[0].jobID != "assistant-1" || got[1].jobID != "assistant-1" {
		t.Fatalf("deliveries = %+v, want both on assistant-1", got)
	}
	if !strings.Contains(got[0].input, "first") || !strings.Contains(got[1].input, "second") {
		t.Fatalf("deliveries = %+v, want them in the order they were sent", got)
	}
	for _, d := range got {
		if !strings.Contains(d.input, "[via Signal from ") {
			t.Fatalf("queued delivery %q lost its provenance tag", d.input)
		}
	}
}

// A launch that fails owes the sender an explanation. Silence on a phone is
// indistinguishable from an assistant that read the message and ignored it.
func TestEnsureOnInboundRepliesWhenLaunchFails(t *testing.T) {
	h := newInboundHarness(t)
	h.registerEndpoint(t, "steward")
	h.m.EnsureAssistant = func(context.Context, string) error {
		return errors.New("chain-reset budget exhausted")
	}

	h.m.handleInbound(inbound("anyone home?"))

	waitFor(t, "the failure reply", func() bool { return h.channel.contains("chain-reset budget exhausted") })
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("deliveries = %+v, want none", got)
	}
}

// The buffer is bounded on purpose. Past the bound the NEWEST message is
// refused — the oldest is closest to delivery — and its sender is told
// immediately rather than discovering the loss by silence.
func TestEnsureOnInboundQueueIsBounded(t *testing.T) {
	h := newInboundHarness(t)
	h.registerEndpoint(t, "steward")

	release := make(chan struct{})
	h.m.EnsureAssistant = func(context.Context, string) error {
		<-release // hold the flush open so the queue actually fills
		return errors.New("never came up")
	}

	for i := 0; i < AssistantQueueMax; i++ {
		h.m.handleInbound(inbound("queued"))
	}
	waitFor(t, "the queue to fill", func() bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return len(h.m.assistantQueue) == AssistantQueueMax
	})

	h.m.handleInbound(inbound("one too many"))

	if !h.channel.contains("was not queued") {
		t.Fatalf("sender got %+v, want an over-capacity reply", h.channel.messages())
	}
	h.m.mu.Lock()
	depth := len(h.m.assistantQueue)
	h.m.mu.Unlock()
	if depth != AssistantQueueMax {
		t.Fatalf("queue depth = %d, want it capped at %d", depth, AssistantQueueMax)
	}
	close(release)
}

// Registering the endpoint is what tells the inbound cascade the ecosystem has
// an assistant at all; a claw registration is a separate, shorter-lived fact
// that must not survive the session that owned it.
func TestDefaultClawRegistrationLifecycle(t *testing.T) {
	h := newInboundHarness(t, "assistant-1")
	h.registerEndpoint(t, "steward")

	claw := LoadDefaultClaw()
	if claw.JobID != "" {
		t.Fatalf("job_id = %q, want empty before any claw registers", claw.JobID)
	}
	if claw.Plan != "steward" || claw.Socket != "/tmp/groved-test.sock" {
		t.Fatalf("endpoint = %+v, want the registering daemon's plan and socket", claw)
	}

	h.m.markDefaultClawJob("assistant-1")
	if got := LoadDefaultClaw().JobID; got != "assistant-1" {
		t.Fatalf("job_id = %q, want assistant-1", got)
	}

	// A re-registration (daemon restart across an upgrade drain) keeps the
	// claw: the assistant's PTY survives the restart, so forgetting its claw
	// would route its mail to the ensure path for nothing.
	h.m.RegisterDefaultClaw("steward")
	if got := LoadDefaultClaw().JobID; got != "assistant-1" {
		t.Fatalf("job_id = %q after re-registration, want it preserved", got)
	}

	h.m.DisableChannel(t.Context(), "assistant-1")
	after := LoadDefaultClaw()
	if after.JobID != "" {
		t.Fatalf("job_id = %q after the session ended, want it cleared", after.JobID)
	}
	if !after.IsEndpoint() {
		t.Fatal("the endpoint was retired with the claw — inbound would stop reaching ensure-on-inbound")
	}
}

// Only an ecosystem that claimed the endpoint can register a claw for it.
// Without this a daemon with no [assistant] block could write a job_id that
// nothing would ever route to and nothing would ever clean up.
func TestMarkDefaultClawJobRequiresAnEndpoint(t *testing.T) {
	h := newInboundHarness(t, "assistant-1")

	h.m.markDefaultClawJob("assistant-1")

	if claw := LoadDefaultClaw(); claw != nil {
		t.Fatalf("default claw = %+v, want none without a registered endpoint", claw)
	}
}

// A delivery failure against the registered claw is the ensure-on-inbound case
// arriving by a different door: the route is live but the agent behind it is
// not. The message must be re-queued behind a supervisor poke, not lost.
func TestUndeliverableDefaultClawRequeues(t *testing.T) {
	h := newInboundHarness(t, "assistant-1")
	h.registerEndpoint(t, "steward")
	h.m.markDefaultClawJob("assistant-1")

	h.mu.Lock()
	h.sendErr = errors.New("session not found")
	h.mu.Unlock()

	ensured := make(chan struct{}, 1)
	h.m.EnsureAssistant = func(context.Context, string) error {
		h.mu.Lock()
		h.sendErr = nil
		h.mu.Unlock()
		select {
		case ensured <- struct{}{}:
		default:
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			h.m.mu.Lock()
			h.m.activeSessions["assistant-2"] = true
			h.m.mu.Unlock()
			h.m.markDefaultClawJob("assistant-2")
		}()
		return nil
	}

	h.m.handleInbound(inbound("are you there?"))

	select {
	case <-ensured:
	case <-time.After(5 * time.Second):
		t.Fatal("an undeliverable default claw did not reach the supervisor")
	}
	waitFor(t, "the re-queued message to be delivered to the new head", func() bool {
		for _, d := range h.deliveries() {
			if d.jobID == "assistant-2" {
				return true
			}
		}
		return false
	})
	if got := LoadDefaultClaw().JobID; got == "assistant-1" {
		t.Fatal("the dead claw's registration survived its delivery failure")
	}
}
