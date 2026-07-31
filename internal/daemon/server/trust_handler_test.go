package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/exectrust"
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

// trustWorktreeConfig approves the worktree's grove.toml in the exec-trust
// store, the way `grove config trust --yes` does.
//
// [claude] is a RiskCapability field (core/config/execgate.go), so the
// exec-provenance gate strips the whole block out of a project-layer
// grove.toml the user has not reviewed — including manageTrust. That gate sits
// in config.LoadFrom, which is exactly how handleSeedTrust resolves the
// profile, so an untrusted fixture makes the handler no-op no matter what the
// fixture says. Tests of the seeding path must open the gate first; the closed
// gate has its own test below.
//
// GROVE_HOME (set by the callers) already redirects paths.StateDir(), and the
// store lives there, so this never touches the developer's real trust
// decisions.
func trustWorktreeConfig(t *testing.T, worktreePath string) {
	t.Helper()
	cfg, err := config.LoadFrom(worktreePath)
	if err != nil {
		t.Fatalf("load worktree config: %v", err)
	}
	if cfg.ExecGate == nil || len(cfg.ExecGate.Files) == 0 {
		t.Fatalf("no gated config files found under %s — fixture is not exercising the gate", worktreePath)
	}
	store := exectrust.Load()
	now := time.Now()
	trusted := 0
	for _, f := range cfg.ExecGate.Files {
		// Only the fixture's own file. The developer's ecosystem config can
		// also land in this report when the test runs from a real checkout,
		// and trusting that would be a side effect, not a fixture.
		if filepath.Dir(f.Path) != worktreePath {
			continue
		}
		store.Trust(f.Path, f.Digest, now)
		trusted++
	}
	if trusted == 0 {
		t.Fatalf("worktree grove.toml under %s carried no gated values to trust", worktreePath)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("save exec-trust store: %v", err)
	}
	// LoadFrom memoizes by startDir for 2s (config/config.go loadCache), and the
	// load above just cached the PRE-trust verdict for this very path. Without
	// this the handler would be handed the gated config it is meant to no
	// longer get, and the test would fail for a reason that has nothing to do
	// with trust.
	config.ResetLoadCache()
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
	// Opt this worktree into grove-managed trust so the handler's config gate
	// (manageTrust=true, default off) passes and the positive tests exercise the
	// actual seed. The disabled/no-op path is covered separately.
	if err := os.WriteFile(filepath.Join(worktreePath, "grove.toml"), []byte("[claude]\nmanageTrust = true\n"), 0o644); err != nil {
		t.Fatalf("write grove.toml: %v", err)
	}
	// ...and approve it, or the exec gate strips [claude] before the handler
	// ever sees manageTrust. See trustWorktreeConfig.
	trustWorktreeConfig(t, worktreePath)
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

// TestHandleSeedTrustSkipsWhenManageTrustDisabled proves the config gate: when
// the worktree's resolved [claude] profile does not enable manageTrust (the
// opt-in default), the handler returns 200 but writes nothing — grove never
// touches ~/.claude.json. This is the daemon-side defense-in-depth mirror of
// the caller gate.
func TestHandleSeedTrustSkipsWhenManageTrustDisabled(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	worktreePath := filepath.Join(t.TempDir(), "my-worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, "core"), 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// A [claude] block WITHOUT manageTrust (unrelated settings only): the gate
	// must resolve disabled and no-op.
	if err := os.WriteFile(filepath.Join(worktreePath, "grove.toml"), []byte("[claude.permissions]\nallow = [\"Bash(git:*)\"]\n"), 0o644); err != nil {
		t.Fatalf("write grove.toml: %v", err)
	}
	// Trust it, so this test fails for ITS OWN reason. Untrusted, the exec gate
	// would strip [claude] wholesale and the no-op would prove nothing about
	// the manageTrust gate.
	trustWorktreeConfig(t, worktreePath)
	if err := worktreeregistry.Save(&worktreeregistry.Entry{AbsPath: worktreePath, Repos: []string{"core"}}); err != nil {
		t.Fatalf("save registry entry: %v", err)
	}

	rec := postSeedTrust(t, map[string]any{"worktree_ref": worktreePath})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if projects := readClaudeProjects(t, home); projects != nil {
		t.Errorf("manageTrust disabled but ~/.claude.json projects were written: %v", projects)
	}
}

// TestHandleSeedTrustSkipsWhenConfigUntrusted proves the exec-provenance gate
// reaches this handler: a worktree that asks for manageTrust in a grove.toml
// the user has NOT approved gets nothing. [claude] is a RiskCapability field,
// so config.LoadFrom strips the block from the untrusted project layer and the
// handler sees no manageTrust to act on.
//
// This is the behaviour that makes the gate worth having here — a repo cloned
// into an ecosystem cannot grant itself Claude folder-trust by shipping a
// grove.toml. It also means grove-managed trust needs `grove config trust`
// once per config file before it propagates.
func TestHandleSeedTrustSkipsWhenConfigUntrusted(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)

	worktreePath := filepath.Join(t.TempDir(), "my-worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, "core"), 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// manageTrust=true, but deliberately NOT trusted.
	if err := os.WriteFile(filepath.Join(worktreePath, "grove.toml"), []byte("[claude]\nmanageTrust = true\n"), 0o644); err != nil {
		t.Fatalf("write grove.toml: %v", err)
	}
	if err := worktreeregistry.Save(&worktreeregistry.Entry{AbsPath: worktreePath, Repos: []string{"core"}}); err != nil {
		t.Fatalf("save registry entry: %v", err)
	}

	rec := postSeedTrust(t, map[string]any{"worktree_ref": worktreePath})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if projects := readClaudeProjects(t, home); projects != nil {
		t.Errorf("untrusted grove.toml but ~/.claude.json projects were written: %v", projects)
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
