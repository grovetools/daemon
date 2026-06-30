package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/grovetools/core/pkg/claudetrust"
	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/core/util/pathutil"
)

// handleSeedTrust handles POST /api/trust/seed — the privileged Claude
// folder-trust write delegated by a sandboxed worktree provisioner. The agent
// sandbox cannot write ~/.claude.json (it lives outside the writable boundary),
// so the in-process claudetrust.SeedTrust fails with EPERM and the caller hands
// the job to this daemon, which runs UNSANDBOXED and on grove's side of the
// boundary.
//
// Security guardrail: the request carries ONLY a worktree ref. The handler
// derives the exact set of paths to trust from its own authority — the worktree
// registry — and IGNORES any paths a caller might try to supply. This prevents
// a compromised sandboxed agent from relocating the self-trust attack behind the
// socket (granting itself trust to arbitrary directories). The
// GROVE_PRESEED_CLAUDE_TRUST gate is enforced inside claudetrust.SeedTrust.
//
// Registered with unixOnly: the 0600 unix socket only, never the unauthenticated
// localhost TCP listener.
func (s *Server) handleSeedTrust(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		WorktreeRef string `json:"worktree_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.WorktreeRef = strings.TrimSpace(req.WorktreeRef)
	if req.WorktreeRef == "" {
		http.Error(w, "worktree_ref required", http.StatusBadRequest)
		return
	}

	// Resolve the ref against the registry — the daemon's own authority.
	entry, err := worktreeregistry.FindByRef(req.WorktreeRef)
	if err != nil || entry == nil {
		http.Error(w, fmt.Sprintf("worktree not found for ref %q: %v", req.WorktreeRef, err), http.StatusNotFound)
		return
	}

	// Derive the trust path set SOLELY from the registry entry: the container
	// root plus <container>/<repo> for each member repo. This mirrors the set
	// prepare.go builds in-process, so the trusted keys match the cwd Claude
	// compares against. Canonicalize with the SAME pathutil.CanonicalPath the
	// launcher uses (macOS case + symlinks) or an un-canonicalized key silently
	// misses.
	rawPaths := make([]string, 0, 1+len(entry.Repos))
	rawPaths = append(rawPaths, entry.AbsPath)
	for _, repo := range entry.Repos {
		rawPaths = append(rawPaths, filepath.Join(entry.AbsPath, repo))
	}
	canonicalPaths := make([]string, 0, len(rawPaths))
	for _, p := range rawPaths {
		canonical, cerr := pathutil.CanonicalPath(p)
		if cerr != nil {
			s.ulog.Debug("trust seed: canonicalize failed").
				Field("path", p).Field("error", cerr.Error()).Log(r.Context())
			continue
		}
		canonicalPaths = append(canonicalPaths, canonical)
	}

	if err := claudetrust.SeedTrust(canonicalPaths...); err != nil {
		http.Error(w, fmt.Sprintf("seed trust: %v", err), http.StatusInternalServerError)
		return
	}

	s.ulog.Debug("trust seeded via daemon").
		Field("ref", req.WorktreeRef).
		Field("count", len(canonicalPaths)).
		Log(r.Context())

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{"trusted": len(canonicalPaths)})
}
