package watcher

import (
	"container/list"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/command"
	"github.com/grovetools/core/logging"
)

// Dead-subtree suppression: why this exists, and why it is not a gitignore
// matcher.
//
// Measured on the live global daemon in steady state, ~99% of watcher scans came
// from ONE repository (~/.config), and every non-.lock waking path under it was
// gcloud/credentials.db, gcloud/access_tokens.db or gcloud/logs/** — all matched
// by that repo's own `.gitignore` line `gcloud`. The scan each event triggers
// costs ~6ms and changes nothing, ~70 times a minute.
//
// Two mechanisms were measured and REJECTED before this one:
//
//   - A compiled path-pattern matcher goes blind to tracked files. Across 452
//     repos / 76,834 tracked files, `git ls-files | git check-ignore --stdin
//     --no-index` found 547 tracked files (0.71%) a pure path matcher would call
//     ignored — hand-edited scripts under local/, go.work in two repos, and two
//     tracked .gitignore files (matched by the pattern `.gitignore`, which would
//     freeze a repo's own ignore rules permanently). 142 are rescued only by
//     negation patterns. A matcher cannot see the index, so the failure is
//     silent, and the only backstop is the collector's HOURLY reconciler — which
//     scoped daemons do not run at all.
//   - allExcludeDirs() (language.go) yields 0% of the measured events (`gcloud`
//     is on no list) while blinding 1451 tracked files (1.89%), mostly testdata
//     and vendor. That list is tuned for source indexing, where skipping
//     testdata is correct; for git status it is a correctness bug.
//
// So the unit of suppression here is not a pattern but A DIRECTORY PREFIX
// PROVABLY INVISIBLE TO GIT, and git itself is the matcher. Per candidate
// directory, once, cached:
//
//  1. `git check-ignore -q -- <dir>` exits 0 — git's own matcher, so negation,
//     per-directory .gitignore at any depth, info/exclude, core.excludesFile and
//     last-match-wins precedence are correct by construction, with zero
//     hand-rolled semantics and zero new dependencies.
//  2. `git ls-files -z -- <dir>` is empty — no tracked file lives under it, which
//     makes the 547-file blindness class structurally impossible rather than
//     merely mitigated.
//
// Both true ⇒ the prefix is dead and every later event under it costs one map
// lookup. On the measured workload that is 2 forks once, replacing ~9,000 forks
// per 12 minutes.
//
// The cache FAILS OPEN in every direction. A false negative (suppressing
// something git can see) costs up to an hour of staleness on a global daemon and
// is PERMANENT on a scoped one, so an unknown answer, a probe error, a queue
// overflow, an invalidation and a shutdown all mean "scan".

const (
	// deadSubtreeMaxDirs bounds proofs per repository. A repo with this many
	// distinct depth-1 directories waking the watcher is not the workload this
	// cache exists for, so the whole repo's proofs are dropped rather than
	// evicted one at a time: re-proving costs two forks per directory that still
	// matters, and none for the ones that stopped churning.
	deadSubtreeMaxDirs = 256
	// deadSubtreeMaxRepos bounds the LRU over repositories.
	deadSubtreeMaxRepos = 1024
	// deadSubtreeQueueDepth bounds the probe backlog. A full queue drops the
	// request (the event scans, as it does today) instead of blocking the
	// FSEvents goroutine.
	deadSubtreeQueueDepth = 64
	// deadSubtreeProbeTimeout bounds one probe's two git invocations. A repo on a
	// stalled network mount must not wedge the prober.
	deadSubtreeProbeTimeout = 5 * time.Second
)

// gitignoreName is the per-directory ignore file. It is both an invalidation
// trigger and a hard suppression exemption.
const gitignoreName = ".gitignore"

