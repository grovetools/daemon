package cmd

import "fmt"

// resolveExplicitScope guards `groved start --scope <path>` against silent
// global fallback. workspace.ResolveScope returns "" for a path that is neither
// a known workspace nor inside a git repository — a deliberate fallback its
// other callers rely on to mean "use the global daemon". For an explicit
// --scope, that fallback is a trap: the operator asked for an isolated scoped
// daemon and would instead get a SECOND GLOBAL daemon sharing the global
// tuimux socket, whose shutdown path can then reap agent PTYs it never owned
// (the 2026-07-28 incident). So an explicit scope that resolves empty is an
// error, not a fallback.
//
// requested is the raw --scope flag value; resolved is what
// workspace.ResolveScope returned for it.
func resolveExplicitScope(requested, resolved string) (string, error) {
	if requested != "" && resolved == "" {
		return "", fmt.Errorf(
			"--scope %q is not a known grove workspace or git repository; "+
				"refusing to fall back to the global daemon scope (a second global daemon can reap agents owned by the real one). "+
				"Use --scope-verbatim to run a daemon on an ad-hoc/sandbox scope path",
			requested)
	}
	return resolved, nil
}
