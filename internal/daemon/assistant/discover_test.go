package assistant

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// stubPlanDirs points resolvePlanDir at a fake notebook: <root>/plans/<name>.
// The real resolver walks workspace discovery and a notebook locator, neither
// of which a unit test should have to stand up.
//
// The directory is really created, so DiscoverTargets' existence check runs
// against the real filesystem rather than another stub. A test that wants the
// "named plan is missing" branch stubs resolvePlanDir itself to somewhere it
// has not created.
//
// It also isolates the config cascade's global layer. LoadConfig reads through
// config.LoadFrom, so without this every discovery test would inherit the
// developer's own ~/.config/grove/grove.toml — and on a machine that enables
// [assistant] globally (the very configuration this feature is for) an
// ecosystem the test wrote as opted-OUT would come back opted-in, failing for
// reasons that have nothing to do with discovery.
func stubPlanDirs(t *testing.T) {
	t.Helper()
	isolateGlobalConfig(t)
	prev := resolvePlanDir
	resolvePlanDir = func(root, plan string) string {
		if root == "" || plan == "" {
			return ""
		}
		dir := filepath.Join(root, "plans", plan)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Cleanup(func() { resolvePlanDir = prev })
}

// ecosystem writes a grove.toml carrying body into a fresh directory named
// name, and returns the directory. Named directories (rather than bare
// t.TempDir()s) keep the sort order in the ambiguity tests predictable.
func ecosystem(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grove.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const optedIn = `
workspaces = ["*"]

[assistant]
enabled = true
plan = "steward"
provider = "grove-agent"
`

// TestDiscoverTargetsKeepsOnlyOptedInEcosystems is the core of the fix: the
// global daemon has no scope to read a config from, so it reads every
// ecosystem's block and keeps the ones that opted in.
func TestDiscoverTargetsKeepsOnlyOptedInEcosystems(t *testing.T) {
	stubPlanDirs(t)

	yes := ecosystem(t, "b-yes", optedIn)
	no := ecosystem(t, "a-no", "workspaces = [\"*\"]\n")
	off := ecosystem(t, "c-off", "[assistant]\nenabled = false\nplan = \"steward\"\n")

	targets, problems := DiscoverTargets([]string{no, off, yes})
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want only the opted-in ecosystem", targets)
	}
	if targets[0].Scope != yes {
		t.Fatalf("scope = %q, want %q", targets[0].Scope, yes)
	}
	if targets[0].PlanDir != filepath.Join(yes, "plans", "steward") {
		t.Fatalf("plan dir = %q, want the absolute plan directory", targets[0].PlanDir)
	}
	if targets[0].Config.Provider != "grove-agent" {
		t.Fatalf("config = %+v, want the ecosystem's own block", targets[0].Config)
	}
}

// One ecosystem's broken grove.toml must not cost another its assistant, and
// must never be fatal — the daemon has to boot.
func TestDiscoverTargetsSurvivesABrokenEcosystem(t *testing.T) {
	stubPlanDirs(t)

	broken := ecosystem(t, "a-broken", "[assistant\nenabled = true\n")
	good := ecosystem(t, "b-good", optedIn)

	targets, problems := DiscoverTargets([]string{broken, good})
	if len(targets) != 1 || targets[0].Scope != good {
		t.Fatalf("targets = %+v, want the healthy ecosystem", targets)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), broken) {
		t.Fatalf("problems = %v, want the broken ecosystem named", problems)
	}
}

// An enabled block whose plan has no resolvable directory is a diagnosis, not a
// target: the supervisor addresses plans by absolute directory and has nothing
// to address without one.
func TestDiscoverTargetsReportsAnUnresolvablePlan(t *testing.T) {
	prev := resolvePlanDir
	resolvePlanDir = func(string, string) string { return "" }
	t.Cleanup(func() { resolvePlanDir = prev })

	root := ecosystem(t, "eco", optedIn)
	targets, problems := DiscoverTargets([]string{root})
	if len(targets) != 0 {
		t.Fatalf("targets = %+v, want none", targets)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "steward") {
		t.Fatalf("problems = %v, want the unresolvable plan named", problems)
	}
}

