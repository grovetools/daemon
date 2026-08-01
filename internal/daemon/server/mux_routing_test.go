package server

import (
	"context"
	"strings"
	"testing"

	"github.com/grovetools/core/pkg/models"
	muxpkg "github.com/grovetools/core/pkg/mux"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestEffectiveMuxPrefersLivePtyOverMuxLabel pins the routing decision that
// broke inbound claw delivery: a treemux/tuimux-hosted agent carrying a
// synthesized tmux target (and, after the old claw stamp, a delivery record
// that literally said "tmux") must still route to its PTY.
func TestEffectiveMuxPrefersLivePtyOverMuxLabel(t *testing.T) {
	tests := []struct {
		name    string
		session models.Session
		want    string
	}{
		{
			name:    "pty with poisoned tmux label routes to the pty",
			session: models.Session{PtyID: "pty-1", Mux: models.MuxTmux, TmuxTarget: "proj:job-x"},
			want:    models.MuxTreemux,
		},
		{
			name:    "tuimux-hosted pty routes to the pty tier",
			session: models.Session{PtyID: "pty-1", Mux: models.MuxTuimux},
			want:    models.MuxTreemux,
		},
		{
			name:    "tuimux label without a pty still means out-of-process",
			session: models.Session{Mux: models.MuxTuimux, TmuxTarget: "proj:job-x"},
			want:    models.MuxTreemux,
		},
		{
			name:    "treemux label",
			session: models.Session{Mux: models.MuxTreemux},
			want:    models.MuxTreemux,
		},
		{
			name:    "implicit pty inference for pre-upgrade sessions",
			session: models.Session{PtyID: "pty-1"},
			want:    models.MuxTreemux,
		},
		{
			name:    "genuine tmux session",
			session: models.Session{Mux: models.MuxTmux, TmuxTarget: "proj:job-x"},
			want:    models.MuxTmux,
		},
		{
			name:    "implicit tmux inference from a target alone",
			session: models.Session{TmuxTarget: "proj:job-x"},
			want:    models.MuxTmux,
		},
		{
			name:    "explicit none stays none",
			session: models.Session{Mux: models.MuxNone, TmuxTarget: "proj:job-x"},
			want:    models.MuxNone,
		},
		{
			name:    "nothing known",
			session: models.Session{},
			want:    models.MuxNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveMux(&tc.session); got != tc.want {
				t.Fatalf("effectiveMux = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeTmuxEngine stands in for a tmux server. The embedded interface satisfies
// the rest of MuxEngine; anything else being called would panic, which is the
// assertion.
type fakeTmuxEngine struct {
	muxpkg.MuxEngine
	paneExists bool
	sentTarget string
	sentKeys   []string
}

func (f *fakeTmuxEngine) PaneExists(_ context.Context, _ string) (bool, error) {
	return f.paneExists, nil
}

func (f *fakeTmuxEngine) SendKeys(_ context.Context, target string, keys ...string) error {
	f.sentTarget = target
	f.sentKeys = append([]string(nil), keys...)
	return nil
}

// useTmuxEngine installs a stub tmux engine for one test.
func useTmuxEngine(t *testing.T, fake *fakeTmuxEngine) {
	t.Helper()
	prev := tmuxEngineForSocket
	tmuxEngineForSocket = func(string) (muxpkg.MuxEngine, error) { return fake, nil }
	t.Cleanup(func() { tmuxEngineForSocket = prev })
}

func newRoutingTestServer(t *testing.T, st *store.Store) *Server {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := New(false)
	s.SetEngine(engine.New(st))
	return s
}

func addSession(st *store.Store, session models.Session) {
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessionIntent,
		Source: "test",
		Payload: &store.SessionIntentPayload{
			JobID:      session.ID,
			Mux:        session.Mux,
			TmuxTarget: session.TmuxTarget,
		},
	})
	if session.PtyID != "" {
		st.SetSessionPtyID(session.ID, session.PtyID)
	}
}

// A synthesized tmux target must never divert a PTY-hosted agent into
// send-keys. With no tuimux client and no connected terminal the treemux tier
// has nowhere to go — but the error proves which tier was chosen.
func TestSendSessionInputRoutesPtyHostedSessionAwayFromTmux(t *testing.T) {
	st := store.New()
	addSession(st, models.Session{
		ID:         "job-claw",
		Mux:        models.MuxTmux, // the poisoned label an old claw stamp left behind
		TmuxTarget: "grovetools_assistant:job-claw",
		PtyID:      "pty-1",
	})
	s := newRoutingTestServer(t, st)
	fake := &fakeTmuxEngine{paneExists: true}
	useTmuxEngine(t, fake)

	err := s.SendSessionInput(context.Background(), "job-claw", "hello")
	if err == nil || !strings.Contains(err.Error(), "treemux route unavailable") {
		t.Fatalf("SendSessionInput err = %v, want a treemux-tier failure", err)
	}
	if fake.sentKeys != nil {
		t.Fatalf("tmux send-keys ran for a PTY-hosted session: %v", fake.sentKeys)
	}
}

// The other half: a genuinely tmux-hosted session still delivers, through an
// engine pinned to the resolved socket rather than ambient auto-detection.
func TestSendSessionInputDeliversToLiveTmuxPane(t *testing.T) {
	st := store.New()
	addSession(st, models.Session{
		ID:         "job-tmux",
		Mux:        models.MuxTmux,
		TmuxTarget: "proj:job-tmux",
	})
	s := newRoutingTestServer(t, st)
	fake := &fakeTmuxEngine{paneExists: true}
	useTmuxEngine(t, fake)

	if err := s.SendSessionInput(context.Background(), "job-tmux", "hello"); err != nil {
		t.Fatalf("SendSessionInput: %v", err)
	}
	if fake.sentTarget != "proj:job-tmux" {
		t.Fatalf("send-keys target = %q, want proj:job-tmux", fake.sentTarget)
	}
	if len(fake.sentKeys) == 0 || fake.sentKeys[len(fake.sentKeys)-1] != "Enter" {
		t.Fatalf("send-keys keys = %v, want a trailing Enter", fake.sentKeys)
	}
}

// A session whose only routing datum is a tmux target that does not exist gets
// an explanation, not an opaque send-keys failure that reaches the operator as
// a bare 500 through the cross-daemon forward.
func TestSendSessionInputExplainsSynthesizedTmuxTarget(t *testing.T) {
	st := store.New()
	addSession(st, models.Session{
		ID:         "job-ghost",
		TmuxTarget: "grovetools_assistant:job-ghost",
	})
	s := newRoutingTestServer(t, st)
	fake := &fakeTmuxEngine{paneExists: false}
	useTmuxEngine(t, fake)

	err := s.SendSessionInput(context.Background(), "job-ghost", "hello")
	if err == nil {
		t.Fatal("SendSessionInput on a nonexistent tmux pane: want an error")
	}
	for _, want := range []string{"grovetools_assistant:job-ghost", "no such pane", "flow agent claw"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if fake.sentKeys != nil {
		t.Fatalf("send-keys ran against a pane that does not exist: %v", fake.sentKeys)
	}
}

func TestSendSessionInterruptExplainsSynthesizedTmuxTarget(t *testing.T) {
	st := store.New()
	addSession(st, models.Session{
		ID:         "job-ghost",
		Mux:        models.MuxTmux,
		TmuxTarget: "grovetools_assistant:job-ghost",
	})
	s := newRoutingTestServer(t, st)
	fake := &fakeTmuxEngine{paneExists: false}
	useTmuxEngine(t, fake)

	err := s.SendSessionInterrupt(context.Background(), "job-ghost")
	if err == nil || !strings.Contains(err.Error(), "no such pane") {
		t.Fatalf("SendSessionInterrupt err = %v, want a missing-pane explanation", err)
	}
	if fake.sentKeys != nil {
		t.Fatalf("C-c sent to a pane that does not exist: %v", fake.sentKeys)
	}
}

func TestResolveTmuxRouteRequiresATarget(t *testing.T) {
	_, _, err := resolveTmuxRoute(context.Background(), &models.Session{ID: "job-x"})
	if err == nil || !strings.Contains(err.Error(), "tmux target missing") {
		t.Fatalf("resolveTmuxRoute err = %v, want a missing-target error", err)
	}
}

func TestResolveTmuxSocketFollowsDaemonEnvironment(t *testing.T) {
	t.Setenv("GROVE_TMUX_SOCKET", "grove-tend-abc")
	if got := resolveTmuxSocket(&models.Session{}); got != "grove-tend-abc" {
		t.Fatalf("resolveTmuxSocket = %q, want grove-tend-abc", got)
	}
	t.Setenv("GROVE_TMUX_SOCKET", "")
	if got := resolveTmuxSocket(&models.Session{}); got != "" {
		t.Fatalf("resolveTmuxSocket = %q, want the default socket", got)
	}
}
