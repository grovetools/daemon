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

// ForDaemon builds the supervisor for a daemon whose own scope is scope.
//
// The two deployments resolve differently and this is the only place that
// knows it:
//
//   - scope != "" — a SCOPED daemon, one per ecosystem worktree. Development
//     only. It reads [assistant] from its own scope root, which is the
//     ecosystem it was started for.
//   - scope == "" — the GLOBAL daemon, which is what production runs. It has no
//     scope to read, so it DISCOVERS the ecosystems it can see and reads their
//     blocks (discover.go). Before this existed the global daemon read
//     LoadConfig(""), got an empty Config, and supervised nothing — the
//     supervisor was dead on arrival in the only deployment that matters.
//
// Resolution is lazy in both cases (see resolution): the daemon wires this
// before its collectors have published a workspace set, and a config read must
// never sit on the boot path.
// socketPath is this daemon's own socket, published to every flow child so the
// sessions the supervisor launches register with the daemon that supervises
// them (see ExecFlowCLI.HostSocket).
func ForDaemon(scope, socketPath string, st *store.Store) *Supervisor {
	var sup *Supervisor
	if scope != "" {
		sup = fromScope(scope, st)
	} else {
		sup = fromDiscovery(st)
	}
	sup.flow = &ExecFlowCLI{HostSocket: socketPath}
	return sup
}

// fromScope resolves the ecosystem rooted at scopeDir: its [assistant] block,
// and the ABSOLUTE directory of the plan that block names.
//
// The absolute directory is not a convenience — it is the only address that
// works. `--at <plan>` resolves through the worktree registry, and the
// assistant plan is deliberately worktree-less (spec §3.1), so it has no entry
// there; `--dir` is a deprecated alias that can silently resolve a different
// plan. Resolving the directory once keeps every flow invocation unambiguous.
// A scoped daemon whose scope is an ecosystem WORKTREE supervises nothing. This
// is the same narrowing DiscoverTargets applies, and it became load-bearing when
// LoadConfig started honouring the global config layer: a single [assistant]
// block in ~/.config/grove/grove.toml is inherited by every worktree of the
// ecosystem, so without this guard every feature-branch daemon would resolve the
// same plan, decide it supervises the assistant, and re-register `default_claw`
// with its own socket — last writer wins, and several ensure loops would each
// conclude the chain is down. One open feature branch must not become a rival
// supervisor. (A scoped daemon started AT the ecosystem root still supervises,
// which is the documented double-supervision hazard; unchanged here.)
//
// isRoot is permissive when the ecosystem set cannot be determined at all — an
// empty answer means "we do not know yet", not "this is a worktree", and a
// transient boot-time unknown must not silently disable a correctly scoped
// development daemon.
func scopeMaySupervise(scopeDir string, st *store.Store) bool {
	roots := EcosystemRoots(st)
	if len(roots) == 0 {
		return true
	}
	for _, r := range roots {
		if r == scopeDir {
			return true
		}
	}
	return false
}

func fromScope(scopeDir string, st *store.Store) *Supervisor {
	return newWired(st, func() *resolution {
		cfg, err := LoadConfig(scopeDir)
		if err != nil {
			return &resolution{cfg: &Config{}, err: fmt.Errorf("read [assistant] config in %s: %w", scopeDir, err)}
		}
		if !cfg.Active() {
			return &resolution{cfg: cfg, scope: scopeDir}
		}
		if !scopeMaySupervise(scopeDir, st) {
			return &resolution{
				cfg:   &Config{},
				scope: scopeDir,
				err:   fmt.Errorf("%s is an ecosystem worktree, not an ecosystem root; its [assistant] block is supervised by the ecosystem's own daemon", scopeDir),
			}
		}
		planDir := coreplan.ResolvePlanDir(scopeDir, cfg.Plan)
		if planDir == "" {
			return &resolution{
				cfg:   &Config{},
				scope: scopeDir,
				err:   fmt.Errorf("cannot resolve the plans directory for %q under %s", cfg.Plan, scopeDir),
			}
		}
		if !planDirExists(planDir) {
			return &resolution{
				cfg:   &Config{},
				scope: scopeDir,
				err:   fmt.Errorf("plan %q under %s resolves to %s, which does not exist", cfg.Plan, scopeDir, planDir),
			}
		}
		return &resolution{cfg: cfg, planDir: planDir, scope: scopeDir}
	})
}

// fromDiscovery resolves by walking the ecosystems this daemon can see and
// reading each one's [assistant] block — the global daemon's path.
func fromDiscovery(st *store.Store) *Supervisor {
	return newWired(st, func() *resolution {
		targets, problems := DiscoverTargets(EcosystemRoots(st))
		target, candidates, ok := SelectTarget(targets)
		if !ok {
			r := &resolution{cfg: &Config{}}
			if len(problems) > 0 {
				r.err = problems[0]
			}
			return r
		}
		r := &resolution{
			cfg:        target.Config,
			planDir:    target.PlanDir,
			scope:      target.Scope,
			candidates: candidates,
		}
		// A problem is only worth surfacing as the supervisor's last error
		// when it explains why resolution did not land cleanly. Once ONE
		// ecosystem has been selected unambiguously, the other roots' skips
		// are the normal outcome, not a fault: a GLOBAL [assistant] block is
		// inherited by every root, so on a machine with five ecosystems and
		// one assistant plan, four "that plan does not exist here" skips are
		// produced on every single resolution. Promoting one of those to
		// LastError puts a permanent `last error:` line under a healthy
		// `Assistant: live` in `groved health` and on the status endpoint —
		// which trains an operator to ignore the field that exists to tell
		// them something is wrong. The skips remain in the returned problems
		// slice for logging.
		if len(candidates) > 0 {
			r.err = AmbiguityError(target.Scope, candidates)
		}
		return r
	})
}

// newWired builds a supervisor over resolve and connects it to the daemon's
// session store: the live-head query it asks on every pass, and the status
// updates it puts on the state stream.
//
// Both closures read the plan name through the supervisor's own resolution
// rather than capturing one, so a lazily-resolved supervisor and the things
// that observe it can never disagree about which plan the assistant lives in.
func newWired(st *store.Store, resolve func() *resolution) *Supervisor {
	sup := newSupervisor(nil, resolve)
	if st == nil {
		return sup
	}
	sup.LiveHead = func() (Head, bool) { return LiveHeadFromStore(st, sup.Plan()) }
	sup.Publish = func(status models.AssistantStatus) {
		st.ApplyUpdate(store.Update{
			Type:    store.UpdateAssistantStatus,
			Source:  "assistant_supervisor",
			Payload: &status,
		})
	}
	return sup
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
