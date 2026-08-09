package store

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

func TestMachineSyncDeltaAppliesAndUnrelatedDeltaPreservesIt(t *testing.T) {
	s := New()
	s.ApplyUpdate(Update{
		Type: UpdateWorkspaces,
		Payload: map[string]*models.EnrichedWorkspace{
			"/ws/demo": {WorkspaceNode: &workspace.WorkspaceNode{Path: "/ws/demo"}},
		},
	})
	projection := &models.MachineSync{
		SchemaVersion: models.MachineSyncSchemaVersion,
		Peers:         []models.MachineSyncPeer{{MachineID: "remote", State: models.MachineSyncEqual}},
	}
	s.ApplyUpdate(Update{
		Type:    UpdateWorkspacesDelta,
		Payload: []*models.WorkspaceDelta{{Path: "/ws/demo", MachineSync: projection}},
	})
	if got := s.Get().Workspaces["/ws/demo"].MachineSync; got != projection {
		t.Fatalf("MachineSync delta did not land: %+v", got)
	}

	s.ApplyUpdate(Update{
		Type:    UpdateWorkspacesDelta,
		Payload: []*models.WorkspaceDelta{{Path: "/ws/demo", NoteCounts: &models.NoteCounts{Inbox: 1}}},
	})
	if got := s.Get().Workspaces["/ws/demo"].MachineSync; got != projection {
		t.Fatalf("unrelated delta replaced MachineSync: %+v", got)
	}
}