// deadSubtreeCache holds, per repository, the depth-1 directories proven
// invisible to git. Suppress() is pure map lookups; every fork happens on the
// single background prober.
type deadSubtreeCache struct {
	mu    sync.Mutex
	repos map[string]*repoProofs
	// lru orders repos most-recently-used first; elements hold the repo root.
	lru *list.List
	// nextGen stamps each repo entry so a probe that finishes after its entry
	// was invalidated cannot record a stale proof.
	nextGen uint64
	// excludeFiles and configFiles are exact input paths learned from git. A
	// separate, bounded observer polls only these inputs and their nearest existing
	// ancestors; they must never widen the recursive repository FSEvents stream.
	// Exact/ancestor matching prevents unrelated sibling churn from causing scans.
	excludeFiles  map[string]bool
	configFiles   map[string]bool
	watchChanged  chan struct{}
	activeDirs    map[string]bool
	requireActive bool

	queue chan probeRequest
	ulog  *logging.UnifiedLogger

	// probeFn runs the two git questions. Indirected only so tests can avoid
	// forking; production is probeDeadSubtree.
	probeFn func(ctx context.Context, req probeRequest) probeResult
	// probed, when non-nil, is called after a verdict is recorded. Test seam for
	// awaiting the async probe; production leaves it nil. Both seams are written
	// before run() starts and never after, so the prober reads them race-free.
	probed func(repoRoot, dir string, dead bool)
}

// repoProofs is one repository's suppression state.
type repoProofs struct {
	elem *list.Element
	gen  uint64
	// dirs maps a depth-1 directory name to its verdict: true = proven dead,
	// false = proven live. A missing key means "unknown, scan and go find out".
	dirs map[string]bool
	// pending marks directories with a probe in flight, so a directory churning
	// 70 times a minute enqueues one probe rather than 70.
	pending map[string]bool
	// excludesResolved records that this repo's core.excludesFile has been looked
	// up (one extra fork per repo, not per directory).
	excludesResolved bool
}

type probeRequest struct {
	repoRoot string
	dir      string
	gen      uint64
	// needExcludes asks the prober for this repo's external git inputs too, set
	// only on the first probe of a repo.
	needExcludes bool
}

type probeResult struct {
	dead           bool
	excludesFile   string
	configFiles    []string
	inputsComplete bool
}

const deadSubtreeMaxObservedFiles = deadSubtreeMaxRepos * 8

// newDeadSubtreeCache starts the background prober, which exits on ctx.
func newDeadSubtreeCache(ctx context.Context) *deadSubtreeCache {
	c := newDeadSubtreeCacheStopped()
	// Production proofs are usable only after every discovered external input's
	// parent is installed in the separate non-recursive exact-input observer.
	c.requireActive = true
	go c.run(ctx)
	return c
}

// newDeadSubtreeCacheStopped builds the cache without its prober, so tests can
// install seams before anything reads them.
func newDeadSubtreeCacheStopped() *deadSubtreeCache {
	return &deadSubtreeCache{
		repos:        make(map[string]*repoProofs),
		lru:          list.New(),
		excludeFiles: make(map[string]bool),
		configFiles:  make(map[string]bool),
		watchChanged: make(chan struct{}, 1),
		activeDirs:   make(map[string]bool),
		queue:        make(chan probeRequest, deadSubtreeQueueDepth),
		ulog:         logging.NewUnifiedLogger("groved.watcher.git.deadsubtree"),
		probeFn:      probeDeadSubtree,
	}
}

// Observe handles the events that VOID cached proofs. It runs before Suppress
// and its return value is informational: an invalidating event is never
// suppressed, because every path it can arrive on is either a hard exemption or
// an internal route.
//
// The triggers are the complete set of inputs to the two proofs:
//   - a .gitignore at ANY depth in the repository (ignore rules moved),
//   - <gitDir>/info/exclude or <commonDir>/info/exclude (same),
//   - a resolved core.excludesFile (same, fleet-wide),
//   - <gitDir>/index — the tracked set moved. This is what makes `git add -f`
//     correct: every emptiness proof in that repo is void, so it is dropped,
//     re-proved, and the directory is now live. tracked→ignored is symmetric.
func (c *deadSubtreeCache) Observe(route *gitEventRoute, path string) bool {
	if c == nil || route == nil {
		return false
	}
	base := filepath.Base(path)
	slashed := filepath.ToSlash(path)
	if base != gitignoreName && !strings.HasSuffix(slashed, "/info/exclude") && !(route.internal && base == "index") {
		return false
	}

	// A git-internal route fans out to every worktree sharing the commondir.
	// Workspace paths are canonicalized exactly like suppression cache keys, so
	// discovery through a symlink cannot leave the real-path proof behind.
	c.drop(route.root)
	for _, node := range route.nodes {
		if node != nil {
			c.drop(resolveEventPath(node.Path))
		}
	}
	return true
}

