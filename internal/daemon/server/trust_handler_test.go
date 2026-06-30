package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/worktreeregistry"
	"github.com/grovetools/core/util/pathutil"
)

// readClaudeProjects reads $HOME/.claude.json and returns its projects map.
// Returns nil when the file does not exist.
func readClaudeProjects(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read ~/.claude.json: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal ~/.claude.json: %v", err)
	}
	projects, _ := root["projects"].(map[string]any)
	return projects
}

// isTrusted reports whether projects[key].hasTrustDialogAccepted == true.
func isTrusted(projects map[string]any, key string) bool {
	entry, ok := projects[key].(map[string]any)
	if !ok {
		return false
	}
	v, _ := entry["hasTrustDialogAccepted"].(bool)
	return v
}

// seedTrustTestEnv isolates StateDir (registry) and HOME (~/.claude.json) under
// temp dirs so the handler never touches the developer's real registry or
// trust file. It returns the worktree container path and the canonical trust
// keys the handler is expected to write.
func seedTrustTestEnv(t *testing.T, repos []string) (home, worktreePath string, wantKeys []string) {
	t.Helper()
	t.Setenv("GROVE_HOME", t.TempDir()) // isolates paths.StateDir() → registry dir
	home = t.TempDir()
	t.Setenv("HOME", home) // isolates os.UserHomeDir() → ~/.claude.json

	worktreePath = filepath.Join(t.TempDir(), "my-worktree")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	for _, r := range repos {
		if err := os.MkdirAll(filepath.Join(worktreePath, r), 0o755); err != nil {
			t.Fatalf("mkdir repo %s: %v", r, err)
		}
	}

	// The handler trusts the container + <container>/<repo>, canonicalized.
	want := func(p string) string {
		c, err := pathutil.CanonicalPath(p)
		if err != nil {
			t.Fatalf("canonicalize %s: %v", p, err)
		}
		return c
	}
	wantKeys = append(wantKeys, want(worktreePath))
	for _, r := range repos {
		wantKeys = append(wantKeys, want(filepath.Join(worktreePath, r)))
	}

	if err := worktreeregistry.Save(&worktreeregistry.Entry{
		AbsPath: worktreePath,
		Repos:   repos,
	}); err != nil {
		t.Fatalf("save registry entry: %v", err)
	}
	return home, worktreePath, wantKeys
}

func postSeedTrust(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/trust/seed", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	New(false).handleSeedTrust(rec, req)
	return rec
}

// TestHandleSeedTrustDerivesFromRegistry proves the handler trusts exactly the
// container + member-repo paths recorded in the registry, and that any paths a
// caller smuggles into the request body are IGNORED (the self-trust guardrail).
func TestHandleSeedTrustDerivesFromRegistry(t *testing.T) {
	repos := []string{"core", "daemon"}
	home, worktreePath, wantKeys := seedTrustTestEnv(t, repos)

	// The attacker tries to self-grant trust to an arbitrary path via an extra
	// field. The handler decodes only worktree_ref, so this must never land.
	attackerPath, _ := pathutil.CanonicalPath(filepath.Join(t.TempDir(), "evil"))
	rec := postSeedTrust(t, map[string]any{
		"worktree_ref": worktreePath,
		"paths":        []string{attackerPath},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	projects := readClaudeProjects(t, home)
	if projects == nil {
		t.Fatal("~/.claude.json was not created")
	}
	for _, key := range wantKeys {
		if !isTrusted(projects, key) {
			t.Errorf("expected %q to be trusted, projects=%v", key, projects)
		}
	}
	if _, present := projects[attackerPath]; present {
		t.Errorf("attacker-supplied path %q was trusted — guardrail breached", attackerPath)
	}
}

// TestHandleSeedTrustHonorsGate proves the GROVE_PRESEED_CLAUDE_TRUST=0 gate is
// enforced daemon-side: SeedTrust no-ops, so no trust file is written.
func TestHandleSeedTrustHonorsGate(t *testing.T) {
	t.Setenv("GROVE_PRESEED_CLAUDE_TRUST", "0")
	home, worktreePath, _ := seedTrustTestEnv(t, []string{"core"})

	rec := postSeedTrust(t, map[string]any{"worktree_ref": worktreePath})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if projects := readClaudeProjects(t, home); projects != nil {
		t.Errorf("gate off but ~/.claude.json projects were written: %v", projects)
	}
}

// TestHandleSeedTrustUnknownRef proves an unresolvable ref returns 404 rather
// than seeding anything.
func TestHandleSeedTrustUnknownRef(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)

	rec := postSeedTrust(t, map[string]any{"worktree_ref": filepath.Join(t.TempDir(), "nope")})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if projects := readClaudeProjects(t, home); projects != nil {
		t.Errorf("unknown ref but trust written: %v", projects)
	}
}

// TestHandleSeedTrustMethodAndBody covers the request-validation edges.
func TestHandleSeedTrustMethodAndBody(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Wrong method.
	getReq := httptest.NewRequest(http.MethodGet, "/api/trust/seed", nil)
	getRec := httptest.NewRecorder()
	New(false).handleSeedTrust(getRec, getReq)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", getRec.Code)
	}

	// Empty ref.
	rec := postSeedTrust(t, map[string]any{"worktree_ref": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty ref status = %d, want 400", rec.Code)
	}
}
