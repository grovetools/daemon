package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/daemon/internal/daemon/pidfile"
	"github.com/spf13/cobra"
)

// daemonEntry is a single entry in the enumerated daemon list.
type daemonEntry struct {
	Scope      string // label only ("" for unscoped); the pidfile's middle segment
	ExactScope string // the daemon's exact resolved scope string, from its .scope sidecar ("" if unscoped or no sidecar)
	PidPath    string
	SockPath   string
	PID        int
	Running    bool
	Age        time.Duration
}

// scopeSidecarPath returns the path to the .scope sidecar that sits next to a
// daemon's pidfile and records its exact resolved scope string. The pidfile
// stores only the PID (every reader Atoi's the whole file), so the exact scope
// — which the successor daemon needs as GROVE_SCOPE so its child clients
// reconnect to the same socket — lives in this sibling file instead.
func scopeSidecarPath(pidPath string) string {
	return strings.TrimSuffix(pidPath, ".pid") + ".scope"
}

// enumerateDaemons scans StateDir() for groved*.pid files and returns a
// summary of each. Stale entries (pidfile present, process gone) are
// returned with Running=false so callers can decide how to handle them.
func enumerateDaemons() ([]daemonEntry, error) {
	dir := paths.StateDir()
	matches, err := filepath.Glob(filepath.Join(dir, "groved*.pid"))
	if err != nil {
		return nil, err
	}

	entries := make([]daemonEntry, 0, len(matches))
	for _, pidPath := range matches {
		scope := scopeFromPidFilename(filepath.Base(pidPath))
		// The socket sits next to the pidfile with the same stem. Derive it from
		// the real filename rather than re-hashing the extracted label: the label
		// is only filepath.Base(scope), so paths.SocketPath(label) re-hashes the
		// short label and yields a DIFFERENT hash than the daemon's actual socket
		// (which hashes the full scope path). That mismatch made `status` print a
		// socket that didn't exist and would mis-target any path-based action.
		sockPath := strings.TrimSuffix(pidPath, ".pid") + ".sock"

		var exactScope string
		if data, err := os.ReadFile(scopeSidecarPath(pidPath)); err == nil {
			exactScope = strings.TrimSpace(string(data))
		}

		running, pid, _ := pidfile.IsRunning(pidPath)

		var age time.Duration
		if info, err := os.Stat(pidPath); err == nil {
			age = time.Since(info.ModTime()).Round(time.Second)
		}

		entries = append(entries, daemonEntry{
			Scope:      scope,
			ExactScope: exactScope,
			PidPath:    pidPath,
			SockPath:   sockPath,
			PID:        pid,
			Running:    running,
			Age:        age,
		})
	}

	return entries, nil
}

// scopeFromPidFilename extracts the scope name from a pidfile basename.
// "groved.pid"                           → ""
// "groved-env-continued-e2435831.pid"    → "env-continued"
// The hash suffix is exactly 8 hex chars (see paths.scopedPath).
func scopeFromPidFilename(name string) string {
	name = strings.TrimSuffix(name, ".pid")
	if name == "groved" {
		return ""
	}
	if !strings.HasPrefix(name, "groved-") {
		return ""
	}
	rest := strings.TrimPrefix(name, "groved-")
	// Hash is the last 8 hex chars after a hyphen.
	idx := strings.LastIndex(rest, "-")
	if idx < 0 {
		return rest
	}
	return rest[:idx]
}

// scopeHashFromPidFilename extracts the 8-hex scope hash suffix from a pidfile
// basename, or "" for the unscoped daemon / a malformed name.
// "groved.pid"                        → ""
// "groved-env-continued-e2435831.pid" → "e2435831"
// The hash is the last hyphen-delimited segment (see paths.scopedPath).
func scopeHashFromPidFilename(name string) string {
	name = strings.TrimSuffix(name, ".pid")
	if !strings.HasPrefix(name, "groved-") {
		return ""
	}
	rest := strings.TrimPrefix(name, "groved-")
	idx := strings.LastIndex(rest, "-")
	if idx < 0 || idx+1 >= len(rest) {
		return ""
	}
	return rest[idx+1:]
}