// ObserveGlobal recognizes arbitrary external ignore files and every active or
// candidate git-config source learned by a probe. It intentionally works without
// a repository route. Config changes also discard learned inputs: until a later
// probe resolves and installs the new effective topology, the cache fails open.
func (c *deadSubtreeCache) ObserveGlobal(path string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	isExclude, isConfig, isAncestor := false, false, false
	matches := func(target string) bool {
		if path == target {
			return true
		}
		// A nearest-existing-parent watch also has to notice creation/removal of
		// the first missing component so the observer can move down to the exact
		// target. Unrelated siblings do not match this boundary-aware prefix.
		if strings.HasPrefix(target, path+string(filepath.Separator)) {
			isAncestor = true
			return true
		}
		return false
	}
	for target := range c.excludeFiles {
		if matches(target) {
			isExclude = true
		}
	}
	for target := range c.configFiles {
		if matches(target) {
			isConfig = true
		}
	}
	if !isExclude && !isConfig {
		c.mu.Unlock()
		return false
	}
	c.clearProofsLocked()
	if isConfig {
		c.excludeFiles = make(map[string]bool)
		c.configFiles = make(map[string]bool)
		c.activeDirs = make(map[string]bool)
		c.signalWatchChangedLocked()
	} else if isAncestor {
		// The target set is still valid, but its nearest existing watch directory
		// may have changed after a parent was created, removed, or renamed.
		c.signalWatchChangedLocked()
	}
	c.mu.Unlock()
	return true
}

// Suppress reports whether an event under repoRoot can be dropped without a git
// status scan. It does pure map lookups: no I/O, no forks, nothing that can
// stall the FSEvents goroutine. An unknown directory enqueues a probe and
// returns false, so the first event on any path always scans.
func (c *deadSubtreeCache) Suppress(repoRoot, path string) bool {
	if c == nil {
		return false
	}
	dir, ok := deadSubtreeCandidate(repoRoot, path)
	if !ok {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	proofs := c.repos[repoRoot]
	if proofs == nil {
		proofs = c.addRepoLocked(repoRoot)
	} else {
		c.lru.MoveToFront(proofs.elem)
	}
	if dead, known := proofs.dirs[dir]; known {
		return dead
	}
	c.enqueueLocked(repoRoot, dir, proofs)
	return false
}

// deadSubtreeCandidate returns the depth-1 directory under repoRoot that path
// lies inside, and whether path is a suppression candidate at all.
//
// DEPTH 1 ONLY. The candidate is the first component of the repo-relative path
// and only when there are at least two components, so a file sitting directly in
// the repository root is never suppressed. Deeper candidates (plans/*/.artifacts
// and friends) produced ZERO measured events; they are not worth a probe until
// instrumentation says otherwise.
func deadSubtreeCandidate(repoRoot, path string) (string, bool) {
	if repoRoot == "" || exemptFromSuppression(path) {
		return "", false
	}
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		return "", false // a file directly in the repository root
	}
	dir := parts[0]
	if dir == "" || dir == "." || dir == ".." {
		return "", false
	}
	// `git ls-files -- <dir>` takes a PATHSPEC, so a directory name carrying
	// pathspec magic (globs, exclusions, the `:(...)` prefix) would be matched as
	// a pattern instead of a prefix — and a pattern that matches nothing looks
	// exactly like "no tracked files here". Refuse to reason about those.
	if strings.ContainsAny(dir, "*?[]:!\\") {
		return "", false
	}
	return dir, true
}

