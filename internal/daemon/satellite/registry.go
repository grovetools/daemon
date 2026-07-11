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
	"github.com/grovetools/core/config"
)

// SatelliteConfig is one entry parsed from the grove config's [satellites.<name>]
// table. The mapstructure decoder behind config.UnmarshalExtension keys off the
// `yaml` tag (see core/config/types.go UnmarshalExtension), so the yaml tags are
// what actually bind config keys to fields; the toml tags document the on-disk
// TOML shape.
type SatelliteConfig struct {
	// Name is stamped from the [satellites.<name>] map key by LoadRegistry; it
	// is never read from the config body (that value is the federation Origin,
	// C6, stable across cattle recreations).
	Name string `yaml:"-" toml:"-"`

	// SSHAddr is the satellite's SSH endpoint as host:port.
	SSHAddr string `yaml:"ssh_addr" toml:"ssh_addr"`

	// User is the SSH login user on the satellite.
	User string `yaml:"user" toml:"user"`

	// HostKey is the pinned host public key in authorized_keys / known_hosts
	// line format (e.g. "ssh-ed25519 AAAA..."). Registry-seeded by P10's
	// `grove satellite up` from provision-time output; ConnManager hard-fails if
	// it is empty or unparseable — pinning is never TOFU (C2).
	HostKey string `yaml:"host_key" toml:"host_key"`

	// IdentityFile is an optional path to a private key added as a second auth
	// method. Empty = agent-only ($SSH_AUTH_SOCK). Private keys never leave the
	// agent otherwise (C2).
	IdentityFile string `yaml:"identity_file" toml:"identity_file"`

	// SocketPath optionally overrides the remote groved socket path. Empty uses
	// the default convention (see remoteSocketPath in connmanager.go). P10 can
	// write the exact value from bootstrap output.
	SocketPath string `yaml:"socket_path" toml:"socket_path"`
}

// Registry holds the parsed satellite configs, keyed by name.
type Registry struct {
	byName map[string]*SatelliteConfig
}

// LoadRegistry parses the [satellites.<name>] tables out of the grove config
// via the existing extension mechanism (mirrors loadNavGroupConfigs' use of
// cfg.UnmarshalExtension in daemon/internal/daemon/server/server.go). An
// absent or empty [satellites] section yields an empty registry, NOT an error:
// satellite-less daemons must boot unchanged.
func LoadRegistry(cfg *config.Config) (*Registry, error) {
	reg := &Registry{byName: make(map[string]*SatelliteConfig)}
	if cfg == nil {
		return reg, nil
	}

	var raw map[string]SatelliteConfig
	if err := cfg.UnmarshalExtension("satellites", &raw); err != nil {
		return nil, err
	}

	for name, sc := range raw {
		entry := sc
		entry.Name = name
		reg.byName[name] = &entry
	}
	return reg, nil
}

// Get returns the config for a satellite by name.
func (r *Registry) Get(name string) (*SatelliteConfig, bool) {
	sc, ok := r.byName[name]
	return sc, ok
}

// Names returns the registry entry names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	return names
}
