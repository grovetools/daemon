package server

import (
	"encoding/json"
	"testing"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/theming"
)

// TestConvertThemeChangedRoundTrip proves the theme_changed wire layers
// agree: store.Update → convertToAPIUpdate → JSON → core daemon.StateUpdate
// → ParseThemeChanged, with the resolved palette surviving intact. A missing
// convertToAPIUpdate case silently drops the event before the SSE wire.
func TestConvertThemeChangedRoundTrip(t *testing.T) {
	payload, ok := theming.BuildPayload("kanagawa")
	if !ok {
		t.Fatal("expected kanagawa to resolve in the theme registry")
	}

	apiUpdate := convertToAPIUpdate(store.Update{
		Type:    store.UpdateThemeChanged,
		Source:  "config",
		Payload: payload,
	})
	if apiUpdate == nil {
		t.Fatal("convertToAPIUpdate dropped theme_changed — the event never reaches the SSE wire")
	}
	if apiUpdate.UpdateType != "theme_changed" {
		t.Fatalf("expected update_type %q, got %q", "theme_changed", apiUpdate.UpdateType)
	}

	data, err := json.Marshal(apiUpdate)
	if err != nil {
		t.Fatalf("marshal apiStateUpdate: %v", err)
	}

	// Wire-shape assertions on the raw JSON.
	var wire map[string]interface{}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal wire JSON: %v", err)
	}
	if wire["update_type"] != "theme_changed" {
		t.Errorf("wire update_type = %v", wire["update_type"])
	}
	p, ok := wire["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("wire payload missing or wrong shape: %v", wire["payload"])
	}
	for _, key := range []string{"name", "family", "mode", "dark"} {
		if _, ok := p[key]; !ok {
			t.Errorf("wire payload missing key %q", key)
		}
	}

	// Client-side decode via the shared helper.
	var stateUpdate coredaemon.StateUpdate
	if err := json.Unmarshal(data, &stateUpdate); err != nil {
		t.Fatalf("unmarshal into StateUpdate: %v", err)
	}
	decoded, ok := coredaemon.ParseThemeChanged(stateUpdate)
	if !ok {
		t.Fatal("ParseThemeChanged failed to decode the wire update")
	}
	if decoded.Name != payload.Name || decoded.Family != payload.Family || decoded.Mode != payload.Mode {
		t.Errorf("decoded header mismatch: got %+v, want %+v", decoded, payload)
	}
	if decoded.Dark == nil || decoded.Dark.Bg != payload.Dark.Bg || decoded.Dark.Name != payload.Dark.Name {
		t.Errorf("dark palette did not survive the round trip: %+v", decoded.Dark)
	}
	if (decoded.Light == nil) != (payload.Light == nil) {
		t.Errorf("light palette presence mismatch: got %v, want %v", decoded.Light, payload.Light)
	}
	if payload.Light != nil && decoded.Light.Bg != payload.Light.Bg {
		t.Errorf("light palette bg mismatch: got %q, want %q", decoded.Light.Bg, payload.Light.Bg)
	}
}

// TestInitialSnapshotCarriesTheme proves the "initial" SSE snapshot decodes
// through ParseThemeChanged, covering the reconnect edge case where a theme
// change happened while a client was disconnected.
func TestInitialSnapshotCarriesTheme(t *testing.T) {
	payload, ok := theming.BuildPayload("terminal")
	if !ok {
		t.Fatal("expected terminal to resolve in the theme registry")
	}
	if payload.Mode != "ansi" {
		t.Fatalf("expected terminal theme mode %q, got %q", "ansi", payload.Mode)
	}

	initial := &apiStateUpdate{
		UpdateType: "initial",
		Theme:      payload,
	}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("marshal initial snapshot: %v", err)
	}

	var stateUpdate coredaemon.StateUpdate
	if err := json.Unmarshal(data, &stateUpdate); err != nil {
		t.Fatalf("unmarshal into StateUpdate: %v", err)
	}
	decoded, ok := coredaemon.ParseThemeChanged(stateUpdate)
	if !ok {
		t.Fatal("ParseThemeChanged failed to decode the initial snapshot theme")
	}
	if decoded.Name != "terminal" || decoded.Mode != "ansi" {
		t.Errorf("decoded initial theme mismatch: %+v", decoded)
	}
	if decoded.Dark == nil || decoded.Dark.Bg != "0" {
		t.Errorf("expected ANSI index strings in the terminal palette, got %+v", decoded.Dark)
	}
}
