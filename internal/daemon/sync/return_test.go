package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grovetools/core/pkg/syncproto"
)

func returnTestClient(t *testing.T, epoch string, snaps map[string]syncproto.SnapshotManifest, content map[string][]byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/capabilities":
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{Capabilities: syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersion}}, ServerEpoch: epoch})
		case "/sync/snapshot":
			_ = json.NewEncoder(w).Encode(snaps[r.URL.Query().Get("workspace")])
		case "/sync/history/blob":
			b, ok := content[r.URL.Query().Get("document_id")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{ServerURL: srv.URL, Token: "x", OriginID: "laptop"})
	if _, err := c.Capabilities(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	return c
}
func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestReturnManifestRepresentsCreateUpdateMoveDelete(t *testing.T) {
	db := openTestDB(t)
	old := []byte("old")
	same := []byte("same")
	for _, d := range []*Document{{DocumentID: "update", Workspace: "ws", Path: "u.md", ContentHash: hash(old)}, {DocumentID: "move", Workspace: "ws", Path: "before.md", ContentHash: hash(old)}, {DocumentID: "delete", Workspace: "ws", Path: "gone.md", ContentHash: hash(old), LastSyncedVersion: 1}, {DocumentID: "same", Workspace: "ws", Path: "same.md", ContentHash: hash(same)}} {
		if err := db.UpsertDocument(d); err != nil {
			t.Fatal(err)
		}
	}
	heads := map[string][]byte{"create": []byte("new"), "update": []byte("changed"), "move": []byte("moved"), "same": same}
	var docs []syncproto.DocumentSnapshot
	for _, x := range []struct{ id, path string }{{"create", "new.md"}, {"update", "u.md"}, {"move", "after.md"}, {"same", "same.md"}} {
		docs = append(docs, syncproto.DocumentSnapshot{ID: x.id, Path: x.path, Version: 2, Hash: hash(heads[x.id]), Size: int64(len(heads[x.id]))})
	}
	c := returnTestClient(t, "epoch-1", map[string]syncproto.SnapshotManifest{"ws": {Workspace: "ws", Cursor: 9, Documents: docs}}, heads)
	m, err := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, op := range m.Operations {
		got[op.Type] = true
	}
	for _, typ := range []string{"create", "update", "move", "delete"} {
		if !got[typ] {
			t.Errorf("missing %s: %+v", typ, m.Operations)
		}
	}
	if err = m.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReturnManifestIgnoresServerAbsenceForNeverSyncedLocalIdentity(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertDocument(&Document{DocumentID: "stale", Workspace: "ws", Path: "plans/old/a.md", ContentHash: hash([]byte("body")), LastSyncedVersion: 0}); err != nil {
		t.Fatal(err)
	}
	c := returnTestClient(t, "epoch-1", map[string]syncproto.SnapshotManifest{"ws": {Workspace: "ws", Cursor: 1}}, nil)
	m, err := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Operations) != 0 {
		t.Fatalf("never-synced local identity produced incoming operations: %+v", m.Operations)
	}
}

