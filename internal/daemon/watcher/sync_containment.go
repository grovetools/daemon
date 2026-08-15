package watcher

// Containment auto-registration (Phase 3, W3.2 — "containment is consent").
//
// The notebook is the only sync knob: once a notebook is shared, a notespace
// INSIDE it syncs without a line of its own in sync.toml, and a notespace
// created inside it later starts syncing on the next reconcile. This file is
// the daemon half of that rule, built entirely on P2 APIs that already exist —
// the stamp (notespace.LoadNotespace), registration (registerRoot → Register),
// and the binding table (UpsertNotespaceBinding). Nothing here invents a wire
// type.
//
// # The two records containment reads
//
//	The notespace's STAMP. A minted `.notespace.toml` is the P2 record that
//	this directory is a notespace with an identity. No stamp, no inheritance:
//	an ordinary directory that happens to sit under notespaces/ is not a
//	notespace, and registration has nothing to send.
//
//	The notebook's SHARE declaration — `[notebooks.<name>.sync] share = true`,
//	written by `grove notebook share` and `grove notebook pull`, parsed by
//	core/pkg/coderoot and projected onto the compiled view as
//	config.Notebook.Shared. That bit is the ONLY consent signal here. It is a
//	recorded decision with a verb behind it, which is what the rule needs:
//	auto-registration begins because an operator said so, at a named notebook,
//	in a file, and stops when they say otherwise.
//
// # Why there is no daemon-wide switch any more
//
// This mechanism used to be gated behind a `ContainmentAutoRegister` field
// that nothing set, because its recorded input did not parse yet and the
// stand-in — "a notebook is shared when some explicit subscription already
// resolves into it" — would have turned any subscribed notespace into consent
// for every sibling beside it. Now that `share = true` parses, the stand-in is
// gone and so is the switch: consent is per notebook, recorded, and read from
// the config the daemon already reloads. A machine that has shared nothing
// inherits nothing, which is the same behavior the dark flag produced.
//
// # The union with explicit subscriptions
//
// Explicit `[[workspaces]]` entries are untouched and still win. A notespace
// named by sync.toml keeps exactly the terms recorded for it
// (effectiveSubscription consults the literal lookup first), and a notespace
// covered only by containment inherits its notebook's terms. Nothing is
// removed from sync.toml by this rule and nothing needs to be added to it: the
// two mechanisms are a union for as long as recorded subscriptions exist.

import (
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/grovetools/core/config"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/workspace"
)

// effectiveSubscription is the subscription that governs a notespace: the
// explicit sync.toml entry naming it, or — when the containing notebook is
// recorded as shared — the one it inherits from that notebook.
// Every caller that used to call subscription(name) directly for ROUTING goes
// through this; subscription(name) itself stays the literal config lookup.
func (r *syncRouting) effectiveSubscription(displayName, root string) *config.SyncWorkspace {
	if sub := r.h.subscription(displayName); sub != nil {
		return sub
	}
	return r.containmentSubscription(root)
}

// containmentSubscription derives the inherited subscription for a stamped
// notespace inside a shared notebook, or nil when containment does not apply.
func (r *syncRouting) containmentSubscription(root string) *config.SyncWorkspace {
	if root == "" {
		return nil
	}
	notebookRoot, ok := containingNotebookRoot(root)
	if !ok {
		return nil
	}
	// A notespace directory that is not there has no stamp to read, and on a
	// discovery pass that is the answer for nearly every workspace.
	if exists, listed := r.notespaceExists(notebookRoot, filepath.Base(root)); listed && !exists {
		return nil
	}
	// Consent is recorded by the mint, not inferred from the directory. The
	// stamp is read here rather than taken from the routing snapshot's index on
	// purpose: the index refuses to build at all when ANY stamp under a recorded
	// notebook is malformed, and one bad stamp must not withdraw containment
	// from every other notespace beside it.
	if stamp, err := notespacepkg.LoadNotespace(root); err != nil || stamp == nil {
		return nil
	}
	template := r.notebookShareTemplate(notebookRoot)
	if template == nil {
		return nil
	}
	inherited := *template
	inherited.Name = filepath.Base(root)
	inherited.Excludes = slices.Clone(template.Excludes)
	return &inherited
}

// discoveredNotespaceRoot resolves a discovered node's notespace root for
// containment purposes only, swallowing the error: the caller does its own
// resolution (and its own routing-error reporting) a few lines later, and a
// node whose root cannot be resolved simply inherits nothing. The
// anySharedNotebook guard keeps it free on a machine that shares nothing.
func (r *syncRouting) discoveredNotespaceRoot(node *workspace.WorkspaceNode) string {
	if !r.anySharedNotebook() {
		return ""
	}
	root, err := r.nodeNotespaceRoot(node)
	if err != nil {
		return ""
	}
	return root
}

// anySharedNotebook reports whether this machine records ANY shared notebook.
// It is the cheap pre-check in front of the per-node work: containment cannot
// apply anywhere until at least one notebook says `share = true`.
func (r *syncRouting) anySharedNotebook() bool {
	cfg := r.cfg
	if cfg == nil || cfg.Notebooks == nil {
		return false
	}
	for _, definition := range cfg.Notebooks.Definitions {
		if definition != nil && definition.Shared {
			return true
		}
	}
	return false
}

