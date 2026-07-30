package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// attributionWorld is the production shape that produced the bad record:
// ONE centralized notebook plans directory (`workspaces/grovetools/plans`)
// shared by the grovetools ecosystem root, its `perf-audit` worktree, and
// member checkouts sitting inside OTHER, unrelated worktree containers.
// Every one of these nodes resolves to the same plans dir, so every one of
// them is a candidate owner for a job file discovered under it.
type attributionWorld struct {
	cfg       *config.Config
	nodes     []*workspace.WorkspaceNode
	ecoRoot   *workspace.WorkspaceNode
	perfAudit *workspace.WorkspaceNode
	tuimux    *workspace.WorkspaceNode
	plansDir  string
}

func newAttributionWorld(t *testing.T) *attributionWorld {
	t.Helper()
	root := t.TempDir()
	notebookRoot := filepath.Join(root, "notebook")
	ecoPath := filepath.Join(root, "code", "grovetools")
	worktrees := filepath.Join(root, "worktrees", "grovetools-0bd46c64")

	w := &attributionWorld{
		plansDir: filepath.Join(notebookRoot, "workspaces", "grovetools", "plans"),
		cfg: &config.Config{Notebooks: &config.NotebooksConfig{
			Definitions: map[string]*config.Notebook{"test": {RootDir: notebookRoot}},
			Rules:       &config.NotebookRules{Default: "test"},
		}},
	}

	w.ecoRoot = &workspace.WorkspaceNode{
		Name: "grovetools", Path: ecoPath,
		Kind:              workspace.KindEcosystemRoot,
		RootEcosystemPath: ecoPath,
	}
	w.perfAudit = &workspace.WorkspaceNode{
		Name: "perf-audit", Path: filepath.Join(worktrees, "perf-audit"),
		Kind:                workspace.KindEcosystemWorktree,
		ParentProjectPath:   ecoPath,
		ParentEcosystemPath: ecoPath,
		RootEcosystemPath:   ecoPath,
	}
	// Member checkouts inside two OTHER containers. `tuimux` is the workspace
	// the buggy lookup filed the perf-audit job under.
	member := func(container, name string) *workspace.WorkspaceNode {
		return &workspace.WorkspaceNode{
			Name: name, Path: filepath.Join(worktrees, container, name),
			Kind:                workspace.KindEcosystemWorktreeSubProjectWorktree,
			ParentProjectPath:   filepath.Join(ecoPath, name),
			ParentEcosystemPath: filepath.Join(worktrees, container),
			RootEcosystemPath:   ecoPath,
		}
	}
	w.tuimux = member("pi-flow-subagents", "tuimux")
	w.nodes = []*workspace.WorkspaceNode{
		w.ecoRoot, w.perfAudit, w.tuimux,
		member("pi-flow-subagents", "daemon"),
		member("misc-fixes", "treemux"),
		member("misc-fixes", "core"),
	}

	if err := os.MkdirAll(filepath.Join(w.plansDir, "perf-audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	return w
}

// enriched wraps the nodes in the given order — the order ComputeWatchPaths
// receives from store.GetWorkspaces(), which is map iteration order.
func (w *attributionWorld) enriched(order []*workspace.WorkspaceNode) []*models.EnrichedWorkspace {
	out := make([]*models.EnrichedWorkspace, 0, len(order))
	for _, node := range order {
		out = append(out, &models.EnrichedWorkspace{WorkspaceNode: node})
	}
	return out
}

// writeJob writes a plan job file with the given frontmatter worktree key.
func (w *attributionWorld) writeJob(t *testing.T, name, id, worktree string) string {
	t.Helper()
	body := "---\nid: " + id + "\ntitle: " + id + "\ntype: interactive_agent\nstatus: idle\n"
	if worktree != "" {
		body += "worktree: " + worktree + "\n"
	}
	body += "---\n\nbody\n"
	path := filepath.Join(w.plansDir, "perf-audit", name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// discover runs the production path — watch-set computation followed by the
// fsnotify handler — and returns the JobInfo the store ended up holding.
func (w *attributionWorld) discover(t *testing.T, order []*workspace.WorkspaceNode, jobPath, id string) *models.JobInfo {
	t.Helper()
	st := store.New()
	// A debounce far longer than the test keeps the refresh timer from firing;
	// only the attribution leg of HandleEvents is under test here.
	h := NewFlowHandler(st, w.cfg, 600000)
	h.ComputeWatchPaths(w.enriched(order))
	if err := h.HandleEvents(context.Background(), []fsnotify.Event{{Name: jobPath, Op: fsnotify.Write}}); err != nil {
		t.Fatal(err)
	}
	job := st.GetJob(id)
	if job == nil {
		t.Fatalf("job %q was not discovered from %q", id, jobPath)
	}
	return job
}

// TestJobAttributionFollowsFrontmatterWorktreeNotASharedPlansDirNeighbour is
// the regression for the persisted record that filed
// `impl-watcher-noop-attribution` under a `tuimux` checkout in the
// `pi-flow-subagents` container. The job's frontmatter names the `perf-audit`
// worktree of the grovetools ecosystem, and that — not any of the other
// workspaces sharing the plans directory — is where the job runs.
func TestJobAttributionFollowsFrontmatterWorktreeNotASharedPlansDirNeighbour(t *testing.T) {
	w := newAttributionWorld(t)
	jobPath := w.writeJob(t, "60-impl-watcher-noop-attribution.md", "impl-watcher-noop-attribution-20d21930", "perf-audit")

	job := w.discover(t, w.nodes, jobPath, "impl-watcher-noop-attribution-20d21930")

	if job.WorkDir != w.perfAudit.Path {
		t.Fatalf("WorkDir = %q, want the frontmatter worktree %q", job.WorkDir, w.perfAudit.Path)
	}
	if job.Repo != "perf-audit" || job.Branch != "perf-audit" {
		t.Fatalf("Repo=%q Branch=%q, want perf-audit/perf-audit", job.Repo, job.Branch)
	}
	if job.WorkDir == w.tuimux.Path {
		t.Fatalf("job was filed under the unrelated member checkout %q", w.tuimux.Path)
	}
}

// TestJobAttributionIsIndependentOfWorkspaceIterationOrder pins the property
// the bug violated. store.GetWorkspaces() builds its slice by ranging a map,
// so the daemon sees a different permutation on every refresh; the persisted
// workspace must not.
func TestJobAttributionIsIndependentOfWorkspaceIterationOrder(t *testing.T) {
	w := newAttributionWorld(t)
	jobPath := w.writeJob(t, "60-impl.md", "impl-job", "perf-audit")
	plainPath := w.writeJob(t, "61-plain.md", "plain-job", "")

	var want, wantPlain *models.JobInfo
	for i := range w.nodes {
		// Rotations plus the reversal below cover both "the right owner is
		// first" and "the right owner is last" for every node.
		order := append(append([]*workspace.WorkspaceNode{}, w.nodes[i:]...), w.nodes[:i]...)
		for _, nodes := range [][]*workspace.WorkspaceNode{order, reversedNodes(order)} {
			job := w.discover(t, nodes, jobPath, "impl-job")
			if want == nil {
				want = job
			} else if job.WorkDir != want.WorkDir || job.Repo != want.Repo || job.Branch != want.Branch {
				t.Fatalf("attribution flipped with workspace order: got %q/%q/%q, first run gave %q/%q/%q",
					job.WorkDir, job.Repo, job.Branch, want.WorkDir, want.Repo, want.Branch)
			}

			// A job with no `worktree:` key has only the plans directory to go
			// on, which is exactly the case with the most candidate owners.
			plain := w.discover(t, nodes, plainPath, "plain-job")
			if wantPlain == nil {
				wantPlain = plain
			} else if plain.WorkDir != wantPlain.WorkDir || plain.Repo != wantPlain.Repo {
				t.Fatalf("owner-derived attribution flipped with workspace order: got %q/%q, first run gave %q/%q",
					plain.WorkDir, plain.Repo, wantPlain.WorkDir, wantPlain.Repo)
			}
		}
	}
	if want.WorkDir != w.perfAudit.Path {
		t.Fatalf("stable but wrong: WorkDir = %q, want %q", want.WorkDir, w.perfAudit.Path)
	}
	// Without a frontmatter worktree the answer is the workspace the plans
	// directory is named after — the ecosystem root — not a member that merely
	// inherits it.
	if wantPlain.WorkDir != w.ecoRoot.Path || wantPlain.Repo != "grovetools" {
		t.Fatalf("owner fallback = %q/%q, want the ecosystem root %q/grovetools",
			wantPlain.WorkDir, wantPlain.Repo, w.ecoRoot.Path)
	}
	if wantPlain.Branch != "" {
		t.Fatalf("Branch = %q, want empty when the frontmatter names no worktree", wantPlain.Branch)
	}
}

// TestJobAttributionKeepsOwnerWhenWorktreeNameIsForeign: a `worktree:` naming
// something outside the plan owner's ecosystem must degrade to the owner, not
// reach across ecosystems. Branch still reports what the file claims.
func TestJobAttributionKeepsOwnerWhenWorktreeNameIsForeign(t *testing.T) {
	w := newAttributionWorld(t)
	jobPath := w.writeJob(t, "62-foreign.md", "foreign-job", "some-other-ecosystems-branch")

	job := w.discover(t, w.nodes, jobPath, "foreign-job")

	if job.WorkDir != w.ecoRoot.Path || job.Repo != "grovetools" {
		t.Fatalf("WorkDir=%q Repo=%q, want the plan owner %q/grovetools", job.WorkDir, job.Repo, w.ecoRoot.Path)
	}
	if job.Branch != "some-other-ecosystems-branch" {
		t.Fatalf("Branch = %q, want the frontmatter value verbatim", job.Branch)
	}
}

// TestOwnerForPathPrefersTheMostSpecificWatchedPath: the watch set holds a
// plans directory AND each plan subdirectory, so a job file is always inside
// several entries. The enclosing plan directory is the answer; a shorter
// prefix must never win just because map iteration reached it first.
func TestOwnerForPathPrefersTheMostSpecificWatchedPath(t *testing.T) {
	plansDir := filepath.FromSlash("/nb/workspaces/grovetools/plans")
	outer := &workspace.WorkspaceNode{Name: "outer", Path: "/eco"}
	inner := &workspace.WorkspaceNode{Name: "inner", Path: "/eco/inner"}

	h := NewFlowHandler(nil, nil, 1)
	h.watchedPaths = map[string]*workspace.WorkspaceNode{
		plansDir:                                  outer,
		filepath.Join(plansDir, "perf-audit"):     inner,
		filepath.FromSlash("/nb/unrelated/plans"): outer,
	}

	got := h.ownerForPath(filepath.Join(plansDir, "perf-audit", "60-job.md"))
	if got != inner {
		t.Fatalf("owner = %+v, want the enclosing plan directory's node %+v", got, inner)
	}
	if got := h.ownerForPath(filepath.Join(plansDir, "other-plan", "01-job.md")); got != outer {
		t.Fatalf("owner = %+v, want the plans-directory node %+v", got, outer)
	}
	if got := h.ownerForPath(filepath.FromSlash("/nb/workspaces/grovetools/notes/x.md")); got != nil {
		t.Fatalf("owner = %+v, want nil for an unwatched path", got)
	}
}

func reversedNodes(nodes []*workspace.WorkspaceNode) []*workspace.WorkspaceNode {
	out := make([]*workspace.WorkspaceNode, 0, len(nodes))
	for i := len(nodes) - 1; i >= 0; i-- {
		out = append(out, nodes[i])
	}
	return out
}
