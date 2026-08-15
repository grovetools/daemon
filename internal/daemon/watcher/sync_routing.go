package watcher

// The routing snapshot.
//
// Every rung of notespace routing — the compiled grove binding, the stamped
// identity, the recorded default — is a pure function of two inputs: the
// recorded config the daemon holds, and the stamps on disk. Nothing in a
// watch-set pass changes either. But the rungs used to be evaluated per
// QUESTION rather than per pass, and the stamped rung's answer is not cheap:
// config.LoadMachineConfig parses machine.toml, and workspace.ResolveNotespaceName
// walks every recorded notebook's notespaces/ directory.
//
// ComputeWatchPaths asks that question once per discovered workspace. On the
// 2026-08-15 profile — 694 workspaces, a refresh every few seconds — that put
// SyncHandler.ComputeWatchPaths at 37% of a groved sustaining 250% CPU, with
// another 36% of the process in GC feeding on the garbage it produced.
//
// syncRouting is the fix: one snapshot per pass, holding the config pointer the
// whole pass routes against and the stamped rung PRECOMPUTED for every display
// name that resolves — so the per-workspace question costs a map lookup and no
// syscalls at all. core memoizes the underlying index (see
// core/pkg/workspace/notespace_index_cache.go), so building a snapshot costs one
// stat per notebook and per stamp rather than a directory walk and a parse of
// every stamp.
//
// The rung ordering, and every condition under which a rung declines to answer,
// are unchanged: this file moved them behind a snapshot, it did not renegotiate
// them.
//
// Pinning the config for the duration of a pass is the second, smaller win: a
// pass that read h.configSnapshot() per question could straddle a hot reload and
// route two workspaces of the same pass against two different configs.

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/pathutil"
)

// syncRouting is one pass's answer to "where does each notespace live".
type syncRouting struct {
	h   *SyncHandler
	cfg *config.Config
	// stamped is the stamped-identity rung: display name -> the recorded
	// notebook holding it. A name that is absent is one that rung declines,
	// which is what sends the caller on to the recorded default.
	stamped map[string]stampedRoute

	// entriesMu guards entries. A snapshot belongs to one pass and one
	// goroutine today; the lock keeps that from being a requirement.
	entriesMu sync.Mutex
	// entries is notebook root -> the names present under its notespaces/
	// directory, filled on demand. nil for a directory that could not be
	// listed, which sends the caller back to the filesystem.
	entries map[string]map[string]bool
}

// stampedRoute is one stamped-identity answer: the recorded notebook's name and
// its root, the same pair recordedNotebookRoot returns.
type stampedRoute struct {
	notebook string
	root     string
}

// newRouting takes a routing snapshot. Callers that resolve more than one name
// — every watch-set and pipeline pass does — take ONE and reuse it.
func (h *SyncHandler) newRouting() *syncRouting {
	r := &syncRouting{h: h, cfg: h.configSnapshot()}
	r.stamped = r.buildStampedRoutes()
	return r
}

// buildStampedRoutes precomputes the stamped-identity rung for every display
// name it can answer, using core's recorded-primary resolver (stamp id +
// machine.toml [primaries]) rather than any name-to-directory guess — the same
// chain nb, grove.nvim and skills already route through.
//
// A name is present only when the chain answers EXACTLY: it is a recorded
// primary, unambiguous across roots, its resolved root is
// <recorded notebook root>/notespaces/<name>, and notebooks.toml records that
// notebook root. Every other case is absent, which leaves the decision to the
// caller's remaining rungs instead of inventing a root. So are all of them when
// there is no readable machine.toml or the index cannot be built: this table is
// only ever a rung that answers or declines.
func (r *syncRouting) buildStampedRoutes() map[string]stampedRoute {
	if r.cfg == nil || r.cfg.Notebooks == nil || len(r.cfg.Notebooks.Definitions) == 0 {
		return nil
	}
	machineCfg, err := config.LoadMachineConfig()
	if err != nil || machineCfg == nil {
		return nil
	}
	resolutions, err := workspace.NotespaceNameRoutes(r.cfg, machineCfg)
	if err != nil || len(resolutions) == 0 {
		return nil
	}

	notebooks := r.recordedNotebooks()
	routes := make(map[string]stampedRoute, len(resolutions))
	for name, resolution := range resolutions {
		// recordedNotebookRoot's contract is a NOTEBOOK root that nodeNotespaceRoot
		// re-joins with "notespaces/<name>", so only a resolution that round-trips
		// through that join can be reported here.
		if resolution.Root == "" || filepath.Base(resolution.Root) != name {
			continue
		}
		notespacesDir := filepath.Dir(resolution.Root)
		if filepath.Base(notespacesDir) != workspace.NotespaceDirectory {
			continue
		}
		notebookRoot := filepath.Dir(notespacesDir)
		if notebook, ok := matchRecordedNotebook(notebooks, notebookRoot); ok {
			routes[name] = stampedRoute{notebook: notebook, root: notebookRoot}
		}
	}
	return routes
}

