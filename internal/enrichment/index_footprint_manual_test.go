package enrichment

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/grovetools/core/pkg/models"
)

// TestIndexFootprintManual measures the real retained cost of one note-index
// generation against this machine's live notebook tree. It is a measurement
// harness, not an assertion: it is skipped unless GROVE_INDEX_FOOTPRINT_ROOT
// points at a notebook root (e.g. ~/notebooks/grovetools/notespaces).
func TestIndexFootprintManual(t *testing.T) {
	root := os.Getenv("GROVE_INDEX_FOOTPRINT_ROOT")
	if root == "" {
		t.Skip("set GROVE_INDEX_FOOTPRINT_ROOT to a notebook root to run")
	}

	// Enumerate the content dirs the way the locator would: every
	// <root>/<notespace>/{notes,plans,chats} that exists. Approximate but
	// faithful in shape — the same indexDirFiles walk over the same files.
	type contentDir struct {
		path    string
		dirType string
		ws      string
	}
	var dirs []contentDir
	nsEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, ns := range nsEntries {
		if !ns.IsDir() {
			continue
		}
		nsPath := filepath.Join(root, ns.Name())
		for _, dt := range []string{"notes", "plans", "chats"} {
			p := nsPath
			if dt != "notes" {
				p = filepath.Join(nsPath, dt)
			}
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				dirs = append(dirs, contentDir{path: p, dirType: dt, ws: ns.Name()})
			}
		}
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	index := make(map[string]*models.NoteIndexEntry)
	for _, d := range dirs {
		entries, err := os.ReadDir(d.path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				if len(e.Name()) > 0 && e.Name()[0] == '.' && !shouldDescendDotDir(e.Name()) {
					continue
				}
				indexDirFiles(index, filepath.Join(d.path, e.Name()), d.ws, d.path, d.dirType, e.Name())
				continue
			}
			if ie := indexFileEntry(filepath.Join(d.path, e.Name()), d.ws, d.path, d.dirType); ie != nil {
				index[ie.Path] = ie
			}
		}
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	n := len(index)
	if n == 0 {
		t.Fatalf("indexed nothing under %s", root)
	}

	// Field-level census, so the slimming levers can be ranked.
	var pathBytes, titleBytes, groupBytes, nameBytes, tagBytes, otherBytes int
	for _, e := range index {
		pathBytes += len(e.Path)
		titleBytes += len(e.Title)
		groupBytes += len(e.Group)
		nameBytes += len(e.Name)
		for _, tg := range e.Tags {
			tagBytes += len(tg) + 16
		}
		otherBytes += len(e.ID) + len(e.PlanRef) + len(e.PlanJob) + len(e.Priority) + len(e.Type) + len(e.Workspace) + len(e.ContentDir)
	}

	t.Logf("content dirs      : %d", len(dirs))
	t.Logf("entries           : %d", n)
	t.Logf("retained heap     : %.1f MB (%d bytes/entry)", float64(retained)/(1<<20), retained/int64(n))
	t.Logf("  Path strings    : %.1f MB", float64(pathBytes)/(1<<20))
	t.Logf("  Title strings   : %.1f MB", float64(titleBytes)/(1<<20))
	t.Logf("  Group strings   : %.1f MB", float64(groupBytes)/(1<<20))
	t.Logf("  Name strings    : %.1f MB", float64(nameBytes)/(1<<20))
	t.Logf("  Tags            : %.1f MB", float64(tagBytes)/(1<<20))
	t.Logf("  other strings   : %.1f MB", float64(otherBytes)/(1<<20))
	t.Logf("  struct+map ovhd : %.1f MB", float64(retained-int64(pathBytes+titleBytes+groupBytes+nameBytes+tagBytes+otherBytes))/(1<<20))
}
