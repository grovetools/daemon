package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/util/delegation"
)

// Job is the subset of a flow job the supervisor reads out of
// `flow status --json`. The full envelope carries far more; binding narrowly
// keeps the argv contract from turning into a schema dependency.
type Job struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	Type         string    `json:"type"`
	Filename     string    `json:"filename"`
	FilePath     string    `json:"file_path"`
	HandoffDepth int       `json:"handoff_depth"`
	HandoffMax   int       `json:"handoff_max"`
	UpdatedAt    time.Time `json:"updated_at"`
	CreatedAt    time.Time `json:"created_at"`
}

// planStatus is the `flow status --json` envelope.
type planStatus struct {
	Plan string `json:"plan"`
	Jobs []Job  `json:"jobs"`
}

// AddJobRequest describes the root job a chain reset creates.
type AddJobRequest struct {
	PlanDir          string
	Title            string
	Provider         string
	Model            string
	Skills           []string
	CoordMode        string
	HandoffThreshold int
	HandoffMax       int
	// Prompt is written to a temp file and passed as --prompt-file: the seeded
	// prompt carries a whole handoff spec, which has no business on an argv.
	Prompt string
}

// AddedJob is the `flow plan add --json` result.
type AddedJob struct {
	Path  string `json:"path"`
	ID    string `json:"id"`
	Title string `json:"title"`
}

// FlowCLI is the supervisor's whole view of orchestration: the `flow` CLI's
// argv. Nothing here links flow's internals, so flow can restructure freely as
// long as these commands keep their contract — the same doctrine the grove-pi
// extensions follow.
//
// Every plan address is an ABSOLUTE PLAN DIRECTORY, never `--at <plan>`.
// `--at` resolves through the worktree registry, and the assistant plan is
// deliberately worktree-less (spec §3.1), so it has no registry entry to find.
// `--dir` is worse than useless here: it is a deprecated alias that can
// silently resolve a DIFFERENT plan. The absolute directory is the one form
// that means exactly one thing.
// Every launching verb takes an explicit agentTarget for the reason
// Config.AgentTarget documents: flow derives routing from the SUBMITTING
// process's environment, and this submitter is a daemon with no mux markers at
// all, so leaving it to the derivation routes every continuation to tmux —
// where pi cannot resume and the assistant pane has no PTY to attach.
type FlowCLI interface {
	// PlanJobs returns the jobs of the plan at planDir, newest last.
	PlanJobs(ctx context.Context, planDir string) ([]Job, error)
	// Resume relaunches a job in place against its existing agent session.
	Resume(ctx context.Context, jobPath, agentTarget string) error
	// Retry resets a job to pending and immediately submits it (-r).
	Retry(ctx context.Context, jobPath, agentTarget string) error
	// AddJob creates a new root job and returns its path.
	AddJob(ctx context.Context, req AddJobRequest) (*AddedJob, error)
	// Run submits a job to the daemon and returns without waiting.
	Run(ctx context.Context, jobPath, agentTarget string) error
	// Claw re-applies channel + autonomous flags to a running agent.
	Claw(ctx context.Context, planDir, jobFile, channel, signalTarget string, idleMinutes int) error
}

// ExecFlowCLI is the real FlowCLI: it shells out through the grove delegator so
// user-configured aliases apply, exactly like every other in-tree flow caller.
type ExecFlowCLI struct {
	// Timeout bounds a single flow invocation. Zero uses DefaultFlowTimeout.
	Timeout time.Duration

	// HostSocket is this daemon's own socket, published to every flow child as
	// GROVE_HOST_DAEMON_SOCKET.
	//
	// It closes a loop that is invisible on a scoped daemon and fatal on the
	// global one. The supervisor decides "is the assistant up?" by reading its
	// OWN session store, so a continuation it launches has to register its
	// session HERE. Without a published host endpoint, flow resolves a daemon
	// from the job's scope (daemon.NewSessionHostClient precedence 3): for a
	// scoped daemon that happens to be itself, but for the global daemon it is
	// the ecosystem's scoped socket — a daemon that is not running and that
	// flow would auto-start. The session would land over there, LiveHead here
	// would never see it, and the supervisor would launch a fresh head every
	// pass until the circuit breaker tripped.
	//
	// Empty leaves the child's environment untouched, which is what tests want.
	HostSocket string
}

// DefaultFlowTimeout bounds one flow invocation. Generous, because
// `flow plan run --background` still does plan loading, context assembly and a
// daemon submit before it returns.
const DefaultFlowTimeout = 3 * time.Minute

func (e *ExecFlowCLI) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return DefaultFlowTimeout
}

