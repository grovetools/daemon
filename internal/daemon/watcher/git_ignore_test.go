package watcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

// probeVerdict is one recorded proof, observed through the cache's test seam.
type probeVerdict struct {
	repo string
	dir  string
	dead bool
}

// newTestDeadCache returns a cache whose real git probes run, plus a channel of
// recorded verdicts so a test can wait for the ASYNC prober instead of sleeping.
func newTestDeadCache(t *testing.T) (*deadSubtreeCache, <-chan probeVerdict) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verdicts := make(chan probeVerdict, 64)
	c := newDeadSubtreeCacheStopped()
	c.probed = func(repo, dir string, dead bool) {
		select {
		case verdicts <- probeVerdict{repo: repo, dir: dir, dead: dead}:
		default:
		}
	}
	go c.run(ctx)
	return c, verdicts
}

// awaitVerdict blocks until the prober records (repo, dir) and returns whether
// the directory was proven dead.
func awaitVerdict(t *testing.T, verdicts <-chan probeVerdict, repo, dir string) bool {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case v := <-verdicts:
			if v.repo == repo && v.dir == dir {
				return v.dead
			}
		case <-deadline:
			t.Fatalf("prober recorded no verdict for %s under %s", dir, repo)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// The headline case: the measured workload. `gcloud` is ignored and holds no
// tracked file, so after ONE proof every event under it costs a map lookup.
func TestDeadSubtreeSuppressesIgnoredTrackedFreeDirectory(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "gcloud\n")
	gitIn(t, repo, "add", ".gitignore")
	gitIn(t, repo, "commit", "-m", "ignore gcloud")
	writeFile(t, filepath.Join(repo, "gcloud", "credentials.db"), "token")

	c, verdicts := newTestDeadCache(t)
	event := filepath.Join(repo, "gcloud", "credentials.db")

	// Fail open: nothing is proven yet, so the first event scans.
	if c.Suppress(repo, event) {
		t.Fatal("suppressed an event before any proof existed")
	}
	if !awaitVerdict(t, verdicts, repo, "gcloud") {
		t.Fatal("an ignored, tracked-file-free directory was not proven dead")
	}
	if !c.Suppress(repo, event) {
		t.Fatal("proven-dead subtree still schedules scans")
	}
	// Depth below the proven prefix rides the same single proof.
	if !c.Suppress(repo, filepath.Join(repo, "gcloud", "logs", "2026-07-30", "gcloud.log")) {
		t.Fatal("deeper path under a proven-dead prefix was not suppressed")
	}
}

// THE most important test in the set. A path matcher calls `local/` ignored and
// goes blind to the hand-edited script force-added inside it; the emptiness proof
// makes that class of blindness structurally impossible.
func TestDeadSubtreeNeverSuppressesForceAddedTrackedFile(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "local/\n")
	gitIn(t, repo, "add", ".gitignore")
	writeFile(t, filepath.Join(repo, "local", "keep.sh"), "#!/bin/sh\necho keep\n")
	gitIn(t, repo, "add", "-f", "local/keep.sh")
	gitIn(t, repo, "commit", "-m", "force-add a tracked script inside an ignored dir")
	// A genuinely ignored sibling, so the directory still looks ignored by pattern.
	writeFile(t, filepath.Join(repo, "local", "scratch.tmp"), "junk")

	c, verdicts := newTestDeadCache(t)
	for _, event := range []string{
		filepath.Join(repo, "local", "keep.sh"),
		filepath.Join(repo, "local", "scratch.tmp"),
	} {
		if c.Suppress(repo, event) {
			t.Fatalf("suppressed %s before any proof existed", event)
		}
	}
	if awaitVerdict(t, verdicts, repo, "local") {
		t.Fatal("a directory containing a force-added tracked file was proven dead")
	}
	for _, event := range []string{
		filepath.Join(repo, "local", "keep.sh"),
		filepath.Join(repo, "local", "scratch.tmp"),
	} {
		if c.Suppress(repo, event) {
			t.Fatalf("suppressed %s in a directory holding a tracked file", event)
		}
	}
}

