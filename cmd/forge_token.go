package cmd

// Daemon-side custody of the forge API token.
//
// This file is the ONLY place in the ecosystem that executes `[forge]
// token_command`, and it lives in the daemon binary's own package on purpose.
// The custody rule (concepts/hosted-git-and-prs/forge-hosting.md, and the sync
// token precedent it cites) is that the global daemon resolves the credential
// and talks to the forge API; nb, git-viewer, nav and every TUI reach forge
// data through the daemon's unix socket and never hold a token. Putting the
// resolver in core/ would have made it importable by all of them — the sync
// config's ResolveToken is exactly that shape, and is exactly what this
// deliberately does not copy.
//
// Two properties the rest of the file exists to preserve:
//
//   - The token never lands in a log, an error, or the poller cache. Failures
//     name the CONFIG KEY, never the command line (a command like
//     `echo hunter2` carries the secret in its own text) and never the child's
//     output.
//   - A poll sweep does not become a secrets-manager stampede. The forgejo
//     provider asks for a token per REQUEST, and a sweep makes several requests
//     per repo; without caching, a `op read ...` command would run dozens of
//     times a minute.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// forgeTokenTimeout bounds one token_command run. Generous, because the
	// command may be an interactive-ish secrets manager doing a network call
	// or a biometric unlock; still bounded, because a hung command must cost
	// one sweep and not the poller.
	forgeTokenTimeout = 30 * time.Second
	// forgeTokenTTL is how long a resolved token is reused. It is a cache
	// lifetime, not a token lifetime: a rotated credential is picked up within
	// this window without a daemon restart, and a sweep costs at most one
	// command run.
	forgeTokenTTL = 5 * time.Minute
)

// forgeTokenResolver resolves and caches the forge API token.
type forgeTokenResolver struct {
	command string
	ttl     time.Duration
	timeout time.Duration

	// Test seams.
	now func() time.Time
	run func(ctx context.Context, command string, timeout time.Duration) (string, error)

	mu      sync.Mutex
	token   string
	expires time.Time
	// runs counts command executions, for tests that assert the cache actually
	// suppresses them.
	runs int
}

// newForgeTokenResolver builds a resolver for a non-empty token_command.
func newForgeTokenResolver(command string) *forgeTokenResolver {
	return &forgeTokenResolver{
		command: strings.TrimSpace(command),
		ttl:     forgeTokenTTL,
		timeout: forgeTokenTimeout,
		now:     time.Now,
		run:     runForgeTokenCommand,
	}
}

// Token satisfies forgejo.TokenFunc. A cached, unexpired token is returned
// without running anything; otherwise the command runs once under the mutex,
// so a burst of concurrent provider calls collapses onto a single execution.
//
// A failure clears the cache and returns an error that names neither the
// command nor its output. The forgejo provider turns that into a
// ClassUnavailable call failure, which the poller records as stale/unknown —
// never as "no pull requests".
func (r *forgeTokenResolver) Token(ctx context.Context) (string, error) {
	if r == nil || r.command == "" {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.token != "" && r.now().Before(r.expires) {
		return r.token, nil
	}

	token, err := r.run(ctx, r.command, r.timeout)
	r.runs++
	if err != nil {
		r.token, r.expires = "", time.Time{}
		return "", err
	}
	if token == "" {
		r.token, r.expires = "", time.Time{}
		return "", fmt.Errorf("[forge] token_command produced no output")
	}
	r.token = token
	r.expires = r.now().Add(r.ttl)
	return token, nil
}

// runForgeTokenCommand executes the command through `sh -c` (matching the sync
// token_command contract) and returns its trimmed stdout.
//
// Stdout is captured and never logged; stderr is discarded rather than
// surfaced, because a command that prints its secret to the wrong stream would
// otherwise leak it into an error string that reaches the poller cache and the
// HTTP read surface. The exit status is the whole diagnosis a caller gets, and
// the remediation is to run the command by hand.
func runForgeTokenCommand(ctx context.Context, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // G204: the command is the operator's own [forge] token_command
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("[forge] token_command timed out after %s", timeout)
		}
		// Deliberately not %w on err and deliberately no stderr: neither may
		// carry the command text or its output into a log line.
		return "", fmt.Errorf("[forge] token_command failed (%s)", exitDescription(err))
	}
	return strings.TrimSpace(string(out)), nil
}

// exitDescription renders just enough of a failure to act on: the exit status
// when there is one, the error's type-level message otherwise.
func exitDescription(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit status %d", ee.ExitCode())
	}
	return "could not run"
}
