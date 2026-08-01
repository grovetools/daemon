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

func TestLoadConfig(t *testing.T) {
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
