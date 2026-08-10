package store

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
)

// The store used to stamp every registration "interactive_agent". A headless
// flow job therefore claimed a terminal it never had, and treemux — which
// reads Session.Type to choose between attaching a PTY and streaming the
// transcript — opened an empty shell when you clicked it.
func TestSessionIntentRecordsTheRegisteredType(t *testing.T) {
	s := newTestStore(t)
	s.ApplyUpdate(Update{
		Type:   UpdateSessionIntent,
		Source: "test",
		Payload: &SessionIntentPayload{
			JobID:    "phase0-member-audit",
			Provider: "claude",
			Mux:      models.MuxNone,
			Type:     models.SessionTypeHeadlessAgent,
		},
	})

	got := s.GetSession("phase0-member-audit")
	if got == nil {
		t.Fatal("session was not created")
	}
	if got.Type != models.SessionTypeHeadlessAgent {
		t.Fatalf("session type = %q, want %q", got.Type, models.SessionTypeHeadlessAgent)
	}
}

// A launcher predating the Type field means what it always meant.
func TestSessionIntentWithoutTypeDefaultsToInteractive(t *testing.T) {
	s := newTestStore(t)
	s.ApplyUpdate(Update{
		Type:    UpdateSessionIntent,
		Source:  "test",
		Payload: &SessionIntentPayload{JobID: "legacy-job", Provider: "claude"},
	})

	got := s.GetSession("legacy-job")
	if got == nil {
		t.Fatal("session was not created")
	}
	if got.Type != models.SessionTypeInteractiveAgent {
		t.Fatalf("session type = %q, want %q", got.Type, models.SessionTypeInteractiveAgent)
	}
}
