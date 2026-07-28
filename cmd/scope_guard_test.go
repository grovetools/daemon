package cmd

import (
	"strings"
	"testing"
)

// An explicit --scope that resolves to nothing must be an error, never a
// silent fall-through to the global daemon: that fall-through is how a
// sandbox smoke daemon became a second global daemon and reaped agents it
// never owned.
func TestResolveExplicitScopeRefusesGlobalFallback(t *testing.T) {
	_, err := resolveExplicitScope("/tmp/gsm.BP2O/ws", "")
	if err == nil {
		t.Fatal("an explicit --scope resolving to \"\" must fail, not silently start a global daemon")
	}
	if !strings.Contains(err.Error(), "--scope-verbatim") {
		t.Fatalf("error must point at --scope-verbatim as the escape hatch, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/tmp/gsm.BP2O/ws") {
		t.Fatalf("error must name the offending path, got: %v", err)
	}
}

func TestResolveExplicitScopePassesThroughResolved(t *testing.T) {
	got, err := resolveExplicitScope("/repo/subdir", "/repo")
	if err != nil {
		t.Fatalf("a successfully resolved scope must pass through, got error: %v", err)
	}
	if got != "/repo" {
		t.Fatalf("expected resolved scope /repo, got %q", got)
	}
}

// No explicit scope means the caller wants the global daemon; the guard must
// not interfere with that path.
func TestResolveExplicitScopeAllowsEmptyRequest(t *testing.T) {
	got, err := resolveExplicitScope("", "")
	if err != nil {
		t.Fatalf("empty requested scope is the legit global case, got error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty scope, got %q", got)
	}
}