// exemptFromSuppression names paths that must reach a scan whatever the cache
// holds. These are checked on the FULL path, before any candidate is derived:
//
//   - .gitignore, because a repo's own ignore rules are frequently tracked and a
//     path matcher that swallowed them would freeze those rules forever;
//   - .git and anything inside a git dir, because HEAD/index/refs writes are how
//     grove sees commits and branch switches, and nothing stops a .gitignore
//     from containing the patterns `index`, `HEAD` or `refs`.
func exemptFromSuppression(path string) bool {
	base := filepath.Base(path)
	if base == gitignoreName || base == ".git" {
		return true
	}
	return strings.Contains(filepath.ToSlash(path), "/.git/")
}

// addRepoLocked creates a repo entry, evicting the least recently used repo when
// the LRU is full. Callers must hold mu.
func (c *deadSubtreeCache) addRepoLocked(repoRoot string) *repoProofs {
	c.nextGen++
	proofs := &repoProofs{
		gen:     c.nextGen,
		dirs:    make(map[string]bool),
		pending: make(map[string]bool),
	}
	proofs.elem = c.lru.PushFront(repoRoot)
	c.repos[repoRoot] = proofs
	for c.lru.Len() > deadSubtreeMaxRepos {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		delete(c.repos, oldest.Value.(string))
	}
	return proofs
}

// enqueueLocked schedules one probe, dropping it when the queue is full so the
// FSEvents goroutine never blocks behind git. Callers must hold mu.
func (c *deadSubtreeCache) enqueueLocked(repoRoot, dir string, proofs *repoProofs) {
	if proofs.pending[dir] {
		return
	}
	proofs.pending[dir] = true
	req := probeRequest{repoRoot: repoRoot, dir: dir, gen: proofs.gen, needExcludes: !proofs.excludesResolved}
	select {
	case c.queue <- req:
	default:
		// Clear the mark so a later event can try again; until then the directory
		// stays unknown and keeps scanning, which is today's behavior.
		delete(proofs.pending, dir)
	}
}

// drop discards one repository's proofs. Its generation is not reused, so a
// probe already in flight cannot record against the replacement entry.
func (c *deadSubtreeCache) drop(repoRoot string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if proofs, ok := c.repos[repoRoot]; ok {
		c.lru.Remove(proofs.elem)
		delete(c.repos, repoRoot)
	}
}

// dropAll discards every proof, used when a fleet-wide ignore source moves.
func (c *deadSubtreeCache) dropAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearProofsLocked()
}

func (c *deadSubtreeCache) clearProofsLocked() {
	c.repos = make(map[string]*repoProofs)
	c.lru.Init()
}

func (c *deadSubtreeCache) signalWatchChangedLocked() {
	select {
	case c.watchChanged <- struct{}{}:
	default:
	}
}

// inputObservationPaths returns the bounded exact file set consumed by the
// Darwin polling observer. It is copied so polling never holds the cache lock.
func (c *deadSubtreeCache) inputObservationPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	paths := make(map[string]bool, len(c.excludeFiles)+len(c.configFiles))
	for path := range c.excludeFiles {
		paths[path] = true
	}
	for path := range c.configFiles {
		paths[path] = true
	}
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// inputWatchDirs describes the nearest-existing-ancestor coverage required by
// the exact input set. It is also the proof-activation key used by the cache.
func (c *deadSubtreeCache) inputWatchDirs() []string {
	paths := c.inputObservationPaths()
	roots := make(map[string]bool, len(paths))
	for _, path := range paths {
		roots[observationRoot(path)] = true
	}
	out := make([]string, 0, len(roots))
	for root := range roots {
		out = append(out, root)
	}
	sort.Strings(out)
	return out
}

func (c *deadSubtreeCache) activateInputWatchDirs(dirs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeDirs = make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		c.activeDirs[dir] = true
	}
}

// resolveObservedInputPath canonicalizes the longest existing prefix while
// preserving every missing suffix component. resolveEventPath only handles one
// missing leaf, whereas include targets may name an absent directory tree.
func resolveObservedInputPath(path string) string {
	path = filepath.Clean(path)
	missing := make([]string, 0)
	for {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return filepath.Clean(real)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
}

// observationRoot returns an existing directory. Config candidates are watched
// even before creation, so their immediate parent may not exist yet.
func observationRoot(path string) string {
	root := resolveEventPath(filepath.Dir(path))
	for {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			return root
		}
		root = parent
	}
}

