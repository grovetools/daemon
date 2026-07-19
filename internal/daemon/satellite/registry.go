// Package satellite implements the laptop-side transport to remote grove
// "satellite" daemons (M2 contract C1-C3, C10). It owns one SSH connection per
// configured satellite and exposes a single downstream primitive,
// DialSatelliteSocket, which opens a direct-streamlocal@openssh.com channel to
// the satellite's global groved unix socket. Everything in this package is
// CLIENT-ONLY: only the laptop dials, the satellite daemon gains no inbound
// verb (C3). P8's SatelliteCollector and P9's dispatch consume the dialer via
// core's daemon.NewRemoteClientWithDialer.
package satellite

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/grovetools/core/config"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/paths"
)

// SatelliteConfig is one entry in the merged satellite registry. Entries come
// from two sources, merged per name per field by LoadRegistry:
//
//   - the grove config's [satellites.<name>] table (user-authored; may live in
//     grove.toml or any config-dir fragment). The mapstructure decoder behind
//     config.UnmarshalExtension keys off the `yaml` tag (see core/config/types.go
//     UnmarshalExtension), so the yaml tags are what actually bind config keys
//     to fields; the toml tags document the on-disk TOML shape.
//   - the CLI-owned provisioning state file $XDG_STATE_HOME/grove/satellites.json
//     written by `grove satellite up`/`down` (machine-derived; json tags bind).
type SatelliteConfig struct {
	// Name is stamped from the [satellites.<name>] map key by LoadRegistry; it
	// is never read from the config body (that value is the federation Origin,
	// C6, stable across cattle recreations).
	Name string `yaml:"-" toml:"-" json:"-"`

	// Kind selects how much of the satellite stack the daemon engages:
	// KindFull (or empty — the default) is the L0–L3 shape with a remote groved
	// the ConnManager dials and keeps healthy; KindExec is L0–L1 only (sshd +
	// grove binary, no groved, no sync), so the ConnManager never dials it and
	// reports it as "exec-only". User-authored in config; `grove satellite up`
	// also writes it into satellites.json state. Use IsExec/EffectiveKind
	// rather than comparing the raw field — empty means KindFull.
	Kind string `yaml:"kind" toml:"kind" json:"kind,omitempty"`

	// SSHAddr is the satellite's SSH endpoint as host:port.
	SSHAddr string `yaml:"ssh_addr" toml:"ssh_addr" json:"ssh_addr"`

	// User is the SSH login user on the satellite.
	User string `yaml:"user" toml:"user" json:"user"`

	// HostKey is the pinned host public key in authorized_keys / known_hosts
	// line format (e.g. "ssh-ed25519 AAAA..."). Registry-seeded by P10's
	// `grove satellite up` from provision-time output; ConnManager hard-fails if
	// it is empty or unparseable — pinning is never TOFU (C2).
	HostKey string `yaml:"host_key" toml:"host_key" json:"host_key"`

	// IdentityFile is an optional path to a private key added as a second auth
	// method. Empty = agent-only ($SSH_AUTH_SOCK). Private keys never leave the
	// agent otherwise (C2).
	IdentityFile string `yaml:"identity_file" toml:"identity_file" json:"identity_file,omitempty"`

	// SocketPath optionally overrides the remote groved socket path. Empty uses
	// the default convention (see remoteSocketPath in connmanager.go). P10 can
	// write the exact value from bootstrap output.
	SocketPath string `yaml:"socket_path" toml:"socket_path" json:"socket_path,omitempty"`

	// SyncLocalPort, when > 0, has the daemon bind 127.0.0.1:<port> and forward
	// every accepted connection over the satellite's SSH connection to
	// SyncRemoteAddr via a direct-tcpip channel (see syncforward.go). This is
	// the daemon-owned replacement for the manual `ssh -L` sync tunnel. 0 or
	// absent = feature off.
	SyncLocalPort int `yaml:"sync_local_port" toml:"sync_local_port" json:"sync_local_port,omitempty"`

	// SyncRemoteAddr is the loopback address on the satellite that the sync
	// forward dials (syncd's bind address). Empty defaults to "127.0.0.1:8788"
	// (see syncRemoteAddr in syncforward.go).
	SyncRemoteAddr string `yaml:"sync_remote_addr" toml:"sync_remote_addr" json:"sync_remote_addr,omitempty"`

	// Note: a [satellites.<name>.sync] subtable may appear in the TOML (owned
	// by the grove CLI side). The mapstructure decode behind LoadRegistry
	// ignores unknown keys, so the daemon tolerates it without a field here —
	// TestLoadRegistryToleratesSyncSubtable pins that behavior.
}