// notespaceExists reports whether <notebookRoot>/notespaces/<name> is there at
// all, from one listing per notebook per pass instead of one probe per caller.
// ok is false when the directory could not be listed, which means the caller
// must go to the filesystem itself rather than treat an unreadable notebook as
// an empty one.
//
// This is a NEGATIVE fast path on purpose. Containment asks "is there a stamp
// here" for every discovered workspace, and on a machine with hundreds of
// workspaces and one shared notebook almost every answer is "that directory
// does not exist" — an open(2) that fails, per workspace, per refresh. A name
// this listing does not hold cannot have a stamp under it; a name it does hold
// still gets its stamp read by the caller.
func (r *syncRouting) notespaceExists(notebookRoot, name string) (bool, bool) {
	r.entriesMu.Lock()
	defer r.entriesMu.Unlock()
	names, listed := r.entries[notebookRoot]
	if !listed {
		if r.entries == nil {
			r.entries = make(map[string]map[string]bool)
		}
		entries, err := os.ReadDir(filepath.Join(notebookRoot, workspace.NotespaceDirectory))
		if err == nil {
			names = make(map[string]bool, len(entries))
			for _, entry := range entries {
				// Every entry, not only directories: a symlinked notespace is
				// one the stamp read below still resolves.
				names[entry.Name()] = true
			}
		}
		r.entries[notebookRoot] = names
	}
	if names == nil {
		return false, false
	}
	return names[name], true
}

// recordedNotebook is one notebook definition's recorded root, in both of the
// spellings samePhysicalPath compares against, canonicalized once per pass
// instead of once per question.
type recordedNotebook struct {
	name      string
	clean     string
	canonical string
}

// recordedNotebooks lists the notebook definitions in the order the stamped
// rung has always consulted them: by notebook name, so a root recorded twice
// resolves to the same notebook every time.
func (r *syncRouting) recordedNotebooks() []recordedNotebook {
	notebooks := make([]recordedNotebook, 0, len(r.cfg.Notebooks.Definitions))
	for _, name := range slices.Sorted(maps.Keys(r.cfg.Notebooks.Definitions)) {
		root := notebookRootDir(r.cfg.Notebooks.Definitions[name])
		if root == "" {
			continue
		}
		entry := recordedNotebook{name: name, clean: filepath.Clean(root)}
		if canonical, err := pathutil.CanonicalPath(root); err == nil {
			entry.canonical = canonical
		}
		notebooks = append(notebooks, entry)
	}
	return notebooks
}

// matchRecordedNotebook is samePhysicalPath against a prepared definition list:
// lexical comparison first, so an answer does not depend on the filesystem, and
// symlink resolution only as a fallback — and only once for the queried root
// rather than once per definition.
func matchRecordedNotebook(notebooks []recordedNotebook, notebookRoot string) (string, bool) {
	if notebookRoot == "" {
		return "", false
	}
	clean := filepath.Clean(notebookRoot)
	canonical, canonicalErr := "", error(nil)
	canonicalDone := false
	for _, notebook := range notebooks {
		if notebook.clean == clean {
			return notebook.name, true
		}
		if notebook.canonical == "" {
			continue
		}
		if !canonicalDone {
			canonical, canonicalErr = pathutil.CanonicalPath(notebookRoot)
			canonicalDone = true
		}
		if canonicalErr == nil && canonical == notebook.canonical {
			return notebook.name, true
		}
	}
	return "", false
}

