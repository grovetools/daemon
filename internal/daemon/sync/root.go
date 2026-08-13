package sync

// Root existence is a PRECONDITION of every incoming apply, never something
// sync creates (Phase 3, W3.2).
//
// The rule it replaces: every write path in this package reached the disk
// through merge.go's writeFile/moveFile, which MkdirAll the destination's
// parent chain. Under a missing notespace root that is a resurrection — the
// daemon silently rebuilds a notebook tree the operator deleted, moved, or
// never materialized, at whatever path config happens to resolve, and then
// fills it from the server. A root that is not there is a refusal with a
// reason, and the recorded route is repaired by the operator (or by
// `grove notebook pull`, which materializes at a RECORDED root on purpose).
//
// The same precondition closes a second, worse hole on the push side:
// anti-entropy's sweepMissingFile enqueues a document_deleted for every
// tracked document whose file is gone. A vanished root (an unmounted volume,
// a deleted directory, a machine that never materialized its replica) makes
// EVERY tracked document look deleted, so one sweep would replicate a
// notespace-wide deletion to the server and from there to every other
// machine. AntiEntropyPass.Run refuses the pass instead.
//
// Nothing here canonicalizes symlinks: a notebook root reached through a
// symlink is a legitimate, common layout (the macOS /var -> /private/var
// aliasing the rest of the daemon already tolerates). os.Stat follows the
// link, so the check answers "is there a directory at the recorded route",
// which is exactly the question.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MissingRootError is the refusal an apply raises instead of materializing a
// notespace root. It names the root and the remediation so a log line, a
// conflict-feed entry, and an API error all say the same thing.
type MissingRootError struct {
	// Root is the resolved path that is not usable.
	Root string
	// Detail says which way it failed (absent, not a directory, unresolved).
	Detail string
}

func (e *MissingRootError) Error() string {
	root := e.Root
	if root == "" {
		root = "(unresolved)"
	}
	return fmt.Sprintf("refusing to sync into notespace root %s: %s; "+
		"sync never creates a notespace root — record it in notebooks.toml and materialize it "+
		"(`grove notebook pull <notebook>`), then the daemon will apply into it", root, e.Detail)
}

// IsMissingRoot reports whether err is (or wraps) a root refusal.
func IsMissingRoot(err error) bool {
	var missing *MissingRootError
	return errors.As(err, &missing)
}

// RequireNotespaceRoot is the precondition every incoming apply and every
// reconciliation pass checks before touching the filesystem. It is one stat;
// callers run it per apply batch and per pass, not per document.
func RequireNotespaceRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return &MissingRootError{Detail: "no local root is recorded for this notespace"}
	}
	if !filepath.IsAbs(root) {
		return &MissingRootError{Root: root, Detail: "the recorded root is not an absolute path"}
	}
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return &MissingRootError{Root: root, Detail: "the recorded root does not exist"}
		}
		return fmt.Errorf("stat notespace root %s: %w", root, err)
	}
	if !info.IsDir() {
		return &MissingRootError{Root: root, Detail: "the recorded root is not a directory"}
	}
	return nil
}

// requireUnderRoot rejects a destination that is not inside the notespace
// root. Wire paths are server-supplied, and filepath.Join happily resolves
// "../.." out of the tree; containment is checked here so no writer has to
// remember to.
func requireUnderRoot(root, dst string) error {
	rel, err := filepath.Rel(root, dst)
	if err != nil {
		return fmt.Errorf("resolve %s against notespace root %s: %w", dst, root, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes notespace root %s: %s", root, dst)
	}
	return nil
}

// writeFileUnderRoot is writeFile with the root precondition and containment
// check applied first. Directories BENEATH an existing root are still created
// on demand (a replica legitimately gains plans/2026/… as documents arrive);
// only the root itself is never conjured.
func writeFileUnderRoot(root, dst string, content []byte, mtime time.Time) error {
	if err := RequireNotespaceRoot(root); err != nil {
		return err
	}
	if err := requireUnderRoot(root, dst); err != nil {
		return err
	}
	return writeFile(dst, content, mtime)
}

// moveFileUnderRoot is moveFile with the same two guards on both ends.
func moveFileUnderRoot(root, src, dst string) error {
	if err := RequireNotespaceRoot(root); err != nil {
		return err
	}
	if err := requireUnderRoot(root, src); err != nil {
		return err
	}
	if err := requireUnderRoot(root, dst); err != nil {
		return err
	}
	return moveFile(src, dst)
}
