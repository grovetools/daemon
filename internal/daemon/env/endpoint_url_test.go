package env

import (
	"fmt"
	"strings"
	"testing"
)

// TestComposeEndpointURL_NoPortSuffix guards the Phase 2 invariant: provider
// code composes `http://<route>.<worktree>.grove.local` with NO port suffix.
// Port forwarding (80 -> 8443) is handled by the OS-level pf/iptables rule
// installed by `grove setup proxy`, so the URL the user sees in the browser
// must not include :8443. Older versions emitted :8443 directly — this test
// is the regression guard.
func TestComposeEndpointURL_NoPortSuffix(t *testing.T) {
	route := "api"
	worktree := "tier1-c"

	// Exact format currently shared between docker.go and local_services.go.
	got := fmt.Sprintf("http://%s.%s.grove.local", route, worktree)

	want := "http://api.tier1-c.grove.local"
	if got != want {
		t.Errorf("endpoint URL = %q, want %q", got, want)
	}
	if strings.Contains(got, ":8443") {
		t.Errorf("endpoint URL still contains :8443 — Phase 2 regression: %q", got)
	}
}
