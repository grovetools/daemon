package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/pkg/registry"
	"github.com/grovetools/core/pkg/syncproto"
)

const guardOwnID = "01KZ00TTW1TDT7X9ABCDEFGHJK"

// newGuardPipeline builds a pull pipeline subscribed to a registry-role
// notespace, with a sandboxed state dir (conflict artifacts land there) and a
// recording conflict callback.
func newGuardPipeline(t *testing.T, role string) (*PullPipeline, *[]string) {
	t.Helper()
	// Full sandbox: conflict artifacts resolve through paths.StateDir(), and
	// anything on this path that reaches for config must not find the
	// developer's real ~/.config/grove.
	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)

	db := openTestDB(t)
	p := NewPullPipeline(
		&config.SyncWorkspace{Name: "registry", Role: role, Pull: true},
		nil, db, logging.NewUnifiedLogger("test.registry.guard"))
	p.OwnMachineID = guardOwnID

	var seen []string
	p.OnRegistryForeignWrite = func(ws, path, detail string) {
		seen = append(seen, ws+" "+path+" "+detail)
	}
	return p, &seen
}

// conflictFiles lists artifact paths relative to the notespace's conflict
// directory. It WALKS, matching handleSyncConflicts: an artifact for
// "machines/<id>.md" nests one directory deep, because the artifact name
// embeds the document's own wire path.
func conflictFiles(t *testing.T, notespace string) []string {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "sync", "conflicts", notespace)
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a missing dir means "no conflicts"
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

// TestGuardDropsForeignWriteToOwnNote is the core safety property: the
// registry is single-writer, so an inbound event for our own path is somebody
// else writing our note. It is dropped, and the evidence is kept.
func TestGuardDropsForeignWriteToOwnNote(t *testing.T) {
	p, seen := newGuardPipeline(t, config.SyncRoleRegistry)
	root := t.TempDir()

	forged := []byte("---\nmachine_id: " + guardOwnID + "\nname: impostor\nrev: 99\n---\n")
	ev := &syncproto.SyncEvent{
		Type:        syncproto.EventDocumentCreated,
		DocumentID:  "doc-forged",
		Path:        registry.NotePath(guardOwnID),
		Version:     1,
		ContentHash: sha(forged),
		Content:     forged,
	}
	if err := p.applyEvent(t.Context(), root, ev); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	// Nothing landed on disk...
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(ev.Path))); err == nil {
		t.Error("the foreign write was applied to the local tree")
	}
	// ...nor in the identity map.
	if doc, _ := p.db.GetDocumentByPath("registry", ev.Path); doc != nil {
		t.Error("the foreign write was recorded in sync.db")
	}
	// ...and it was surfaced.
	if len(*seen) != 1 || !strings.Contains((*seen)[0], registry.NotePath(guardOwnID)) {
		t.Fatalf("conflict broadcast = %v", *seen)
	}

	// The artifact carries the KIND in its name, which is the only way the
	// artifact-backed conflicts endpoint can report it.
	files := conflictFiles(t, "registry")
	if len(files) != 1 {
		t.Fatalf("conflict artifacts = %v, want exactly one", files)
	}
	if !strings.Contains(files[0], ConflictKindRegistryForeignWrite) {
		t.Errorf("artifact name %q does not carry the kind", files[0])
	}
	body, err := os.ReadFile(filepath.Join(paths.StateDir(), "sync", "conflicts", "registry", files[0]))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(forged) {
		t.Errorf("artifact does not hold the rejected content:\n%s", body)
	}
}

// TestGuardDropsAMoveOntoOwnNote: renaming a foreign document onto our path is
// the same write by another spelling.
func TestGuardDropsAMoveOntoOwnNote(t *testing.T) {
	p, seen := newGuardPipeline(t, config.SyncRoleRegistry)
	err := p.applyEvent(t.Context(), t.TempDir(), &syncproto.SyncEvent{
		Type:       syncproto.EventDocumentMoved,
		DocumentID: "doc-moved",
		PrevPath:   registry.NotePath(guardOwnID),
		Path:       "machines/01SOMEONEELSE.md",
	})
	if err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("a move touching our note was not guarded: %v", *seen)
	}
}

