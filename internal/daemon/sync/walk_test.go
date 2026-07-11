package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeWalkFile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWalkTreePrunes locks in the two contract requirements of the shared walk:
// every Included directory is enumerated, and excluded subtrees (.git/,
// .artifacts/) are pruned rather than descended (the prune proof: visited dir
// count == Included dir count, not total). It also pins the P3 onFile contract:
// files fire under Included dirs, never under pruned ones.
func TestWalkTreePrunes(t *testing.T) {
	root := t.TempDir()

	included := []string{"quick", filepath.Join("inbox", "deep"), filepath.Join("plans", "myplan", "sub"), "chats"}
	excludedSubtrees := []string{filepath.Join(".git", "objects"), filepath.Join(".artifacts", "x")}
	for _, d := range append(append([]string{}, included...), excludedSubtrees...) {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeWalkFile(t, filepath.Join(root, "inbox", "deep", "note.md"))
	writeWalkFile(t, filepath.Join(root, ".artifacts", "x", "briefing.xml"))

	d := NewDocSpace(nil)

	var gotDirs, gotFiles []string
	err := d.WalkTree(root,
		func(_, rel string) error { gotDirs = append(gotDirs, rel); return nil },
		func(_, rel string, _ fs.DirEntry) error { gotFiles = append(gotFiles, rel); return nil },
	)
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}

	wantDirs := map[string]bool{
		"": true, "quick": true, "inbox": true, "inbox/deep": true,
		"plans": true, "plans/myplan": true, "plans/myplan/sub": true, "chats": true,
	}
	gotSet := map[string]bool{}
	for _, r := range gotDirs {
		gotSet[r] = true
		if strings.HasPrefix(r, ".git") || strings.HasPrefix(r, ".artifacts") {
			t.Errorf("excluded dir was visited: %q", r)
		}
	}
	for w := range wantDirs {
		if !gotSet[w] {
			t.Errorf("expected dir %q visited; got %v", w, gotDirs)
		}
	}
	// Prune proof: exactly the Included dirs were visited — .git/objects and
	// .artifacts/x were never entered.
	if len(gotDirs) != len(wantDirs) {
		t.Errorf("visited %d dirs, want %d: %v", len(gotDirs), len(wantDirs), gotDirs)
	}

	sort.Strings(gotFiles)
	if len(gotFiles) != 1 || gotFiles[0] != "inbox/deep/note.md" {
		t.Errorf("onFile = %v, want [inbox/deep/note.md] (never the pruned .artifacts file)", gotFiles)
	}
}
