package assistant

import (
	"strings"
	"testing"
)

// TestWithAgentTargetBuildsTheFlag: the supervisor's whole view of
// orchestration is an argv, and this is the one function that turns a resolved
// target into the flag flow reads. Every test above it drives a fake, so
// nothing else would notice the flag going missing — and a missing flag is
// silent: flow falls back to deriving the target from groved's environment,
// which answers "tmux" and routes the assistant somewhere its pane cannot
// reach.
func TestWithAgentTargetBuildsTheFlag(t *testing.T) {
	base := []string{"plan", "resume", "/plans/steward/01-steward.md"}

	got := withAgentTarget("native", base...)
	want := append(append([]string{}, base...), "--agent-target", "native")
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", got, want)
	}

	// An empty target leaves the flag off, restoring flow's own derivation.
	// That is the right degradation for a caller with no opinion, and it is
	// what keeps this function safe to apply to every verb unconditionally.
	for _, empty := range []string{"", "   "} {
		if got := withAgentTarget(empty, base...); strings.Join(got, " ") != strings.Join(base, " ") {
			t.Errorf("argv with target %q = %v, want the bare %v", empty, got, base)
		}
	}
}
