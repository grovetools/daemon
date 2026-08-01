package assistant

import (
	"fmt"

	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// liveSessionStatuses are the session states that count as a live head. A
// session that is pending_user or idle is very much alive — the assistant is
// waiting for its operator, which is the resting state of a standing agent —
// so only terminal states mean "the chain needs a continuation".
var liveSessionStatuses = map[string]bool{
	"running":      true,
	"idle":         true,
	"pending_user": true,
	"pending_llm":  true,
	"starting":     true,
}

// FromScope builds a supervisor for the ecosystem rooted at scopeDir, reading
// its [assistant] grove.toml block and resolving the assistant plan to an
// ABSOLUTE plan directory.
//
// The absolute directory is not a convenience — it is the only address that
// works. `--at <plan>` resolves through the worktree registry, and the
// assistant plan is deliberately worktree-less (spec §3.1), so it has no entry
// there; `--dir` is a deprecated alias that can silently resolve a different
// plan. Resolving the directory once, here, keeps every flow invocation
// unambiguous.
//
// A disabled or absent block yields an inert supervisor and no error: not
// opting in is the normal case.
func FromScope(scopeDir string, st *store.Store) (*Supervisor, error) {
	cfg, err := LoadConfig(scopeDir)
	if err != nil {
		return nil, fmt.Errorf("read [assistant] config: %w", err)
	}

	planDir := ""
	if cfg.Active() {
		planDir = coreplan.ResolvePlanDir(scopeDir, cfg.Plan)
		if planDir == "" {
			return nil, fmt.Errorf("cannot resolve the plans directory for %q under %s", cfg.Plan, scopeDir)
		}
	}

	sup := NewSupervisor(cfg, planDir, &ExecFlowCLI{})
	if st != nil {
		sup.LiveHead = func() (Head, bool) { return LiveHeadFromStore(st, cfg.Plan) }
		sup.Publish = func(status models.AssistantStatus) {
			st.ApplyUpdate(store.Update{
				Type:    store.UpdateAssistantStatus,
				Source:  "assistant_supervisor",
				Payload: &status,
			})
		}
	}
	return sup, nil
}

// LiveHeadFromStore resolves the current head of the assistant chain from the
// daemon session store: the newest live interactive agent session belonging to
// plan. This is the same resolution the rail pane performs against the session
// stream, done here against the store the stream is derived from — one
// definition of "the assistant is up", shared by the pane and the supervisor.
//
// Satellite-origin sessions are excluded: a session running on another host is
// not this daemon's assistant, and it has no local PTY for the pane to attach.
func LiveHeadFromStore(st *store.Store, plan string) (Head, bool) {
	if st == nil || plan == "" {
		return Head{}, false
	}
	var head *models.Session
	for _, s := range st.GetSessions() {
		if s == nil || s.Origin != "" || s.PlanName != plan {
			continue
		}
		switch s.Type {
		case "interactive_agent", "claude_session":
		default:
			continue
		}
		if s.EndedAt != nil || !liveSessionStatuses[s.Status] {
			continue
		}
		if head == nil || s.StartedAt.After(head.StartedAt) {
			head = s
		}
	}
	if head == nil {
		return Head{}, false
	}
	return Head{JobID: head.ID, JobFile: filepathBase(head.JobFilePath)}, true
}