// TestGuardLetsOtherMachinesThrough: the guard is about OUR path only. Every
// other machine's note must replicate normally, or the registry shows nothing.
func TestGuardLetsOtherMachinesThrough(t *testing.T) {
	p, seen := newGuardPipeline(t, config.SyncRoleRegistry)
	root := t.TempDir()

	peer := []byte("---\nmachine_id: 01PEER\nname: solm4\nrev: 1\n---\n")
	peerPath := registry.NotePath("01PEER")
	if err := p.applyEvent(t.Context(), root, &syncproto.SyncEvent{
		Type:        syncproto.EventDocumentCreated,
		DocumentID:  "doc-peer",
		Path:        peerPath,
		Version:     1,
		ContentHash: sha(peer),
		Content:     peer,
	}); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if len(*seen) != 0 {
		t.Errorf("a peer's note was flagged: %v", *seen)
	}
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(peerPath)))
	if err != nil {
		t.Fatalf("peer note not materialized: %v", err)
	}
	if string(got) != string(peer) {
		t.Errorf("peer note content = %q", got)
	}
}

// TestGuardIsScopedToTheRegistryRole: a machines/<id>.md path inside an
// ORDINARY notebook notespace is just a document. Dropping it there would be a
// silent data-loss bug, not a safety property.
func TestGuardIsScopedToTheRegistryRole(t *testing.T) {
	for _, role := range []string{"", config.SyncRolePeer, config.SyncRoleSatellite} {
		p, seen := newGuardPipeline(t, role)
		root := t.TempDir()
		content := []byte("ordinary document\n")
		if err := p.applyEvent(t.Context(), root, &syncproto.SyncEvent{
			Type:        syncproto.EventDocumentCreated,
			DocumentID:  "doc-ordinary",
			Path:        registry.NotePath(guardOwnID),
			Version:     1,
			ContentHash: sha(content),
			Content:     content,
		}); err != nil {
			t.Fatalf("role %q: applyEvent: %v", role, err)
		}
		if len(*seen) != 0 {
			t.Errorf("role %q: guard fired outside the registry role", role)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(registry.NotePath(guardOwnID)))); err != nil {
			t.Errorf("role %q: ordinary document was dropped: %v", role, err)
		}
	}
}

// TestGuardIsDormantWithoutAnIdentity: a machine with no minted id degrades to
// the pre-identity behavior rather than guarding a path it cannot name.
func TestGuardIsDormantWithoutAnIdentity(t *testing.T) {
	p, seen := newGuardPipeline(t, config.SyncRoleRegistry)
	p.OwnMachineID = ""
	content := []byte("x\n")
	if err := p.applyEvent(t.Context(), t.TempDir(), &syncproto.SyncEvent{
		Type:        syncproto.EventDocumentCreated,
		DocumentID:  "doc-x",
		Path:        registry.NotePath(guardOwnID),
		Version:     1,
		ContentHash: sha(content),
		Content:     content,
	}); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if len(*seen) != 0 {
		t.Errorf("guard fired without an identity: %v", *seen)
	}
}

// TestConflictArtifactNamesRoundTrip pins the naming scheme the writer and the
// conflicts endpoint share. A legacy (merge) name must keep parsing exactly as
// it did, or every conflict artifact already on disk becomes unreadable.
func TestConflictArtifactNamesRoundTrip(t *testing.T) {
	cases := []struct {
		path, docID, kind string
	}{
		{"inbox/note.md", "0f8a1d2e-1111-2222-3333-444455556666", ConflictKindMerge},
		{"machines/01ABC.md", "0f8a1d2e-1111-2222-3333-444455556666", ConflictKindRegistryForeignWrite},
		{"plans/a.b.c.md", "doc-1", ConflictKindMerge},
	}
	for _, c := range cases {
		name := conflictArtifactName(c.path, c.docID, c.kind)
		gotPath, gotID, gotKind, ok := ParseConflictArtifactName(name)
		if !ok {
			t.Fatalf("%q did not parse", name)
		}
		if gotPath != c.path || gotID != c.docID || gotKind != c.kind {
			t.Errorf("%q -> (%q, %q, %q), want (%q, %q, %q)",
				name, gotPath, gotID, gotKind, c.path, c.docID, c.kind)
		}
	}

	// The historical shape, written by a daemon that predates kinds.
	p, id, kind, ok := ParseConflictArtifactName("inbox/note.md.doc-7.conflict.md")
	if !ok || p != "inbox/note.md" || id != "doc-7" || kind != ConflictKindMerge {
		t.Errorf("legacy name parsed as (%q, %q, %q, %v)", p, id, kind, ok)
	}
	if _, _, _, ok := ParseConflictArtifactName("not-an-artifact.md"); ok {
		t.Error("a non-artifact parsed")
	}
	if _, _, _, ok := ParseConflictArtifactName("noduid.conflict.md"); ok {
		t.Error("an artifact with no document id parsed")
	}
}
