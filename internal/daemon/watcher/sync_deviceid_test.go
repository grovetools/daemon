package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/syncproto"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// transportLoop is the primary of the two sync-client construction sites (the
// other is the server's history/restore client). It must identify this host
// with the durable machine ULID as DeviceID, distinct from the per-sync.db
// OriginID that dies with the database.
func TestTransportLoopSendsMachineDeviceID(t *testing.T) {
	// GROVE_HOME sandboxes paths.StateDir() so minting never touches the
	// developer's real state directory.
	t.Setenv("GROVE_HOME", t.TempDir())

	captured := make(chan syncproto.CapabilitiesRequest, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/capabilities" {
			http.NotFound(w, r)
			return
		}
		var req syncproto.CapabilitiesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		select {
		case captured <- req:
		default:
		}
		_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
			Capabilities: syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersion}},
		})
	}))
	t.Cleanup(srv.Close)

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	syncCfg := &config.SyncConfig{
		Server:     srv.URL,
		Token:      "t",
		Workspaces: []config.SyncWorkspace{{Name: "ws"}},
	}
	h := NewSyncHandler(nil, nil, syncCfg, db, 50, 500)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.transportLoop(ctx)

	var req syncproto.CapabilitiesRequest
	select {
	case req = <-captured:
	case <-time.After(10 * time.Second):
		t.Fatal("transportLoop never performed a capabilities handshake")
	}
	cancel()

	wantID := machine.ID()
	if wantID == "" {
		t.Fatal("machine.ID() is empty in the sandbox — the test cannot prove anything")
	}
	if req.DeviceID != wantID {
		t.Fatalf("DeviceID on the wire = %q, want the machine id %q", req.DeviceID, wantID)
	}
	if req.OriginID != db.OriginID() {
		t.Errorf("OriginID = %q, want sync.db's %q", req.OriginID, db.OriginID())
	}
	if req.DeviceID == req.OriginID {
		t.Error("DeviceID and OriginID must be distinct identifiers")
	}
}