// containedNotespace is one notespace that syncs by containment alone.
type containedNotespace struct {
	sub  config.SyncWorkspace
	root string
}

// containedNotespaces enumerates stamped notespaces inside shared notebooks
// that no explicit subscription and no code-notespace discovery covers — the
// bare notes notespace with no repo behind it, which discovery cannot see, and
// the notespace an operator created inside a shared notebook a minute ago.
// Without this the containment rule would hold only for notespaces that happen
// to have a checkout under a scan root.
func (r *syncRouting) containedNotespaces(covered map[string]bool) []containedNotespace {
	cfg := r.cfg
	if cfg == nil || cfg.Notebooks == nil {
		return nil
	}
	var found []containedNotespace
	for _, notebook := range slices.Sorted(maps.Keys(cfg.Notebooks.Definitions)) {
		definition := cfg.Notebooks.Definitions[notebook]
		if definition == nil || !definition.Shared {
			continue
		}
		notebookRoot := notebookRootDir(definition)
		if notebookRoot == "" {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(notebookRoot, workspace.NotespaceDirectory))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || covered[entry.Name()] {
				continue
			}
			root := filepath.Join(notebookRoot, workspace.NotespaceDirectory, entry.Name())
			sub := r.containmentSubscription(root)
			if sub == nil {
				continue
			}
			found = append(found, containedNotespace{sub: *sub, root: root})
		}
	}
	return found
}

// containingNotebookRoot returns the notebook root holding a notespace root,
// i.e. the "<notebook>" in "<notebook>/notespaces/<name>". Anything else is
// not a notebook-contained notespace and inherits nothing.
func containingNotebookRoot(notespaceRoot string) (string, bool) {
	parent := filepath.Dir(notespaceRoot)
	if filepath.Base(parent) != workspace.NotespaceDirectory {
		return "", false
	}
	return filepath.Dir(parent), true
}

// containmentTerms is what an inherited subscription syncs on when the shared
// notebook has no explicit subscription to copy terms from — the ordinary case
// now that sharing a notebook is the whole act.
//
// Bidirectional on purpose. `share = true` is written by exactly two verbs and
// they are the two directions: `grove notebook share` on the machine that
// publishes, `grove notebook pull` on the machine that receives, into the same
// single recorded bit. Pull's own help states the consequence — "so the
// notebook is in scope for sync in both directions" — and there is no second
// knob for a machine to say it only wants one of them. A push-only default
// would make `notebook pull` record a fact that does nothing, which is the bug
// this file exists to close, one layer further along.
//
// The role is `peer` for the same reason: peer IS "another machine of the same
// operator, mirroring the same notebook", and it is the role whose guards
// permit pulling. Satellites keep their explicit push-only entries, which
// still win.
func containmentTerms() config.SyncWorkspace {
	return config.SyncWorkspace{
		Role: config.SyncRolePeer,
		Mode: config.SyncModeFull,
		Pull: true,
	}
}

// notebookShareTemplate answers "is this notebook shared, and on what terms".
//
// The consent half is the recorded one and only the recorded one: the compiled
// definition whose root IS this directory has to say `share = true`. A
// notebook this machine does not record, or records without sharing, inherits
// nothing no matter what else resolves into it.
//
// The terms half prefers what the operator already wrote down. An inherited
// subscription must sync the same way its notebook does (mode, pull, excludes,
// size cap), because containment is a statement about the NOTEBOOK — so an
// explicit subscription already resolving into this notebook root is the
// template, chosen deterministically by name, and containmentTerms is the
// answer when there is none.
//
// A registry-role subscription is never a template. The registry notespace is
// single-writer by construction and its role carries guarantees that have
// nothing to do with the notebook holding it; inheriting `role = "registry"`
// onto a sibling would hand an ordinary notespace the own-note guard and the
// registry's pull posture. Since `grove join` writes exactly one registry entry
// and nothing else, skipping it is what makes the default terms the usual
// answer rather than a rare one.
func (r *syncRouting) notebookShareTemplate(notebookRoot string) *config.SyncWorkspace {
	if !r.notebookRecordedShared(notebookRoot) {
		return nil
	}
	subs := r.h.subscriptionsSnapshot()
	slices.SortFunc(subs, func(a, b config.SyncWorkspace) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})
	for i := range subs {
		sub := subs[i]
		if sub.Mode == config.SyncModeSearchOnly || sub.Role == config.SyncRoleRegistry {
			continue
		}
		_, subNotebookRoot, err := r.recordedNotebookRoot(sub.Name)
		if err != nil {
			continue
		}
		if samePhysicalPath(subNotebookRoot, notebookRoot) {
			return &sub
		}
	}
	terms := containmentTerms()
	return &terms
}

// notebookRecordedShared reports whether the compiled view records the
// notebook AT THIS ROOT as shared. The lookup is by root rather than by name
// because that is what the caller has, and roots are compared physically for
// the same reason every other comparison in this file is: a recorded spelling
// and a resolved path are not the same string.
func (r *syncRouting) notebookRecordedShared(notebookRoot string) bool {
	cfg := r.cfg
	if cfg == nil || cfg.Notebooks == nil {
		return false
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Notebooks.Definitions)) {
		definition := cfg.Notebooks.Definitions[name]
		if definition == nil || !definition.Shared {
			continue
		}
		if samePhysicalPath(notebookRootDir(definition), notebookRoot) {
			return true
		}
	}
	return false
}