// Satellite kinds (the SatelliteConfig.Kind axis).
const (
	// KindFull is the default full-stack satellite: remote groved dialed and
	// health-checked by the ConnManager. An empty Kind means KindFull.
	KindFull = "full"
	// KindExec is an sshd-plus-grove-binary endpoint with no groved daemon:
	// the ConnManager never dials it, never backs off, and never binds a sync
	// forward — it only reports the entry as exec-only.
	KindExec = "exec"
)

// EffectiveKind normalizes Kind: empty means KindFull.
func (sc *SatelliteConfig) EffectiveKind() string {
	if sc.Kind == "" {
		return KindFull
	}
	return sc.Kind
}

// IsExec reports whether this entry is an exec-only (no groved) satellite.
func (sc *SatelliteConfig) IsExec() bool {
	return sc.Kind == KindExec
}

// Registry holds the parsed satellite configs, keyed by name.
//
// It is SHARED MUTABLE state: groved.go constructs one Registry at boot and
// hands the same pointer to both the ConnManager and the SatelliteCollector,
// and ConnManager.Reload later swaps its contents in place (replace) so both
// consumers observe registry changes without a daemon restart. The mutex
// exists solely for that hot-reload path; entries themselves are treated as
// immutable once inserted (Reload replaces pointers, never mutates a
// *SatelliteConfig in place — live goroutines hold captured pointers).
type Registry struct {
	mu     sync.RWMutex
	byName map[string]*SatelliteConfig
}

// NewRegistry builds a Registry from an already-parsed name→config map. It
// stamps each entry's Name from its key (mirroring LoadRegistry). A nil map
// yields an empty registry. This is the direct constructor used by LoadRegistry
// and by callers that assemble configs by other means (e.g. tests, P10's
// `grove satellite` writing entries programmatically).
func NewRegistry(configs map[string]*SatelliteConfig) *Registry {
	reg := &Registry{byName: make(map[string]*SatelliteConfig, len(configs))}
	for name, sc := range configs {
		if sc == nil {
			continue
		}
		entry := *sc
		entry.Name = name
		reg.byName[name] = &entry
	}
	return reg
}

// satelliteStateFileName is the CLI-owned provisioning state file under
// paths.StateDir() (default ~/.local/state/grove). `grove satellite up`
// upserts an entry per provisioned satellite; `down` removes it.
const satelliteStateFileName = "satellites.json"

// defaultSatelliteStatePath resolves the state file the same way the grove CLI
// writes it: $XDG_STATE_HOME/grove/satellites.json (GROVE_HOME/XDG overrides
// honored via paths.StateDir). Empty when no state home resolves.
func defaultSatelliteStatePath() string {
	dir := paths.StateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, satelliteStateFileName)
}

// LoadRegistry builds the registry as config ∪ state, merged per satellite
// name per field:
//
//   - [satellites.<name>] tables from the grove config (the existing extension
//     mechanism, mirrors loadNavGroupConfigs' use of cfg.UnmarshalExtension in
//     daemon/internal/daemon/server/server.go) carry the user-authored view.
//   - the grove CLI's provisioning state file (satellites.json under the state
//     dir) carries the machine-derived view written by `grove satellite up`.
//
// Merge rule (mergeSatelliteEntry): churny provisioning fields (ssh_addr,
// host_key, socket_path, sync_remote_addr) prefer a non-empty STATE value;
// user-authored fields (user, identity_file, sync_local_port, kind) prefer a
// non-empty CONFIG value. A satellite present in only one source still yields
// a complete entry.
//
// An absent or empty [satellites] section and an absent state file yield an
// empty registry, NOT an error: satellite-less daemons must boot unchanged. A
// corrupt/unreadable state file is a warning, never a boot failure.
func LoadRegistry(cfg *config.Config) (*Registry, error) {
	return loadRegistryFromSources(cfg, defaultSatelliteStatePath())
}

