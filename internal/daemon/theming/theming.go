// Package theming resolves the daemon's view of the global tui.theme value
// and builds the theme_changed SSE payload from core's theme registry. The
// groved ConfigWatcher wiring uses it to diff the resolved theme across
// config reloads, and the SSE handler uses it to stamp the current theme on
// the initial snapshot so a change during a disconnect isn't lost.
//
// Payload assembly itself lives in core (coredaemon.BuildThemePayload), so
// the daemon and grove.nvim's `internal theme` subcommand emit an identical
// wire shape from ONE implementation; this package only adds the daemon's
// global-layer config resolution on top.
package theming

import (
	"github.com/grovetools/core/config"
	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/tui/theme"
)

// NormalizeName canonicalizes a theme name so diffed and broadcast names
// match what theme.SetTheme resolves. It delegates to core's exported
// normalizer.
func NormalizeName(name string) string {
	return theme.NormalizeName(name)
}

// CurrentThemeName resolves tui.theme from the GLOBAL config layer
// (~/.config/grove/grove.toml plus modular fragments, later fragments
// winning). It deliberately ignores project layers — the daemon's
// ConfigWatcher only covers the global config dir and tui.theme is
// x-layer=global — and bypasses the hierarchical loader's TTL cache so a
// load right after an fsnotify event never sees a stale value.
func CurrentThemeName() string {
	layered, err := config.LoadLayered(paths.ConfigDir())
	if err != nil || layered == nil {
		return theme.DefaultThemeName
	}

	name := ""
	if layered.Global != nil && layered.Global.TUI != nil {
		name = layered.Global.TUI.Theme
	}
	for _, frag := range layered.GlobalFragments {
		if frag.Config != nil && frag.Config.TUI != nil && frag.Config.TUI.Theme != "" {
			name = frag.Config.TUI.Theme
		}
	}

	if normalized := theme.NormalizeName(name); normalized != "" {
		return normalized
	}
	return theme.DefaultThemeName
}

// BuildPayload assembles the theme_changed wire payload for a theme
// selection (family name, variant name, or legacy alias). It delegates to
// the shared implementation in core.
func BuildPayload(name string) (*coredaemon.ThemeChangedPayload, bool) {
	return coredaemon.BuildThemePayload(name)
}

// CurrentPayload resolves the current global theme and builds its payload.
// It returns nil when the configured theme is unknown to the registry.
func CurrentPayload() *coredaemon.ThemeChangedPayload {
	payload, ok := BuildPayload(CurrentThemeName())
	if !ok {
		return nil
	}
	return payload
}