// run is the single prober. Every fork the cache makes happens here.
func (c *deadSubtreeCache) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-c.queue:
			result := c.probeFn(ctx, req)
			if ctx.Err() != nil {
				return
			}
			c.record(req, result)
		}
	}
}

// record stores a verdict, unless the repo entry was invalidated or evicted
// while the probe ran — in which case the proof describes a state that no longer
// exists and is discarded.
func (c *deadSubtreeCache) record(req probeRequest, result probeResult) {
	c.mu.Lock()
	proofs, ok := c.repos[req.repoRoot]
	if !ok || proofs.gen != req.gen {
		c.mu.Unlock()
		return
	}
	delete(proofs.pending, req.dir)
	inputsObservable := true
	if req.needExcludes {
		proofs.excludesResolved = true
		inputsObservable = result.inputsComplete
		changed := false
		addInput := func(file string, target map[string]bool) {
			if file == "" {
				return
			}
			file = resolveEventPath(file)
			if len(c.excludeFiles)+len(c.configFiles) >= deadSubtreeMaxObservedFiles && !c.excludeFiles[file] && !c.configFiles[file] {
				inputsObservable = false
				return
			}
			if !target[file] {
				target[file] = true
				changed = true
			}
			if c.requireActive && !c.activeDirs[observationRoot(file)] {
				inputsObservable = false
			}
		}
		addInput(result.excludesFile, c.excludeFiles)
		for _, file := range result.configFiles {
			addInput(file, c.configFiles)
		}
		if changed {
			c.signalWatchChangedLocked()
		}
	}
	dead := result.dead && inputsObservable
	if len(proofs.dirs) >= deadSubtreeMaxDirs {
		proofs.dirs = make(map[string]bool)
	}
	proofs.dirs[req.dir] = dead
	hook := c.probed
	c.mu.Unlock()

	if dead {
		c.ulog.Debug("git watcher: subtree proven invisible to git").
			Field("repo", req.repoRoot).
			Field("dir", req.dir).
			Log(context.Background())
	}
	if hook != nil {
		hook(req.repoRoot, req.dir, dead)
	}
}

// probeState reports a directory's cached verdict. Test-only accessor: it lets a
// test wait for the async probe to land before asserting on Suppress.
func (c *deadSubtreeCache) probeState(repoRoot, dir string) (dead, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	proofs, ok := c.repos[repoRoot]
	if !ok {
		return false, false
	}
	dead, known = proofs.dirs[dir]
	return dead, known
}

// probeDeadSubtree asks git the two questions, in the cheap-first order: a
// directory that is not ignored needs no emptiness proof. It also resolves
// core.excludesFile, so the cache knows which outside file invalidates it.
//
// Any error, any ambiguity, any timeout ⇒ not dead.
func probeDeadSubtree(ctx context.Context, req probeRequest) probeResult {
	ctx, cancel := context.WithTimeout(ctx, deadSubtreeProbeTimeout)
	defer cancel()

	repoRoot, dir := req.repoRoot, req.dir
	result := probeResult{}
	if req.needExcludes {
		var excludesOK, configOK bool
		result.excludesFile, excludesOK = resolveExcludesFile(ctx, repoRoot)
		result.configFiles, configOK = resolveConfigFiles(ctx, repoRoot)
		result.inputsComplete = excludesOK && configOK
	}

	// check-ignore is index-aware by default: a directory holding a force-added
	// file already reports "not ignored" here. The ls-files proof below is the
	// belt to that suspenders, because the same is NOT true of a directory whose
	// tracked file was never on disk (a sparse checkout, a deleted-but-staged
	// path).
	_, code, err := runGitProbe(ctx, repoRoot, "check-ignore", "-q", "--", dir)
	if err != nil || code != 0 {
		return result
	}
	out, code, err := runGitProbe(ctx, repoRoot, "ls-files", "-z", "--", dir)
	if err != nil || code != 0 {
		return result
	}
	result.dead = len(strings.Trim(string(out), "\x00")) == 0
	return result
}

