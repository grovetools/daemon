package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/coderoot"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/registry"
	"github.com/grovetools/core/pkg/workspace"
)

func writeMachineSyncTip(t *testing.T, repo, branch, sha string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "refs", "heads", branch), []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectMachineSyncTierZeroStatesAndMetadata(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "demo")
	localSHA := "1111111111111111111111111111111111111111"
	writeMachineSyncTip(t, repo, "main", localSHA)

	enabled, disabled := true, false
	codeRoots := coderoot.Table{Roots: map[string]coderoot.Root{
		"code": {Path: root, Scan: true, Enabled: &enabled},
	}}
	workspaces := map[string]*models.EnrichedWorkspace{
		repo: {WorkspaceNode: &workspace.WorkspaceNode{Path: repo, Name: "demo"}},
	}
	note := func(id, name string, root registry.NoteRoot, repos ...registry.NoteRepo) registry.Machine {
		return registry.Machine{
			PathID: id,
			Note:   &registry.Note{Name: name, LastSeen: "2026-08-08", Roots: []registry.NoteRoot{root}, Repos: repos},
		}
	}
	machines := []registry.Machine{
		note("self", "local", registry.NoteRoot{Name: "code", Enabled: true, Exists: true}),
		note("equal", "Equal", registry.NoteRoot{Name: "code", Enabled: true, Exists: true}, registry.NoteRepo{Root: "code", Path: "demo", Branch: "main", SHA: localSHA}),
		note("diverged", "Diverged", registry.NoteRoot{Name: "code", Enabled: true, Exists: true}, registry.NoteRepo{Root: "code", Path: "demo", Branch: "topic", SHA: "2222222222222222222222222222222222222222"}),
		note("absent", "Absent", registry.NoteRoot{Name: "code", Enabled: true, Exists: true}),
		note("excluded", "Excluded", registry.NoteRoot{Name: "code", Enabled: disabled, Exists: true}, registry.NoteRepo{Root: "code", Path: "demo", Branch: "main", SHA: localSHA}),
		note("unknown", "Unknown", registry.NoteRoot{Name: "other", Enabled: true, Exists: true}),
		{PathID: "suspect", Note: &registry.Note{Name: "Suspect", LastSeen: "2026-08-08"}, Suspect: []string{"rev regressed"}},
	}

	now := time.Date(2026, 8, 9, 1, 0, 0, 0, time.UTC)
	got := projectMachineSync(workspaces, "self", codeRoots, machines, now)[repo]
	if got == nil {
		t.Fatal("projection is nil")
	}
	if got.RootID != "root:code" || got.RepoPath != "demo" || got.LocalSHA != localSHA {
		t.Fatalf("local identity/tip = %+v", got)
	}
	if len(got.Peers) != 6 {
		t.Fatalf("len(Peers) = %d, want 6 (self omitted)", len(got.Peers))
	}
	want := []models.MachineSyncState{
		models.MachineSyncEqual,
		models.MachineSyncDiverged,
		models.MachineSyncAbsent,
		models.MachineSyncExcluded,
		models.MachineSyncUnknown,
		models.MachineSyncUnknown,
	}
	for i, state := range want {
		if got.Peers[i].State != state {
			t.Errorf("peer %s state = %q, want %q", got.Peers[i].MachineID, got.Peers[i].State, state)
		}
	}
	if !got.Peers[0].SameBranch || got.Peers[1].SameBranch {
		t.Errorf("branch metadata wrong: equal=%v diverged=%v", got.Peers[0].SameBranch, got.Peers[1].SameBranch)
	}
	if got.Peers[0].AgeSeconds == nil || *got.Peers[0].AgeSeconds != 25*60*60 {
		t.Errorf("age = %v, want 90000 seconds", got.Peers[0].AgeSeconds)
	}
	if !got.Peers[5].Suspect || got.Peers[5].State != models.MachineSyncUnknown {
		t.Errorf("suspect peer became believable: %+v", got.Peers[5])
	}
}

func TestProjectMachineSyncUnknownNeverEqualsWithoutLocalTip(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "demo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	codeRoots := coderoot.Table{Roots: map[string]coderoot.Root{
		"code": {Path: root, Scan: true},
	}}
	remote := registry.Machine{PathID: "remote", Note: &registry.Note{
		Name: "remote", LastSeen: "not-a-date",
		Roots: []registry.NoteRoot{{Name: "code", Enabled: true, Exists: true}},
		Repos: []registry.NoteRepo{{Root: "code", Path: "demo", Branch: "main", SHA: "1111111111111111111111111111111111111111"}},
	}}
	got := projectMachineSync(map[string]*models.EnrichedWorkspace{
		repo: {WorkspaceNode: &workspace.WorkspaceNode{Path: repo}},
	}, "self", codeRoots, []registry.Machine{remote}, time.Now())[repo]
	if got.Peers[0].State != models.MachineSyncUnknown {
		t.Fatalf("state = %q, want unknown", got.Peers[0].State)
	}
	if got.Peers[0].AgeSeconds != nil {
		t.Fatalf("malformed last_seen produced age %v", *got.Peers[0].AgeSeconds)
	}
}

func TestFindRemoteMachineSyncRootUsesEcosystemCardIdentityAndFilters(t *testing.T) {
	note := &registry.Note{Ecosystems: []registry.NoteEcosystem{{
		Name: "renamed", Enabled: true, State: registry.StatePresent,
		Repos: []string{"core"}, Card: &registry.NoteCard{ID: "01ECOSYSTEM"},
	}}}
	got, ok := findRemoteMachineSyncRoot(note, machineSyncRoot{name: "local-name", id: "ecosystem:01ECOSYSTEM", ecosystem: true})
	if !ok || got.name != "renamed" || !got.present {
		t.Fatalf("card identity did not match renamed ecosystem: %+v, %v", got, ok)
	}
	if !got.includes("core") || got.includes("nav") {
		t.Fatalf("partial subscription filter not preserved: %+v", got)
	}
}