// TestSelectTargetIsDeterministicAndNeverSilent pins the single-vs-many
// boundary. A daemon supervises one ecosystem; with several opted in the choice
// is by lowest path and every candidate travels with it, so the operator sees
// the ambiguity instead of an assistant that quietly belongs to whichever
// ecosystem sorted first.
func TestSelectTargetIsDeterministicAndNeverSilent(t *testing.T) {
	stubPlanDirs(t)

	base := t.TempDir()
	mk := func(name string) string {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "grove.toml"), []byte(optedIn), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	second := mk("zzz-second")
	first := mk("aaa-first")

	targets, _ := DiscoverTargets([]string{second, first})
	chosen, candidates, ok := SelectTarget(targets)
	if !ok {
		t.Fatal("SelectTarget found nothing among two opted-in ecosystems")
	}
	if chosen.Scope != first {
		t.Fatalf("chose %q, want the lowest path %q", chosen.Scope, first)
	}
	if len(candidates) != 2 || candidates[0] != first || candidates[1] != second {
		t.Fatalf("candidates = %v, want both ecosystems in path order", candidates)
	}

	// A second run must choose the same one regardless of input order.
	targets2, _ := DiscoverTargets([]string{first, second})
	chosen2, _, _ := SelectTarget(targets2)
	if chosen2.Scope != chosen.Scope {
		t.Fatalf("selection is not deterministic: %q then %q", chosen.Scope, chosen2.Scope)
	}

	// The single case carries no candidates: an ordinary configuration must
	// not print an ambiguity warning.
	single, none, ok := SelectTarget(targets[:1])
	if !ok || len(none) != 0 || single.Scope == "" {
		t.Fatalf("single = %+v candidates = %v, want a clean single selection", single, none)
	}

	if _, _, ok := SelectTarget(nil); ok {
		t.Fatal("SelectTarget invented a target out of nothing")
	}
}

// TestGlobalDaemonResolvesByDiscovery is the end of the defect: a daemon with
// scope "" used to read LoadConfig("") and supervise nothing. It now discovers
// the ecosystem and reports it.
func TestGlobalDaemonResolvesByDiscovery(t *testing.T) {
	stubPlanDirs(t)
	root := ecosystem(t, "grovetools", optedIn)

	sup := newSupervisor(&fakeFlow{}, func() *resolution {
		targets, _ := DiscoverTargets([]string{root})
		target, candidates, ok := SelectTarget(targets)
		if !ok {
			return disabledResolution()
		}
		return &resolution{cfg: target.Config, planDir: target.PlanDir, scope: target.Scope, candidates: candidates}
	})

	if !sup.Enabled() {
		t.Fatal("the global daemon supervises nothing — the defect is still there")
	}
	if got := sup.Plan(); got != "steward" {
		t.Fatalf("plan = %q, want steward", got)
	}
	if got := sup.Scope(); got != root {
		t.Fatalf("scope = %q, want the discovered ecosystem %q", got, root)
	}
	status := sup.Status()
	if !status.Enabled || status.Scope != root || status.PlanDir != filepath.Join(root, "plans", "steward") {
		t.Fatalf("status = %+v, want the discovered ecosystem", status)
	}
}

// The default claw cannot be claimed at wiring time on a lazily-resolved
// supervisor, because there is no plan name yet. SetOnResolved is the hook that
// closes that gap, and it has to fire exactly once with the resolved facts —
// whether it was registered before or after resolution happened.
func TestSetOnResolvedDeliversTheResolvedPlan(t *testing.T) {
	stubPlanDirs(t)
	root := ecosystem(t, "grovetools", optedIn)

	newSup := func() *Supervisor {
		return newSupervisor(&fakeFlow{}, func() *resolution {
			cfg, _ := LoadConfig(root)
			return &resolution{cfg: cfg, planDir: resolvePlanDir(root, cfg.Plan), scope: root}
		})
	}

	t.Run("registered before resolution", func(t *testing.T) {
		sup := newSup()
		var seen []models.AssistantStatus
		sup.SetOnResolved(func(s models.AssistantStatus) { seen = append(seen, s) })
		if len(seen) != 0 {
			t.Fatalf("hook fired before anything asked for a resolution: %+v", seen)
		}
		_ = sup.Enabled()
		_ = sup.Status()
		if len(seen) != 1 {
			t.Fatalf("hook fired %d times, want exactly once", len(seen))
		}
		if seen[0].Plan != "steward" || seen[0].Scope != root || !seen[0].Enabled {
			t.Fatalf("hook saw %+v, want the resolved ecosystem", seen[0])
		}
	})

	t.Run("registered after resolution", func(t *testing.T) {
		sup := newSup()
		_ = sup.Enabled() // resolve first
		var seen []models.AssistantStatus
		sup.SetOnResolved(func(s models.AssistantStatus) { seen = append(seen, s) })
		if len(seen) != 1 || seen[0].Plan != "steward" {
			t.Fatalf("hook saw %+v, want an immediate replay — the default claw would never be claimed", seen)
		}
	})
}

