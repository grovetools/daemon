package channels

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultClawInfo describes an ecosystem's standing claw — the assistant
// (assistant-pane spec §3.4).
//
// It lives in state.json rather than in memory because the daemon that owns
// inbound Signal and the daemon that owns the assistant are not necessarily the
// same process. Inbound is owned by the GLOBAL daemon: it is the only one that
// runs signal-cli, so it is the only one that ever executes the inbound
// cascade. state.json is the channel stack's existing cross-process rendezvous
// — the same file that already carries inbound routes — so the default claw
// travels the same way, with no new transport.
//
// BOTH deployments write this record, and Socket is what tells them apart:
//
//   - PRODUCTION — the global daemon supervises the assistant itself, so Socket
//     is its OWN socket. Every consumer compares Socket against m.socketPath
//     before forwarding, so the "default claw is me" case delivers in process
//     and never dials itself (see ensureAssistant and deliverInbound).
//   - DEVELOPMENT — a scoped daemon in an ecosystem worktree supervises the
//     assistant, and Socket names that daemon. The global daemon forwards
//     across the hop.
//
// The record has two halves with deliberately different lifetimes:
//
//   - Plan/Scope/Socket — the ENDPOINT. Written once at boot when an [assistant]
//     block is enabled. It says "this ecosystem has an assistant, and here is
//     where its supervisor listens", which is true whether or not the assistant
//     happens to be up. That is exactly the fact ensure-on-inbound needs,
//     because the case it exists for is the assistant being DOWN.
//   - JobID — the currently REGISTERED claw. Set when the assistant's job
//     enables signal, cleared when that session's channel is disabled. The
//     supervisor re-claws after every (re)launch, so this tracks the head of
//     the chain across handoffs and restarts without anyone polling.
type DefaultClawInfo struct {
	// JobID is the assistant job whose claw is registered right now. Empty
	// means the assistant is not currently reachable — the ensure-on-inbound
	// case, not an error.
	JobID string `json:"job_id,omitempty"`

	// Plan is the assistant's home plan name. Its presence is what makes this
	// record an endpoint at all.
	Plan string `json:"plan,omitempty"`

	// Scope is the ECOSYSTEM ROOT whose [assistant] block configured the
	// assistant — not the owning daemon's scope, which is empty for the
	// global daemon and would make `groved claws` print nothing useful in the
	// production deployment.
	Scope string `json:"scope,omitempty"`

	// Socket is the owning daemon's socket — where POST /api/assistant/ensure
	// goes. It equals the reader's own socket when that daemon supervises the
	// assistant itself, which is the signal to skip the hop and ensure in
	// process rather than dialing a loop back to ourselves.
	Socket string `json:"socket,omitempty"`

	// UpdatedAt is when this record last changed, for `groved claws` and for
	// telling a fresh registration from a fossil.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// IsEndpoint reports whether an ecosystem has claimed the default claw at all.
// A nil or plan-less record means no ecosystem opted in, and the inbound
// cascade must keep its pre-phase-3 behaviour: drop, and say so.
func (d *DefaultClawInfo) IsEndpoint() bool {
	return d != nil && strings.TrimSpace(d.Plan) != ""
}

// LoadDefaultClaw returns the registered default claw, or nil when no ecosystem
// has one. Read-only and cheap enough for the inbound path: one small JSON file
// that the routing code already reads on every message.
func LoadDefaultClaw() *DefaultClawInfo {
	state, err := loadChannelState()
	if err != nil {
		return nil
	}
	return state.DefaultClaw
}

// RegisterDefaultClaw publishes this daemon as the assistant endpoint of the
// ecosystem rooted at ecoScope. Called the moment the supervisor resolves what
// it supervises; plan and ecoScope both come from that resolution, so the
// record can never name a plan the supervisor is not maintaining.
//
// ecoScope is the ECOSYSTEM, not the daemon's scope. On a scoped daemon they
// are the same string; on the global daemon the daemon's scope is empty and the
// ecosystem is whatever discovery selected, which is the fact `groved claws`
// and `groved health` need to report truthfully.
//
// Registration deliberately does NOT clear a JobID left by a previous run of
// this daemon: an upgrade drain leaves the assistant's PTY and its claw alive
// across the restart, and forgetting the claw would route its mail to the
// ensure path for no reason. A JobID that really is stale costs nothing —
// routing verifies the claw is still registered before using it.
func (m *Manager) RegisterDefaultClaw(plan, ecoScope string) {
	plan = strings.TrimSpace(plan)
	ecoScope = strings.TrimSpace(ecoScope)
	// Sandboxes are checked on BOTH scopes: a tend/temp-dir ecosystem that
	// carries an [assistant] block must not publish itself into the real
	// state.json just because the daemon running it happens to be unscoped.
	if plan == "" || isSandboxPath(m.scope) || isSandboxPath(ecoScope) {
		return
	}

	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil {
		return
	}
	next := DefaultClawInfo{Plan: plan, Scope: ecoScope, Socket: m.socketPath}
	if prev := state.DefaultClaw; prev != nil {
		next.JobID = prev.JobID
		if prev.Plan == next.Plan && prev.Scope == next.Scope &&
			prev.Socket == next.Socket && prev.JobID == next.JobID {
			return
		}
	}
	next.UpdatedAt = time.Now()
	state.DefaultClaw = &next
	if err := saveStateAtomic(state); err != nil {
		m.ulog.Warn("Failed to register the default claw endpoint").Err(err).
			Field("plan", plan).Log(m.ctx)
		return
	}
	m.ulog.Info("Default claw endpoint registered").
		Field("plan", plan).
		Field("ecosystem", ecoScope).
		Field("daemon_scope", m.scope).
		Field("socket", m.socketPath).
		Log(m.ctx)
}

// markDefaultClawJob records jobID as the ecosystem's registered claw. Called
// from the claw-enable path, which is the moment the assistant becomes
// reachable — the same event the ensure-on-inbound queue waits on.
func (m *Manager) markDefaultClawJob(jobID string) {
	if jobID == "" || m.isSandboxScope() {
		return
	}

	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil {
		return
	}
	// Only an ecosystem that registered an endpoint can register a claw for it:
	// without a plan there is nothing to be the default claw OF.
	if !state.DefaultClaw.IsEndpoint() {
		return
	}
	if state.DefaultClaw.JobID == jobID {
		return
	}
	state.DefaultClaw.JobID = jobID
	state.DefaultClaw.UpdatedAt = time.Now()
	if err := saveStateAtomic(state); err != nil {
		return
	}
	m.ulog.Info("Default claw registered").
		Field("job_id", jobID).
		Field("plan", state.DefaultClaw.Plan).
		Log(m.ctx)
}

// clearDefaultClawJob forgets jobID as the registered claw, leaving the
// endpoint in place. Called on channel disable — the assistant is gone, but the
// ecosystem still HAS an assistant, which is what keeps inbound flowing into
// ensure-on-inbound instead of falling back to a drop.
func (m *Manager) clearDefaultClawJob(jobID string) {
	if jobID == "" {
		return
	}

	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := loadChannelState()
	if err != nil || state.DefaultClaw == nil || state.DefaultClaw.JobID != jobID {
		return
	}
	state.DefaultClaw.JobID = ""
	state.DefaultClaw.UpdatedAt = time.Now()
	if err := saveStateAtomic(state); err != nil {
		return
	}
	m.ulog.Info("Default claw unregistered").Field("job_id", jobID).Log(m.ctx)
}

// ensureAssistant asks the assistant supervisor for a live chain and reports
// whether it could deliver one. Two hops are possible and the record says which:
// a socket belonging to ANOTHER daemon means the supervisor is over there, and
// anything else — including our own socket — means it is in this process.
//
// That second case is the production one and the `claw.Socket != m.socketPath`
// guard is what makes it safe. The global daemon owns signal-cli AND the
// assistant, so the endpoint it reads back out of state.json is its own; a
// forward here would be this process dialing itself over a unix socket and
// blocking an inbound-path goroutine on its own HTTP handler. Delivering in
// process is both correct and the only thing that terminates.
//
// The error is the whole point of this being synchronous. Ensure-on-inbound
// promises the sender an answer either way (spec §3.4), and "the assistant
// could not be started because <reason>" is only sayable if somebody waited for
// the reason.
func (m *Manager) ensureAssistant(ctx context.Context, reason string) error {
	claw := LoadDefaultClaw()
	if claw != nil && claw.Socket != "" && claw.Socket != m.socketPath {
		return m.forwardAssistantEnsure(ctx, claw.Socket, reason)
	}
	if m.EnsureAssistant == nil {
		return fmt.Errorf("no assistant supervisor is wired on this daemon")
	}
	return m.EnsureAssistant(ctx, reason)
}

// assistantEnsureTimeout bounds one cross-daemon ensure. The supervisor shells
// out to `flow plan resume`/`run`, which does plan loading and context assembly
// before returning, so this is generous — but it is still bounded, because the
// sender is waiting on the other end of it.
const assistantEnsureTimeout = 3 * time.Minute

// forwardAssistantEnsure POSTs /api/assistant/ensure at the scoped daemon that
// owns the assistant. It is the inbound-path twin of forwardSessionInput: same
// unix-socket transport, same discipline of carrying the far daemon's own
// explanation back across the hop rather than reducing it to a status code.
//
// force is never sent. A tripped circuit breaker means the assistant is
// crash-looping, and letting an inbound text re-arm it would defeat the breaker
// with the most easily spammed input in the system.
func (m *Manager) forwardAssistantEnsure(ctx context.Context, socketPath, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, assistantEnsureTimeout)
	defer cancel()

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: assistantEnsureTimeout,
	}
	endpoint := "http://unix/api/assistant/ensure?reason=" + url.QueryEscape(reason)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("assistant daemon at %s is unreachable: %w", socketPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		explanation := strings.TrimSpace(string(detail))
		if explanation == "" {
			explanation = resp.Status
		}
		return fmt.Errorf("%s", explanation)
	}
	// The body is the supervisor's status snapshot; nothing on the inbound path
	// needs it beyond "no error", and the queue learns the assistant is really
	// back from the claw registration, not from this reply.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}
