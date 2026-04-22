package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/grovetools/core/pkg/paths"
	"github.com/spf13/cobra"
)

// newGrovedClawsCmd inspects the host-wide Signal claw state from the
// two on-disk routing tables and the running signal-cli process. Gives
// a quick read of which agents are claw-enabled and which daemons own
// their sessions, without needing to dial any daemon.
func newGrovedClawsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "claws",
		Short: "Show active claws (agents with Signal enabled)",
		Long: `List every agent currently designated as a Signal claw.

Reads two on-disk tables under $(groved state-dir)/channels/:
  signal_routes.json — timestamp→jobID for recent outbound messages
                       (used for quote-reply routing)
  routing.json       — jobID→scoped-daemon-socket for cross-daemon
                       inbound forwarding (written by scoped daemons
                       when a user claws a session)

Also reports signal-cli daemon health and the list of running groveds
so you can tell which daemon owns each claw-enabled session.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --- signal-cli daemon health -------------------------------------
			sigPID, sigAge := signalCLIProcess()
			fmt.Println("signal-cli daemon:")
			if sigPID == 0 {
				fmt.Println("  not running (spawns on first outbound or on global-daemon boot with Signal.Enabled)")
			} else {
				fmt.Printf("  PID %d  running %s\n", sigPID, sigAge)
			}
			fmt.Println()

			// --- routing.json (cross-daemon inbound routes) -------------------
			routes, routesErr := loadRoutingJSON()
			fmt.Println("Cross-daemon inbound routes (routing.json):")
			if routesErr != nil {
				fmt.Printf("  error reading file: %v\n", routesErr)
			} else if len(routes) == 0 {
				fmt.Println("  (empty — no claws registered by scoped daemons)")
			} else {
				jobIDs := sortedKeys(routes)
				for _, jobID := range jobIDs {
					sock := routes[jobID]
					scope := scopeFromSocketPath(sock)
					fmt.Printf("  %-30s → [%s] %s\n", jobID, displayScope(scope), filepath.Base(sock))
				}
			}
			fmt.Println()

			// --- signal_routes.json (timestamp → jobID for quote-reply) -------
			sigRoutes, sigRoutesErr := loadSignalRoutesJSON()
			fmt.Println("Outbound route table (signal_routes.json):")
			if sigRoutesErr != nil {
				fmt.Printf("  error reading file: %v\n", sigRoutesErr)
			} else if len(sigRoutes) == 0 {
				fmt.Println("  (empty — no recent outbound messages)")
			} else {
				// Aggregate by jobID so the output stays compact.
				byJob := map[string]int{}
				for _, jobID := range sigRoutes {
					byJob[jobID]++
				}
				jobIDs := make([]string, 0, len(byJob))
				for j := range byJob {
					jobIDs = append(jobIDs, j)
				}
				sort.Strings(jobIDs)
				for _, j := range jobIDs {
					fmt.Printf("  %-30s (%d outbound msgs tracked for quote-reply)\n", j, byJob[j])
				}
			}
			fmt.Println()

			// --- Claw summary: union of both tables ---------------------------
			unique := map[string]bool{}
			for j := range routes {
				unique[j] = true
			}
			for _, j := range sigRoutes {
				unique[j] = true
			}
			fmt.Printf("Total distinct claw-designated sessions: %d\n", len(unique))

			// --- Sanity check: daemon process listing -------------------------
			entries, _ := enumerateDaemons()
			running := 0
			for _, e := range entries {
				if e.Running {
					running++
				}
			}
			fmt.Printf("Running daemons: %d (see `groved status` for details)\n", running)

			return nil
		},
	}
}

// signalCLIProcess returns (pid, elapsed) for a running signal-cli daemon
// subprocess, or (0, "") if none is running.
func signalCLIProcess() (int, string) {
	out, err := exec.Command("pgrep", "-f", "signal-cli.*daemon.*--socket").Output()
	if err != nil || len(out) == 0 {
		return 0, ""
	}
	// Take the first matching PID only — there should only be one.
	pid := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	if pid == "" {
		return 0, ""
	}
	age, _ := exec.Command("ps", "-o", "etime=", "-p", pid).Output()
	var pidNum int
	fmt.Sscanf(pid, "%d", &pidNum)
	return pidNum, strings.TrimSpace(string(age))
}

func channelsDir() string {
	return filepath.Join(paths.StateDir(), "channels")
}

func loadRoutingJSON() (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(channelsDir(), "routing.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	if len(data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func loadSignalRoutesJSON() (map[string]string, error) {
	// File stores numeric timestamps as JSON object keys; values are jobIDs.
	data, err := os.ReadFile(filepath.Join(channelsDir(), "signal_routes.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	// routeTable is map[int64]string on the Go side, but json.Marshal
	// encodes integer map keys as strings, so we decode into map[string]string
	// and keep it that way for display.
	out := map[string]string{}
	if len(data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// scopeFromSocketPath extracts the scope-basename from a socket path like
// /Users/.../groved-env-continued-e2435831.sock. Returns "" if unscoped.
func scopeFromSocketPath(sockPath string) string {
	base := filepath.Base(sockPath)
	base = strings.TrimSuffix(base, ".sock")
	if base == "groved" {
		return ""
	}
	if !strings.HasPrefix(base, "groved-") {
		return ""
	}
	rest := strings.TrimPrefix(base, "groved-")
	idx := strings.LastIndex(rest, "-")
	if idx < 0 {
		return rest
	}
	return rest[:idx]
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
