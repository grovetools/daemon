package satellite

import (
	"testing"

	"github.com/grovetools/core/config"
)

// TestLoadRegistryToleratesSyncSubtable (gate e): a [satellites.<name>.sync]
// subtable is owned by the grove CLI side — the daemon must parse the entry's
// sync_local_port / sync_remote_addr fields while ignoring the subtable (the
// mapstructure decode drops unknown keys).
func TestLoadRegistryToleratesSyncSubtable(t *testing.T) {
	cfg := &config.Config{
		Extensions: map[string]interface{}{
			"satellites": map[string]interface{}{
				"sat": map[string]interface{}{
					"ssh_addr":         "203.0.113.5:22",
					"user":             "grovedev",
					"host_key":         "ssh-ed25519 AAAA",
					"sync_local_port":  int64(8788), // TOML integers decode as int64
					"sync_remote_addr": "127.0.0.1:9999",
					// CLI-owned subtable the daemon must ignore.
					"sync": map[string]interface{}{
						"workspaces": []interface{}{"grovetools"},
						"mode":       "push-only",
					},
				},
			},
		},
	}

	reg, err := LoadRegistry(cfg)
	if err != nil {
		t.Fatalf("LoadRegistry with .sync subtable: %v", err)
	}

	sc, ok := reg.Get("sat")
	if !ok {
		t.Fatalf("satellite %q missing from registry", "sat")
	}
	if sc.SyncLocalPort != 8788 {
		t.Errorf("SyncLocalPort = %d, want 8788", sc.SyncLocalPort)
	}
	if sc.SyncRemoteAddr != "127.0.0.1:9999" {
		t.Errorf("SyncRemoteAddr = %q, want %q", sc.SyncRemoteAddr, "127.0.0.1:9999")
	}
	if sc.SSHAddr != "203.0.113.5:22" {
		t.Errorf("SSHAddr = %q, want %q", sc.SSHAddr, "203.0.113.5:22")
	}
}

// TestLoadRegistrySyncFieldsAbsent: entries without the sync fields keep the
// feature off (zero port) and the default remote addr resolution applies.
func TestLoadRegistrySyncFieldsAbsent(t *testing.T) {
	cfg := &config.Config{
		Extensions: map[string]interface{}{
			"satellites": map[string]interface{}{
				"sat": map[string]interface{}{
					"ssh_addr": "203.0.113.5:22",
					"user":     "grovedev",
					"host_key": "ssh-ed25519 AAAA",
				},
			},
		},
	}

	reg, err := LoadRegistry(cfg)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	sc, ok := reg.Get("sat")
	if !ok {
		t.Fatalf("satellite %q missing from registry", "sat")
	}
	if sc.SyncLocalPort != 0 {
		t.Errorf("SyncLocalPort = %d, want 0 (feature off)", sc.SyncLocalPort)
	}
	if got := syncRemoteAddr(sc); got != defaultSyncRemoteAddr {
		t.Errorf("syncRemoteAddr = %q, want default %q", got, defaultSyncRemoteAddr)
	}
}
