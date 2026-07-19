package satellite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
)

// isolateStateHome points GROVE_HOME at a temp root so LoadRegistry never
// reads the developer machine's real ~/.local/state/grove/satellites.json,
// and returns the isolated state file path
// ($GROVE_HOME/state/grove/satellites.json per paths.StateDir).
func isolateStateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)
	return filepath.Join(home, "state", "grove", satelliteStateFileName)
}

// writeStateFile writes raw satellites.json content at path, creating dirs.
func writeStateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadRegistryToleratesSyncSubtable (gate e): a [satellites.<name>.sync]
// subtable is owned by the grove CLI side — the daemon must parse the entry's
// sync_local_port / sync_remote_addr fields while ignoring the subtable (the
// mapstructure decode drops unknown keys).
func TestLoadRegistryToleratesSyncSubtable(t *testing.T) {
	isolateStateHome(t)
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
	isolateStateHome(t)
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

// TestLoadRegistryMergesStateFile pins the config ∪ state merge: the CLI's
// satellites.json contributes the machine-derived fields (state wins over a
// stale config value), the config keeps the user-authored fields, and both
// state-only and config-only satellites yield complete Name-stamped entries.
func TestLoadRegistryMergesStateFile(t *testing.T) {
	statePath := isolateStateHome(t)
	writeStateFile(t, statePath, `{
  "satellites": {
    "both": {
      "ssh_addr": "203.0.113.7:22",
      "host_key": "ssh-ed25519 FRESH",
      "socket_path": "/run/user/1001/grove/groved.sock",
      "sync_remote_addr": "127.0.0.1:8788",
      "user": "state-user",
      "identity_file": "/state/key",
      "sync_local_port": 8788
    },
    "cattle": {
      "ssh_addr": "203.0.113.8:22",
      "host_key": "ssh-ed25519 CATTLE",
      "user": "state-u"
    }
  }
}`)

	cfg := &config.Config{
		Extensions: map[string]interface{}{
			"satellites": map[string]interface{}{
				"both": map[string]interface{}{
					"ssh_addr":        "198.51.100.1:22", // stale — state must win
					"host_key":        "ssh-ed25519 STALE",
					"user":            "cfg-user", // user-authored — config must win
					"identity_file":   "/cfg/key",
					"sync_local_port": int64(9999),
				},
				"hand": map[string]interface{}{ // hand-managed VM, config-only
					"ssh_addr": "192.0.2.1:22",
					"user":     "hand-u",
					"host_key": "ssh-ed25519 HAND",
				},
			},
		},
	}

	reg, err := LoadRegistry(cfg)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if got := len(reg.Names()); got != 3 {
		t.Fatalf("registry names = %v, want 3", reg.Names())
	}

	both, ok := reg.Get("both")
	if !ok {
		t.Fatal("satellite both missing")
	}
	if both.Name != "both" {
		t.Errorf("Name = %q, want stamped from key", both.Name)
	}
	if both.SSHAddr != "203.0.113.7:22" || both.HostKey != "ssh-ed25519 FRESH" {
		t.Errorf("state did not win ssh_addr/host_key: %+v", both)
	}
	if both.SocketPath != "/run/user/1001/grove/groved.sock" || both.SyncRemoteAddr != "127.0.0.1:8788" {
		t.Errorf("state did not fill socket_path/sync_remote_addr: %+v", both)
	}
	if both.User != "cfg-user" || both.IdentityFile != "/cfg/key" || both.SyncLocalPort != 9999 {
		t.Errorf("config did not win user-authored fields: %+v", both)
	}

	cattle, ok := reg.Get("cattle")
	if !ok || cattle.Name != "cattle" || cattle.SSHAddr != "203.0.113.8:22" || cattle.User != "state-u" {
		t.Errorf("state-only entry = %+v (ok=%v)", cattle, ok)
	}
	hand, ok := reg.Get("hand")
	if !ok || hand.Name != "hand" || hand.SSHAddr != "192.0.2.1:22" || hand.HostKey != "ssh-ed25519 HAND" {
		t.Errorf("config-only entry = %+v (ok=%v)", hand, ok)
	}
}

// TestMergeSatelliteEntryKind pins kind's merge precedence: user-authored
// config wins when set, the CLI-stamped state value is the fallback (the
// user/identity_file pattern), and empty in both stays empty (= KindFull via
// EffectiveKind, so the daemon-side default never overrides a state value).
func TestMergeSatelliteEntryKind(t *testing.T) {
	cases := []struct {
		name       string
		cfg, state string
		want       string
	}{
		{"config wins over state", KindFull, KindExec, KindFull},
		{"state fills empty config", "", KindExec, KindExec},
		{"config-only", KindExec, "", KindExec},
		{"empty both stays empty", "", "", ""},
	}
	for _, tc := range cases {
		got := mergeSatelliteEntry(
			SatelliteConfig{Kind: tc.cfg},
			SatelliteConfig{Kind: tc.state},
		)
		if got.Kind != tc.want {
			t.Errorf("%s: Kind = %q, want %q", tc.name, got.Kind, tc.want)
		}
	}

	// Normalization helpers: empty means full.
	empty := &SatelliteConfig{}
	if empty.EffectiveKind() != KindFull || empty.IsExec() {
		t.Errorf("empty kind: EffectiveKind=%q IsExec=%v, want %q/false",
			empty.EffectiveKind(), empty.IsExec(), KindFull)
	}
	exec := &SatelliteConfig{Kind: KindExec}
	if exec.EffectiveKind() != KindExec || !exec.IsExec() {
		t.Errorf("exec kind: EffectiveKind=%q IsExec=%v, want %q/true",
			exec.EffectiveKind(), exec.IsExec(), KindExec)
	}
}

// TestLoadRegistryStateOnly: with no [satellites] config section at all, the
// state file alone still yields a complete registry (a nil config too).
func TestLoadRegistryStateOnly(t *testing.T) {
	statePath := isolateStateHome(t)
	writeStateFile(t, statePath, `{"satellites": {"sat": {
  "ssh_addr": "203.0.113.5:22",
  "user": "grovedev",
  "host_key": "ssh-ed25519 AAAA",
  "sync_local_port": 8788
}}}`)

	reg, err := LoadRegistry(&config.Config{})
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	sc, ok := reg.Get("sat")
	if !ok {
		t.Fatal("state-only satellite missing")
	}
	if sc.Name != "sat" || sc.SSHAddr != "203.0.113.5:22" || sc.User != "grovedev" || sc.SyncLocalPort != 8788 {
		t.Errorf("state-only entry = %+v", sc)
	}

	// nil config must not panic and still surfaces the state entries.
	reg, err = LoadRegistry(nil)
	if err != nil {
		t.Fatalf("LoadRegistry(nil): %v", err)
	}
	if _, ok := reg.Get("sat"); !ok {
		t.Error("state entry missing with nil config")
	}
}

// TestLoadRegistryToleratesCorruptStateFile: a corrupt satellites.json must
// not kill daemon startup — LoadRegistry warns and returns the config-only
// registry. An absent state file is silent and equally config-only.
func TestLoadRegistryToleratesCorruptStateFile(t *testing.T) {
	statePath := isolateStateHome(t)
	writeStateFile(t, statePath, "{ this is not json")

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
		t.Fatalf("LoadRegistry with corrupt state file: %v", err)
	}
	if got := len(reg.Names()); got != 1 {
		t.Fatalf("registry names = %v, want config-only single entry", reg.Names())
	}
	if sc, ok := reg.Get("sat"); !ok || sc.SSHAddr != "203.0.113.5:22" {
		t.Errorf("config entry lost on corrupt state: %+v (ok=%v)", sc, ok)
	}

	// Absent state file: same config-only result, no error.
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	reg, err = LoadRegistry(cfg)
	if err != nil {
		t.Fatalf("LoadRegistry with absent state file: %v", err)
	}
	if got := len(reg.Names()); got != 1 {
		t.Fatalf("registry names = %v, want 1", reg.Names())
	}
}
