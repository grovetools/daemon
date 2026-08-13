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
// The ROOT check does not canonicalize symlinks: a notebook root reached
// through a symlink is a legitimate, common layout (the macOS /var ->
// /private/var aliasing the rest of the daemon already tolerates). os.Stat
// follows the link, so the check answers "is there a directory at the recorded
// route", which is exactly the question.
//
// Server-supplied CHILD paths are a different question and get the opposite
// answer — see requireUnderRoot. "Is this path inside the tree" cannot be
// settled lexically once a symlink inside the tree can point out of it, so
// both sides are canonicalized there before they are compared. The two rules
// coexist because the root is a RECORDED route this machine chose, while a
// child path is an input from the other end of the wire.

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
//
// The check is lexical AND physical. Lexical alone is not enough: filepath.Rel
// cleans "notes/link/../../../etc/x" to a contained-looking relative path
// without ever asking what `link` points at, so a symlink INSIDE the tree is a
// legitimate route out of it and the write follows the link. The header's
// argument for not canonicalizing applies to the ROOT — a notebook reached
// through a symlink is a real layout — and does not transfer to server-supplied
// CHILD paths. Both sides are canonicalized before comparison, so the macOS
// /var -> /private/var aliasing the rest of the daemon tolerates still passes.
func requireUnderRoot(root, dst string) error {
	if err := lexicallyUnderRoot(root, dst); err != nil {
		return err
	}
	resolvedRoot, err := resolveExisting(root)
	if err != nil {
		return fmt.Errorf("resolve notespace root %s: %w", root, err)
	}
	resolvedDst, err := resolveExisting(dst)
	if err != nil {
		return fmt.Errorf("resolve %s against notespace root %s: %w", dst, root, err)
	}
	return lexicallyUnderRoot(resolvedRoot, resolvedDst)
}

// lexicallyUnderRoot is the pure path-arithmetic half of the containment check.
func lexicallyUnderRoot(root, dst string) error {
	rel, err := filepath.Rel(root, dst)
	if err != nil {
		return fmt.Errorf("resolve %s against notespace root %s: %w", dst, root, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes notespace root %s: %s", root, dst)
	}
	return nil
}

// resolveExisting canonicalizes the deepest existing ancestor of path and
// re-joins the components that do not exist yet.
//
// filepath.EvalSymlinks fails outright on a path whose tail is missing, which
// is the ordinary case for an incoming write: plans/2026/note.md arrives before
// plans/2026 does. Resolving the part that DOES exist is what matters for
// containment — a symlink can only be traversed where it exists — and the
// missing tail cannot introduce one.
func resolveExisting(path string) (string, error) {
	current := filepath.Clean(path)
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Walked to the filesystem root without finding anything that
			// exists: there is no symlink to resolve, so the lexical answer
			// already is the physical one.
			return filepath.Clean(path), nil
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
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

// deleteFileUnderRoot is deleteFile with the same two guards.
//
// Deletes need containment MORE than writes, not less: a write at least has to
// name a document the server can produce content for, while a delete needs no
// precondition at all — no DB row for a prefix delete, no hash, no prior state.
// One event from a compromised, confused, or simply buggy server would
// otherwise remove anything the daemon can reach.
func deleteFileUnderRoot(root, dst string) error {
	if err := RequireNotespaceRoot(root); err != nil {
		return err
	}
	if err := requireUnderRoot(root, dst); err != nil {
		return err
	}
	return deleteFile(dst)
}

// deleteDirUnderRoot is deleteDir (a recursive RemoveAll) with the same two
// guards. This is the most destructive call in the package.
func deleteDirUnderRoot(root, dst string) error {
	if err := RequireNotespaceRoot(root); err != nil {
		return err
	}
	if err := requireUnderRoot(root, dst); err != nil {
		return err
	}
	// Refusing the root itself is not containment, it is the same rule
	// RequireNotespaceRoot exists for: sync never creates a notespace root, so
	// it must not remove one either. A prefix delete addressing "" or "." would
	// otherwise take the whole tree.
	if same, err := samePath(root, dst); err != nil {
		return err
	} else if same {
		return fmt.Errorf("refusing to delete the notespace root itself: %s", root)
	}
	return deleteDir(dst)
}

// samePath reports whether two paths name the same directory after symlink
// resolution.
func samePath(a, b string) (bool, error) {
	resolvedA, err := resolveExisting(a)
	if err != nil {
		return false, err
	}
	resolvedB, err := resolveExisting(b)
	if err != nil {
		return false, err
	}
	return resolvedA == resolvedB, nil
}
