package theming

import "testing"

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"  Kanagawa ":     "kanagawa",
		"gruvbox_dark":    "gruvbox-dark",
		"Tokyonight Moon": "tokyonight-moon",
		"":                "",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildPayload_FamilyCarriesBothAppearances(t *testing.T) {
	payload, ok := BuildPayload("kanagawa")
	if !ok {
		t.Fatal("expected kanagawa to resolve")
	}
	if payload.Name != "kanagawa" || payload.Family != "kanagawa" {
		t.Errorf("unexpected header: %+v", payload)
	}
	if payload.Mode != "hex" {
		t.Errorf("expected mode hex, got %q", payload.Mode)
	}
	if payload.Dark == nil || payload.Dark.Appearance != "dark" {
		t.Fatalf("expected dark palette, got %+v", payload.Dark)
	}
	if payload.Light == nil || payload.Light.Appearance != "light" {
		t.Fatalf("expected light palette, got %+v", payload.Light)
	}
	// Every role color of a hex palette must be populated (Phase 1
	// derivation guarantees full palettes).
	for role, v := range map[string]string{
		"bg": payload.Dark.Bg, "fg": payload.Dark.Fg, "comment": payload.Dark.Comment,
		"border": payload.Dark.Border, "git.add": payload.Dark.Git.Add,
		"diagnostics.error":     payload.Dark.Diagnostics.Error,
		"terminal.black":        payload.Dark.Terminal.Black,
		"terminal.white_bright": payload.Dark.Terminal.WhiteBright,
	} {
		if len(v) != 7 || v[0] != '#' {
			t.Errorf("dark.%s = %q, want #rrggbb hex", role, v)
		}
	}
}

func TestBuildPayload_VariantOccupiesItsAppearanceSlot(t *testing.T) {
	payload, ok := BuildPayload("catppuccin-frappe")
	if !ok {
		t.Fatal("expected catppuccin-frappe to resolve")
	}
	if payload.Name != "catppuccin-frappe" || payload.Family != "catppuccin" {
		t.Errorf("unexpected header: %+v", payload)
	}
	if payload.Dark == nil || payload.Dark.Name != "catppuccin-frappe" {
		t.Errorf("expected the selected variant in the dark slot, got %+v", payload.Dark)
	}
	if payload.Light == nil || payload.Light.Name != "catppuccin-latte" {
		t.Errorf("expected the family light default in the light slot, got %+v", payload.Light)
	}
}

func TestBuildPayload_LegacyAliasResolves(t *testing.T) {
	// "gruvbox-dark" is a legacy alias for the adaptive "gruvbox" family.
	payload, ok := BuildPayload("gruvbox-dark")
	if !ok {
		t.Fatal("expected gruvbox-dark alias to resolve")
	}
	if payload.Family != "gruvbox" {
		t.Errorf("expected family gruvbox, got %q", payload.Family)
	}
	if payload.Dark == nil || payload.Dark.Name != "gruvbox-dark" {
		t.Errorf("expected gruvbox-dark palette in dark slot, got %+v", payload.Dark)
	}
}

func TestBuildPayload_ANSITerminalTheme(t *testing.T) {
	payload, ok := BuildPayload("terminal")
	if !ok {
		t.Fatal("expected terminal to resolve")
	}
	if payload.Mode != "ansi" {
		t.Fatalf("expected mode ansi, got %q", payload.Mode)
	}
	if payload.Dark == nil {
		t.Fatal("expected a dark palette for the terminal theme")
	}
	// ANSI palettes carry index strings, not hex.
	if payload.Dark.Bg != "0" || payload.Dark.Fg != "7" {
		t.Errorf("expected ANSI index strings, got bg=%q fg=%q", payload.Dark.Bg, payload.Dark.Fg)
	}
}

func TestBuildPayload_UnknownTheme(t *testing.T) {
	if _, ok := BuildPayload("definitely-not-a-theme"); ok {
		t.Error("expected unknown theme to fail")
	}
}