// Git internals are how grove learns about commits and branch switches, and
// nothing stops a .gitignore from containing `index`, `HEAD` or `refs`. The
// route's internal flag plus the hard path exemptions must hold for the main
// worktree AND for a linked worktree's gitdir under <main>/.git/worktrees/<id>/.
func TestDeadSubtreeNeverSuppressesGitInternals(t *testing.T) {
	main := gitInitRepo(t)
	writeFile(t, filepath.Join(main, ".gitignore"), "index\nHEAD\nrefs\nlogs\nworktrees\n")
	gitIn(t, main, "add", ".gitignore")
	gitIn(t, main, "commit", "-m", "ignore patterns that also name git internals")

	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, main, "worktree", "add", "-b", "linked", linked)
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	wtDirs, err := os.ReadDir(filepath.Join(main, ".git", "worktrees"))
	if err != nil || len(wtDirs) != 1 {
		t.Fatalf("expected one linked worktree gitdir, got %v (%v)", wtDirs, err)
	}
	linkedGitDir := filepath.Join(main, ".git", "worktrees", wtDirs[0].Name())

	ctx := context.Background()
	routes := buildGitEventRoutes(ctx, []*models.EnrichedWorkspace{
		{WorkspaceNode: &workspace.WorkspaceNode{Name: filepath.Base(main), Path: main, Kind: workspace.KindStandaloneProject}},
		{WorkspaceNode: &workspace.WorkspaceNode{Name: "linked", Path: linked, Kind: workspace.KindStandaloneProjectWorktree}},
	})

	c, verdicts := newTestDeadCache(t)
	for _, event := range []string{
		filepath.Join(main, ".git", "index"),
		filepath.Join(main, ".git", "HEAD"),
		filepath.Join(main, ".git", "refs", "heads", "main"),
		filepath.Join(main, ".git", "refs", "heads", "linked"),
		filepath.Join(main, ".git", "logs", "HEAD"),
		filepath.Join(linkedGitDir, "HEAD"),
		filepath.Join(linkedGitDir, "index"),
		filepath.Join(linked, ".git"), // the linked worktree's .git FILE
	} {
		path := resolveEventPath(event)
		route, nodes := routeGitEvent(path, routes)
		if route == nil || len(nodes) == 0 {
			t.Fatalf("%s routed nowhere; the test no longer exercises suppression", event)
		}
		c.Observe(route, path)
		if !route.internal && c.Suppress(route.root, path) {
			t.Fatalf("suppressed git-internal path %s", event)
		}
	}

	// Nothing above may even be probed: a proof about .git internals is a proof
	// nobody is allowed to act on.
	select {
	case v := <-verdicts:
		t.Fatalf("probed %s under %s for a git-internal event", v.dir, v.repo)
	case <-time.After(250 * time.Millisecond):
	}
}

// Editing the ignore rules themselves — at the repository root or at any depth —
// invalidates every proof in that repo and always scans.
func TestDeadSubtreeGitignoreEditInvalidates(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "gcloud\n")
	gitIn(t, repo, "add", ".gitignore")
	gitIn(t, repo, "commit", "-m", "ignore gcloud")
	writeFile(t, filepath.Join(repo, "gcloud", "credentials.db"), "token")

	ctx := context.Background()
	node := &workspace.WorkspaceNode{Name: filepath.Base(repo), Path: repo, Kind: workspace.KindStandaloneProject}
	routes := buildGitEventRoutes(ctx, []*models.EnrichedWorkspace{{WorkspaceNode: node}})

	c, verdicts := newTestDeadCache(t)
	event := filepath.Join(repo, "gcloud", "credentials.db")

	for _, ignorePath := range []string{
		filepath.Join(repo, ".gitignore"),
		filepath.Join(repo, "pkg", "nested", ".gitignore"),
		filepath.Join(repo, "gcloud", ".gitignore"),
	} {
		// Re-establish the proof before each invalidation.
		c.Suppress(repo, event)
		if !awaitVerdict(t, verdicts, repo, "gcloud") {
			t.Fatal("gcloud was not proven dead")
		}
		if !c.Suppress(repo, event) {
			t.Fatal("proven-dead subtree still schedules scans")
		}

		path := resolveEventPath(ignorePath)
		route, _ := routeGitEvent(path, routes)
		if route == nil {
			t.Fatalf("%s routed nowhere", ignorePath)
		}
		if !c.Observe(route, path) {
			t.Fatalf("%s did not invalidate the repository's proofs", ignorePath)
		}
		if c.Suppress(route.root, path) {
			t.Fatalf("suppressed the ignore file %s itself", ignorePath)
		}
		if _, known := c.probeState(repo, "gcloud"); known {
			t.Fatalf("%s left a stale proof for gcloud", ignorePath)
		}
	}
}

