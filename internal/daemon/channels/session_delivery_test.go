package channels

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func newDeliveryTestManager(t *testing.T, sessions ...models.Session) *Manager {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	st := store.New()
	for _, session := range sessions {
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
	m := NewManager(st, SignalConfig{}, HAConfig{}, "test-scope", "test.sock")
	// Start() would spawn watchers; the logging calls only need a context.
	m.ctx = t.Context()
	return m
}

// The regression that broke inbound claw delivery: `flow agent claw` stamped a
// synthesized tmux pane name onto a treemux-hosted agent, and recording that
// target rewrote the session's whole delivery record as {mux: tmux, pty_id: ""}
// — so after a restart the daemon ran tmux send-keys against a pane that had
// never existed. A tmux target must be additive.
func TestSaveSessionDeliveryTmuxTargetKeepsPtyRoute(t *testing.T) {
	m := newDeliveryTestManager(t, models.Session{
		ID:    "job-claw",
		Mux:   models.MuxTreemux,
		PtyID: "pty-1",
	})

	m.saveSessionDeliveryTmuxTarget("job-claw", "grovetools_assistant:job-claw")

	info := GetSessionDelivery("job-claw")
	if info == nil {
		t.Fatal("no delivery info recorded")
	}
	if info.PtyID != "pty-1" {
		t.Fatalf("pty_id = %q, want pty-1 — the live PTY route was dropped", info.PtyID)
	}
	if info.Mux != models.MuxTreemux {
		t.Fatalf("mux = %q, want treemux — the session was demoted to tmux", info.Mux)
	}
	if info.TmuxTarget != "grovetools_assistant:job-claw" {
		t.Fatalf("tmux_target = %q, want the stamped target", info.TmuxTarget)
	}
}

// With nothing else known about the session, a tmux target is the route.
func TestSaveSessionDeliveryTmuxTargetRecordsTmuxWhenNothingElseIsKnown(t *testing.T) {
	m := newDeliveryTestManager(t)

	m.saveSessionDeliveryTmuxTarget("job-tmux", "proj:job-tmux")

	info := GetSessionDelivery("job-tmux")
	if info == nil {
		t.Fatal("no delivery info recorded")
	}
	if info.Mux != models.MuxTmux || info.TmuxTarget != "proj:job-tmux" || info.PtyID != "" {
		t.Fatalf("delivery info = %+v, want a plain tmux route", *info)
	}
}

// The store may not have the session yet (claw can beat the session
// collector), but a previously recorded PTY route must still survive.
func TestSaveSessionDeliveryTmuxTargetKeepsPreviouslyRecordedPty(t *testing.T) {
	m := newDeliveryTestManager(t)
	writeSessionDelivery("job-claw", SessionDeliveryInfo{Mux: models.MuxTuimux, PtyID: "pty-9"}, false)

	m.saveSessionDeliveryTmuxTarget("job-claw", "proj:job-claw")

	info := GetSessionDelivery("job-claw")
	if info == nil {
		t.Fatal("no delivery info recorded")
	}
	if info.PtyID != "pty-9" || info.Mux != models.MuxTuimux {
		t.Fatalf("delivery info = %+v, want the recorded tuimux PTY route preserved", *info)
	}
}

// Enabling a channel is what publishes a session's route to state.json — the
// only routing datum that survives a restart, and the one a daemon whose store
// never saw the session falls back to.
func TestEnableChannelRecordsDeliveryRoute(t *testing.T) {
	m := newDeliveryTestManager(t, models.Session{
		ID:    "job-claw",
		Mux:   models.MuxTuimux,
		PtyID: "pty-1",
	})
	m.signalCfg.Enabled = true
	m.isRunning = true // don't spawn signal-cli

	if err := m.EnableChannel(t.Context(), "job-claw", "signal"); err != nil {
		t.Fatalf("EnableChannel: %v", err)
	}

	info := GetSessionDelivery("job-claw")
	if info == nil {
		t.Fatal("EnableChannel recorded no delivery info")
	}
	if info.Mux != models.MuxTuimux || info.PtyID != "pty-1" {
		t.Fatalf("delivery info = %+v, want the session's live tuimux PTY route", *info)
	}
}

func TestRemoveInboundRouteReportsOnlyTheRemovalTransition(t *testing.T) {
	m := newDeliveryTestManager(t)
	state := &ChannelState{
		InboundRoutes:   map[string]string{"job-down": "scope.sock"},
		QuoteRoutes:     map[int64]string{},
		SessionDelivery: map[string]SessionDeliveryInfo{},
	}
	if err := saveStateAtomic(state); err != nil {
		t.Fatalf("seed channel state: %v", err)
	}

	removed, err := m.removeInboundRoute("job-down")
	if err != nil || !removed {
		t.Fatalf("first removal = (%v, %v), want (true, nil)", removed, err)
	}
	removed, err = m.removeInboundRoute("job-down")
	if err != nil || removed {
		t.Fatalf("repeated removal = (%v, %v), want (false, nil)", removed, err)
	}
}

// SyncSessionDelivery heals a stale record — a PTY created or re-adopted after
// the record was written — but never invents one for a session with no channel.
func TestSyncSessionDeliveryRefreshesOnlyExistingRecords(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	SyncSessionDelivery("job-unclawed", models.MuxTreemux, "", "pty-1")
	if GetSessionDelivery("job-unclawed") != nil {
		t.Fatal("SyncSessionDelivery created a record for a session with no channel enabled")
	}

	writeSessionDelivery("job-claw", SessionDeliveryInfo{Mux: models.MuxTmux, TmuxTarget: "proj:job-claw"}, false)
	SyncSessionDelivery("job-claw", models.MuxTreemux, "proj:job-claw", "pty-1")

	info := GetSessionDelivery("job-claw")
	if info == nil {
		t.Fatal("delivery record disappeared")
	}
	if info.Mux != models.MuxTreemux || info.PtyID != "pty-1" {
		t.Fatalf("delivery info = %+v, want the live PTY route", *info)
	}
}
