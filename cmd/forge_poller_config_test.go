package cmd

// End-to-end coverage of the config→provider→poll path the pipeline-live trial
// found missing (plan hosted-git-and-prs, .artifacts/forge-pipeline-live/
// report.md finding 1: "[forge] url/remote_name/token_command are parse-only").
//
// The headline test drives a REAL Forgejo REST conversation from a REAL [forge]
// TOML block: the real provider constructor, a real `token_command`
// subprocess, a real git remote read off disk, and the real poller — against an
// httptest server speaking Gitea's v1 wire shapes. No `gh` shim anywhere, which
// is exactly what was impossible before this wiring existed.
//
// Nothing here touches the network or the operator's grove roots: the server is
// loopback, the git repo and the token script live in t.TempDir(), and the
// store is a fake.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	grovelogging "github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// ---------------------------------------------------------------------------
// Fakes and fixtures
// ---------------------------------------------------------------------------

// fakeForgeStore is the narrow forgeStore the poller writes through.
type fakeForgeStore struct {
	mu      sync.Mutex
	state   store.State
	updates []store.Update
}

func newFakeForgeStore(paths ...string) *fakeForgeStore {
	ws := make(map[string]*models.EnrichedWorkspace, len(paths))
	for _, p := range paths {
		ws[p] = &models.EnrichedWorkspace{
			WorkspaceNode: &workspace.WorkspaceNode{Path: p, Name: filepath.Base(p)},
		}
	}
	return &fakeForgeStore{state: store.State{Workspaces: ws}}
}

func (s *fakeForgeStore) Get() store.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *fakeForgeStore) ApplyUpdate(u store.Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, u)
}

// forgejoFixture speaks just enough of the Gitea v1 API for one repository, and
// requires the bearer token the daemon is expected to have resolved.
type forgejoFixture struct {
	*httptest.Server
	wantToken string

	mu         sync.Mutex
	authHeader string
}

func newForgejoFixture(t *testing.T, owner, name, token string) *forgejoFixture {
	t.Helper()
	f := &forgejoFixture{wantToken: token}
	base := "/api/v1/repos/" + owner + "/" + name

	mux := http.NewServeMux()
	mux.HandleFunc(base+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorize(w, r) {
			return
		}
		// Page 2 and beyond are empty; the provider stops on a short page.
		if r.URL.Query().Get("page") != "1" {
			writeTestJSON(w, []any{})
			return
		}
		writeTestJSON(w, []map[string]any{{
			"number":     7,
			"title":      "wire [forge] config to the poller",
			"state":      "open",
			"draft":      false,
			"html_url":   "https://forge.invalid/" + owner + "/" + name + "/pulls/7",
			"user":       map[string]any{"login": "operator"},
			"head":       map[string]any{"ref": "feature/poller-wiring", "sha": "cafebabe"},
			"base":       map[string]any{"ref": "main"},
			"created_at": "2026-08-02T10:00:00Z",
			"updated_at": "2026-08-02T11:00:00Z",
		}})
	})
	mux.HandleFunc(base+"/commits/cafebabe/statuses", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorize(w, r) {
			return
		}
		if r.URL.Query().Get("page") != "1" {
			writeTestJSON(w, []any{})
			return
		}
		writeTestJSON(w, []map[string]any{{
			"status":     "success",
			"context":    "grove/build",
			"updated_at": "2026-08-02T11:00:00Z",
		}})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *forgejoFixture) authorize(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	f.authHeader = r.Header.Get("Authorization")
	got := f.authHeader
	f.mu.Unlock()
	if got != "token "+f.wantToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *forgejoFixture) seenAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authHeader
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// initRepoWithRemote creates a real git repo carrying one named remote.
func initRepoWithRemote(t *testing.T, remoteName, remoteURL string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runGitIn(t, dir, "init", "-q")
	runGitIn(t, dir, "remote", "add", remoteName, remoteURL)
	return dir
}

// runGitIn runs git with the operator's own git identity redirected into the
// temp dir — no global config, no system config, nothing outside t.TempDir().
func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(dir, "gitconfig-system"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeTokenScript writes an executable that prints token on stdout and records
// each invocation in a witness file. Returns the script path (the
// token_command) and the witness path.
func writeTokenScript(t *testing.T, token string) (command, witness string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("`sh -c` is the token_command contract")
	}
	dir := t.TempDir()
	witness = filepath.Join(dir, "runs")
	script := filepath.Join(dir, "token.sh")
	body := fmt.Sprintf("#!/bin/sh\necho run >> %q\nprintf %%s %q\n", witness, token)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write token script: %v", err)
	}
	return script, witness
}

func countRuns(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return len(strings.Fields(string(data)))
}

