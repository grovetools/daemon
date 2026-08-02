package cmd

// Custody tests for `[forge] token_command`. Two properties are load-bearing
// and both are asserted here rather than left to review: the token never
// escapes into an error string, and one sweep costs at most one command run.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForgeTokenResolverCachesWithinTTL(t *testing.T) {
	r := newForgeTokenResolver("irrelevant — the run seam is stubbed")
	base := time.Unix(1_700_000_000, 0)
	now := base
	r.now = func() time.Time { return now }
	r.ttl = time.Minute
	r.run = func(context.Context, string, time.Duration) (string, error) {
		return fmt.Sprintf("token-%d", now.Unix()), nil
	}

	first, err := r.Token(context.Background())
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Several more calls inside the window: the forgejo provider asks per
	// REQUEST, and a sweep makes many.
	for i := 0; i < 5; i++ {
		now = base.Add(time.Duration(i) * time.Second)
		got, err := r.Token(context.Background())
		if err != nil {
			t.Fatalf("cached resolve %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("cached resolve %d returned %q, want the cached %q", i, got, first)
		}
	}
	if r.runs != 1 {
		t.Errorf("command ran %d times inside the TTL, want 1", r.runs)
	}

	// Past the TTL the command runs again, so a rotated credential is picked
	// up without a daemon restart.
	now = base.Add(2 * time.Minute)
	rotated, err := r.Token(context.Background())
	if err != nil {
		t.Fatalf("post-TTL resolve: %v", err)
	}
	if rotated == first {
		t.Error("the token was not re-resolved after the TTL expired")
	}
	if r.runs != 2 {
		t.Errorf("command ran %d times total, want 2", r.runs)
	}
}

func TestForgeTokenResolverFailureIsNotCachedAndLeaksNothing(t *testing.T) {
	const secret = "hunter2-do-not-print-me"
	r := newForgeTokenResolver("echo " + secret)
	r.now = time.Now
	calls := 0
	r.run = func(context.Context, string, time.Duration) (string, error) {
		calls++
		// A realistic hostile case: the underlying error text carries the
		// command line, which carries the secret.
		return "", errors.New("sh -c echo " + secret + ": exit status 1")
	}

	for i := 0; i < 2; i++ {
		_, err := r.Token(context.Background())
		if err == nil {
			t.Fatal("a failing token_command returned no error")
		}
	}
	if calls != 2 {
		t.Errorf("failures were cached: command ran %d times, want 2", calls)
	}

	// The resolver's OWN error text (the one that reaches logs and the poller
	// cache) is built by runForgeTokenCommand, not by the seam above; assert
	// on the real one.
	script := writeFailingTokenScript(t, secret)
	_, err := runForgeTokenCommand(context.Background(), script, 10*time.Second)
	if err == nil {
		t.Fatal("a failing script produced no error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the token leaked into the error text: %q", err)
	}
	if strings.Contains(err.Error(), script) {
		t.Fatalf("the command path leaked into the error text: %q", err)
	}
	if !strings.Contains(err.Error(), "token_command") {
		t.Errorf("error %q does not name the config key that failed", err)
	}
}

func TestForgeTokenResolverEmptyOutputIsAnError(t *testing.T) {
	r := newForgeTokenResolver("true")
	r.now = time.Now
	r.run = func(context.Context, string, time.Duration) (string, error) { return "", nil }
	if _, err := r.Token(context.Background()); err == nil {
		t.Fatal("an empty token_command output was accepted as a token")
	}
}

func TestForgeTokenResolverNoCommandIsUnauthenticated(t *testing.T) {
	// A public instance needs no token; the provider treats ("", nil) as
	// "send unauthenticated", which is the correct behavior and not an error.
	var r *forgeTokenResolver
	tok, err := r.Token(context.Background())
	if err != nil || tok != "" {
		t.Fatalf("nil resolver returned (%q, %v), want (\"\", nil)", tok, err)
	}
}

func TestRunForgeTokenCommandTimesOut(t *testing.T) {
	start := time.Now()
	_, err := runForgeTokenCommand(context.Background(), "sleep 30", 200*time.Millisecond)
	if err == nil {
		t.Fatal("a hung token_command did not time out")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not name the timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout took %s — the bound is not being enforced", elapsed)
	}
}

func TestRunForgeTokenCommandTrimsOutput(t *testing.T) {
	got, err := runForgeTokenCommand(context.Background(), "printf '  abc123\\n\\n'", 10*time.Second)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "abc123" {
		t.Errorf("token = %q, want %q (trimmed)", got, "abc123")
	}
}

func writeFailingTokenScript(t *testing.T, secret string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token-fails.sh")
	body := fmt.Sprintf("#!/bin/sh\necho %q >&2\nexit 3\n", secret)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}