// recordedNotebookRoot returns the authoritative name+root pair. An exact
// compiled code-root binding is literal rung 0; a stamped notespace whose id is
// the recorded primary for its subject is rung 1; the recorded default is the
// only fallback. The compiled NotebookRoot is returned directly, never
// re-resolved through Definitions by notebook name.
func (r *syncRouting) recordedNotebookRoot(name string) (string, string, error) {
	cfg := r.cfg
	if cfg == nil || name == "" {
		return "", "", fmt.Errorf("notespace %q has no recorded code-root/notebook binding", name)
	}
	if grove, ok := cfg.Groves[name]; ok {
		if grove.Notebook != "" || grove.NotebookRoot != "" {
			if grove.Notebook == "" || grove.NotebookRoot == "" {
				return "", "", fmt.Errorf("notespace %q has an incomplete recorded code-root/notebook binding", name)
			}
			return grove.Notebook, grove.NotebookRoot, nil
		}
	}
	// Identity rung, ahead of the default: a notes-plane subscription with no
	// compiled code-root binding is still locatable BY IDENTITY — its stamp id
	// has to be the recorded primary for its subject in machine.toml. Dropping
	// straight to notebooks.rules.default here is what sent a notespace bound
	// to a non-default notebook to <default>/notespaces/<name>, the wrong-root
	// class P2 exists to eliminate. Nothing is inferred: an unstamped tree (a
	// pull replica that does not exist yet) or a name that does not identify
	// exactly one recorded primary is absent from the stamped table and falls
	// through to the default rung below, byte for byte as before.
	if route, ok := r.stamped[name]; ok {
		return route.notebook, route.root, nil
	}
	if cfg.Notebooks == nil || cfg.Notebooks.Rules == nil || cfg.Notebooks.Rules.Default == "" {
		return "", "", fmt.Errorf("notespace %q has no recorded code-root/notebook binding or default notebook", name)
	}
	notebook := cfg.Notebooks.Rules.Default
	definition := cfg.Notebooks.Definitions[notebook]
	if definition == nil || definition.RootDir == "" {
		return "", "", fmt.Errorf("notespace %q routes to default notebook %q without a recorded root", name, notebook)
	}
	return notebook, notebookRootDir(definition), nil
}

func (r *syncRouting) syntheticNodeFor(name string) (*workspace.WorkspaceNode, error) {
	node := &workspace.WorkspaceNode{Name: name}
	notebook, _, err := r.recordedNotebookRoot(name)
	if err != nil {
		return node, err
	}
	node.NotebookName = notebook
	return node, nil
}

// nodeNotespaceRoot consumes a compiled root literally for discovered nodes as
// well as synthetic subscriptions. It has no existence requirement: pull must
// be able to materialize a replica into a tree that does not exist yet.
func (r *syncRouting) nodeNotespaceRoot(node *workspace.WorkspaceNode) (string, error) {
	if node == nil {
		return "", fmt.Errorf("cannot route a nil notespace node")
	}
	_, root, err := r.recordedNotebookRoot(node.Name)
	if err != nil && node.Path != "" {
		binding := config.ResolveNotebook(config.NotebookQuery{
			Path:       node.Path,
			OwnerPaths: []string{node.ParentProjectPath, node.ParentEcosystemPath, node.RootEcosystemPath},
		}, r.cfg)
		if binding.Notebook != "" && binding.NotebookRoot != "" {
			root, err = binding.NotebookRoot, nil
		}
	}
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("notespace %q has non-absolute recorded notebook root %q", node.Name, root)
	}
	return filepath.Join(root, "notespaces", node.Name), nil
}

// The one-off entry points. Callers with a single name to route — the registry
// note, an adoption, a test — take a snapshot of their own; the cost is one
// machine.toml parse and the memoized index's stat sweep.

func (h *SyncHandler) syntheticNodeFor(name string) (*workspace.WorkspaceNode, error) {
	return h.newRouting().syntheticNodeFor(name)
}

func (h *SyncHandler) nodeNotespaceRoot(node *workspace.WorkspaceNode) (string, error) {
	return h.newRouting().nodeNotespaceRoot(node)
}

func (h *SyncHandler) effectiveSubscription(displayName, root string) *config.SyncWorkspace {
	return h.newRouting().effectiveSubscription(displayName, root)
}

func (h *SyncHandler) containmentSubscription(root string) *config.SyncWorkspace {
	return h.newRouting().containmentSubscription(root)
}

func (h *SyncHandler) anySharedNotebook() bool {
	return h.newRouting().anySharedNotebook()
}

func (h *SyncHandler) containedNotespaces(covered map[string]bool) []containedNotespace {
	return h.newRouting().containedNotespaces(covered)
}