// `git add -f` inside a suppressed directory changes the tracked set, which no
// path matcher can see. The index event voids the proof; the re-proof finds the
// tracked file and the directory stops being suppressed.
func TestDeadSubtreeIndexEventInvalidatesForceAdd(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "local/\n")
	gitIn(t, repo, "add", ".gitignore")
	gitIn(t, repo, "commit", "-m", "ignore local")
	writeFile(t, filepath.Join(repo, "local", "keep.sh"), "#!/bin/sh\n")

	ctx := context.Background()
	node := &workspace.WorkspaceNode{Name: filepath.Base(repo), Path: repo, Kind: workspace.KindStandaloneProject}
	routes := buildGitEventRoutes(ctx, []*models.EnrichedWorkspace{{WorkspaceNode: node}})

	c, verdicts := newTestDeadCache(t)
	event := filepath.Join(repo, "local", "keep.sh")

	c.Suppress(repo, event)
	if !awaitVerdict(t, verdicts, repo, "local") {
		t.Fatal("an ignored, untracked directory was not proven dead")
	}
	if !c.Suppress(repo, event) {
		t.Fatal("proven-dead subtree still schedules scans")
	}

	gitIn(t, repo, "add", "-f", "local/keep.sh")

	indexPath := resolveEventPath(filepath.Join(repo, ".git", "index"))
	route, nodes := routeGitEvent(indexPath, routes)
	if route == nil || !route.internal || len(nodes) == 0 {
		t.Fatalf("index event routed to %+v, want the internal git-dir route", route)
	}
	if !c.Observe(route, indexPath) {
		t.Fatal("an index write did not invalidate the repository's proofs")
	}

	// The later edit re-proves and now emits, because the file is tracked.
	if c.Suppress(repo, event) {
		t.Fatal("suppressed an edit after the tracked set changed")
	}
	if awaitVerdict(t, verdicts, repo, "local") {
		t.Fatal("re-proof still called a directory with a tracked file dead")
	}
	if c.Suppress(repo, event) {
		t.Fatal("suppressed an edit to a force-added tracked file")
	}
}

func TestDeadSubtreeExternalExcludesAndConfigInvalidate(t *testing.T) {
	repo := gitInitRepo(t)
	externalDir := t.TempDir()
	first := filepath.Join(externalDir, "ignore-one")
	second := filepath.Join(externalDir, "ignore-two")
	writeFile(t, first, "dead/\n")
	writeFile(t, second, "dead/\n")
	gitIn(t, repo, "config", "core.excludesFile", first)
	writeFile(t, filepath.Join(repo, "dead", "state.db"), "one")

	c, verdicts := newTestDeadCache(t)
	event := filepath.Join(repo, "dead", "state.db")
	c.Suppress(repo, event)
	if !awaitVerdict(t, verdicts, repo, "dead") || !c.Suppress(repo, event) {
		t.Fatal("external excludes file did not establish a dead proof")
	}
	if !c.ObserveGlobal(resolveEventPath(first)) {
		t.Fatal("arbitrary external excludes file was not observable without a route")
	}
	if _, known := c.probeState(repo, "dead"); known {
		t.Fatal("external excludes edit left a stale proof")
	}

	// Re-prove, then change the effective setting itself. The repository config
	// source must invalidate and force re-resolution to the second arbitrary path.
	c.Suppress(repo, event)
	awaitVerdict(t, verdicts, repo, "dead")
	gitIn(t, repo, "config", "core.excludesFile", second)
	configPath := resolveEventPath(filepath.Join(repo, ".git", "config"))
	if !c.ObserveGlobal(configPath) {
		t.Fatal("effective config source change was not observable")
	}
	c.Suppress(repo, event)
	awaitVerdict(t, verdicts, repo, "dead")
	c.mu.Lock()
	observesSecond := c.excludeFiles[resolveEventPath(second)]
	c.mu.Unlock()
	if !observesSecond {
		t.Fatal("cache did not re-resolve the changed core.excludesFile")
	}
}

