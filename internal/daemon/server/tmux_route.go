package server

import (
	"context"
	"fmt"

	"github.com/grovetools/core/pkg/models"
	muxpkg "github.com/grovetools/core/pkg/mux"
)

// tmuxEngineForSocket builds a tmux engine bound to a specific server socket
// ("" = tmux's default server). It is a package var so routing tests can run
// without a tmux binary.
var tmuxEngineForSocket = func(socket string) (muxpkg.MuxEngine, error) {
	engine, err := muxpkg.NewTmuxEngineWithSocket(socket)
	if err != nil {
		return nil, err
	}
	return engine, nil
}

// resolveTmuxSocket returns the tmux server socket to address a genuinely
// tmux-hosted session on. There is no per-session socket on the wire, so the
// only socket groved can know is its own GROVE_TMUX_SOCKET — the isolated
// server a tend harness (or an operator) put both the daemon and its agents
// on. Empty means tmux's default server, where an unadorned `flow agent`
// launch puts its panes.
func resolveTmuxSocket(_ *models.Session) string {
	return muxpkg.GetTmuxSocketPath()
}

// resolveTmuxRoute prepares the send path for a session routed to the tmux
// tier: an engine pinned to the right tmux server, plus proof that the
// recorded target is a live pane.
//
// Both halves matter. Auto-detection (agentstream's default) resolves the mux
// from the *daemon's* environment and cheerfully hands back a tuimux engine,
// which then reports "execute: session not found" naming a tmux target tuimux
// never owned. And a session whose only routing datum is a synthesized tmux
// target — the shape `flow agent claw` used to stamp onto every agent,
// treemux-hosted ones included — must say so, rather than surfacing a bare 500
// to whoever forwarded the message.
func resolveTmuxRoute(ctx context.Context, session *models.Session) (muxpkg.MuxEngine, string, error) {
	if session.TmuxTarget == "" {
		return nil, "", fmt.Errorf("tmux target missing for session %s", session.ID)
	}
	socket := resolveTmuxSocket(session)
	engine, err := tmuxEngineForSocket(socket)
	if err != nil {
		return nil, socket, fmt.Errorf("tmux not available for session %s (socket %q): %w", session.ID, tmuxSocketLabel(socket), err)
	}
	exists, err := engine.PaneExists(ctx, session.TmuxTarget)
	if err != nil {
		return nil, socket, fmt.Errorf("could not verify tmux pane %q for session %s (socket %q): %w",
			session.TmuxTarget, session.ID, tmuxSocketLabel(socket), err)
	}
	if !exists {
		return nil, socket, fmt.Errorf(
			"session %s is recorded as tmux-hosted at %q but no such pane exists on tmux socket %q; "+
				"the agent is most likely hosted on an out-of-process PTY (treemux/tuimux) whose pty_id was never recorded, "+
				"so the recorded target is a synthesized name rather than a real route — "+
				"re-run `flow agent claw` against the running agent to re-record its delivery route",
			session.ID, session.TmuxTarget, tmuxSocketLabel(socket))
	}
	return engine, socket, nil
}

// tmuxSocketLabel names the default tmux server explicitly; an empty string in
// an error message reads as "the socket is missing" when it means the opposite.
func tmuxSocketLabel(socket string) string {
	if socket == "" {
		return "default"
	}
	return socket
}
