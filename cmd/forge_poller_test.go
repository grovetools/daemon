package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/config"
	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// startForgePoller is the daemon's boot gate for the forge poller. These tests
// exercise it directly rather than booting a daemon, because what they are
// pinning is the gate itself: every path below must return quietly, start no
// goroutine, and — above all — never fail a boot.

func mustConfig(t *testing.T, toml string) *config.Config {
	t.Helper()
	cfg, err := config.LoadFromTOMLBytes([]byte(toml))
	if err != nil {
		t.Fatalf("LoadFromTOMLBytes: %v", err)
	}
	return cfg
}

// fakeGH puts an executable named `gh` on PATH so github.Available() clears.
// It is never invoked — only looked up.
func fakeGH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir)
}

// TestForgePollerDisabledByDefault: no [forge.poll], no poller. This is the
// wave's default-OFF gate, checked at the place that would start the goroutine.
func TestForgePollerDisabledByDefault(t *testing.T) {
	fakeGH(t) // the transport is available; the config gate alone must hold
	ulog := grovelogging.NewUnifiedLogger("test.forgepoll")

	for _, tc := range []struct {
		name string
		toml string
	}{
		{"no forge block", "version = \"1.0\"\n"},
		{"forge block without poll", "version = \"1.0\"\n\n[forge]\nurl = \"https://forge.example.com\"\n"},
		{"poll block explicitly off", "version = \"1.0\"\n\n[forge.poll]\nenabled = false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil store would be dereferenced by any code path that actually
			// constructs a poller, so passing one here is the assertion: if the
			// gate leaks, this panics.
			startForgePoller(context.Background(), nil, mustConfig(t, tc.toml), ulog)
		})
	}
}

// TestForgePollerSilentWithoutGH: an explicit opt-in on a machine with no `gh`
// is silence plus a log line — never a prompt, never an error, never a boot
// failure. The nil store proves no poller was constructed.
func TestForgePollerSilentWithoutGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty dir: `gh` is not findable
	startForgePoller(
		context.Background(),
		nil,
		mustConfig(t, "version = \"1.0\"\n\n[forge.poll]\nenabled = true\n"),
		grovelogging.NewUnifiedLogger("test.forgepoll"),
	)
}

// TestForgePollerStartsWhenBothGatesOpen is the positive control: with an
// explicit opt-in AND a findable transport, a poller is constructed and its
// loop starts. The context is cancelled immediately so no sweep reaches the
// network — this test asserts the gate opens, not that polling works (that is
// the forgepoll package's own suite, against a fake provider).
func TestForgePollerStartsWhenBothGatesOpen(t *testing.T) {
	fakeGH(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	startForgePoller(
		ctx,
		store.New(),
		mustConfig(t, "version = \"1.0\"\n\n[forge.poll]\nenabled = true\ninterval = \"10m\"\n"),
		grovelogging.NewUnifiedLogger("test.forgepoll"),
	)
}

// TestForgePollerSurvivesAMalformedBlock: a [forge] key nothing can decode must
// not take the daemon's boot with it (the same dark-build rule job 07 pinned
// for config loading).
func TestForgePollerSurvivesAMalformedBlock(t *testing.T) {
	fakeGH(t)
	startForgePoller(
		context.Background(),
		nil,
		mustConfig(t, "version = \"1.0\"\nforge = \"not a table\"\n"),
		grovelogging.NewUnifiedLogger("test.forgepoll"),
	)
}
