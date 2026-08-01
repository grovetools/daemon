package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/paths"
	"github.com/spf13/cobra"
)

// newGrovedClawsCmd inspects the host-wide Signal claw state from the
// on-disk channel state and the running signal-cli process. Gives a quick
// read of which agents are claw-enabled and which daemons own their
// sessions, without needing to dial any daemon.
func newGrovedClawsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claws",
		Short: "Show active claws (agents with Signal enabled)",
		Long: `List every agent currently designated as a Signal claw.

Reads the consolidated channel state at
$(groved state-dir)/channels/state.json, which holds four tables:
  inbound_routes   — jobID→scoped-daemon-socket for cross-daemon inbound
                     forwarding (written by scoped daemons when a user
                     claws a session)
  quote_routes     — timestamp→jobID for recent outbound messages (used
                     for quote-reply routing)
  session_delivery — jobID→mux/target, the delivery address routing falls
                     back to after a daemon restart
  default_claw     — the ecosystem's standing assistant: where inbound goes
                     when no quote or @tag resolves it, and whose supervisor
                     is woken when it is not up

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

			state, stateErr := loadChannelState()

			// --- inbound_routes (cross-daemon inbound routes) -----------------
			fmt.Println("Cross-daemon inbound routes (state.json inbound_routes):")
			switch {
			case stateErr != nil:
				fmt.Printf("  error reading %s: %v\n", channelStatePath(), stateErr)
			case len(state.InboundRoutes) == 0:
				fmt.Println("  (empty — no claws registered by scoped daemons)")
			default:
				for _, jobID := range sortedKeys(state.InboundRoutes) {
					sock := state.InboundRoutes[jobID]
					scope := scopeFromSocketPath(sock)
					fmt.Printf("  %-30s → [%s] %s\n", jobID, displayScope(scope), filepath.Base(sock))
				}
			}
			fmt.Println()

			// --- quote_routes (timestamp → jobID for quote-reply) -------------
			fmt.Println("Outbound route table (state.json quote_routes):")
			switch {
			case stateErr != nil:
				fmt.Println("  (unavailable)")
			case len(state.QuoteRoutes) == 0:
				fmt.Println("  (empty — no recent outbound messages)")
			default:
				// Aggregate by jobID so the output stays compact.
				byJob := map[string]int{}
				for _, jobID := range state.QuoteRoutes {
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

			// --- session_delivery (persisted mux/target per claw) -------------
			fmt.Println("Persisted delivery targets (state.json session_delivery):")
			switch {
			case stateErr != nil:
				fmt.Println("  (unavailable)")
			case len(state.SessionDelivery) == 0:
				fmt.Println("  (empty — no claw has recorded a delivery target)")
			default:
				jobIDs := make([]string, 0, len(state.SessionDelivery))
				for j := range state.SessionDelivery {
					jobIDs = append(jobIDs, j)
				}
				sort.Strings(jobIDs)
				for _, j := range jobIDs {
					d := state.SessionDelivery[j]
					target := d.TmuxTarget
					if target == "" {
						target = d.PtyID
					}
					mux := d.Mux
					if mux == "" {
						mux = "(unset)"
					}
					fmt.Printf("  %-30s → %s %s\n", j, mux, target)
				}
			}
			fmt.Println()

			// --- default_claw (the standing assistant) ------------------------
			fmt.Println("Default claw (state.json default_claw):")
			switch {
			case stateErr != nil:
				fmt.Println("  (unavailable)")
			case state.DefaultClaw == nil || state.DefaultClaw.Plan == "":
				fmt.Println("  (none — unresolved inbound is dropped, not routed)")
			default:
				dc := state.DefaultClaw
				fmt.Printf("  plan %s  [%s]\n", dc.Plan, displayScope(dc.Scope))
				if dc.JobID == "" {
					fmt.Println("  registered claw: (none — inbound queues and wakes the supervisor)")
				} else {
					fmt.Printf("  registered claw: %s\n", dc.JobID)
				}
				if dc.Socket != "" {
					fmt.Printf("  supervisor socket: %s\n", filepath.Base(dc.Socket))
				}
			}
			fmt.Println()

			// --- Claw summary: union of all three tables ----------------------
			unique := map[string]bool{}
			if stateErr == nil {
				for j := range state.InboundRoutes {
					unique[j] = true
				}
				for _, j := range state.QuoteRoutes {
					unique[j] = true
				}
				for j := range state.SessionDelivery {
					unique[j] = true
				}
			}
			fmt.Printf("Total distinct claw-designated sessions: %d\n", len(unique))

			// --- Recent inbound routing log -----------------------------------
			client := daemon.NewGlobalClient()
			defer func() { _ = client.Close() }()
			if client.IsRunning() {
				ctx := context.Background()
				if chStatus, err := client.GetChannelStatus(ctx); err == nil && len(chStatus.RecentInbound) > 0 {
					fmt.Println("Recent inbound routing log:")
					for _, rec := range chStatus.RecentInbound {
						status := "✓"
						if !rec.Delivered {
							status = "✗"
						}
						detail := rec.TargetJob
						if rec.Error != "" {
							detail = rec.Error
						}
						fmt.Printf("  %s %s  %-22s  %-20s  %s\n",
							status,
							rec.Timestamp.Format("15:04:05"),
							rec.Sender,
							rec.Strategy,
							detail)
					}
					fmt.Println()
				}
			}

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
	cmd.AddCommand(newClawsCleanupCmd())
	return cmd
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
	age, _ := exec.Command("ps", "-o", "etime=", "-p", pid).Output() //nolint:gosec // G204: args are from daemon-internal pid
	var pidNum int
	_, _ = fmt.Sscanf(pid, "%d", &pidNum)
	return pidNum, strings.TrimSpace(string(age))
}

func channelsDir() string {
	return filepath.Join(paths.StateDir(), "channels")
}

// channelStatePath is the consolidated channel state file. It replaced the
// separate routing.json + signal_routes.json tables; reading those legacy
// paths against a current daemon produced empty tables and a "0 claws"
// summary while state.json held the real routes.
func channelStatePath() string {
	return filepath.Join(channelsDir(), "state.json")
}

// sessionDelivery mirrors channels.SessionDeliveryInfo. It is duplicated
// rather than imported because this command is deliberately daemon-free: it
// reads the file so it can report claw state with no daemon running at all.
type sessionDelivery struct {
	Mux        string `json:"mux,omitempty"`
	TmuxTarget string `json:"tmux_target,omitempty"`
	PtyID      string `json:"pty_id,omitempty"`
}

// defaultClaw mirrors channels.DefaultClawInfo: the ecosystem's standing
// assistant claw, where unresolved inbound goes instead of being dropped.
type defaultClaw struct {
	JobID  string `json:"job_id,omitempty"`
	Plan   string `json:"plan,omitempty"`
	Scope  string `json:"scope,omitempty"`
	Socket string `json:"socket,omitempty"`
}

// channelState mirrors channels.ChannelState. Integer map keys are encoded as
// JSON strings, so quote_routes decodes as map[string]string here.
type channelState struct {
	InboundRoutes   map[string]string          `json:"inbound_routes"`
	QuoteRoutes     map[string]string          `json:"quote_routes"`
	SessionDelivery map[string]sessionDelivery `json:"session_delivery"`
	DefaultClaw     *defaultClaw               `json:"default_claw,omitempty"`
}

func emptyChannelState() *channelState {
	return &channelState{
		InboundRoutes:   map[string]string{},
		QuoteRoutes:     map[string]string{},
		SessionDelivery: map[string]sessionDelivery{},
	}
}

// loadChannelState reads channels/state.json. A missing or empty file is the
// normal pre-first-claw state, not an error.
func loadChannelState() (*channelState, error) {
	data, err := os.ReadFile(channelStatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return emptyChannelState(), nil
		}
		return emptyChannelState(), err
	}
	state := emptyChannelState()
	if len(data) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(data, state); err != nil {
		return emptyChannelState(), err
	}
	if state.InboundRoutes == nil {
		state.InboundRoutes = map[string]string{}
	}
	if state.QuoteRoutes == nil {
		state.QuoteRoutes = map[string]string{}
	}
	if state.SessionDelivery == nil {
		state.SessionDelivery = map[string]sessionDelivery{}
	}
	return state, nil
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

func newClawsCleanupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cleanup",
		Short: "Purge ghost jobs and stale routes from the signal channel table",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := daemon.NewGlobalClient()
			defer func() { _ = client.Close() }()

			if !client.IsRunning() {
				fmt.Println("Daemon is not running.")
				return nil
			}

			fmt.Println("Cleaning up orphaned claw sessions...")

			resp, err := client.CleanupChannels(context.Background())
			if err != nil {
				return fmt.Errorf("failed to clean up channels: %w", err)
			}

			if resp.Purged > 0 {
				fmt.Printf("Purged %d stale route(s).\n", resp.Purged)
			} else {
				fmt.Println("No stale routes found. Routing table is clean.")
			}

			return nil
		},
	}
}
