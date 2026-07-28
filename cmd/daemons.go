package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/paths"
	"github.com/spf13/cobra"
)

// daemonEntry is a single entry in the enumerated daemon list.
//
// R3 moved the struct and the scan itself to core/pkg/daemon so non-CLI
// consumers (the inspector's Daemons fleet tab, which cannot import this
// binary's cmd package) enumerate daemons through exactly the same code.
// The local name and the thin wrappers below are kept so the ~8 existing call
// sites in this package read unchanged.
type daemonEntry = coredaemon.FleetEntry

// scopeSidecarPath returns the path to the .scope sidecar next to a pidfile.
func scopeSidecarPath(pidPath string) string {
	return coredaemon.ScopeSidecarPath(pidPath)
}

// enumerateDaemons scans StateDir() for groved*.pid files and returns a
// summary of each. Stale entries (pidfile present, process gone) are
// returned with Running=false so callers can decide how to handle them.
func enumerateDaemons() ([]daemonEntry, error) {
	return coredaemon.EnumerateDaemons()
}

// scopeFromPidFilename extracts the scope name from a pidfile basename.
func scopeFromPidFilename(name string) string {
	return coredaemon.ScopeFromPidFilename(name)
}

// scopeHashFromPidFilename extracts the 8-hex scope hash suffix from a pidfile
// basename, or "" for the unscoped daemon / a malformed name.
func scopeHashFromPidFilename(name string) string {
	return coredaemon.ScopeHashFromPidFilename(name)
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