func TestDeadSubtreeProofWaitsForExternalWatchTopology(t *testing.T) {
	repo := gitInitRepo(t)
	external := filepath.Join(t.TempDir(), "global-ignore")
	writeFile(t, external, "dead/\n")
	gitIn(t, repo, "config", "core.excludesFile", external)
	writeFile(t, filepath.Join(repo, "dead", "state.db"), "one")

	c, verdicts := newTestDeadCache(t)
	c.mu.Lock()
	c.requireActive = true
	c.mu.Unlock()
	event := filepath.Join(repo, "dead", "state.db")
	c.Suppress(repo, event)
	if awaitVerdict(t, verdicts, repo, "dead") {
		t.Fatal("proof became active before its external inputs were watched")
	}
	dirs := c.inputWatchDirs()
	if len(dirs) == 0 {
		t.Fatal("probe did not request external watcher topology")
	}
	// This is the production rebuild ordering: clear generations, start the
	// exact-input observer, then mark exactly its successful directories active.
	c.dropAll()
	c.activateInputWatchDirs(dirs)
	c.Suppress(repo, event)
	if !awaitVerdict(t, verdicts, repo, "dead") || !c.Suppress(repo, event) {
		t.Fatal("proof did not become usable after watcher topology activation")
	}
}

func TestDeadSubtreeMissingIncludeCreationInvalidatesProof(t *testing.T) {
	repo := gitInitRepo(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	global := filepath.Join(home, "global.gitconfig")
	missingInclude := filepath.Join(home, "missing.cfg")
	ignored := filepath.Join(home, "ignored")
	visible := filepath.Join(home, "visible")
	writeFile(t, ignored, "dead/\n")
	writeFile(t, visible, "")
	writeFile(t, global, "[core]\n\texcludesFile = "+ignored+"\n[include]\n\tpath = missing.cfg\n[includeIf \"gitdir:/does/not/match/\"]\n\tpath = ~/conditional-missing.cfg\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	writeFile(t, filepath.Join(repo, "dead", "state.db"), "one")

	c, verdicts := newTestDeadCache(t)
	event := filepath.Join(repo, "dead", "state.db")
	c.Suppress(repo, event)
	if !awaitVerdict(t, verdicts, repo, "dead") || !c.Suppress(repo, event) {
		t.Fatal("initial global excludes file did not establish a dead proof")
	}
	c.mu.Lock()
	observesMissing := c.configFiles[resolveEventPath(missingInclude)]
	observesConditional := c.configFiles[resolveEventPath(filepath.Join(home, "conditional-missing.cfg"))]
	c.mu.Unlock()
	if !observesMissing || !observesConditional {
		t.Fatalf("declared include targets were not observed: regular=%v conditional=%v", observesMissing, observesConditional)
	}

	// The formerly missing include overrides core.excludesFile. Route its create
	// through the same exact-input invalidator production uses.
	writeFile(t, missingInclude, "[core]\n\texcludesFile = "+visible+"\n")
	if !c.ObserveGlobal(resolveEventPath(missingInclude)) {
		t.Fatal("missing include creation was not recognized as a config input")
	}
	if _, known := c.probeState(repo, "dead"); known {
		t.Fatal("include creation left the old dead proof alive")
	}
	if c.Suppress(repo, event) {
		t.Fatal("include creation suppressed before re-proof")
	}
	if awaitVerdict(t, verdicts, repo, "dead") {
		t.Fatal("re-proof ignored the newly created include")
	}
	if c.Suppress(repo, event) {
		t.Fatal("directory remained suppressed after include made it visible")
	}
}

func TestResolveIncludeTargetFormsAndUnsafeValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	origin := filepath.Join(t.TempDir(), "config")
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "config-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value string
		want  string
		ok    bool
	}{
		{value: "relative.cfg", want: filepath.Join(filepath.Dir(origin), "relative.cfg"), ok: true},
		{value: "~/included.cfg", want: filepath.Join(home, "included.cfg"), ok: true},
		{value: filepath.Join(home, "absolute.cfg"), want: filepath.Join(home, "absolute.cfg"), ok: true},
		{value: "missing/tree/included.cfg", want: filepath.Join(realDir, "missing", "tree", "included.cfg"), ok: true},
		{value: "", ok: false},
		{value: "~another-user/config", ok: false},
	} {
		testOrigin := origin
		if tc.value == "missing/tree/included.cfg" {
			testOrigin = filepath.Join(linkDir, "config")
		}
		got, ok := resolveIncludeTarget(tc.value, testOrigin)
		want := resolveObservedInputPath(tc.want)
		if ok != tc.ok || (ok && got != want) {
			t.Errorf("resolveIncludeTarget(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, want, tc.ok)
		}
	}
	if _, ok := resolveIncludeTarget("relative.cfg", ""); ok {
		t.Fatal("relative include without a file origin did not fail open")
	}
}

func TestDeadSubtreeUnrelatedInputDirectoryChurnDoesNotInvalidate(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "dead/\n")
	writeFile(t, filepath.Join(repo, "dead", "state.db"), "one")
	c, verdicts := newTestDeadCache(t)
	event := filepath.Join(repo, "dead", "state.db")
	c.Suppress(repo, event)
	if !awaitVerdict(t, verdicts, repo, "dead") || !c.Suppress(repo, event) {
		t.Fatal("dead proof was not established")
	}

	c.mu.Lock()
	var configInput string
	for path := range c.configFiles {
		configInput = path
		break
	}
	c.mu.Unlock()
	if configInput == "" {
		t.Fatal("probe learned no config input")
	}
	unrelated := filepath.Join(filepath.Dir(configInput), "unrelated-churn.tmp")
	if c.ObserveGlobal(resolveEventPath(unrelated)) {
		t.Fatal("unrelated sibling churn matched the exact-input observer")
	}
	if !c.Suppress(repo, event) {
		t.Fatal("unrelated sibling churn invalidated a fleet-wide proof")
	}
}

