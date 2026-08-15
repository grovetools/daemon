package cmd

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func TestRegistryRecoveryCorroboratorAliasMatrix(t *testing.T) {
	metadata := sessions.SessionMetadata{SessionID: "job", JobID: "job", ClaudeSessionID: "native"}

	t.Run("absent", func(t *testing.T) {
		if !registryRecoveryCorroborator(store.New(), "")("native", metadata) {
			t.Fatal("absent daemon row did not corroborate dead lock cleanup")
		}
	})

	t.Run("foreign scope", func(t *testing.T) {
		foreign := metadata
		foreign.Scope = "/other"
		if registryRecoveryCorroborator(store.New(), "/mine")("native", foreign) {
			t.Fatal("foreign-scope record corroborated cleanup")
		}
	})

	t.Run("terminal alias", func(t *testing.T) {
		st := store.New()
		st.ApplyUpdate(store.Update{Type: store.UpdateSessions, Payload: []*models.Session{
			{ID: "job", ClaudeSessionID: "native", Status: "interrupted"},
		}})
		if !registryRecoveryCorroborator(st, "")("native", metadata) {
			t.Fatal("terminal daemon row did not corroborate dead lock cleanup")
		}
	})

	for _, status := range []string{"pending", "running", "idle", "pending_user"} {
		t.Run("active_"+status, func(t *testing.T) {
			st := store.New()
			st.ApplyUpdate(store.Update{Type: store.UpdateSessions, Payload: []*models.Session{
				{ID: "job", ClaudeSessionID: "native", Status: status},
			}})
			if registryRecoveryCorroborator(st, "")("native", metadata) {
				t.Fatalf("%s daemon row incorrectly corroborated cleanup", status)
			}
		})
	}
}
