package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/syncproto"
)

// The history/restore client is the second of the two sync-client
// construction sites (the first is SyncHandler.transportLoop). It must send
// this machine's durable ID as DeviceID, not the empty string it used to.
//
// The server is free to ignore what it receives — rendezvous stays dumb —
// which is exactly why the assertion lives on the client's outbound wire.
func TestHistoryClientSendsMachineDeviceID(t *testing.T) {
	sandboxGroveHome(t)

	captured := make(chan syncproto.CapabilitiesRequest, 1)
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
		if len(req.ProtocolVersions) != 1 || req.ProtocolVersions[0] != syncproto.ProtocolVersionLegacy {
			t.Errorf("offered protocol versions = %v, want legacy", req.ProtocolVersions)
			http.Error(w, "unsupported protocol offer", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
			ProtocolVersion: syncproto.ProtocolVersionLegacy,
			Capabilities:    syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersionLegacy}},
		})
	}))
	t.Cleanup(srv.Close)

	writeSandboxSyncConfig(t, srv.URL)

	s := newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))
	if _, err := s.historyClient(context.Background()); err != nil {
		t.Fatalf("historyClient: %v", err)
	}

	req := <-captured
	wantID := machine.ID()
	if wantID == "" {
		t.Fatal("machine.ID() is empty in the sandbox — the test cannot prove anything")
	}
	if req.DeviceID != wantID {
		t.Fatalf("DeviceID on the wire = %q, want the machine id %q", req.DeviceID, wantID)
	}
	if req.OriginID == "" || req.OriginID == req.DeviceID {
		t.Errorf("OriginID %q must still be sent and remain distinct from DeviceID", req.OriginID)
	}
}

func writeSandboxSyncConfig(t *testing.T, serverURL string) {
	t.Helper()
	configDir := filepath.Join(os.Getenv("GROVE_HOME"), "config", "grove")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	body := fmt.Sprintf("server = %q\ntoken = \"t\"\n\n[[workspaces]]\nname = \"ws\"\n", serverURL)
	if err := os.WriteFile(filepath.Join(configDir, "sync.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write sync.toml: %v", err)
	}
}