func TestDeadSubtreeSymlinkWorkspaceIndexInvalidatesCanonicalKey(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "local/\n")
	writeFile(t, filepath.Join(repo, "local", "keep.sh"), "echo keep\n")
	symlink := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(repo, symlink); err != nil {
		t.Fatal(err)
	}
	node := &workspace.WorkspaceNode{Name: "linked", Path: symlink, Kind: workspace.KindStandaloneProject}
	routes := buildGitEventRoutes(context.Background(), []*models.EnrichedWorkspace{{WorkspaceNode: node}})
	canonical := resolveEventPath(symlink)

	c, verdicts := newTestDeadCache(t)
	event := filepath.Join(canonical, "local", "keep.sh")
	c.Suppress(canonical, event)
	if !awaitVerdict(t, verdicts, canonical, "local") {
		t.Fatal("ignored directory was not proven dead")
	}
	gitIn(t, repo, "add", "-f", "local/keep.sh")
	index := resolveEventPath(filepath.Join(repo, ".git", "index"))
	route, _ := routeGitEvent(index, routes)
	if route == nil || !c.Observe(route, index) {
		t.Fatal("symlink-addressed workspace index did not invalidate")
	}
	if _, known := c.probeState(canonical, "local"); known {
		t.Fatal("index invalidation left the canonical cache key alive")
	}
}

func TestDeadSubtreeTopologyRebuildDropsUnseenMutation(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "dead/\n")
	writeFile(t, filepath.Join(repo, "dead", "state.db"), "one")
	c, verdicts := newTestDeadCache(t)
	event := filepath.Join(repo, "dead", "state.db")
	c.Suppress(repo, event)
	if !awaitVerdict(t, verdicts, repo, "dead") || !c.Suppress(repo, event) {
		t.Fatal("initial dead proof was not established")
	}

	// runGlobalGitEvents calls dropAll before every topology rebuild. Mutate while
	// absent, then model re-add: no proof from the earlier topology may survive.
	c.dropAll()
	writeFile(t, filepath.Join(repo, ".gitignore"), "")
	if c.Suppress(repo, event) {
		t.Fatal("proof survived workspace removal and re-add")
	}
	if awaitVerdict(t, verdicts, repo, "dead") {
		t.Fatal("unseen mutation was not reflected by the re-proof")
	}
}