// Resolution is cached for the daemon's lifetime so a slow discovery walk is
// not repeated on every tick. A forced ensure is the operator's escape hatch
// for an [assistant] block added since boot, on a daemon that is hosting live
// agents and must not be restarted.
func TestForcedEnsureReresolves(t *testing.T) {
	stubPlanDirs(t)
	root := filepath.Join(t.TempDir(), "grovetools")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "grove.toml"), []byte("workspaces = [\"*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sup := newSupervisor(&fakeFlow{}, func() *resolution {
		cfg, err := LoadConfig(root)
		if err != nil {
			return disabledResolution()
		}
		return &resolution{cfg: cfg, planDir: resolvePlanDir(root, cfg.Plan), scope: root}
	})

	if sup.Enabled() {
		t.Fatal("enabled before any [assistant] block existed")
	}

	if err := os.WriteFile(filepath.Join(root, "grove.toml"), []byte(optedIn), 0o644); err != nil {
		t.Fatal(err)
	}
	if sup.Enabled() {
		t.Fatal("resolution was not cached — a discovery walk on every read")
	}

	if _, err := sup.Ensure(t.Context(), "operator", true); err != nil {
		t.Fatalf("forced Ensure: %v", err)
	}
	if !sup.Enabled() || sup.Plan() != "steward" {
		t.Fatalf("enabled = %v plan = %q, want the forced ensure to have re-resolved", sup.Enabled(), sup.Plan())
	}
}

// AmbiguityError has to name the winner and every other candidate: it is what
// `groved health` and the rail placeholder print, and a message that only says
// "ambiguous" is not actionable.
func TestAmbiguityErrorNamesEverything(t *testing.T) {
	err := AmbiguityError("/eco/a", []string{"/eco/a", "/eco/b"})
	msg := err.Error()
	for _, want := range []string{"/eco/a", "/eco/b", "chose /eco/a"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

// TestEcosystemRootsIgnoresWorktrees is why discovery narrows to
// KindEcosystemRoot. An ecosystem WORKTREE carries a copy of its ecosystem's
// grove.toml — [assistant] block included — but it is the same ecosystem, with
// the same notebook workspace and the same assistant plan. Counting worktrees
// would make every open feature branch a rival "opted-in ecosystem" and fire
// the multi-ecosystem ambiguity at a single-ecosystem user.
func TestEcosystemRootsIgnoresWorktrees(t *testing.T) {
	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkspaces,
		Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{
			"/eco": {WorkspaceNode: &workspace.WorkspaceNode{
				Path: "/eco", Kind: workspace.KindEcosystemRoot,
			}},
			"/wt/feature": {WorkspaceNode: &workspace.WorkspaceNode{
				Path: "/wt/feature", Kind: workspace.KindEcosystemWorktree,
			}},
			"/eco/daemon": {WorkspaceNode: &workspace.WorkspaceNode{
				Path: "/eco/daemon", Kind: workspace.KindEcosystemSubProject,
			}},
		},
	})

	got := ecosystemRootsFromStore(st)
	if len(got) != 1 || got[0] != "/eco" {
		t.Fatalf("roots = %v, want only the ecosystem root", got)
	}
}

// Resolution is reached from the collector goroutine, the HTTP status handler,
// and the channels manager's inbound path at once. It must run exactly once and
// must not deadlock — Status publishes a state transition under the same mutex
// the ensure loop uses, so resolving from inside a locked section would hang the
// first status read.
func TestResolutionIsOnceAndConcurrencySafe(t *testing.T) {
	stubPlanDirs(t)
	root := ecosystem(t, "grovetools", optedIn)

	var calls atomic.Int32
	sup := newSupervisor(&fakeFlow{}, func() *resolution {
		calls.Add(1)
		cfg, _ := LoadConfig(root)
		return &resolution{cfg: cfg, planDir: resolvePlanDir(root, cfg.Plan), scope: root}
	})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				_ = sup.Enabled()
			case 1:
				_ = sup.Status()
			case 2:
				_ = sup.Plan()
			default:
				_ = sup.PlanDir()
			}
		}(i)
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver ran %d times, want exactly 1", got)
	}
}

