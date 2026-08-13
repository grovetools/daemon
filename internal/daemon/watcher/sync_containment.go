package watcher

// Containment auto-registration (Phase 3, W3.2 — "containment is consent").
//
// The notebook is meant to become the only sync knob: once a notebook is
// shared, a notespace CREATED INSIDE it is auto-registered by the daemon
// rather than needing its own line in sync.toml. This file is the daemon half
// of that rule, built entirely on P2 APIs that already exist — the stamp
// (notespace.LoadNotespace), registration (registerRoot → Register), and the
// binding table (UpsertNotespaceBinding). Nothing here invents a wire type.
//
// # The two records containment reads
//
//	The notespace's STAMP. A minted `.notespace.toml` is the P2 record that
//	this directory is a notespace with an identity. No stamp, no inheritance:
//	an ordinary directory that happens to sit under notespaces/ is not a
//	notespace, and registration has nothing to send.
//
//	The notebook's SHARE declaration. This is the half that does not exist
//	yet. W3.2 records it as `[notebooks.<name>.sync] share = true`, and
//	core/pkg/coderoot reserves exactly that table today (`Sync
//	map[string]interface{}`, every key inside it rejected loudly) — the typed
//	parse lands with the core/sync half of Phase 3, not here. Until it does,
//	notebookShareTemplate answers from what this daemon can observe without
//	guessing: a notebook is shared when an EXPLICIT sync.toml subscription
//	already resolves to a notespace under that same notebook root.
//
// # Why it is dark by default
//
// ContainmentAutoRegister gates the whole mechanism and nothing in the daemon
// sets it. On a P2 machine, flipping it on would silently begin syncing every
// stamped notespace in any notebook that has one subscribed notespace — a real
// behavior change, in untested territory, driven by a config knob that does not
// exist yet. So the mechanism ships complete and tested, and the switch waits
// for its recorded input: when `share = true` parses in core,
// notebookShareTemplate reads it and grove's `notebook share` verb becomes the thing that turns this
// on, per notebook, on purpose.

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
// explicit sync.toml entry naming it, or — when containment is enabled and the
// containing notebook is shared — the one it inherits from that notebook.
// Every caller that used to call subscription(name) directly for ROUTING goes
// through this; subscription(name) itself stays the literal config lookup.
func (h *SyncHandler) effectiveSubscription(displayName, root string) *config.SyncWorkspace {
	if sub := h.subscription(displayName); sub != nil {
		return sub
	}
	return h.containmentSubscription(root)
}

// containmentSubscription derives the inherited subscription for a stamped
// notespace inside a shared notebook, or nil when containment does not apply.
func (h *SyncHandler) containmentSubscription(root string) *config.SyncWorkspace {
	if !h.ContainmentAutoRegister || root == "" {
		return nil
	}
	notebookRoot, ok := containingNotebookRoot(root)
	if !ok {
		return nil
	}
	// Consent is recorded by the mint, not inferred from the directory.
	if stamp, err := notespacepkg.LoadNotespace(root); err != nil || stamp == nil {
		return nil
	}
	template := h.notebookShareTemplate(notebookRoot)
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
// node whose root cannot be resolved simply inherits nothing. It costs nothing
// while containment is dark.
func (h *SyncHandler) discoveredNotespaceRoot(node *workspace.WorkspaceNode) string {
	if !h.ContainmentAutoRegister {
		return ""
	}
	root, err := h.nodeNotespaceRoot(node)
	if err != nil {
		return ""
	}
	return root
}

// containedNotespace is one notespace that syncs by containment alone.
type containedNotespace struct {
	sub  config.SyncWorkspace
	root string
}

// containedNotespaces enumerates stamped notespaces inside shared notebooks
// that no explicit subscription and no code-notespace discovery covers — the
// bare notes notespace with no repo behind it, which discovery cannot see.
// Without this the containment rule would hold only for notespaces that happen
// to have a checkout under a scan root.
func (h *SyncHandler) containedNotespaces(covered map[string]bool) []containedNotespace {
	if !h.ContainmentAutoRegister || h.cfg == nil || h.cfg.Notebooks == nil {
		return nil
	}
	var found []containedNotespace
	for _, notebook := range slices.Sorted(maps.Keys(h.cfg.Notebooks.Definitions)) {
		notebookRoot := notebookRootDir(h.cfg.Notebooks.Definitions[notebook])
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
			sub := h.containmentSubscription(root)
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

// notebookShareTemplate answers "is this notebook shared, and on what terms".
//
// The terms matter: an inherited subscription must sync the same way its
// notebook does (mode, pull, excludes, size cap), because containment is a
// statement about the NOTEBOOK. The template is the explicit subscription
// already resolving into this notebook root, chosen deterministically by name.
//
// A registry-role subscription is never a template. The registry notespace is
// single-writer by construction and its role carries guarantees that have
// nothing to do with the notebook holding it; inheriting `role = "registry"`
// onto a sibling would hand an ordinary notespace the own-note guard and the
// registry's pull posture. A notebook whose only subscription is the registry
// is therefore not shared for containment purposes.
//
// This is the function that reads `[notebooks.<name>.sync] share = true` once
// core parses it — the observable-state answer below is the stand-in, not the
// intended long-term source of truth.
func (h *SyncHandler) notebookShareTemplate(notebookRoot string) *config.SyncWorkspace {
	subs := h.subscriptionsSnapshot()
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
		_, subNotebookRoot, err := h.recordedNotebookRoot(sub.Name)
		if err != nil {
			continue
		}
		if samePhysicalPath(subNotebookRoot, notebookRoot) {
			return &sub
		}
	}
	return nil
}
