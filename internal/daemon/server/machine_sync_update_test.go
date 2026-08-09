package server

import (
	"encoding/json"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func TestMachineSyncSurvivesWorkspaceDeltaWire(t *testing.T) {
	projection := &models.MachineSync{
		SchemaVersion: models.MachineSyncSchemaVersion,
		RootID:        "ecosystem:01DEMO",
		RepoPath:      "core",
		Peers: []models.MachineSyncPeer{{
			MachineID: "remote", Label: "laptop (remote)",
			State:  models.MachineSyncDiverged,
			Reason: "committed tips differ; direction and distance are unknown",
		}},
	}
	apiUpdate := convertToAPIUpdate(store.Update{
		Type: store.UpdateWorkspacesDelta, Source: "machine_sync",
		Payload: []*models.WorkspaceDelta{{Path: "/ws/core", MachineSync: projection}},
	})
	if apiUpdate == nil {
		t.Fatal("convertToAPIUpdate dropped the workspaces_delta")
	}
	data, err := json.Marshal(apiUpdate)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Deltas []*models.WorkspaceDelta `json:"workspace_deltas"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Deltas) != 1 || wire.Deltas[0].MachineSync == nil {
		t.Fatalf("machine sync did not survive the wire: %+v", wire.Deltas)
	}
	peer := wire.Deltas[0].MachineSync.Peers[0]
	if peer.State != models.MachineSyncDiverged {
		t.Fatalf("state = %q, want bounded divergence", peer.State)
	}
}
