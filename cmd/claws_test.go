package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadChannelState covers the source-of-truth fix: `groved claws` used to
// read routing.json + signal_routes.json, which the channels manager
// consolidated into state.json. Against a current daemon that showed empty
// tables and "0 claws" while real routes existed.
func TestLoadChannelState(t *testing.T) {
	writeState := func(t *testing.T, body string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("XDG_STATE_HOME", home)
		t.Setenv("HOME", home)
		dir := channelsDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("reads all three tables", func(t *testing.T) {
		writeState(t, `{
		  "inbound_routes": {"steward-66dd4eb3": "/tmp/groved-grovetools-abcd1234.sock"},
		  "quote_routes": {"1754000000000": "steward-66dd4eb3"},
		  "session_delivery": {"steward-66dd4eb3": {"mux": "tmux", "tmux_target": "grovetools:job-steward"}}
		}`)

		state, err := loadChannelState()
		if err != nil {
			t.Fatalf("loadChannelState: %v", err)
		}
		if got := state.InboundRoutes["steward-66dd4eb3"]; got == "" {
			t.Errorf("inbound route missing: %+v", state.InboundRoutes)
		}
		if got := state.QuoteRoutes["1754000000000"]; got != "steward-66dd4eb3" {
			t.Errorf("quote route = %q", got)
		}
		d, ok := state.SessionDelivery["steward-66dd4eb3"]
		if !ok || d.Mux != "tmux" || d.TmuxTarget != "grovetools:job-steward" {
			t.Errorf("session delivery = %+v (ok=%v)", d, ok)
		}
	})

	t.Run("a state file with only session_delivery still reports a claw", func(t *testing.T) {
		// This is the shape a single-ecosystem laptop actually has: one clawed
		// standing agent, no cross-daemon routes, no recent outbound. The old
		// reader printed "Total distinct claw-designated sessions: 0" for it.
		writeState(t, `{"inbound_routes":{},"quote_routes":{},"session_delivery":{"steward-66dd4eb3":{"mux":"tmux","tmux_target":"grovetools:job-steward"}}}`)

		state, err := loadChannelState()
		if err != nil {
			t.Fatalf("loadChannelState: %v", err)
		}
		unique := map[string]bool{}
		for j := range state.InboundRoutes {
			unique[j] = true
		}
		for _, j := range state.QuoteRoutes {
			unique[j] = true
		}
		for j := range state.SessionDelivery {
			unique[j] = true
		}
		if len(unique) != 1 {
			t.Errorf("distinct claws = %d, want 1", len(unique))
		}
	})

	t.Run("a missing state file is the pre-first-claw state, not an error", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("XDG_STATE_HOME", home)
		t.Setenv("HOME", home)

		state, err := loadChannelState()
		if err != nil {
			t.Fatalf("loadChannelState: %v", err)
		}
		if len(state.InboundRoutes) != 0 || len(state.QuoteRoutes) != 0 || len(state.SessionDelivery) != 0 {
			t.Errorf("state = %+v, want empty", state)
		}
	})

	t.Run("malformed JSON errors instead of pretending the tables are empty", func(t *testing.T) {
		writeState(t, "{not json")
		if _, err := loadChannelState(); err == nil {
			t.Error("want a parse error")
		}
	})
}