// loadRegistryFromSources is LoadRegistry with an explicit state file path
// (tests point it at temp dirs).
func loadRegistryFromSources(cfg *config.Config, statePath string) (*Registry, error) {
	var fromConfig map[string]SatelliteConfig
	if cfg != nil {
		if err := cfg.UnmarshalExtension("satellites", &fromConfig); err != nil {
			return nil, err
		}
	}

	fromState := readSatelliteState(statePath)

	reg := &Registry{byName: make(map[string]*SatelliteConfig, len(fromConfig)+len(fromState))}
	for name, sc := range fromConfig {
		if !declaresSatellite(sc) {
			continue
		}
		entry := sc
		entry.Name = name
		reg.byName[name] = &entry
	}
	for name, st := range fromState {
		if existing, ok := reg.byName[name]; ok {
			merged := mergeSatelliteEntry(*existing, st)
			merged.Name = name
			reg.byName[name] = &merged
			continue
		}
		entry := st
		entry.Name = name
		reg.byName[name] = &entry
	}
	return reg, nil
}

// declaresSatellite reports whether a config-half [satellites.<name>] table
// actually declares a satellite. The grove CLI owns several subtables under
// the same name ([satellites.<name>.infra], .provision, .sync) whose keys the
// mapstructure decode drops as unknown, so a table carrying ONLY those decodes
// to an all-zero SatelliteConfig. Such a table must not conjure a registry
// entry: the ConnManager would dial it, hard-fail on the missing pinned
// host_key, and leave a permanent "disconnected" status row for a satellite
// that no longer exists (the residue `grove satellite down` leaves behind).
// The state half is unaffected — it is merged in below regardless.
func declaresSatellite(sc SatelliteConfig) bool {
	return sc != SatelliteConfig{}
}

// readSatelliteState reads the CLI-owned satellites.json. Absent file (or no
// resolvable path) is the normal satellite-less/hand-managed case and returns
// nil silently; any read/parse failure returns nil with a warning — a corrupt
// state file must not kill daemon startup (the config-only view still loads).
func readSatelliteState(path string) map[string]SatelliteConfig {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			grovelogging.NewUnifiedLogger("groved.satellite").
				Warn("Satellite state file unreadable; loading config-only registry").
				Field("path", path).Err(err).Log(context.Background())
		}
		return nil
	}
	var sf struct {
		Satellites map[string]SatelliteConfig `json:"satellites"`
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		grovelogging.NewUnifiedLogger("groved.satellite").
			Warn("Satellite state file corrupt; loading config-only registry").
			Field("path", path).Err(err).Log(context.Background())
		return nil
	}
	return sf.Satellites
}

// mergeSatelliteEntry merges one satellite's config entry with its state
// entry. State wins for the machine-derived fields that churn on every VM
// recreate; config wins for the user-authored fields, with the state's
// resolved snapshot as the fallback.
func mergeSatelliteEntry(cfg, state SatelliteConfig) SatelliteConfig {
	out := cfg
	if state.SSHAddr != "" {
		out.SSHAddr = state.SSHAddr
	}
	if state.HostKey != "" {
		out.HostKey = state.HostKey
	}
	if state.SocketPath != "" {
		out.SocketPath = state.SocketPath
	}
	if state.SyncRemoteAddr != "" {
		out.SyncRemoteAddr = state.SyncRemoteAddr
	}
	if out.User == "" {
		out.User = state.User
	}
	if out.IdentityFile == "" {
		out.IdentityFile = state.IdentityFile
	}
	if out.Kind == "" {
		// Kind is user-authored first, but `grove satellite up` also stamps it
		// into satellites.json — the state snapshot is the fallback, exactly
		// like user/identity_file. Empty in both sources = KindFull.
		out.Kind = state.Kind
	}
	if out.SyncLocalPort == 0 {
		out.SyncLocalPort = state.SyncLocalPort
	}
	return out
}

// Get returns the config for a satellite by name.
func (r *Registry) Get(name string) (*SatelliteConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sc, ok := r.byName[name]
	return sc, ok
}

// Names returns the registry entry names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	return names
}

// snapshot returns a shallow copy of the name→config map. The entry pointers
// are shared — safe because entries are immutable once inserted (see the
// Registry doc comment).
func (r *Registry) snapshot() map[string]*SatelliteConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*SatelliteConfig, len(r.byName))
	for name, sc := range r.byName {
		out[name] = sc
	}
	return out
}

// replace swaps this registry's contents with src's, in place. This is the
// hot-reload primitive: ConnManager.Reload applies a freshly-loaded registry
// into the boot-time Registry object so every holder of the original pointer
// (the collector, the ConnManager itself) sees the new entry set atomically.
// src is a private, just-built Registry, so reading it unlocked is fine.
func (r *Registry) replace(src *Registry) {
	entries := src.snapshot()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName = entries
}
