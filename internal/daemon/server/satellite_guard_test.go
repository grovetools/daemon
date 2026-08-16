package server

import (
	"context"
	"strings"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestServerMutationGuardsRefuseRemoteSessions proves the C8(b) guards: kill,
// input, and interrupt on a federated (Origin!="") session return an error and
// never fall through to syscall.Kill / PTY writes. A federated session is
// composite-keyed in the store, so we address it by that composite key (the
// only key GetSession resolves it under) to reach the guard — with a bogus PID,
// so if the guard were absent killSession would ESRCH-succeed and return nil.
func TestServerMutationGuardsRefuseRemoteSessions(t *testing.T) {
	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSatelliteSnapshot,
		Origin: "sat",
		Payload: &store.SatelliteSnapshotPayload{
			Origin: "sat",
			Sessions: []*models.Session{{
				ID:     "R",
				Origin: "sat",
				Status: "running",
				PID:    999999, // bogus — must never be signalled
			}},
		},
	})

	s := New(false)
	s.SetEngine(engine.New(st))

	// The composite key GetSession resolves the remote row under.
	key := "sat\x00R"

	if err := s.killSession(key, ""); err == nil || !strings.Contains(err.Error(), "satellite") {
		t.Fatalf("killSession on a remote session: err = %v, want a satellite-refusal error", err)
	}

	ctx := context.Background()
	if err := s.SendSessionInput(ctx, key, "hello"); err == nil || !strings.Contains(err.Error(), "satellite") {
		t.Fatalf("SendSessionInput on a remote session: err = %v, want a satellite-refusal error", err)
	}
	if err := s.SendSessionInterrupt(ctx, key); err == nil || !strings.Contains(err.Error(), "satellite") {
		t.Fatalf("SendSessionInterrupt on a remote session: err = %v, want a satellite-refusal error", err)
	}
}
