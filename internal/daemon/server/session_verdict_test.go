package server

import (
	"testing"

	"github.com/grovetools/daemon/internal/daemon/store"
)

func TestSessionVerdictUpdateRefreshesSessionConsumers(t *testing.T) {
	payload := &store.SessionVerdictPayload{JobID: "job", Verified: "stale"}
	got := convertUpdatePayload(store.Update{Type: store.UpdateSessionVerdict, Source: "collector", Payload: payload})
	if got == nil {
		t.Fatal("session verdict update was dropped from SSE")
	}
	if got.UpdateType != "session" || got.Payload != payload {
		t.Fatalf("wire update = %#v, want collapsed session update with verdict payload", got)
	}
}