// resolveConfigFiles returns active include-expanded config origins, declared
// include targets (even when absent/empty), and candidate global/system files.
// Any include whose target cannot be resolved safely makes the graph incomplete
// and therefore disables dead-subtree proofs.
func resolveConfigFiles(ctx context.Context, repoRoot string) ([]string, bool) {
	files := make(map[string]bool)
	out, code, err := runGitProbe(ctx, repoRoot, "config", "-z", "--show-origin", "--list", "--includes")
	if err != nil || code != 0 {
		return nil, false
	}
	fields := strings.Split(string(out), "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%2 != 0 {
		return nil, false
	}
	for i := 0; i < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		key, value, hasValue := strings.Cut(entry, "\n")
		originFile := ""
		if strings.HasPrefix(origin, "file:") {
			originFile = strings.TrimPrefix(origin, "file:")
			if !filepath.IsAbs(originFile) {
				originFile = filepath.Join(repoRoot, originFile)
			}
			originFile = resolveObservedInputPath(originFile)
			files[originFile] = true
		}
		lowerKey := strings.ToLower(key)
		isInclude := lowerKey == "include.path" || (strings.HasPrefix(lowerKey, "includeif.") && strings.HasSuffix(lowerKey, ".path"))
		if !isInclude {
			continue
		}
		if !hasValue {
			return nil, false
		}
		target, ok := resolveIncludeTarget(value, originFile)
		if !ok {
			return nil, false
		}
		files[target] = true
	}
	if home, err := os.UserHomeDir(); err == nil {
		files[resolveObservedInputPath(filepath.Join(home, ".gitconfig"))] = true
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		files[resolveObservedInputPath(filepath.Join(xdg, "git", "config"))] = true
	}
	for _, candidate := range []string{os.Getenv("GIT_CONFIG_GLOBAL"), os.Getenv("GIT_CONFIG_SYSTEM"), "/etc/gitconfig"} {
		if candidate != "" {
			files[resolveObservedInputPath(candidate)] = true
		}
	}
	result := make([]string, 0, len(files))
	for file := range files {
		result = append(result, file)
	}
	sort.Strings(result)
	return result, true
}

func resolveIncludeTarget(value, originFile string) (string, bool) {
	if value == "" {
		return "", false
	}
	path := value
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	} else if strings.HasPrefix(path, "~") {
		// Git supports ~user, but resolving another account through platform
		// databases in the watcher hot path is neither bounded nor portable.
		return "", false
	} else if !filepath.IsAbs(path) {
		if originFile == "" {
			return "", false
		}
		path = filepath.Join(filepath.Dir(originFile), path)
	}
	return resolveObservedInputPath(path), true
}

// resolveExcludesFile returns the repository's effective core.excludesFile, or
// git's documented default location when the setting is unset. The path is
// canonicalized the same way event paths are, so it can be compared to one.
func resolveExcludesFile(ctx context.Context, repoRoot string) (string, bool) {
	out, code, err := runGitProbe(ctx, repoRoot, "config", "--path", "--get", "core.excludesFile")
	if err != nil || (code != 0 && code != 1) {
		return "", false
	}
	file := ""
	if code == 0 {
		file = strings.TrimSpace(string(out))
	}
	if file == "" {
		// git's fallback: $XDG_CONFIG_HOME/git/ignore, else ~/.config/git/ignore.
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", false
			}
			base = filepath.Join(home, ".config")
		}
		file = filepath.Join(base, "git", "ignore")
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(repoRoot, file)
	}
	return resolveObservedInputPath(file), true
}

// runGitProbe runs one read-only git command in repoRoot and returns its stdout
// and exit code. A non-zero exit is a RESULT, not an error, for these probes:
// `check-ignore -q` exits 1 to mean "not ignored".
func runGitProbe(ctx context.Context, repoRoot string, args ...string) ([]byte, int, error) {
	cmd, err := command.NewSafeBuilder().Build(ctx, "git", args...)
	if err != nil {
		return nil, -1, err
	}
	execCmd := cmd.Exec()
	execCmd.Dir = repoRoot
	out, err := execCmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && ctx.Err() == nil {
			return out, exitErr.ExitCode(), nil
		}
		return out, -1, err
	}
	return out, 0, nil
}