// The hot path must never do I/O. With a prober that never answers, Suppress
// still returns immediately — and returns false, so the event scans.
func TestDeadSubtreeCacheMissFailsOpenWithoutIO(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "gcloud\n")
	writeFile(t, filepath.Join(repo, "gcloud", "credentials.db"), "token")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	c := newDeadSubtreeCacheStopped()
	c.probeFn = func(context.Context, probeRequest) probeResult {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
		return probeResult{dead: true}
	}
	go c.run(ctx)
	defer close(blocked)

	event := filepath.Join(repo, "gcloud", "credentials.db")
	done := make(chan bool, 1)
	go func() { done <- c.Suppress(repo, event) }()
	select {
	case suppressed := <-done:
		if suppressed {
			t.Fatal("cache miss suppressed the event instead of failing open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Suppress blocked on the prober: the FSEvents goroutine would stall")
	}

	<-entered
	// While the probe is stuck, every further event keeps scanning, and the queue
	// is not re-filled with duplicates of the same directory.
	for i := 0; i < 3; i++ {
		if c.Suppress(repo, event) {
			t.Fatal("suppressed an event whose probe has not answered")
		}
	}
	if got := len(c.queue); got != 0 {
		t.Fatalf("queued %d duplicate probes for one pending directory", got)
	}
}

// A file sitting directly in the repository root has no depth-1 directory to
// prove dead, so it is never suppressed — even when the ignore rules match it
// (go.work and tracked .gitignore files were both in the measured blindness set).
func TestDeadSubtreeNeverSuppressesRepoRootFiles(t *testing.T) {
	repo := gitInitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "go.work\n.gitignore\n")
	writeFile(t, filepath.Join(repo, "go.work"), "go 1.24\n")

	c, verdicts := newTestDeadCache(t)
	for _, event := range []string{
		filepath.Join(repo, "go.work"),
		filepath.Join(repo, ".gitignore"),
		repo,
	} {
		if c.Suppress(repo, event) {
			t.Fatalf("suppressed repository-root path %s", event)
		}
	}
	select {
	case v := <-verdicts:
		t.Fatalf("probed %q for a repository-root event", v.dir)
	case <-time.After(250 * time.Millisecond):
	}
}

// deadSubtreeCandidate is the whole eligibility rule, and it is pure. Depth 1
// only, hard exemptions on the full path, and no pathspec magic — `git ls-files
// -- <dir>` matches a PATTERN, and a pattern that matches nothing is
// indistinguishable from "no tracked files here".
func TestDeadSubtreeCandidate(t *testing.T) {
	const repo = "/code/repo"
	tests := []struct {
		name string
		path string
		want string
	}{
		{"depth-1 directory", repo + "/gcloud/credentials.db", "gcloud"},
		{"deep path yields the first component", repo + "/gcloud/logs/a/b.log", "gcloud"},
		{"file in the repository root", repo + "/go.work", ""},
		{"the repository root itself", repo, ""},
		{"a bare directory entry at depth 1", repo + "/gcloud", ""},
		{"gitignore at the root", repo + "/.gitignore", ""},
		{"gitignore at depth", repo + "/dead/nested/.gitignore", ""},
		{"the .git entry itself", repo + "/.git", ""},
		{"inside the git dir", repo + "/.git/index", ""},
		{"inside a nested git dir", repo + "/vendor/dep/.git/HEAD", ""},
		{"outside the repository", "/code/other/file", ""},
		{"glob magic", repo + "/we*rd/file", ""},
		{"pathspec prefix magic", repo + "/:(top)/file", ""},
		{"character class magic", repo + "/dir[1]/file", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, ok := deadSubtreeCandidate(repo, tc.path)
			if tc.want == "" {
				if ok {
					t.Fatalf("deadSubtreeCandidate(%q) = %q, want ineligible", tc.path, dir)
				}
				return
			}
			if !ok || dir != tc.want {
				t.Fatalf("deadSubtreeCandidate(%q) = (%q, %v), want %q", tc.path, dir, ok, tc.want)
			}
		})
	}
}