func TestReturnManifestLocalAndServerChangesInvalidateGeneration(t *testing.T) {
	db := openTestDB(t)
	body := []byte("head")
	if err := db.UpsertDocument(&Document{DocumentID: "d", Workspace: "ws", Path: "a.md", ContentHash: hash([]byte("base"))}); err != nil {
		t.Fatal(err)
	}
	snap := syncproto.SnapshotManifest{Workspace: "ws", Cursor: 1, Documents: []syncproto.DocumentSnapshot{{ID: "d", Path: "a.md", Version: 1, Hash: hash(body)}}}
	c := returnTestClient(t, "epoch-1", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": body})
	a, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	_ = db.UpsertDocument(&Document{DocumentID: "d", Workspace: "ws", Path: "a.md", ContentHash: hash([]byte("local edit"))})
	b, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if a.Generation == b.Generation {
		t.Fatal("local state change did not stale reviewed generation")
	}
	c2 := returnTestClient(t, "epoch-2", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": body})
	cman, _ := BuildReturnManifest(context.Background(), c2, db, []string{"ws"})
	if b.Generation == cman.Generation {
		t.Fatal("server epoch change did not change generation")
	}
}

func TestReviewedReturnManifestStaleRefusal(t *testing.T) {
	db := openTestDB(t)
	body := []byte("head")
	snap := syncproto.SnapshotManifest{Workspace: "ws", Cursor: 1, Documents: []syncproto.DocumentSnapshot{{ID: "d", Path: "a.md", Version: 1, Hash: hash(body)}}}
	c := returnTestClient(t, "epoch", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": body})
	reviewed, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err := db.UpsertDocument(&Document{DocumentID: "local", Workspace: "ws", Path: "local.md", ContentHash: hash([]byte("edit"))}); err != nil {
		t.Fatal(err)
	}
	current, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err := ValidateReviewedManifest(reviewed, current); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("got %v", err)
	}
}

func writeApplyEscrow(t *testing.T, ops []ReturnOperation, content map[string][]byte) (string, ReturnManifest) {
	t.Helper()
	m := ReturnManifest{Schema: ReturnManifestSchema, OperationID: "apply-test", ServerEpoch: "epoch", Generation: strings.Repeat("a", 64), Workspaces: []string{"one", "two"}, Operations: ops}
	m.ManifestSHA256 = manifestHash(m)
	p := filepath.Join(t.TempDir(), "escrow.json")
	b, err := json.Marshal(ReturnEscrow{Manifest: m, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, m
}

func TestApplyReturnEscrowAllOperationsMultiWorkspace(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	oldUpdate, oldMove, oldDelete := []byte("old update"), []byte("old move"), []byte("old delete")
	if err := os.WriteFile(filepath.Join(one, "update.md"), oldUpdate, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(two, "before.md"), oldMove, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(two, "delete.md"), oldDelete, 0o600); err != nil {
		t.Fatal(err)
	}
	content := map[string][]byte{"c": []byte("created"), "u": []byte("updated"), "m": []byte("moved head")}
	ops := []ReturnOperation{
		{Type: "create", Workspace: "one", DocumentID: "c", Path: "nested/create.md", HeadHash: hash(content["c"]), HeadVersion: 1},
		{Type: "update", Workspace: "one", DocumentID: "u", Path: "update.md", BaseHash: hash(oldUpdate), HeadHash: hash(content["u"]), HeadVersion: 2},
		{Type: "move", Workspace: "two", DocumentID: "m", PreviousPath: "before.md", Path: "nested/after.md", BaseHash: hash(oldMove), HeadHash: hash(content["m"]), HeadVersion: 3},
		{Type: "delete", Workspace: "two", DocumentID: "d", Path: "delete.md", BaseHash: hash(oldDelete)},
	}
	escrow, m := writeApplyEscrow(t, ops, content)
	counts, err := ApplyReturnEscrow(escrow, m.Generation, ReturnApplyOptions{WorkspaceRoots: map[string]string{"one": one, "two": two}})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Create != 1 || counts.Update != 1 || counts.Move != 1 || counts.Delete != 1 {
		t.Fatalf("counts: %+v", counts)
	}
	for filename, want := range map[string]string{
		filepath.Join(one, "nested/create.md"): "created",
		filepath.Join(one, "update.md"):        "updated",
		filepath.Join(two, "nested/after.md"):  "moved head",
	} {
		got, err := os.ReadFile(filename)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", filename, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(two, "before.md")); !os.IsNotExist(err) {
		t.Fatal("move source remains")
	}
	if _, err := os.Stat(filepath.Join(two, "delete.md")); !os.IsNotExist(err) {
		t.Fatal("delete remains")
	}
}

func TestApplyReturnEscrowRollbackAndPathRefusal(t *testing.T) {
	root := t.TempDir()
	old := []byte("old")
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(root, name), old, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	content := map[string][]byte{"a": []byte("new a"), "b": []byte("new b")}
	ops := []ReturnOperation{
		{Type: "update", Workspace: "one", DocumentID: "a", Path: "a.md", BaseHash: hash(old), HeadHash: hash(content["a"]), HeadVersion: 1},
		{Type: "update", Workspace: "one", DocumentID: "b", Path: "b.md", BaseHash: hash(old), HeadHash: hash(content["b"]), HeadVersion: 1},
	}
	escrow, m := writeApplyEscrow(t, ops, content)
	_, err := ApplyReturnEscrow(escrow, m.Generation, ReturnApplyOptions{WorkspaceRoots: map[string]string{"one": root, "two": t.TempDir()}, BeforeCommit: func(i int, _ ReturnOperation) error {
		if i == 1 {
			return errors.New("injected")
		}
		return nil
	}})
	if err == nil {
		t.Fatal("injected failure succeeded")
	}
	for _, name := range []string{"a.md", "b.md"} {
		got, _ := os.ReadFile(filepath.Join(root, name))
		if string(got) != "old" {
			t.Fatalf("rollback left %s = %q", name, got)
		}
	}
	if err := VerifyReturnEscrow(escrow, m.Generation); err != nil {
		t.Fatalf("escrow not retained: %v", err)
	}

	bad := []ReturnOperation{{Type: "create", Workspace: "one", DocumentID: "x", Path: "../escape.md", HeadHash: hash([]byte("x")), HeadVersion: 1}}
	badEscrow, badManifest := writeApplyEscrow(t, bad, map[string][]byte{"x": []byte("x")})
	if _, err = ApplyReturnEscrow(badEscrow, badManifest.Generation, ReturnApplyOptions{WorkspaceRoots: map[string]string{"one": root, "two": t.TempDir()}}); err == nil {
		t.Fatal("traversal accepted")
	}
}

// applyRoots is the two-workspace root pair every writeApplyEscrow manifest
// expects.
func applyRoots(one, two string) ReturnApplyOptions {
	return ReturnApplyOptions{WorkspaceRoots: map[string]string{"one": one, "two": two}}
}

// assertNoReturnResidue proves a failed or completed batch left no staging or
// backup artifacts behind in the notebook tree.
func assertNoReturnResidue(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".record-return-") {
				t.Fatalf("residue left in %s: %s", dir, entry.Name())
			}
		}
	}
}

func TestApplyReturnEscrowRefusesLocalHashDrift(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	base, head := []byte("base"), []byte("head")
	if err := os.WriteFile(filepath.Join(one, "a.md"), []byte("drifted locally"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := []ReturnOperation{{Type: "update", Workspace: "one", DocumentID: "a", Path: "a.md", BaseHash: hash(base), HeadHash: hash(head), HeadVersion: 2}}
	escrow, m := writeApplyEscrow(t, ops, map[string][]byte{"a": head})
	_, err := ApplyReturnEscrow(escrow, m.Generation, applyRoots(one, two))
	if err == nil || !strings.Contains(err.Error(), "local hash drift") {
		t.Fatalf("got %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(one, "a.md"))
	if string(got) != "drifted locally" {
		t.Fatalf("drifted file was mutated: %q", got)
	}
	assertNoReturnResidue(t, one, two)
}

func TestApplyReturnEscrowRefusesOccupiedDestinations(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(one, two string)
		ops        func() ([]ReturnOperation, map[string][]byte)
	}{
		{
			name: "create destination occupied",
			want: "create destination exists",
			setup: func(one, _ string) {
				if err := os.WriteFile(filepath.Join(one, "c.md"), []byte("mine"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			ops: func() ([]ReturnOperation, map[string][]byte) {
				body := []byte("incoming")
				return []ReturnOperation{{Type: "create", Workspace: "one", DocumentID: "c", Path: "c.md", HeadHash: hash(body), HeadVersion: 1}}, map[string][]byte{"c": body}
			},
		},
		{
			name: "move destination occupied",
			want: "move destination exists",
			setup: func(one, _ string) {
				src, dst := []byte("src"), []byte("mine")
				if err := os.WriteFile(filepath.Join(one, "before.md"), src, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(one, "after.md"), dst, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			ops: func() ([]ReturnOperation, map[string][]byte) {
				body := []byte("head")
				return []ReturnOperation{{Type: "move", Workspace: "one", DocumentID: "m", PreviousPath: "before.md", Path: "after.md", BaseHash: hash([]byte("src")), HeadHash: hash(body), HeadVersion: 2}}, map[string][]byte{"m": body}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			one, two := t.TempDir(), t.TempDir()
			tc.setup(one, two)
			ops, content := tc.ops()
			escrow, m := writeApplyEscrow(t, ops, content)
			_, err := ApplyReturnEscrow(escrow, m.Generation, applyRoots(one, two))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			assertNoReturnResidue(t, one, two)
		})
	}
}

func TestApplyReturnEscrowRefusesSymlinkEscape(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.md"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(one, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(one, "linkfile.md")); err != nil {
		t.Fatal(err)
	}
	head := []byte("head")
	for _, tc := range []struct{ name, path string }{
		{"symlinked parent", "linkdir/target.md"},
		{"symlinked leaf", "linkfile.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := []ReturnOperation{{Type: "update", Workspace: "one", DocumentID: "s", Path: tc.path, BaseHash: hash([]byte("base")), HeadHash: hash(head), HeadVersion: 2}}
			escrow, m := writeApplyEscrow(t, ops, map[string][]byte{"s": head})
			_, err := ApplyReturnEscrow(escrow, m.Generation, applyRoots(one, two))
			if err == nil || !strings.Contains(err.Error(), "symlink is not allowed") {
				t.Fatalf("got %v", err)
			}
		})
	}
	got, _ := os.ReadFile(filepath.Join(outside, "target.md"))
	if string(got) != "base" {
		t.Fatalf("wrote through symlink: %q", got)
	}
}

func TestApplyReturnEscrowRefusesStaleAndMalformedEscrow(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	body := []byte("incoming")
	ops := []ReturnOperation{{Type: "create", Workspace: "one", DocumentID: "c", Path: "c.md", HeadHash: hash(body), HeadVersion: 1}}
	escrow, m := writeApplyEscrow(t, ops, map[string][]byte{"c": body})

	if _, err := ApplyReturnEscrow(escrow, strings.Repeat("b", 64), applyRoots(one, two)); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale generation: got %v", err)
	}

	for _, tc := range []struct {
		name, want string
		mutate     func(raw []byte) []byte
	}{
		{"trailing data", "trailing data", func(raw []byte) []byte { return append(raw, []byte("\n{}")...) }},
		{"unknown field", "unknown field", func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `{"manifest"`, `{"smuggled":1,"manifest"`, 1))
		}},
		{"head hash mismatch", "content hash mismatch", func(raw []byte) []byte {
			var e ReturnEscrow
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			e.Content["c"] = []byte("tampered")
			out, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"unbound content", "unbound content", func(raw []byte) []byte {
			var e ReturnEscrow
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			e.Content["stowaway"] = []byte("extra")
			out, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(escrow)
			if err != nil {
				t.Fatal(err)
			}
			broken := filepath.Join(t.TempDir(), "escrow.json")
			if err = os.WriteFile(broken, tc.mutate(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err = ApplyReturnEscrow(broken, m.Generation, applyRoots(one, two)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if _, err = os.Stat(filepath.Join(one, "c.md")); !os.IsNotExist(err) {
				t.Fatal("malformed escrow mutated the notebook")
			}
		})
	}
}

func TestApplyReturnEscrowAlreadyAbsentDeleteIsNoop(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	present := []byte("still here")
	if err := os.WriteFile(filepath.Join(two, "present.md"), present, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := []ReturnOperation{
		{Type: "delete", Workspace: "one", DocumentID: "gone", Path: "nested/gone.md", BaseHash: hash([]byte("whatever"))},
		{Type: "delete", Workspace: "two", DocumentID: "present", Path: "present.md", BaseHash: hash(present)},
	}
	escrow, m := writeApplyEscrow(t, ops, map[string][]byte{})
	counts, err := ApplyReturnEscrow(escrow, m.Generation, applyRoots(one, two))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Noop != 1 || counts.Delete != 1 {
		t.Fatalf("counts: %+v", counts)
	}
	if _, err = os.Stat(filepath.Join(two, "present.md")); !os.IsNotExist(err) {
		t.Fatal("present delete did not apply")
	}
	// The absent target's parent must not be conjured into existence.
	if _, err = os.Stat(filepath.Join(one, "nested")); !os.IsNotExist(err) {
		t.Fatal("no-op delete created its parent directory")
	}
	assertNoReturnResidue(t, one, two)
}

func TestApplyReturnEscrowRollbackRestoresMoveAndDelete(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	moved, deleted := []byte("move source"), []byte("delete target")
	if err := os.WriteFile(filepath.Join(two, "before.md"), moved, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(two, "doomed.md"), deleted, 0o600); err != nil {
		t.Fatal(err)
	}
	head, created := []byte("moved head"), []byte("created")
	ops := []ReturnOperation{
		{Type: "move", Workspace: "two", DocumentID: "m", PreviousPath: "before.md", Path: "nested/after.md", BaseHash: hash(moved), HeadHash: hash(head), HeadVersion: 2},
		{Type: "delete", Workspace: "two", DocumentID: "d", Path: "doomed.md", BaseHash: hash(deleted)},
		{Type: "create", Workspace: "one", DocumentID: "c", Path: "c.md", HeadHash: hash(created), HeadVersion: 1},
	}
	escrow, m := writeApplyEscrow(t, ops, map[string][]byte{"m": head, "c": created})
	opts := applyRoots(one, two)
	opts.BeforeCommit = func(i int, _ ReturnOperation) error {
		if i == 2 {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := ApplyReturnEscrow(escrow, m.Generation, opts); err == nil {
		t.Fatal("injected failure succeeded")
	}
	for name, want := range map[string]string{"before.md": "move source", "doomed.md": "delete target"} {
		got, err := os.ReadFile(filepath.Join(two, name))
		if err != nil || string(got) != want {
			t.Fatalf("rollback left %s = %q (%v)", name, got, err)
		}
	}
	for _, absent := range []string{filepath.Join(two, "nested", "after.md"), filepath.Join(two, "nested"), filepath.Join(one, "c.md")} {
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("rollback left %s behind", absent)
		}
	}
	assertNoReturnResidue(t, one, two)
	if err := VerifyReturnEscrow(escrow, m.Generation); err != nil {
		t.Fatalf("escrow not retained: %v", err)
	}
}

func TestReturnManifestValidateRejectsMalformedOperations(t *testing.T) {
	good := hash([]byte("x"))
	for _, tc := range []struct {
		name string
		ops  []ReturnOperation
		ws   []string
	}{
		{name: "create carrying a base hash", ops: []ReturnOperation{{Type: "create", Workspace: "one", DocumentID: "a", Path: "a.md", BaseHash: good, HeadHash: good, HeadVersion: 1}}},
		{name: "create without a head version", ops: []ReturnOperation{{Type: "create", Workspace: "one", DocumentID: "a", Path: "a.md", HeadHash: good}}},
		{name: "update with a short hash", ops: []ReturnOperation{{Type: "update", Workspace: "one", DocumentID: "a", Path: "a.md", BaseHash: "abc", HeadHash: good, HeadVersion: 1}}},
		{name: "delete carrying a head hash", ops: []ReturnOperation{{Type: "delete", Workspace: "one", DocumentID: "a", Path: "a.md", BaseHash: good, HeadHash: good}}},
		{name: "move without a previous path", ops: []ReturnOperation{{Type: "move", Workspace: "one", DocumentID: "a", Path: "a.md", BaseHash: good, HeadHash: good, HeadVersion: 1}}},
		{name: "absolute path", ops: []ReturnOperation{{Type: "create", Workspace: "one", DocumentID: "a", Path: "/etc/passwd", HeadHash: good, HeadVersion: 1}}},
		{name: "traversal path", ops: []ReturnOperation{{Type: "create", Workspace: "one", DocumentID: "a", Path: "../a.md", HeadHash: good, HeadVersion: 1}}},
		{name: "unclean path", ops: []ReturnOperation{{Type: "create", Workspace: "one", DocumentID: "a", Path: "./a.md", HeadHash: good, HeadVersion: 1}}},
		{name: "workspace outside the reviewed set", ops: []ReturnOperation{{Type: "create", Workspace: "three", DocumentID: "a", Path: "a.md", HeadHash: good, HeadVersion: 1}}},
		{name: "duplicate document id", ops: []ReturnOperation{
			{Type: "create", Workspace: "one", DocumentID: "a", Path: "a.md", HeadHash: good, HeadVersion: 1},
			{Type: "create", Workspace: "one", DocumentID: "a", Path: "b.md", HeadHash: good, HeadVersion: 1},
		}},
		{name: "unsorted workspace set", ws: []string{"two", "one"}},
		{name: "duplicate workspace", ws: []string{"one", "one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := tc.ws
			if ws == nil {
				ws = []string{"one", "two"}
			}
			m := ReturnManifest{Schema: ReturnManifestSchema, OperationID: "v", ServerEpoch: "epoch", Generation: strings.Repeat("a", 64), Workspaces: ws, Operations: tc.ops}
			m.ManifestSHA256 = manifestHash(m)
			if err := m.Validate(); err == nil {
				t.Fatal("accepted malformed manifest")
			}
		})
	}
}

func TestPendingReturnPushRefusesUnpushedLocalChange(t *testing.T) {
	m := ReturnManifest{Workspaces: []string{"ws"}, Operations: []ReturnOperation{
		{Type: "update", Workspace: "ws", DocumentID: "d", Path: "a.md", BaseHash: hash([]byte("base")), HeadHash: hash([]byte("head")), HeadVersion: 2},
		{Type: "move", Workspace: "ws", DocumentID: "m", PreviousPath: "before.md", Path: "after.md", BaseHash: hash([]byte("s")), HeadHash: hash([]byte("h")), HeadVersion: 2},
	}}
	for _, tc := range []struct {
		name  string
		entry *OutboxEntry
		want  string
	}{
		{name: "clean outbox", want: ""},
		{name: "queued push on an updated path", entry: &OutboxEntry{DocumentID: "other", Workspace: "ws", EventType: "document.updated", Path: "a.md", ContentHash: hash([]byte("mine"))}, want: "ws/a.md"},
		{name: "queued push on a move source", entry: &OutboxEntry{DocumentID: "other", Workspace: "ws", EventType: "document.updated", Path: "before.md", ContentHash: hash([]byte("mine"))}, want: "ws/after.md"},
		{name: "queued push under the adopted identity", entry: &OutboxEntry{DocumentID: "d", Workspace: "ws", EventType: "document.updated", Path: "elsewhere.md", ContentHash: hash([]byte("mine"))}, want: "ws/a.md"},
		{name: "queued push on an untouched path", entry: &OutboxEntry{DocumentID: "other", Workspace: "ws", EventType: "document.updated", Path: "unrelated.md", ContentHash: hash([]byte("mine"))}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			if tc.entry != nil {
				if _, err := db.EnqueueOutbox(tc.entry); err != nil {
					t.Fatal(err)
				}
			}
			got, err := db.PendingReturnPush(m)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReconcileReturnEscrowConvergesAndClearsEcho(t *testing.T) {
	db := openTestDB(t)
	base, head := []byte("base"), []byte("head")
	if err := db.UpsertDocument(&Document{DocumentID: "d", Workspace: "ws", Path: "a.md", ContentHash: hash(base), LastSyncedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	snap := syncproto.SnapshotManifest{Workspace: "ws", Cursor: 3, Documents: []syncproto.DocumentSnapshot{{ID: "d", Path: "a.md", Version: 2, Hash: hash(head)}}}
	c := returnTestClient(t, "epoch", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": head})
	m, err := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Operations) != 1 || m.Operations[0].Type != "update" {
		t.Fatalf("expected one update: %+v", m.Operations)
	}
	// An echo of the apply's own write, racing the filesystem commit.
	if _, err = db.EnqueueOutbox(&OutboxEntry{DocumentID: "d", Workspace: "ws", EventType: "document.updated", Path: "a.md", ContentHash: hash(head)}); err != nil {
		t.Fatal(err)
	}
	if err = db.ReconcileReturnEscrow(ReturnEscrow{Manifest: m, Content: map[string][]byte{"d": head}}); err != nil {
		t.Fatal(err)
	}
	if n, err := db.CountOutboxForPath("ws", "a.md"); err != nil || n != 0 {
		t.Fatalf("echo not cleared: n=%d err=%v", n, err)
	}
	// Re-review after a successful adoption must be clean and idempotent.
	again, err := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Operations) != 0 {
		t.Fatalf("re-review after apply is not clean: %+v", again.Operations)
	}
}

func TestReturnEscrowDurableHashVerification(t *testing.T) {
	db := openTestDB(t)
	body := []byte("guest only")
	snap := syncproto.SnapshotManifest{Workspace: "ws", Cursor: 2, Documents: []syncproto.DocumentSnapshot{{ID: "d", Path: "new.md", Version: 1, Hash: hash(body)}}}
	c := returnTestClient(t, "epoch", map[string]syncproto.SnapshotManifest{"ws": snap}, map[string][]byte{"d": body})
	m, _ := BuildReturnManifest(context.Background(), c, db, []string{"ws"})
	path, err := WriteReturnEscrow(context.Background(), c, m, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyReturnEscrow(path, m.Generation); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	b = []byte(strings.Replace(string(b), "Z3Vlc3Qgb25seQ==", "dGFtcGVyZWQ=", 1))
	_ = os.WriteFile(path, b, 0o600)
	if err = VerifyReturnEscrow(path, m.Generation); err == nil {
		t.Fatal("tampered escrow verified")
	}
}
