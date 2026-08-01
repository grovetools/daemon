package assistant

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGroveToml(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "grove.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// isolateGlobalConfig points the config cascade's global layer at an empty
// directory. LoadConfig reads through config.LoadFrom, so without this every
// case below would inherit the developer's own ~/.config/grove/grove.toml — and
// a machine that happens to enable [assistant] there would turn the
// "no block means no supervision" cases green-to-red for reasons having nothing
// to do with the code under test.
func isolateGlobalConfig(t *testing.T) string {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	dir := filepath.Join(cfgHome, "grove")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeGlobalGroveToml seeds the cascade's GLOBAL layer, the one an operator
// edits via `grove config`.
func writeGlobalGroveToml(t *testing.T, body string) {
	t.Helper()
	dir := isolateGlobalConfig(t)
	if err := os.WriteFile(filepath.Join(dir, "grove.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig(t *testing.T) {
	isolateGlobalConfig(t)

	t.Run("reads the assistant block", func(t *testing.T) {
		dir := writeGroveToml(t, `
workspaces = ["*"]

[assistant]
enabled = true
plan = "steward"
provider = "grove-agent"
skills = ["grove-worktree-analyzer", "grove-feature-coordinator-v2"]
idle_minutes = 45
handoff_threshold = 70
handoff_max = 20
`)
		cfg, err := LoadConfig(dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if !cfg.Active() {
			t.Fatal("config should be active")
		}
		if cfg.Plan != "steward" || cfg.Provider != "grove-agent" {
			t.Errorf("cfg = %+v", cfg)
		}
		if len(cfg.Skills) != 2 {
			t.Errorf("skills = %v", cfg.Skills)
		}
		if cfg.IdleMinutes != 45 || cfg.HandoffThreshold != 70 || cfg.HandoffMax != 20 {
			t.Errorf("cfg = %+v", cfg)
		}
		// Defaults fill in what the block left out.
		if cfg.Channel != DefaultChannel {
			t.Errorf("channel = %q, want %q", cfg.Channel, DefaultChannel)
		}
		if cfg.MaxChainResetsPerDay != DefaultMaxChainResetsPerDay {
			t.Errorf("reset budget = %d", cfg.MaxChainResetsPerDay)
		}
	})

	t.Run("no block means no supervision", func(t *testing.T) {
		cfg, err := LoadConfig(writeGroveToml(t, "workspaces = [\"*\"]\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Active() {
			t.Error("a grove.toml with no [assistant] block must not activate the supervisor")
		}
	})

	t.Run("a missing grove.toml is not an error", func(t *testing.T) {
		cfg, err := LoadConfig(t.TempDir())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Active() {
			t.Error("want inactive")
		}
	})

	t.Run("enabled without a plan stays inert", func(t *testing.T) {
		cfg, err := LoadConfig(writeGroveToml(t, "[assistant]\nenabled = true\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Active() {
			t.Error("a supervisor that guessed the plan could resume the wrong chain")
		}
	})

	t.Run("enabled = false is honored", func(t *testing.T) {
		cfg, err := LoadConfig(writeGroveToml(t, "[assistant]\nenabled = false\nplan = \"steward\"\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Active() {
			t.Error("want inactive")
		}
	})

	t.Run("a malformed grove.toml is an error, not a panic", func(t *testing.T) {
		if _, err := LoadConfig(writeGroveToml(t, "[assistant\nenabled = true\n")); err == nil {
			t.Error("want a parse error")
		}
	})

	t.Run("an empty directory is inert", func(t *testing.T) {
		cfg, err := LoadConfig("")
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Active() {
			t.Error("want inactive")
		}
	})
}

// TestLoadConfigReadsTheGlobalLayer pins the bug this cascade fix exists for.
//
// An operator who configures the assistant the way every other global grove
// preference is configured — `grove config`, which writes
// ~/.config/grove/grove.toml — used to get silence: LoadConfig read
// <root>/grove.toml directly, saw no [assistant] block, and the daemon reported
// `disabled (no [assistant] block enabled in grove.toml)` while naming a file
// the operator had in fact configured correctly.
func TestLoadConfigReadsTheGlobalLayer(t *testing.T) {
	writeGlobalGroveToml(t, `
[assistant]
enabled = true
plan = 'steward'
provider = 'pi'
skills = ['grove-steward']
idle_minutes = 30
handoff_threshold = 70
handoff_max = 20
`)
	// An ecosystem whose OWN grove.toml says nothing about the assistant.
	cfg, err := LoadConfig(writeGroveToml(t, "workspaces = [\"*\"]\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Active() {
		t.Fatal("a global [assistant] block must reach the ecosystem through the config cascade")
	}
	if cfg.Plan != "steward" || cfg.Provider != "pi" {
		t.Errorf("cfg = %+v", cfg)
	}

	// The snake_case keys are the real trap: UnmarshalExtension decodes with
	// mapstructure under TagName "yaml", so a field without a yaml tag falls
	// back to case-insensitive NAME matching, which cannot match an
	// underscored key. These three would silently read back as 0 — and
	// withDefaults would paper over idle_minutes, making the loss invisible.
	if cfg.IdleMinutes != 30 {
		t.Errorf("idle_minutes = %d, want 30 (snake_case key dropped by the decoder)", cfg.IdleMinutes)
	}
	if cfg.HandoffThreshold != 70 {
		t.Errorf("handoff_threshold = %d, want 70 (snake_case key dropped by the decoder)", cfg.HandoffThreshold)
	}
	if cfg.HandoffMax != 20 {
		t.Errorf("handoff_max = %d, want 20 (snake_case key dropped by the decoder)", cfg.HandoffMax)
	}
	if len(cfg.Skills) != 1 || cfg.Skills[0] != "grove-steward" {
		t.Errorf("skills = %v", cfg.Skills)
	}
}

// TestProjectLayerOverridesGlobal keeps the cascade's precedence honest: an
// ecosystem that disables the assistant locally must win over a global opt-in,
// or there would be no way to exempt one ecosystem from a global preference.
func TestProjectLayerOverridesGlobal(t *testing.T) {
	writeGlobalGroveToml(t, "[assistant]\nenabled = true\nplan = 'steward'\n")
	cfg, err := LoadConfig(writeGroveToml(t, "workspaces = [\"*\"]\n\n[assistant]\nenabled = false\nplan = 'steward'\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Active() {
		t.Error("a local enabled = false must override the global opt-in")
	}
}
