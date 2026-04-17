// Package autonomous implements the idle pinger for autonomous agent sessions.
package autonomous

import (
	"context"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/daemon/internal/daemon/store"
)

const (
	// DefaultPrompt is used when a session has no custom autonomous prompt configured.
	DefaultPrompt = "System: You have been idle. Please check for new work or summarize your status."
)

// Pinger monitors sessions and injects prompts when agents go idle.
type Pinger struct {
	store         *store.Store
	defaultPrompt string
	ulog          *logging.UnifiedLogger

	// SendInput is the function used to inject messages into tmux sessions.
	SendInput func(ctx context.Context, tmuxTarget, message string) error
}

// NewPinger creates a new idle pinger.
func NewPinger(st *store.Store, defaultPrompt string) *Pinger {
	if defaultPrompt == "" {
		defaultPrompt = DefaultPrompt
	}
	return &Pinger{
		store:         st,
		defaultPrompt: defaultPrompt,
		ulog:          logging.NewUnifiedLogger("groved.autonomous"),
	}
}

// Name returns the collector name.
func (p *Pinger) Name() string {
	return "autonomous_pinger"
}

// Run implements the collector.Collector interface. Ticks every 60 seconds.
func (p *Pinger) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.checkSessions(ctx, updates)
		}
	}
}

// checkSessions iterates all sessions and pings idle ones.
func (p *Pinger) checkSessions(ctx context.Context, updates chan<- store.Update) {
	sessions := p.store.GetSessions()
	for _, session := range sessions {
		if session.Autonomous == nil || !session.Autonomous.Enabled {
			continue
		}
		if session.Status != "running" && session.Status != "idle" {
			continue
		}
		if session.TmuxTarget == "" {
			continue
		}

		idleThreshold := time.Duration(session.Autonomous.IdleMinutes) * time.Minute
		if idleThreshold == 0 {
			continue
		}

		// Must be idle since last real activity
		if time.Since(session.LastActivity) <= idleThreshold {
			continue
		}

		// Must not have been pinged recently (prevents ping→response→ping loops)
		if session.LastIdlePingAt != nil && time.Since(*session.LastIdlePingAt) <= idleThreshold {
			continue
		}

		// Resolve prompt
		prompt := session.Autonomous.Prompt
		if prompt == "" {
			prompt = p.defaultPrompt
		}

		// Send ping
		if p.SendInput == nil {
			p.ulog.Error("SendInput not wired — idle ping dropped").
				Field("job_id", session.ID).
				Log(ctx)
			continue
		}

		p.ulog.Info("Injecting idle ping into agent").
			Field("job_id", session.ID).
			Field("tmux_target", session.TmuxTarget).
			Field("pty_id", session.PtyID).
			Field("idle_since", time.Since(session.LastActivity).String()).
			Field("prompt_len", len(prompt)).
			Log(ctx)
		if err := p.SendInput(ctx, session.TmuxTarget, prompt); err != nil {
			p.ulog.Error("Failed to send idle ping").
				Err(err).
				Field("job_id", session.ID).
				Field("tmux_target", session.TmuxTarget).
				Log(ctx)
			continue
		}

		// Record ping time (do NOT update LastActivity)
		updates <- store.Update{
			Type:   store.UpdateSessionPing,
			Source: "autonomous_pinger",
			Payload: &store.SessionPingPayload{
				JobID: session.ID,
			},
		}

		p.ulog.Success("Sent idle ping").Field("job_id", session.ID).Log(ctx)
	}
}
