// Package assistant implements the daemon-side assistant supervisor: the
// ensure-running loop that keeps an ecosystem's standing assistant chain alive
// (assistant-pane spec §3.3).
//
// The supervisor is a sibling of the autonomous Pinger — daemon-owned because
// it must outlive every TUI, be reachable from the Signal path, and already
// sits next to session liveness. It reaches orchestration only through the
// `flow` CLI's argv, never through flow's internals: "the contract is the CLIs'
// argv, never a database" (agent/README.md:33).
package assistant

import (
	"strings"

	coreconfig "github.com/grovetools/core/config"
)

// Config is the `[assistant]` block of an ecosystem's grove.toml.
//
//	[assistant]
//	enabled = true
//	plan = "steward"
//	provider = "grove-agent"
//
// Every field carries BOTH a toml and a yaml tag, and they must stay in sync.
// The toml tag names the key an operator writes; the yaml tag is what actually
// binds it, because the block arrives through config.UnmarshalExtension, which
// decodes the generic extension map with mapstructure under TagName "yaml"
// (core/config/types.go). Without a yaml tag, mapstructure falls back to
// case-insensitive field-NAME matching, which matches single-word keys like
// `enabled` and `plan` but silently drops every snake_case one —
// `idle_minutes`, `handoff_threshold`, `handoff_max`, `signal_target`,
// `max_chain_resets_per_day` — leaving them zero and letting the defaults below
// mask the loss. claudenotebook.ClaudeConfig carries the same dual tags for the
// same reason.
type Config struct {
	// Enabled turns the supervisor on. Absent or false means the daemon
	// supervises nothing, which is the default for every ecosystem that has
	// not opted in.
	Enabled bool `toml:"enabled" yaml:"enabled"`

	// Plan is the assistant's home plan name (spec §3.1 proposes "steward").
	Plan string `toml:"plan" yaml:"plan"`

	// Provider is the agent CLI provider stamped on chain-reset root jobs.
	Provider string `toml:"provider" yaml:"provider"`

	// Model optionally pins the successor's model. Agent jobs do NOT inherit
	// the plan's default model (orchestration job.go InheritsPlanModel), so
	// leaving this empty rides the provider's own default.
	Model string `toml:"model" yaml:"model"`

	// Skills is the skill_sequence stamped on chain-reset root jobs. They
	// must also be authorized in [skills] use, and `pi` must be in [skills]
	// providers or .pi/skills never syncs.
	Skills []string `toml:"skills" yaml:"skills"`

	// Channel is the messaging channel re-applied after every (re)launch
	// (spec §3.3 "re-claw"). Empty disables re-clawing.
	Channel string `toml:"channel" yaml:"channel"`

	// SignalTarget is the named signal contact/group for outbound messages.
	SignalTarget string `toml:"signal_target" yaml:"signal_target"`

	// IdleMinutes is the autonomous idle-ping interval re-applied with the
	// claw. Zero uses DefaultIdleMinutes.
	IdleMinutes int `toml:"idle_minutes" yaml:"idle_minutes"`

	// HandoffThreshold and HandoffMax are stamped on chain-reset root jobs so
	// a reset chain inherits the same context budget as the one it replaces.
	// Zero leaves flow's own defaults in place.
	HandoffThreshold int `toml:"handoff_threshold" yaml:"handoff_threshold"`
	HandoffMax       int `toml:"handoff_max" yaml:"handoff_max"`

	// MaxChainResetsPerDay rate-limits chain resets (spec §6 proposes 3).
	// A chain that burns through its reset budget is a runaway, not a
	// resilient assistant, so the breaker trips instead. Zero uses
	// DefaultMaxChainResetsPerDay.
	MaxChainResetsPerDay int `toml:"max_chain_resets_per_day" yaml:"max_chain_resets_per_day"`
}

const (
	// DefaultIdleMinutes matches the standing job's autonomous config.
	DefaultIdleMinutes = 30
	// DefaultChannel is the channel re-applied when [assistant] names none.
	DefaultChannel = "signal"
	// DefaultMaxChainResetsPerDay is the spec §6 proposal: past three resets
	// in a day the assistant is not recovering, it is looping.
	DefaultMaxChainResetsPerDay = 3
	// DefaultCoordMode keeps a reset chain autonomous, so its successor
	// handoffs continue to fire without an operator.
	DefaultCoordMode = "autonomous"
)

// LoadConfig resolves the [assistant] block for the ecosystem rooted at dir.
//
// It goes through config.LoadFrom, which is grove's ONE config cascade:
//
//  1. global    ~/.config/grove/grove.toml (+ that directory's *.toml fragments)
//  2. project   <dir>/grove.toml — overrides global
//  3. override  grove.override.toml — overrides all
//
// Reading <dir>/grove.toml directly instead — which this did originally — makes
// [assistant] the one block in the ecosystem that a global preference cannot
// reach. That is a silent trap, not a small one: an operator who puts a working
// [assistant] block in ~/.config/grove/grove.toml (where `grove config` writes,
// and where every other global preference lives) gets no error and no warning,
// only a daemon that reports `disabled (no [assistant] block enabled in
// grove.toml)` while pointing at a file they did in fact configure.
//
// Note the consequence for discovery: a GLOBAL block opts in every ecosystem
// root at once, because the global layer is common to all of them. That is why
// DiscoverTargets requires the named plan's directory to EXIST rather than
// merely be computable — existence is what distinguishes the ecosystem the
// operator actually meant from the ones that only inherited the block.
//
// A missing file, a missing block, or a block with enabled = false all yield a
// disabled Config and no error: not opting in is the normal case, not a
// failure. Only a malformed grove.toml is an error, and even then the caller
// treats it as "supervise nothing" after logging — a typo in an unrelated block
// must not stop the daemon from booting.
func LoadConfig(dir string) (*Config, error) {
	if strings.TrimSpace(dir) == "" {
		return &Config{}, nil
	}
	cfg, err := coreconfig.LoadFrom(dir)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &Config{}, nil
	}
	var parsed Config
	if err := cfg.UnmarshalExtension("assistant", &parsed); err != nil {
		return nil, err
	}
	return parsed.withDefaults(), nil
}

// withDefaults fills the zero values that have a sensible default. It does NOT
// default Plan: a supervisor that guesses which plan is the assistant could
// resume the wrong chain, so an enabled block without a plan is inert.
func (c *Config) withDefaults() *Config {
	out := *c
	out.Plan = strings.TrimSpace(out.Plan)
	out.Provider = strings.TrimSpace(out.Provider)
	if out.IdleMinutes <= 0 {
		out.IdleMinutes = DefaultIdleMinutes
	}
	if strings.TrimSpace(out.Channel) == "" {
		out.Channel = DefaultChannel
	}
	if out.MaxChainResetsPerDay <= 0 {
		out.MaxChainResetsPerDay = DefaultMaxChainResetsPerDay
	}
	return &out
}

// Active reports whether this config asks the daemon to supervise anything.
func (c *Config) Active() bool {
	return c != nil && c.Enabled && c.Plan != ""
}