// TestGlobalBlockResolvesToTheEcosystemThatHasThePlan is the discovery half of
// the cascade fix, and the reason DiscoverTargets requires the plan directory
// to exist.
//
// A global [assistant] block is inherited by EVERY ecosystem root at once, so
// opting in stops being a per-ecosystem signal. Without an existence check all
// four roots below are "opted in" with a confidently computed plan directory,
// SelectTarget takes the alphabetically lowest, and the supervisor drives an
// ecosystem the operator never named — against a directory that was never
// there. Existence is what identifies the one ecosystem that really has the
// assistant.
func TestGlobalBlockResolvesToTheEcosystemThatHasThePlan(t *testing.T) {
	dir := isolateGlobalConfig(t)
	if err := os.WriteFile(filepath.Join(dir, "grove.toml"),
		[]byte("[assistant]\nenabled = true\nplan = 'steward'\nprovider = 'pi'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Four ecosystems, none of which mentions [assistant] itself.
	base := t.TempDir()
	var roots []string
	for _, name := range []string{"a-plugins", "b-grovetools", "c-solutils", "d-xdg-test"} {
		root := filepath.Join(base, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "grove.toml"), []byte("workspaces = [\"*\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
	}
	// Only the SECOND one actually has a steward plan on disk. Note it is not
	// the lowest path, so a run that ignores existence picks a-plugins.
	withPlan := roots[1]
	if err := os.MkdirAll(filepath.Join(withPlan, "plans", "steward"), 0o755); err != nil {
		t.Fatal(err)
	}

	prev := resolvePlanDir
	resolvePlanDir = func(root, plan string) string { return filepath.Join(root, "plans", plan) }
	t.Cleanup(func() { resolvePlanDir = prev })

	targets, problems := DiscoverTargets(roots)
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want exactly the ecosystem whose plan exists", targets)
	}
	if targets[0].Scope != withPlan {
		t.Fatalf("scope = %q, want %q — a global block must not hand the supervisor an ecosystem it only inherited", targets[0].Scope, withPlan)
	}
	// The three skips are reported, not swallowed: an operator who named a
	// plan that does not exist yet has to be able to see why.
	if len(problems) != 3 {
		t.Fatalf("problems = %v, want the three ecosystems without the plan named", problems)
	}

	// And the collapse is unambiguous — no candidate list, so no warning.
	target, candidates, ok := SelectTarget(targets)
	if !ok || target.Scope != withPlan || len(candidates) != 0 {
		t.Fatalf("SelectTarget = (%q, %v, %v), want a clean single resolution", target.Scope, candidates, ok)
	}
}

// A clean single resolution must not report an error, even though the other
// ecosystem roots were skipped along the way.
//
// With [assistant] readable from the global config layer, skips are the normal
// case rather than a symptom: every root inherits the block, and every root
// without that plan is skipped on every resolution. Surfacing one of them as
// LastError would print a permanent `last error:` under a healthy
// `Assistant: live` in `groved health`, which is how an operator learns to stop
// reading the field.
func TestCleanResolutionReportsNoError(t *testing.T) {
	stubPlanDirs(t)

	yes := ecosystem(t, "b-yes", optedIn)
	// An ecosystem that opted in but whose plan does not exist — a skip.
	skipped := ecosystem(t, "a-skipped", optedIn)
	prev := resolvePlanDir
	resolvePlanDir = func(root, plan string) string {
		dir := filepath.Join(root, "plans", plan)
		if root == yes {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	t.Cleanup(func() { resolvePlanDir = prev })

	// Drive the REAL fromDiscovery closure by giving the store the two roots,
	// rather than re-implementing its logic in the test.
	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkspaces,
		Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{
			skipped: {WorkspaceNode: &workspace.WorkspaceNode{Path: skipped, Kind: workspace.KindEcosystemRoot}},
			yes:     {WorkspaceNode: &workspace.WorkspaceNode{Path: yes, Kind: workspace.KindEcosystemRoot}},
		},
	})

	sup := fromDiscovery(st)
	status := sup.Status()
	if !sup.Enabled() || status.Scope != yes {
		t.Fatalf("scope = %q enabled = %v, want a clean resolution to %q", status.Scope, sup.Enabled(), yes)
	}
	if status.LastError != "" {
		t.Fatalf("LastError = %q, want empty: the other root's skip is the normal case, not a fault", status.LastError)
	}
}
