package channels

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grovetools/daemon/internal/daemon/store"
)

// The default claw was designed as a CROSS-DAEMON rendezvous: a scoped daemon
// publishes an endpoint into state.json and the global daemon — the only one
// that runs signal-cli — forwards unresolved inbound to it. Production does not
// look like that. Production runs one unscoped daemon that owns signal-cli AND
// the assistant supervisor, so the endpoint it reads back out of state.json is
// its own, and "forwarding" would mean this process dialing itself over a unix
// socket and blocking an inbound goroutine on its own HTTP handler.
//
// The tests below pin the discriminator in both directions. The in-process
// EnsureAssistant hook is only ever invoked on the local branch, so whether it
// fires is exactly the question "did we forward or not?".

// TestDefaultClawOnThisDaemonEnsuresInProcess is the production shape: the
// registered socket is our own, so ensure happens here.
func TestDefaultClawOnThisDaemonEnsuresInProcess(t *testing.T) {
	h := newInboundHarness(t)
	h.registerEndpoint(t, "steward")

	claw := LoadDefaultClaw()
	if claw.Socket != h.m.socketPath {
		t.Fatalf("socket = %q, want this daemon's own %q", claw.Socket, h.m.socketPath)
	}

	var ensured atomic.Int32
	h.m.EnsureAssistant = func(context.Context, string) error {
		ensured.Add(1)
		// Attach the way the supervisor's re-claw would, so the flush ends
		// in a delivery rather than a timeout.
		h.m.mu.Lock()
		h.m.activeSessions["assistant-1"] = true
		h.m.mu.Unlock()
		h.m.markDefaultClawJob("assistant-1")
		return nil
	}

	h.m.handleInbound(inbound("worktree status?"))

	waitFor(t, "the queued message to be delivered in process", func() bool {
		return len(h.deliveries()) == 1
	})
	if got := ensured.Load(); got != 1 {
		t.Fatalf("in-process ensure calls = %d, want 1 — the daemon forwarded to its own socket", got)
	}
	if got := h.deliveries()[0].jobID; got != "assistant-1" {
		t.Fatalf("delivered to %q, want assistant-1", got)
	}
}

// TestDefaultClawOnAnotherDaemonForwards keeps the development shape working:
// a scoped daemon owns the assistant, so ensure crosses the hop and the local
// supervisor is never consulted. The forward fails here (nothing is listening
// on that path), which is the point — the sender learns the far daemon is
// unreachable instead of the message being quietly handled locally.
func TestDefaultClawOnAnotherDaemonForwards(t *testing.T) {
	h := newInboundHarness(t)

	// A different daemon claims the endpoint.
	other := NewManager(store.New(), SignalConfig{Enabled: true}, HAConfig{}, "",
		"/tmp/groved-some-other-daemon.sock")
	other.ctx = t.Context()
	other.RegisterDefaultClaw("steward", testEcosystem)
	if got := LoadDefaultClaw().Socket; got == h.m.socketPath {
		t.Fatalf("socket = %q, want the OTHER daemon's", got)
	}

	var ensured atomic.Int32
	h.m.EnsureAssistant = func(context.Context, string) error {
		ensured.Add(1)
		return nil
	}

	h.m.handleInbound(inbound("worktree status?"))

	waitFor(t, "the unreachable-daemon reply", func() bool {
		return h.channel.contains("unreachable") || h.channel.contains("Assistant unavailable")
	})
	if got := ensured.Load(); got != 0 {
		t.Fatalf("in-process ensure calls = %d, want 0 — a foreign endpoint was handled locally", got)
	}
}

// TestDefaultClawDeliversLocallyWhenTheInboundRouteIsOurOwn covers the delivery
// half of the same self-reference. state.json's inbound_routes can name this
// daemon (the global daemon writes its own socket there), and deliverInbound
// must recognise that and inject through SendInput rather than POSTing
// /api/sessions/.../input at itself.
func TestDefaultClawDeliversLocallyWhenTheInboundRouteIsOurOwn(t *testing.T) {
	h := newInboundHarness(t, "assistant-1")
	h.registerEndpoint(t, "steward")
	h.m.markDefaultClawJob("assistant-1")

	if err := h.m.addInboundRoute("assistant-1"); err != nil {
		t.Fatalf("addInboundRoute: %v", err)
	}
	if got, ok := h.m.lookupInboundRoute("assistant-1"); !ok || got != h.m.socketPath {
		t.Fatalf("route = %q/%v, want this daemon's own socket", got, ok)
	}

	h.m.handleInbound(inbound("plan the retry work"))

	got := h.deliveries()
	if len(got) != 1 || got[0].jobID != "assistant-1" {
		t.Fatalf("deliveries = %+v, want one local injection — the daemon forwarded to itself", got)
	}
	if !strings.Contains(got[0].input, "plan the retry work") {
		t.Fatalf("input = %q, want the message body", got[0].input)
	}
}

// TestRegisterDefaultClawRecordsTheEcosystemNotTheDaemonScope pins the field
// that makes `groved claws` and `groved health` truthful in production: the
// global daemon has no scope of its own, so recording m.scope would leave the
// record unable to say which ecosystem's assistant this is.
func TestRegisterDefaultClawRecordsTheEcosystemNotTheDaemonScope(t *testing.T) {
	h := newInboundHarness(t)
	if h.m.scope != "" {
		t.Fatalf("harness daemon scope = %q, want the unscoped/global shape", h.m.scope)
	}
	h.m.RegisterDefaultClaw("steward", testEcosystem)

	claw := LoadDefaultClaw()
	if claw.Scope != testEcosystem {
		t.Fatalf("scope = %q, want the ecosystem root %q", claw.Scope, testEcosystem)
	}
	if claw.Plan != "steward" {
		t.Fatalf("plan = %q, want steward", claw.Plan)
	}
	if claw.UpdatedAt.IsZero() || time.Since(claw.UpdatedAt) > time.Minute {
		t.Fatalf("updated_at = %v, want a fresh registration", claw.UpdatedAt)
	}
}

// TestRegisterDefaultClawRefusesSandboxEcosystems keeps tests and tend runs out
// of the host's real state.json. The daemon's own scope no longer disqualifies
// a sandbox on its own — an unscoped daemon has no scope — so the ecosystem
// path has to be checked too.
func TestRegisterDefaultClawRefusesSandboxEcosystems(t *testing.T) {
	h := newInboundHarness(t)
	h.m.RegisterDefaultClaw("steward", t.TempDir())

	if LoadDefaultClaw().IsEndpoint() {
		t.Fatal("a sandbox ecosystem published itself as the host's default claw")
	}
}
