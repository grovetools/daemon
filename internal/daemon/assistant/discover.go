package assistant

import (
	"fmt"
	"sort"
	"strings"

	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// Deployment model — why this file exists.
//
// Scoped daemons (one per ecosystem worktree) are a DEVELOPMENT convenience.
// Production is always the unscoped/global daemon, whose scope is "". The first
// cut of the supervisor read [assistant] from its own scope root, which made it
// inert in exactly the deployment it is meant to run in: LoadConfig("") returns
// an empty Config, so the global daemon supervised nothing and the default claw
// was never published.
//
// So a daemon with no scope of its own DISCOVERS the ecosystems it can see and
// reads their [assistant] blocks. The mechanism is the memory watcher's, not a
// new one (watcher/memory.go assistantMemoryDir + ComputeWatchPaths): walk the
// workspace nodes the daemon already tracks, keep the ecosystems, call
// LoadConfig on each one's checkout.
//
// One deliberate narrowing versus the watcher: only ecosystem ROOTS are
// candidates, never ecosystem WORKTREES. A worktree carries a copy of its
// ecosystem's grove.toml — including [assistant] — but it is the same
// ecosystem, sharing one notebook workspace and therefore one assistant plan.
// Counting worktrees would turn every open feature branch into another
// "opted-in ecosystem" and make the multi-ecosystem ambiguity below fire
// constantly for a single-ecosystem user.

// resolvePlanDir maps an ecosystem root and a plan name to the plan's absolute
// directory. Indirected through a variable so discovery can be tested without
// standing up a whole notebook layout on disk; production always uses
// coreplan.ResolvePlanDir.
var resolvePlanDir = coreplan.ResolvePlanDir

// Target is one ecosystem's opted-in assistant: the ecosystem root, the
// [assistant] block found there, and the absolute directory of the plan that
// block names.
type Target struct {
	// Scope is the ecosystem root — the directory whose grove.toml was read.
	Scope string
	// Config is that ecosystem's [assistant] block, defaults applied.
	Config *Config
	// PlanDir is the ABSOLUTE assistant plan directory. See FlowCLI for why
	// every flow address is a directory rather than `--at <plan>`.
	PlanDir string
}

// EcosystemRoots lists the ecosystem roots this daemon can see.
//
// The live workspace set in the store is preferred — it is the same list every
// watcher handler is driven from, kept current by the workspace collector — and
// a fresh discovery walk is the fallback for the boot window, when the
// supervisor's first ensure can fire before that collector has published
// anything.
func EcosystemRoots(st *store.Store) []string {
	if roots := ecosystemRootsFromStore(st); len(roots) > 0 {
		return roots
	}
	return ecosystemRootsFromDisk()
}

func ecosystemRootsFromStore(st *store.Store) []string {
	if st == nil {
		return nil
	}
	var roots []string
	for _, ew := range st.GetWorkspaces() {
		if ew == nil || ew.WorkspaceNode == nil {
			continue
		}
		if ew.WorkspaceNode.Kind == workspace.KindEcosystemRoot {
			roots = append(roots, ew.WorkspaceNode.Path)
		}
	}
	return dedupeSorted(roots)
}

func ecosystemRootsFromDisk() []string {
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	nodes, err := workspace.GetProjects(log)
	if err != nil {
		return nil
	}
	var roots []string
	for _, n := range nodes {
		if n != nil && n.Kind == workspace.KindEcosystemRoot {
			roots = append(roots, n.Path)
		}
	}
	return dedupeSorted(roots)
}

func dedupeSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// DiscoverTargets reads the [assistant] block of every root and returns the
// ones that opted in, ordered by path.
//
// A root whose grove.toml is malformed, or whose named plan has no resolvable
// directory, is skipped rather than fatal: one ecosystem's typo must not stop
// the daemon from supervising another's assistant, and it must never stop the
// daemon from booting.
func DiscoverTargets(roots []string) ([]Target, []error) {
	var targets []Target
	var problems []error
	for _, root := range roots {
		cfg, err := LoadConfig(root)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: read [assistant]: %w", root, err))
			continue
		}
		if !cfg.Active() {
			continue
		}
		planDir := resolvePlanDir(root, cfg.Plan)
		if planDir == "" {
			problems = append(problems, fmt.Errorf("%s: cannot resolve the plans directory for %q", root, cfg.Plan))
			continue
		}
		targets = append(targets, Target{Scope: root, Config: cfg, PlanDir: planDir})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Scope < targets[j].Scope })
	return targets, problems
}

// SelectTarget picks the ONE ecosystem this daemon supervises.
//
// ── The single-vs-many boundary ──────────────────────────────────────────────
// Spec §3.1 says there is one assistant per ecosystem and that multi-ecosystem
// users get one each, disambiguated on Signal by @tag. Everything downstream of
// this function is singleton today: one Supervisor on the server, one
// AssistantStatus on the state stream, one GET /api/assistant/status, and one
// `default_claw` record in channels/state.json — and that last one is singleton
// by NATURE, because "where does unresolved inbound go" has exactly one answer
// per Signal account until @tag routing exists.
//
// So a daemon supervises one ecosystem, and going to N is an additive change:
// DiscoverTargets already returns all of them, and the layer that collapses
// them to one is this function alone. Making the registry per-ecosystem means
// keeping every Target instead of dropping to targets[0], keying the supervisor
// map and the status endpoint by Scope, and giving DefaultClawInfo a per-scope
// shape — none of which requires re-doing discovery or config.
//
// When more than one ecosystem opts in, the choice is deterministic (lowest
// path) and never silent: every candidate is returned so the status, the log
// line, and `groved health` all name them.
func SelectTarget(targets []Target) (Target, []string, bool) {
	if len(targets) == 0 {
		return Target{}, nil, false
	}
	if len(targets) == 1 {
		return targets[0], nil, true
	}
	candidates := make([]string, 0, len(targets))
	for _, t := range targets {
		candidates = append(candidates, t.Scope)
	}
	return targets[0], candidates, true
}

// AmbiguityError describes the multi-opt-in case in the words an operator needs
// to fix it. It is surfaced through AssistantStatus.LastError, so it reaches
// `groved health`, the rail pane placeholder, and the status endpoint.
func AmbiguityError(chosen string, candidates []string) error {
	return fmt.Errorf(
		"%d ecosystems enable [assistant] (%s); this daemon supervises one, and chose %s. "+
			"Disable the others' [assistant] blocks, or run a scoped daemon per ecosystem",
		len(candidates), strings.Join(candidates, ", "), chosen)
}