// run executes one flow invocation and returns its stdout. Stderr is folded
// into the error, because flow reports why it refused there (an exhausted
// handoff budget, a job in the wrong status) and that reason is the whole
// value of the failure to the operator reading `groved health`.
func (e *ExecFlowCLI) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()

	cmd := delegation.CommandContext(ctx, "flow", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A supervisor-launched flow must never inherit an interactive stdin: the
	// commands it runs prompt when they think a human is present.
	cmd.Stdin = nil
	if e.HostSocket != "" {
		cmd.Env = append(os.Environ(), daemon.HostSocketEnv+"="+e.HostSocket)
	}

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("flow %s: %w: %s",
			strings.Join(args, " "), err, tail(strings.TrimSpace(stderr.String()), 400))
	}
	return stdout.String(), nil
}

func (e *ExecFlowCLI) PlanJobs(ctx context.Context, planDir string) ([]Job, error) {
	out, err := e.run(ctx, "status", "--json", planDir)
	if err != nil {
		return nil, err
	}
	var status planStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return nil, fmt.Errorf("parse flow status --json: %w", err)
	}
	return status.Jobs, nil
}

func (e *ExecFlowCLI) Resume(ctx context.Context, jobPath, agentTarget string) error {
	_, err := e.run(ctx, withAgentTarget(agentTarget, "plan", "resume", jobPath)...)
	return err
}

func (e *ExecFlowCLI) Retry(ctx context.Context, jobPath, agentTarget string) error {
	_, err := e.run(ctx, withAgentTarget(agentTarget, "plan", "retry", "-r", jobPath)...)
	return err
}

// withAgentTarget appends --agent-target when one is named. An empty target
// leaves the flag off, which restores flow's own environment derivation — the
// right degradation for a caller that genuinely has no opinion, and the shape
// every test fake sees.
func withAgentTarget(agentTarget string, args ...string) []string {
	if strings.TrimSpace(agentTarget) == "" {
		return args
	}
	return append(args, "--agent-target", agentTarget)
}

func (e *ExecFlowCLI) AddJob(ctx context.Context, req AddJobRequest) (*AddedJob, error) {
	promptFile, cleanup, err := writePromptFile(req.Prompt)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	args := []string{
		"plan", "add", req.PlanDir,
		"--json",
		"--type", "interactive_agent",
		"--title", req.Title,
		"--prompt-file", promptFile,
	}
	if req.Provider != "" {
		args = append(args, "--provider", req.Provider)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if len(req.Skills) > 0 {
		args = append(args, "--skill-sequence", strings.Join(req.Skills, ","))
	}
	if req.CoordMode != "" {
		args = append(args, "--coord-mode", req.CoordMode)
	}
	if req.HandoffThreshold > 0 {
		args = append(args, "--handoff-threshold", strconv.Itoa(req.HandoffThreshold))
	}
	if req.HandoffMax > 0 {
		args = append(args, "--handoff-max", strconv.Itoa(req.HandoffMax))
	}

	out, err := e.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var added AddedJob
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &added); err != nil {
		return nil, fmt.Errorf("parse flow plan add --json: %w", err)
	}
	if added.Path == "" {
		return nil, fmt.Errorf("flow plan add --json returned no path")
	}
	return &added, nil
}

func (e *ExecFlowCLI) Run(ctx context.Context, jobPath, agentTarget string) error {
	_, err := e.run(ctx, withAgentTarget(agentTarget, "plan", "run", "--yes", "--background", jobPath)...)
	return err
}

func (e *ExecFlowCLI) Claw(ctx context.Context, planDir, jobFile, channel, signalTarget string, idleMinutes int) error {
	args := []string{"agent", "claw", planDir, jobFile, "--channel", channel}
	if idleMinutes > 0 {
		args = append(args, "--idle", strconv.Itoa(idleMinutes))
	}
	if signalTarget != "" {
		args = append(args, "--signal-target", signalTarget)
	}
	_, err := e.run(ctx, args...)
	return err
}

// writePromptFile stages a seeded prompt on disk for --prompt-file. A chain
// reset's prompt embeds the predecessor's handoff spec, so it is far too large
// (and too newline-rich) to ride an argv.
func writePromptFile(prompt string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "grove-assistant-prompt-*.md")
	if err != nil {
		return "", func() {}, fmt.Errorf("stage assistant prompt: %w", err)
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("write assistant prompt: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close assistant prompt: %w", err)
	}
	return name, cleanup, nil
}

// tail returns the last n bytes of s, prefixed with an ellipsis when truncated.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// jobFileName returns the plan-relative filename of a job, preferring the
// explicit field and falling back to the path's base.
func jobFileName(j Job) string {
	if j.Filename != "" {
		return j.Filename
	}
	return filepath.Base(j.FilePath)
}
