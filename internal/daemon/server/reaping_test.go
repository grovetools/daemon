package server

import (
	"testing"

	"github.com/grovetools/core/pkg/sessions"
)

// TestLookupRegistryPID verifies the cross-daemon PID bridge: a session
// confirmed (and registered) by one daemon can have its real PID recovered by
// another daemon — the global registry is the shared link. This is what lets
// killSession SIGTERM, and the collector reap, a session whose in-store record
// carries PID 0.
func TestLookupRegistryPID(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())

	reg, err := sessions.NewFileSystemRegistry()
	if err != nil {
		t.Fatalf("NewFileSystemRegistry: %v", err)
	}
	const jobID = "coordinate-some-feature-abc123"
	if err := reg.Register(sessions.SessionMetadata{
		SessionID:       jobID,
		JobID:           jobID,
		ClaudeSessionID: "11111111-2222-3333-4444-555555555555",
		PID:             4242,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	md := lookupRegistryPID(jobID)
	if md == nil {
		t.Fatal("lookupRegistryPID returned nil for a registered job")
	}
	if md.PID != 4242 {
		t.Errorf("recovered PID = %d, want 4242", md.PID)
	}
	if md.ClaudeSessionID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("recovered native id = %q, want the registered UUID", md.ClaudeSessionID)
	}

	if got := lookupRegistryPID("no-such-job"); got != nil {
		t.Errorf("lookupRegistryPID for a missing job = %+v, want nil", got)
	}
}
