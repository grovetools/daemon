package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/machine"
)

// sandboxGroveHome redirects paths.ConfigDir()/paths.StateDir() into a temp
// dir so identity minting never touches the developer's real home.
func sandboxGroveHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)
	configDir := filepath.Join(home, "config", "grove")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	return configDir
}

// The identity headline (contract T2) needs machine_name + machine_id on the
// status payload, and they must be present even when sync is disabled:
// identity does not depend on sync being configured.
func TestHandleSyncStatusReportsMachineIdentity(t *testing.T) {
	configDir := sandboxGroveHome(t)
	if err := os.WriteFile(filepath.Join(configDir, "machine.toml"), []byte("[machine]\nname = \"mbp\"\n"), 0o644); err != nil {
		t.Fatalf("write machine.toml: %v", err)
	}

	for _, tc := range []struct {
		name        string
		withSyncDB  bool
		wantEnabled bool
	}{
		{"sync disabled", false, false},
		{"sync enabled", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s *Server
			if tc.withSyncDB {
				s = newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))
			} else {
				s = New(false)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/sync/status", nil)
			w := httptest.NewRecorder()
			s.handleSyncStatus(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
			}

			var out syncStatusResponse
			if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Enabled != tc.wantEnabled {
				t.Fatalf("Enabled = %v, want %v", out.Enabled, tc.wantEnabled)
			}
			if out.MachineName != "mbp" {
				t.Errorf("MachineName = %q, want mbp", out.MachineName)
			}
			if out.MachineID == "" {
				t.Fatal("MachineID is empty — the status handler must mint-or-read the identity")
			}

			// The handler is also a minting site: whatever it reported must be
			// what is now on disk, and must not change on the next call.
			persisted, err := machine.Load()
			if err != nil || persisted == nil {
				t.Fatalf("identity not persisted: id=%v err=%v", persisted, err)
			}
			if persisted.ID != out.MachineID {
				t.Errorf("reported id %q != persisted id %q", out.MachineID, persisted.ID)
			}
		})
	}
}

// The machine id is stable across restarts; the origin id is not the identity
// and is reported separately.
func TestHandleSyncStatusMachineIDIsStableAndDistinctFromOrigin(t *testing.T) {
	sandboxGroveHome(t)
	s := newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))

	get := func() syncStatusResponse {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleSyncStatus(w, httptest.NewRequest(http.MethodGet, "/api/sync/status", nil))
		var out syncStatusResponse
		if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	first, second := get(), get()
	if first.MachineID != second.MachineID {
		t.Fatalf("machine id changed between calls: %q vs %q", first.MachineID, second.MachineID)
	}
	if first.MachineID == first.OriginID {
		t.Fatalf("machine id and origin id must be distinct identifiers, both %q", first.MachineID)
	}
	if first.OriginID == "" {
		t.Error("OriginID should still be reported as a diagnostic")
	}

	// A wiped sync.db is "same machine, new origin" — the ID survives it.
	s2 := newSyncTestServer(t, filepath.Join(t.TempDir(), "sync.db"))
	w := httptest.NewRecorder()
	s2.handleSyncStatus(w, httptest.NewRequest(http.MethodGet, "/api/sync/status", nil))
	var afterWipe syncStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&afterWipe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if afterWipe.MachineID != first.MachineID {
		t.Errorf("machine id did not survive a fresh sync.db: %q vs %q", afterWipe.MachineID, first.MachineID)
	}
	if afterWipe.OriginID == first.OriginID {
		t.Errorf("a fresh sync.db should have a new origin id, got %q twice", afterWipe.OriginID)
	}
}

// With no machine.toml the name falls back to the hostname — a machine is
// never nameless on a surface.
func TestHandleSyncStatusMachineNameFallsBackToHostname(t *testing.T) {
	sandboxGroveHome(t)
	s := New(false)

	w := httptest.NewRecorder()
	s.handleSyncStatus(w, httptest.NewRequest(http.MethodGet, "/api/sync/status", nil))
	var out syncStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	host, _ := os.Hostname()
	if out.MachineName != host {
		t.Fatalf("MachineName = %q, want hostname %q", out.MachineName, host)
	}
}
