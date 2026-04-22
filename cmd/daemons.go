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
	Scope    string // "" for unscoped
	PidPath  string
	SockPath string
	PID      int
	Running  bool
	Age      time.Duration
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

	var entries []daemonEntry
	for _, pidPath := range matches {
		scope := scopeFromPidFilename(filepath.Base(pidPath))
		var sockPath string
		if scope == "" {
			sockPath = paths.SocketPath()
		} else {
			sockPath = paths.SocketPath(scope)
		}

		running, pid, _ := pidfile.IsRunning(pidPath)

		var age time.Duration
		if info, err := os.Stat(pidPath); err == nil {
			age = time.Since(info.ModTime()).Round(time.Second)
		}

		entries = append(entries, daemonEntry{
			Scope:    scope,
			PidPath:  pidPath,
			SockPath: sockPath,
			PID:      pid,
			Running:  running,
			Age:      age,
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

// displayScope returns a human-friendly scope label for display.
func displayScope(scope string) string {
	if scope == "" {
		return "(unscoped)"
	}
	return scope
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