// ---------------------------------------------------------------------------
// The acceptance case
// ---------------------------------------------------------------------------

// TestForgePollerPollsSelfHostedForgeThroughConfig: a daemon whose only input
// is a [forge] TOML block polls a live (httptest) Forgejo through the real
// provider, with the token resolved daemon-side.
func TestForgePollerPollsSelfHostedForgeThroughConfig(t *testing.T) {
	const (
		owner = "grovetools"
		name  = "flow"
		token = "s3cr3t-forge-token"
	)
	fixture := newForgejoFixture(t, owner, name, token)
	tokenCmd, witness := writeTokenScript(t, token)

	// The repo's `forge` remote points at the fixture; its `origin` is a GitHub
	// URL that is never contacted. Polling the former proves the poller
	// followed [forge] remote_name rather than the historical origin-only path.
	repoPath := initRepoWithRemote(t, "forge", fixture.URL+"/"+owner+"/"+name+".git")
	runGitIn(t, repoPath, "remote", "add", "origin", "git@github.com:"+owner+"/"+name+".git")

	cfg := mustConfig(t, fmt.Sprintf(`version = "1.0"

[forge]
url = %q
remote_name = "forge"
token_command = %q

[forge.poll]
enabled = true
interval = "1m"
stale_after = "2m"
`, fixture.URL, tokenCmd))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller := startForgePoller(ctx, newFakeForgeStore(repoPath), cfg, grovelogging.NewUnifiedLogger("test.forgepoll"))
	if poller == nil {
		t.Fatal("startForgePoller returned nil for an enabled, fully configured self-hosted forge")
	}
	if got := poller.ProviderName(); got != "forgejo" {
		t.Fatalf("provider = %q, want forgejo (the whole point of the wiring)", got)
	}

	states := waitForForgeState(t, poller, store.ForgeStateFresh)
	if len(states) != 1 {
		t.Fatalf("cache holds %d repos, want 1: %+v", len(states), states)
	}
	got := states[0]

	wantRepo := hostOf(t, fixture.URL) + "/" + owner + "/" + name
	if got.Repo != wantRepo {
		t.Errorf("repo identity = %q, want %q (the host must come from the [forge] url, not github.com)", got.Repo, wantRepo)
	}
	if got.Provider != "forgejo" {
		t.Errorf("entry provider = %q, want forgejo", got.Provider)
	}
	if len(got.PRs) != 1 || got.PRs[0].Number != 7 {
		t.Fatalf("PRs = %+v, want the fixture's #7", got.PRs)
	}
	if got.PRs[0].HeadRef != "feature/poller-wiring" {
		t.Errorf("head ref = %q, want feature/poller-wiring", got.PRs[0].HeadRef)
	}
	rollup, ok := got.Checks[7]
	if !ok {
		t.Fatal("no check rollup for PR 7 — the statuses call never happened")
	}
	if string(rollup.State.Normalized()) != "success" {
		t.Errorf("rollup = %q, want success", rollup.State)
	}
	if got.LastError != "" {
		t.Errorf("last_error = %q, want empty after a successful sweep", got.LastError)
	}

	// Token custody: the DAEMON ran the command and sent the result. The
	// fixture 401s anything else, so a fresh entry is already proof; the
	// header assertion makes a regression name itself.
	if seen := fixture.seenAuth(); seen != "token "+token {
		t.Errorf("Authorization = %q, want the token_command output", seen)
	}
	// And it ran ONCE: a sweep makes several requests (pulls, then statuses,
	// each paged), which must not mean several secrets-manager invocations.
	if runs := countRuns(t, witness); runs != 1 {
		t.Errorf("token_command ran %d times in one sweep, want exactly 1 (the resolver caches)", runs)
	}
}

