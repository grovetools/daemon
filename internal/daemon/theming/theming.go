// Package theming resolves the daemon's view of the global tui.theme value
// and builds the theme_changed SSE payload from core's theme registry. The
// groved ConfigWatcher wiring uses it to diff the resolved theme across
// config reloads, and the SSE handler uses it to stamp the current theme on
// the initial snapshot so a change during a disconnect isn't lost.
package theming

import (
	"strings"

	"github.com/grovetools/core/config"
	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/tui/theme"
)

// defaultThemeName mirrors the unexported default in core/tui/theme: the
// theme in effect when config does not set tui.theme.
const defaultThemeName = "kanagawa"

// NormalizeName mirrors core/tui/theme's unexported normalizeThemeName so
// diffed and broadcast names match what theme.SetTheme resolves.
func NormalizeName(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, " ", "-")
	normalized = strings.ReplaceAll(normalized, "_", "-")
	return normalized
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
		return defaultThemeName
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

	if normalized := NormalizeName(name); normalized != "" {
		return normalized
	}
	return defaultThemeName
}

// BuildPayload assembles the theme_changed wire payload for a theme
// selection (family name, variant name, or legacy alias). The selected
// variant occupies its own appearance slot; the family's default variant for
// the opposite appearance fills the other slot when the family has one.
func BuildPayload(name string) (*coredaemon.ThemeChangedPayload, bool) {
	selected, ok := theme.Lookup(name)
	if !ok {
		return nil, false
	}

	dark, light := familyDefaults(selected.Meta.Family)
	if selected.Meta.Appearance == "light" {
		light = &selected
	} else {
		dark = &selected
	}

	mode := "hex"
	if selected.Meta.ANSI {
		mode = "ansi"
	}

	return &coredaemon.ThemeChangedPayload{
		Name:   NormalizeName(name),
		Family: selected.Meta.Family,
		Mode:   mode,
		Dark:   wirePalette(dark),
		Light:  wirePalette(light),
	}, true
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

// familyDefaults finds the family's default palette per appearance,
// replicating the registry's rule (first variant by name wins unless a later
// one is flagged default). Lookup resolves legacy aliases before exact names,
// so a variant whose name is shadowed by an alias (e.g. "gruvbox-light" →
// family "gruvbox" → dark default) cannot be fetched; such slots are dropped
// rather than filled with the wrong appearance.
func familyDefaults(family string) (dark, light *theme.Palette) {
	for _, meta := range theme.List() {
		if meta.Family != family {
			continue
		}
		p, ok := theme.Lookup(meta.Name)
		if !ok || p.Meta.Name != meta.Name || p.Meta.Appearance != meta.Appearance {
			continue // alias-shadowed name; skip rather than mis-slot
		}
		switch meta.Appearance {
		case "dark":
			if dark == nil || (meta.Default && !dark.Meta.Default) {
				dark = &p
			}
		case "light":
			if light == nil || (meta.Default && !light.Meta.Default) {
				light = &p
			}
		}
	}
	return dark, light
}

// wirePalette maps a fully derived theme.Palette onto the JSON wire struct.
func wirePalette(p *theme.Palette) *coredaemon.ThemePalette {
	if p == nil {
		return nil
	}
	c := p.Colors
	t := p.Terminal
	return &coredaemon.ThemePalette{
		Name:       p.Meta.Name,
		Variant:    p.Meta.Variant,
		Appearance: p.Meta.Appearance,

		Bg:          c.Bg,
		BgDark:      c.BgDark,
		BgHighlight: c.BgHighlight,
		BgVisual:    c.BgVisual,

		Fg:        c.Fg,
		FgDark:    c.FgDark,
		FgGutter:  c.FgGutter,
		FgInverse: c.FgInverse,
		Comment:   c.Comment,
		Border:    c.Border,

		Red:     c.Red,
		Green:   c.Green,
		Yellow:  c.Yellow,
		Blue:    c.Blue,
		Magenta: c.Magenta,
		Cyan:    c.Cyan,
		Orange:  c.Orange,
		Purple:  c.Purple,

		Git: coredaemon.ThemeGitColors{
			Add:    c.Git.Add,
			Change: c.Git.Change,
			Delete: c.Git.Delete,
		},
		Diagnostics: coredaemon.ThemeDiagnosticColors{
			Error:   c.Diagnostics.Error,
			Warning: c.Diagnostics.Warning,
			Info:    c.Diagnostics.Info,
			Hint:    c.Diagnostics.Hint,
		},
		Terminal: coredaemon.ThemeTerminalColors{
			Black:         t.Black,
			Red:           t.Red,
			Green:         t.Green,
			Yellow:        t.Yellow,
			Blue:          t.Blue,
			Magenta:       t.Magenta,
			Cyan:          t.Cyan,
			White:         t.White,
			BlackBright:   t.BlackBright,
			RedBright:     t.RedBright,
			GreenBright:   t.GreenBright,
			YellowBright:  t.YellowBright,
			BlueBright:    t.BlueBright,
			MagentaBright: t.MagentaBright,
			CyanBright:    t.CyanBright,
			WhiteBright:   t.WhiteBright,
		},
	}
}