// resolveUpgradeTarget builds the predicate that selects which running daemon
// `groved upgrade` targets, plus a human-readable description for errors.
//
// Precedence: global (unscoped) > scope (legacy label match) > cwdScope (the
// CWD-inferred ecosystem-boundary path). An empty cwdScope means the CWD is
// outside any ecosystem, so the unscoped daemon is the target.
//
// For the CWD path we prefer the exact scope string from a daemon's .scope
// sidecar (ExactScope); legacy daemons without a sidecar are matched by the
// 8-hex scope hash embedded in their pidfile name (paths.scopedPath scheme),
// recomputed via paths.PidFilePath so the hashing is not duplicated here.
func resolveUpgradeTarget(global bool, scope, cwdScope string) (func(daemonEntry) bool, string) {
	switch {
	case global:
		return func(e daemonEntry) bool { return e.Scope == "" }, "(unscoped)"
	case scope != "":
		lbl := scope
		return func(e daemonEntry) bool { return e.Scope == lbl }, fmt.Sprintf("scope %q", scope)
	case cwdScope == "":
		return func(e daemonEntry) bool { return e.Scope == "" }, "(unscoped, from cwd)"
	default:
		targetHash := scopeHashFromPidFilename(filepath.Base(paths.PidFilePath(cwdScope)))
		matches := func(e daemonEntry) bool {
			if e.ExactScope != "" {
				return e.ExactScope == cwdScope
			}
			return targetHash != "" && scopeHashFromPidFilename(filepath.Base(e.PidPath)) == targetHash
		}
		return matches, fmt.Sprintf("scope %q (from cwd)", cwdScope)
	}
}

// displayScope returns a human-friendly scope label for display.
func displayScope(scope string) string {
	if scope == "" {
		return "(unscoped)"
	}
	return scope
}

// trimStatusError collapses a channel error to a single status line: newlines
// become spaces and the result is truncated to keep `groved status` readable.
func trimStatusError(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	const maxLen = 160
	if len(s) > maxLen {
		return s[:maxLen-1] + "…"
	}
	return s
}

func newGrovedKillCmd() *cobra.Command {
	var waitSec int
	cmd := &cobra.Command{
		Use:   "kill <target>",
		Short: "Kill a running groved by scope name",
		Long: `Kill a running groved daemon, or several.

Targets:
  <scope-name>     Kill the daemon whose scope basename matches (e.g. env-continued)
  unscoped         Kill the global/unscoped daemon (groved.sock)
  global           Alias for "unscoped"
  scoped           Kill every scoped daemon; leave the unscoped global running
  all              Kill every running daemon

Sends SIGTERM, waits briefly, then SIGKILL if the process hasn't exited.
Stale pidfiles whose PIDs are already gone are removed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			entries, err := enumerateDaemons()
			if err != nil {
				return fmt.Errorf("enumerate: %w", err)
			}

			var toKill []daemonEntry
			for _, e := range entries {
				if !e.Running {
					// Clean up stale pidfiles; they can't be killed anyway.
					_ = os.Remove(e.PidPath)
					continue
				}
				switch target {
				case "all":
					toKill = append(toKill, e)
				case "scoped":
					if e.Scope != "" {
						toKill = append(toKill, e)
					}
				case "unscoped", "global":
					if e.Scope == "" {
						toKill = append(toKill, e)
					}
				default:
					if e.Scope == target {
						toKill = append(toKill, e)
					}
				}
			}

			if len(toKill) == 0 {
				fmt.Printf("No running daemon matched '%s'\n", target)
				return nil
			}

			waitDur := time.Duration(waitSec) * time.Second
			for _, e := range toKill {
				killOne(e, waitDur)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&waitSec, "wait", 2, "Seconds to wait for SIGTERM before escalating to SIGKILL")
	return cmd
}

func killOne(e daemonEntry, wait time.Duration) {
	// A definitive stop has no successor, so the daemon won't unlink its own
	// .scope sidecar (it leaves that to the upgrade path). Clean it up here once
	// we've signaled; the daemon releases the pidfile on SIGTERM.
	defer func() { _ = os.Remove(scopeSidecarPath(e.PidPath)) }()

	proc, err := os.FindProcess(e.PID)
	if err != nil {
		fmt.Printf("  [%s] find process %d: %v\n", displayScope(e.Scope), e.PID, err)
		return
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		fmt.Printf("  [%s] SIGTERM to %d: %v\n", displayScope(e.Scope), e.PID, err)
		return
	}
	fmt.Printf("  [%s] SIGTERM sent to PID %d\n", displayScope(e.Scope), e.PID)

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			// Process is gone.
			fmt.Printf("  [%s] exited cleanly\n", displayScope(e.Scope))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := proc.Signal(syscall.SIGKILL); err != nil {
		fmt.Printf("  [%s] SIGKILL to %d: %v\n", displayScope(e.Scope), e.PID, err)
		return
	}
	fmt.Printf("  [%s] escalated to SIGKILL on PID %d\n", displayScope(e.Scope), e.PID)
}