// TestForgePollerBackoffStateIsExposed covers the trial's finding 4: after a
// failure the next attempt is minutes out, and the read surface must say so
// instead of letting a surface render designed silence as a hang.
func TestForgePollerBackoffStateIsExposed(t *testing.T) {
	const owner, name = "grovetools", "flow"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	repoPath := initRepoWithRemote(t, "forge", srv.URL+"/"+owner+"/"+name+".git")
	cfg := mustConfig(t, fmt.Sprintf(`version = "1.0"

[forge]
url = %q

[forge.poll]
enabled = true
`, srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poller := startForgePoller(ctx, newFakeForgeStore(repoPath), cfg, grovelogging.NewUnifiedLogger("test.forgepoll"))
	if poller == nil {
		t.Fatal("poller did not start")
	}

	states := waitForForgeState(t, poller, store.ForgeStateUnknown)
	got := states[0]
	if got.LastError == "" {
		t.Error("a failed sweep left no last_error")
	}
	if got.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d, want 1", got.ConsecutiveFailures)
	}
	if got.NextAttemptAt.IsZero() {
		t.Fatal("next_attempt_at is zero after a failure — the quiet period is invisible to surfaces")
	}
	if !got.NextAttemptAt.After(time.Now()) {
		t.Errorf("next_attempt_at = %s is not in the future", got.NextAttemptAt)
	}
	// D4: a failure degrades, it does not fabricate an answer.
	if got.PRs != nil {
		t.Errorf("PRs = %+v, want nil — a repo that never fetched has no pull requests to report", got.PRs)
	}
}

// TestForgePollerProviderOptions pins the three axes that used to be
// unconfigurable, and the one that must NOT change for GitHub.
func TestForgePollerProviderOptions(t *testing.T) {
	tests := []struct {
		name       string
		doc        string
		wantName   string
		wantHosts  []string
		wantRemote string
	}{
		{
			name: "auto with a url picks forgejo, host and remote from config",
			doc: `
[forge]
url = "https://forge.example.com:8443/base"
remote_name = "hub"
`,
			wantName:   "forgejo",
			wantHosts:  []string{"forge.example.com"},
			wantRemote: "hub",
		},
		{
			name: "forgejo defaults remote_name to forge",
			doc: `
[forge]
url = "http://127.0.0.1:3000"
`,
			wantName:   "forgejo",
			wantHosts:  []string{"127.0.0.1"},
			wantRemote: "forge",
		},
		{
			name: "explicit github ignores the url and keeps origin/github.com",
			doc: `
[forge]
url = "https://forge.example.com"
remote_name = "hub"
provider = "github"
`,
			wantName:   "github",
			wantHosts:  nil, // unset → forgepoll.DefaultHosts
			wantRemote: "",  // unset → git.DefaultRemoteName ("origin")
		},
		{
			name:       "no url at all keeps the pre-wiring github behavior",
			doc:        "[forge.poll]\nenabled = true\n",
			wantName:   "github",
			wantHosts:  nil,
			wantRemote: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantName == "github" {
				fakeGH(t)
			}
			cfg := mustConfig(t, "version = \"1.0\"\n"+tc.doc)
			forgeCfg, err := cfg.Forge()
			if err != nil {
				t.Fatalf("decode [forge]: %v", err)
			}
			opts, reason := forgePollerProviderOptions(forgeCfg)
			if opts.Provider == nil {
				t.Fatalf("no provider constructed: %s", reason)
			}
			if got := opts.Provider.Name(); got != tc.wantName {
				t.Errorf("provider = %q, want %q", got, tc.wantName)
			}
			if strings.Join(opts.Hosts, ",") != strings.Join(tc.wantHosts, ",") {
				t.Errorf("hosts = %v, want %v", opts.Hosts, tc.wantHosts)
			}
			if opts.RemoteName != tc.wantRemote {
				t.Errorf("remote = %q, want %q", opts.RemoteName, tc.wantRemote)
			}
		})
	}
}

// TestForgePollerRejectsInvalidConfig: an operator who typed enabled = true
// deserves a log line naming the problem, and a daemon that still boots.
func TestForgePollerRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"non-http url", "[forge]\nurl = \"ssh://forge.example.com\"\n\n[forge.poll]\nenabled = true\n"},
		{"forgejo forced without a url", "[forge]\nprovider = \"forgejo\"\n\n[forge.poll]\nenabled = true\n"},
		{"unknown provider", "[forge]\nprovider = \"gitlab\"\nurl = \"https://f.example\"\n\n[forge.poll]\nenabled = true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil store is the assertion: constructing a poller would
			// dereference it.
			if p := startForgePoller(
				context.Background(),
				nil,
				mustConfig(t, "version = \"1.0\"\n"+tc.doc),
				grovelogging.NewUnifiedLogger("test.forgepoll"),
			); p != nil {
				t.Fatal("an invalid [forge] block started a poller")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// waitForForgeState polls the cache until an entry reaches want. The poller
// sweeps immediately on Start, so this waits on a goroutine handoff and one
// loopback round trip — never on a poll interval.
func waitForForgeState(t *testing.T, poller interface {
	Snapshot() []store.ForgeRepoState
}, want string,
) []store.ForgeRepoState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last []store.ForgeRepoState
	for time.Now().Before(deadline) {
		last = poller.Snapshot()
		// An unknown entry is only interesting once an attempt has failed;
		// "unknown with no error" is just the entry being created.
		if len(last) > 0 && last[0].State == want &&
			(want != store.ForgeStateUnknown || last[0].LastError != "") {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("cache never reached state %q; last snapshot: %+v", want, last)
	return nil
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return strings.ToLower(u.Hostname())
}
